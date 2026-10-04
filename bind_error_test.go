// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderRequest is the fixture used by every binding test.
type orderItem struct {
	Price float64 `json:"price" binding:"gt=0"`
	Name  string  `json:"name" binding:"required"`
}

type orderRequest struct {
	ID    string      `json:"id" binding:"required"`
	Items []orderItem `json:"items" binding:"required,dive"`
}

// Tags deliberately differ from the Go field names to prove path translation
// uses request (JSON) names, including for structs nested inside array items.
type nestedDetail struct {
	CostAmount float64 `json:"unit_cost" binding:"gt=0"`
}
type nestedItem struct {
	Detail nestedDetail `json:"detail" binding:"required"`
}
type nestedRequest struct {
	LineItems []nestedItem `json:"line_items" binding:"required,dive"`
}

// countingReader records how many Read calls/bytes the binding phase draws from
// the request body. The adapter must never read from it again.
type countingReader struct {
	reader    io.Reader
	readCalls int
	bytesRead int
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	cr.readCalls++
	cr.bytesRead += n
	return n, err
}

func performBindingRequest(t *testing.T, accept, body string) (*httptest.ResponseRecorder, *Context, *countingReader) {
	t.Helper()
	counter := &countingReader{reader: strings.NewReader(body)}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders", counter)
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		// A real client sends one comma-joined Accept header line; httptest's
		// Header.Set canonicalizes comma lists into multiple slice elements, so
		// join them to emulate a single wire value.
		req.Header["Accept"] = []string{accept}
	}

	c, _ := CreateTestContext(w)
	c.Request = req

	var obj orderRequest
	err := c.ShouldBindJSON(&obj)
	require.Error(t, err)
	handled := c.RespondBindingError(&obj, err)
	require.True(t, handled)
	return w, c, counter
}

func TestRespondBindingError_ValidationPath_JSON(t *testing.T) {
	// items[0].price is -3 -> gt=0 fails; items[1].name is missing.
	body := "{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"},{\"price\":2}]}"
	w, _, _ := performBindingRequest(t, "application/json", body)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var resp BindingErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, bindErrorTypeValidation, resp.Error)
	require.Len(t, resp.Fields, 2)
	assert.Equal(t, BindingFieldError{Field: "items[0].price", Rule: "gt", Param: "0"}, resp.Fields[0])
	assert.Equal(t, "items[1].name", resp.Fields[1].Field)
	assert.Equal(t, "required", resp.Fields[1].Rule)

	// No raw request data or Go type names leak into the payload.
	raw := w.Body.String()
	assert.NotContains(t, raw, "-3")
	assert.NotContains(t, raw, "orderItem")
}

func TestRespondBindingError_ValidationPath_ProblemDetails(t *testing.T) {
	body := "{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"}]}"
	w, _, _ := performBindingRequest(t, MIMEProblemDetails, body)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/problem+json; charset=utf-8", w.Header().Get("Content-Type"))

	var problem ProblemDetails
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
	assert.Equal(t, http.StatusBadRequest, problem.Status)
	assert.Equal(t, bindProblemTitleValidation, problem.Title)
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, "items[0].price", problem.Errors[0].Field)
	assert.Equal(t, "gt", problem.Errors[0].Rule)
	assert.Equal(t, "0", problem.Errors[0].Param)
	assert.NotContains(t, w.Body.String(), "-3")
}

func TestRespondBindingError_NestedArrayPath_UsesJSONTags(t *testing.T) {
	// Go path: LineItems[0].Detail.CostAmount -> wire path below.
	body := "{\"line_items\":[{\"detail\":{\"unit_cost\":-1}}]}"
	counter := &countingReader{reader: strings.NewReader(body)}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders", counter)
	req.Header["Accept"] = []string{"application/json"}
	c, _ := CreateTestContext(w)
	c.Request = req

	var obj nestedRequest
	err := c.ShouldBindJSON(&obj)
	require.Error(t, err)
	require.True(t, c.RespondBindingError(&obj, err))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var resp BindingErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Fields, 1)
	assert.Equal(t, "line_items[0].detail.unit_cost", resp.Fields[0].Field)
	assert.Equal(t, "gt", resp.Fields[0].Rule)
}

func TestRespondBindingError_MalformedJSON_Is400(t *testing.T) {
	w, _, _ := performBindingRequest(t, "application/json", "{\"id\":")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	var resp BindingErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, bindErrorTypeMalformed, resp.Error)
	assert.Nil(t, resp.Fields)

	// The codec error text (which may quote offsets/types) must not leak.
	assert.NotContains(t, w.Body.String(), "invalid character")
}

