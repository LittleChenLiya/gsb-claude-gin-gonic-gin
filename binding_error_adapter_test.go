// Copyright 2025 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

type adapterTestOrder struct {
	Items []adapterTestItem `json:"items" binding:"required,dive"`
}

type adapterTestItem struct {
	Price float64 `json:"price" binding:"required,gt=0"`
}

func newAdapterTestContext(accept, body string) (*Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", MIMEJSON)
	if accept != "" {
		c.Request.Header.Set("Accept", accept)
	}
	return c, w
}

func TestBindingErrorAdapter_ValidationFieldPathJSON(t *testing.T) {
	body := `{"items":[{"price":0},{"price":-5}]}`
	c, w := newAdapterTestContext("application/json", body)

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)

	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, &order, err)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.True(t, c.IsAborted())
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var resp BindingErrorResponse
	require.NoError(t, unmarshalBody(w, &resp))
	assert.Equal(t, http.StatusBadRequest, resp.Status)
	require.Len(t, resp.Errors, 2)
	assert.Equal(t, "items[0].price", resp.Errors[0].Field)
	assert.Equal(t, "required", resp.Errors[0].Code)
	assert.Equal(t, "items[1].price", resp.Errors[1].Field)
	assert.Equal(t, "gt", resp.Errors[1].Code)
}

func TestBindingErrorAdapter_ValidationFieldPathProblemJSON(t *testing.T) {
	body := `{"items":[{"price":0}]}`
	c, w := newAdapterTestContext("application/problem+json", body)

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)

	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, &order, err)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.True(t, c.IsAborted())
	assert.Equal(t, MIMEProblemJSON, w.Header().Get("Content-Type"))

	var pd ProblemDetails
	require.NoError(t, unmarshalBody(w, &pd))
	assert.Equal(t, "https://gin-gonic.com/problems/validation-failed", pd.Type)
	assert.Equal(t, "Validation failed", pd.Title)
	assert.Equal(t, http.StatusBadRequest, pd.Status)
	require.Len(t, pd.Errors, 1)
	assert.Equal(t, "items[0].price", pd.Errors[0].Field)
	assert.Equal(t, "required", pd.Errors[0].Code)
}

func TestBindingErrorAdapter_JSONParseError(t *testing.T) {
	// Malformed JSON: trailing comma, missing value.
	body := `{"items":[{"price":}]}`
	c, w := newAdapterTestContext("application/json", body)

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)

	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, &order, err)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp BindingErrorResponse
	require.NoError(t, unmarshalBody(w, &resp))
	assert.Equal(t, http.StatusBadRequest, resp.Status)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, "", resp.Errors[0].Field)
	assert.Equal(t, "invalid_json", resp.Errors[0].Code)
	assert.Equal(t, "invalid request body", resp.Errors[0].Message)
	// Raw request data must not be leaked.
	assert.NotContains(t, w.Body.String(), "price")
	assert.NotContains(t, w.Body.String(), body)
}

func TestBindingErrorAdapter_InternalError(t *testing.T) {
	body := `{"items":[{"price":1}]}`
	c, w := newAdapterTestContext("application/json", body)

	internalErr := errors.New("something broke internally")
	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, nil, internalErr)

	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var resp BindingErrorResponse
	require.NoError(t, unmarshalBody(w, &resp))
	assert.Equal(t, http.StatusInternalServerError, resp.Status)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, "", resp.Errors[0].Field)
	assert.Equal(t, "internal_error", resp.Errors[0].Code)
	assert.Equal(t, "internal server error", resp.Errors[0].Message)
	// Internal error details must not be leaked.
	assert.NotContains(t, w.Body.String(), "something broke internally")
}

type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.reads++
	return n, err
}

