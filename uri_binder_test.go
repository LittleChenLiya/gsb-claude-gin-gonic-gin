// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type articleParams struct {
	ID   int `uri:"id,required"`
	Page int `uri:"page,required"`
}

var articleBinder = MustNewUriBinder(articleParams{})

func newParamContext(params Params) *Context {
	c, _ := CreateTestContext(httptest.NewRecorder())
	c.Request, _ = http.NewRequest(http.MethodGet, "/anything/at/all", nil)
	c.Params = params
	return c
}

// Acceptance: id=12 is valid but page=bad is not. The destination must keep
// its original values entirely; with both valid the two fields update together.
func TestUriBinderAcceptanceAtomicUpdate(t *testing.T) {
	c := newParamContext(Params{
		{Key: "id", Value: "12"},
		{Key: "page", Value: "bad"},
	})

	dst := articleParams{ID: 101, Page: 202}
	err := c.ShouldBindUriWith(articleBinder, &dst)
	require.Error(t, err)

	var pe *binding.UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, binding.UriErrorInvalid, pe.Kind)
	assert.Equal(t, "page", pe.Param)
	assert.Contains(t, err.Error(), "page")

	// Original values fully preserved despite id parsing fine.
	assert.Equal(t, articleParams{ID: 101, Page: 202}, dst)

	// Both valid: updated together in one call.
	c2 := newParamContext(Params{
		{Key: "id", Value: "12"},
		{Key: "page", Value: "3"},
	})
	dst2 := articleParams{ID: 101, Page: 202}
	require.NoError(t, c2.ShouldBindUriWith(articleBinder, &dst2))
	assert.Equal(t, articleParams{ID: 12, Page: 3}, dst2)
}

// The legacy binder must keep working unchanged.
func TestUriBinderDoesNotBreakShouldBindUri(t *testing.T) {
	r := New()
	r.GET("/users/:id/:page", func(c *Context) {
		var p articleParams
		err := c.ShouldBindUri(&p)
		require.NoError(t, err)
		assert.Equal(t, 42, p.ID)
		assert.Equal(t, 7, p.Page)
		c.Status(http.StatusOK)
	})
	w := PerformRequest(r, http.MethodGet, "/users/42/7")
	assert.Equal(t, http.StatusOK, w.Code)
}

// Two fields bound to the same route parameter name fail compilation, before
// any request is served.
func TestUriBinderCompileConflictOnDuplicateID(t *testing.T) {
	type twoIDs struct {
		ID     int `uri:"id,required"`
		Other  int `uri:"id"`
		Unused int `uri:"unused"`
	}
	_, err := NewUriBinder(twoIDs{})
	require.Error(t, err)

	var pe *binding.UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, binding.UriErrorConflict, pe.Kind)
	assert.Equal(t, "id", pe.Param)
	assert.Contains(t, err.Error(), "id")

	assert.Panics(t, func() { MustNewUriBinder(twoIDs{}) })
}

type aliasParams struct {
	Article int `uri:"article,required,alias=id|article_id"`
}

func TestUriBinderFieldAlias(t *testing.T) {
	b := MustNewUriBinder(aliasParams{})

	for _, raw := range []string{"article", "id", "article_id"} {
		c := newParamContext(Params{{Key: raw, Value: "55"}})
		var p aliasParams
		require.NoError(t, c.ShouldBindUriWith(b, &p), raw)
		assert.Equal(t, 55, p.Article, raw)
	}

	// None of the accepted names present -> missing, primary name reported.
	c := newParamContext(Params{{Key: "other", Value: "55"}})
	var p aliasParams
	err := c.ShouldBindUriWith(b, &p)
	require.Error(t, err)
	var pe *binding.UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, binding.UriErrorMissing, pe.Kind)
	assert.Equal(t, "article", pe.Param)
}

type rangeParams struct {
	N int8 `uri:"n,required"`
}

func TestUriBinderIntegerRange(t *testing.T) {
	b := MustNewUriBinder(rangeParams{})
	c := newParamContext(Params{{Key: "n", Value: "300"}})
	var p rangeParams
	err := c.ShouldBindUriWith(b, &p)
	require.Error(t, err)
	var pe *binding.UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "n", pe.Param)
	assert.Contains(t, err.Error(), "out of range")
	assert.ErrorIs(t, err, strconv.ErrRange)
}

type customBinderType struct {
	// converted via binding.BindUnmarshaler
	V int
}

var errCustomBoom = errors.New("boom")

func (t *customBinderType) UnmarshalParam(s string) error {
	if s == "boom" {
		return errCustomBoom
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	t.V = n
	return nil
}

type customHolder struct {
	C customBinderType `uri:"c,required"`
	N int              `uri:"n,required"`
}

func TestUriBinderCustomErrorAndAtomicity(t *testing.T) {
	b := MustNewUriBinder(customHolder{})
	c := newParamContext(Params{
		{Key: "c", Value: "boom"},
		{Key: "n", Value: "9"},
	})
	dst := customHolder{}
	dst.N = 71
	err := c.ShouldBindUriWith(b, &dst)
	require.Error(t, err)
	var pe *binding.UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "c", pe.Param)
	assert.ErrorIs(t, err, errCustomBoom)
	assert.Equal(t, 71, dst.N) // n=9 was never committed
}

func TestUriBinderBindDirectlyWithParams(t *testing.T) {
	var p articleParams
	require.NoError(t, articleBinder.Bind(&p, Params{
		{Key: "id", Value: "8"},
		{Key: "page", Value: "1"},
	}))
	assert.Equal(t, 8, p.ID)
	assert.Equal(t, 1, p.Page)
}

// End-to-end through the router: Params come straight from tree matching; the
// URL is never re-parsed by the binder.
func TestUriBinderRouterIntegration(t *testing.T) {
	r := New()
	r.GET("/articles/:id/pages/:page", func(c *Context) {
		var p articleParams
		if err := c.ShouldBindUriWith(articleBinder, &p); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.String(http.StatusOK, "id=%d,page=%d", p.ID, p.Page)
	})

	ok := PerformRequest(r, http.MethodGet, "/articles/12/pages/3")
	assert.Equal(t, http.StatusOK, ok.Code)
	assert.Equal(t, "id=12,page=3", ok.Body.String())

	bad := PerformRequest(r, http.MethodGet, "/articles/12/pages/bad")
	assert.Equal(t, http.StatusBadRequest, bad.Code)
	assert.Contains(t, bad.Body.String(), "page")
}