func TestRespondBindingError_UnexpectedError_Is500_Generic(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/orders", nil)

	errBoom := errors.New("db driver *sql.Conn exploded with secret token")
	handled := c.RespondBindingError(&orderRequest{}, errBoom)
	require.True(t, handled)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var resp BindingErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, bindErrorTypeInternal, resp.Error)
	assert.Nil(t, resp.Fields)
	raw := w.Body.String()
	assert.NotContains(t, raw, "sql.Conn")
	assert.NotContains(t, raw, "secret")
}

func TestRespondBindingError_NotAcceptable(t *testing.T) {
	body := "{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"}]}"
	w, _, _ := performBindingRequest(t, "application/xml", body)

	assert.Equal(t, http.StatusNotAcceptable, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, bindErrorTypeNotAcceptable, resp["error"])
}

func TestRespondBindingError_AcceptPriority(t *testing.T) {
	body := "{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"}]}"

	// problem+json has the higher q-value despite appearing second.
	w, _, _ := performBindingRequest(t,
		"application/json;q=0.5, application/problem+json;q=0.9", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/problem+json; charset=utf-8", w.Header().Get("Content-Type"))

	// Tie: first offer (JSON) wins.
	w2, _, _ := performBindingRequest(t,
		"application/problem+json, application/json", body)
	assert.Equal(t, "application/json; charset=utf-8", w2.Header().Get("Content-Type"))

	// Wildcard falls back to the default JSON.
	w3, _, _ := performBindingRequest(t, "*/*", body)
	assert.Equal(t, "application/json; charset=utf-8", w3.Header().Get("Content-Type"))

	// Empty Accept defaults to JSON.
	w4, _, _ := performBindingRequest(t, "", body)
	assert.Equal(t, "application/json; charset=utf-8", w4.Header().Get("Content-Type"))
}

func TestRespondBindingError_BodyReadExactlyOnce(t *testing.T) {
	body := "{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"}]}"
	counter := &countingReader{reader: strings.NewReader(body)}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders", counter)
	req.Header.Set("Content-Type", "application/json")
	c, _ := CreateTestContext(w)
	c.Request = req

	var obj orderRequest
	bindErr := c.ShouldBindJSON(&obj)
	require.Error(t, bindErr)

	// Snapshot the exact moment binding finished. The adapter that follows must
	// not draw a single extra byte from the body.
	callsAfterBind := counter.readCalls
	bytesAfterBind := counter.bytesRead
	require.Greater(t, callsAfterBind, 0, "binding phase should have read the body")

	require.True(t, c.RespondBindingError(&obj, bindErr))
	require.Equal(t, http.StatusBadRequest, w.Code)

	assert.Equal(t, callsAfterBind, counter.readCalls, "adapter must not re-read the body")
	assert.Equal(t, bytesAfterBind, counter.bytesRead, "adapter must not re-read the body")

	// And nothing remains on the body for later handlers either.
	extra, _ := io.Copy(io.Discard, c.Request.Body)
	assert.Equal(t, 0, int(extra))
}

func TestRespondBindingError_AbortsLaterHandlers(t *testing.T) {
	r := New()
	var laterCalled bool
	r.POST("/orders", func(c *Context) {
		var obj orderRequest
		if err := c.ShouldBindJSON(&obj); err != nil {
			c.RespondBindingError(&obj, err)
			return
		}
	}, func(c *Context) {
		laterCalled = true
		c.JSON(http.StatusOK, H{"ok": true})
	})

	body := bytes.NewBufferString("{\"id\":\"x\",\"items\":[{\"price\":-3,\"name\":\"a\"}]}")
	req := httptest.NewRequest(http.MethodPost, "/orders", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.False(t, laterCalled, "later handlers must not run after binding error")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var resp BindingErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, bindErrorTypeValidation, resp.Error)
}

func TestRespondBindingError_DoesNotRewriteCommittedResponse(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/orders", nil)

	c.JSON(http.StatusTeapot, H{"brew": true})
	before := w.Body.String()

	handled := c.RespondBindingError(&orderRequest{}, errors.New("late error"))
	assert.True(t, handled)
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.Equal(t, before, w.Body.String())
}

func TestRespondBindingError_NilError(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	assert.False(t, c.RespondBindingError(&orderRequest{}, nil))
	assert.Equal(t, 0, w.Body.Len(), "no response should be written")
}