func TestBindingErrorAdapter_BodyReadOnlyOnce(t *testing.T) {
	body := `{"items":[{"price":0}]}`
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	cr := &countingReader{r: strings.NewReader(body)}
	c.Request = httptest.NewRequest(http.MethodPost, "/items", io.NopCloser(cr))
	c.Request.Header.Set("Content-Type", MIMEJSON)
	c.Request.Header.Set("Accept", "application/json")

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)
	readsAfterBind := cr.reads
	require.Greater(t, readsAfterBind, 0, "binding should read the body")

	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, &order, err)
	assert.Equal(t, http.StatusBadRequest, code)

	// The adapter must not read the body again.
	assert.Equal(t, readsAfterBind, cr.reads, "adapter must not read the body again")
}

func TestBindingErrorAdapter_NegotiationPriority(t *testing.T) {
	body := `{"items":[{"price":0}]}`

	tests := []struct {
		name        string
		accept      string
		wantType    string
		wantCode    int
		wantCT      string
	}{
		{
			name:     "default JSON when no Accept",
			accept:   "",
			wantType: "json",
			wantCode: http.StatusBadRequest,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "explicit JSON",
			accept:   "application/json",
			wantType: "json",
			wantCode: http.StatusBadRequest,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "explicit problem+json",
			accept:   "application/problem+json",
			wantType: "problem",
			wantCode: http.StatusBadRequest,
			wantCT:   MIMEProblemJSON,
		},
		{
			name:     "JSON wins with higher q",
			accept:   "application/json, application/problem+json;q=0.9",
			wantType: "json",
			wantCode: http.StatusBadRequest,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "problem+json wins with higher q",
			accept:   "application/problem+json, application/json;q=0.9",
			wantType: "problem",
			wantCode: http.StatusBadRequest,
			wantCT:   MIMEProblemJSON,
		},
		{
			name:     "first listed wins on tie",
			accept:   "application/problem+json, application/json",
			wantType: "problem",
			wantCode: http.StatusBadRequest,
			wantCT:   MIMEProblemJSON,
		},
		{
			name:     "q=0 excludes JSON, problem+json selected",
			accept:   "application/json;q=0, application/problem+json",
			wantType: "problem",
			wantCode: http.StatusBadRequest,
			wantCT:   MIMEProblemJSON,
		},
		{
			name:     "wildcard accepts JSON",
			accept:   "application/*",
			wantType: "json",
			wantCode: http.StatusBadRequest,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "star wildcard with q accepts JSON",
			accept:   "*/*;q=0.5",
			wantType: "json",
			wantCode: http.StatusBadRequest,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "unsupported type returns 406",
			accept:   "application/xml",
			wantType: "none",
			wantCode: http.StatusNotAcceptable,
			wantCT:   "application/json; charset=utf-8",
		},
		{
			name:     "no supported type returns 406",
			accept:   "text/html, application/xml",
			wantType: "none",
			wantCode: http.StatusNotAcceptable,
			wantCT:   "application/json; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newAdapterTestContext(tt.accept, body)
			var order adapterTestOrder
			err := c.ShouldBindJSON(&order)
			require.Error(t, err)

			adapter := NewBindingErrorAdapter()
			code := adapter.Handle(c, &order, err)

			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantCode, w.Code)
			assert.Equal(t, tt.wantCT, w.Header().Get("Content-Type"))

			if tt.wantType == "json" {
				var resp BindingErrorResponse
				require.NoError(t, unmarshalBody(w, &resp))
				assert.Equal(t, "items[0].price", resp.Errors[0].Field)
			} else if tt.wantType == "problem" {
				var pd ProblemDetails
				require.NoError(t, unmarshalBody(w, &pd))
				assert.Equal(t, "items[0].price", pd.Errors[0].Field)
				assert.Contains(t, pd.Type, "validation-failed")
			}
		})
	}
}

func TestBindingErrorAdapter_AlreadyWrittenResponse(t *testing.T) {
	body := `{"items":[{"price":0}]}`
	c, w := newAdapterTestContext("application/json", body)

	// Simulate a handler that already wrote a successful response.
	c.JSON(http.StatusOK, H{"already": "written"})
	require.True(t, c.Writer.Written())

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)

	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, &order, err)

	// The adapter must not overwrite the already-written response.
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "already")
	assert.NotContains(t, w.Body.String(), "items")
}

func TestBindingErrorAdapter_AbortsSubsequentHandlers(t *testing.T) {
	body := `{"items":[{"price":0}]}`
	secondRan := false
	r := New()
	r.POST("/items", func(c *Context) {
		var order adapterTestOrder
		if err := c.ShouldBindJSON(&order); err != nil {
			NewBindingErrorAdapter().Handle(c, &order, err)
			return
		}
		c.JSON(http.StatusOK, H{"ok": true})
	}, func(c *Context) {
		secondRan = true
		c.JSON(http.StatusOK, H{"second": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(body))
	req.Header.Set("Content-Type", MIMEJSON)
	req.Header.Set("Accept", "application/json")
	r.ServeHTTP(w, req)

	assert.False(t, secondRan, "subsequent handler must not run after abort")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "items[0].price")
	assert.NotContains(t, w.Body.String(), "second")
}

func TestBindingErrorAdapter_HandleNilError(t *testing.T) {
	c, w := newAdapterTestContext("application/json", `{}`)
	adapter := NewBindingErrorAdapter()
	code := adapter.Handle(c, nil, nil)
	assert.Equal(t, 0, code)
	assert.False(t, c.Writer.Written())
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestBindingErrorAdapter_IncludeMessagesFalse(t *testing.T) {
	body := `{"items":[{"price":0}]}`
	c, w := newAdapterTestContext("application/json", body)

	var order adapterTestOrder
	err := c.ShouldBindJSON(&order)
	require.Error(t, err)

	adapter := NewBindingErrorAdapter()
	adapter.IncludeMessages = false
	code := adapter.Handle(c, &order, err)

	assert.Equal(t, http.StatusBadRequest, code)
	var resp BindingErrorResponse
	require.NoError(t, unmarshalBody(w, &resp))
	assert.Equal(t, "items[0].price", resp.Errors[0].Field)
	assert.Equal(t, "required", resp.Errors[0].Code)
	assert.Empty(t, resp.Errors[0].Message)
}

func TestBindingErrorAdapter_LoggerCalledForInternalError(t *testing.T) {
	body := `{"items":[{"price":1}]}`
	c, _ := newAdapterTestContext("application/json", body)

	var logged error
	adapter := NewBindingErrorAdapter()
	adapter.Logger = func(err error) { logged = err }

	internalErr := errors.New("internal failure")
	code := adapter.Handle(c, nil, internalErr)

	assert.Equal(t, http.StatusInternalServerError, code)
	require.Error(t, logged)
	assert.Equal(t, "internal failure", logged.Error())
}

func unmarshalBody(w *httptest.ResponseRecorder, v any) error {
	return jsonUnmarshal(w.Body.Bytes(), v)
}

func TestFieldPath(t *testing.T) {
	type address struct {
		Street string `json:"street"`
		City   string `json:"city"`
	}
	type order struct {
		ID      int      `json:"id"`
		Address address  `json:"address"`
		Items   []string `json:"items"`
	}

	tests := []struct {
		namespace string
		want      string
	}{
		{"order.ID", "id"},
		{"order.Address.Street", "address.street"},
		{"order.Address.City", "address.city"},
		{"order.Items[0]", "items[0]"},
		{"order.Items[1]", "items[1]"},
		{"order.Items", "items"},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			got := fieldPath(reflect.TypeOf(order{}), tt.namespace)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFieldPathFallbackToLowerCamel(t *testing.T) {
	// When the struct type is unavailable, fall back to lowerCamel of the
	// struct field name.
	got := fieldPath(nil, "Order.Items[0].Price")
	assert.Equal(t, "items[0].price", got)
}
