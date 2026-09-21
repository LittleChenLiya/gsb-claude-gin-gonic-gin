// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func performNamedRequest(engine *Engine, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+target, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestNamedRouteNestedGroupsRoundTrip(t *testing.T) {
	r := New()
	var gotID string
	var gotTags []string
	api := r.Group("/api")
	v1 := api.Group("/v1")
	require.NoError(t, v1.NamedGET("user.show", "/users/:id", func(c *Context) {
		gotID = c.Param("id")
		gotTags = c.QueryArray("tag")
		c.Status(http.StatusOK)
	}))

	u, err := r.URLFor("user.show", map[string]string{"id": "42"},
		url.Values{"tag": {"a", "b"}, "q": {"x y"}})
	require.NoError(t, err)
	// Stable query order (keys sorted) and repeated values preserved.
	assert.Equal(t, "/api/v1/users/42?q=x+y&tag=a&tag=b", u)

	w := performNamedRequest(r, http.MethodGet, u)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "42", gotID)
	assert.Equal(t, []string{"a", "b"}, gotTags)
}

func TestNamedRouteCatchAllRoundTrip(t *testing.T) {
	r := New()
	var gotPath string
	require.NoError(t, r.NamedGET("file", "/files/*path", func(c *Context) {
		gotPath = c.Param("path")
		c.Status(http.StatusOK)
	}))

	u, err := r.URLFor("file", map[string]string{"path": "a/b/c.txt"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/files/a/b/c.txt", u)

	w := performNamedRequest(r, http.MethodGet, u)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/a/b/c.txt", gotPath)

	// Catch-all values are escaped segment by segment.
	u, err = r.URLFor("file", map[string]string{"path": "a b/中文/%2F"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/files/a%20b/%E4%B8%AD%E6%96%87/%252F", u)

	// Dot segments must not escape the prefix.
	for _, v := range []string{"..", "a/../b", "./a", "a/./b"} {
		_, err = r.URLFor("file", map[string]string{"path": v}, nil)
		assert.ErrorIs(t, err, ErrRouteUnsafeParam, "value %q", v)
	}
}

func TestNamedRouteParamEscaping(t *testing.T) {
	r := New()
	require.NoError(t, r.NamedGET("item", "/items/:id", func(c *Context) { c.Status(http.StatusOK) }))

	cases := map[string]string{
		"a/b":  "/items/a%2Fb",
		"中文":   "/items/%E4%B8%AD%E6%96%87",
		"%2F":  "/items/%252F",
		"a b":  "/items/a%20b",
		"a?b":  "/items/a%3Fb",
		"100%": "/items/100%25",
	}
	for value, want := range cases {
		u, err := r.URLFor("item", map[string]string{"id": value}, nil)
		require.NoError(t, err, "value %q", value)
		assert.Equal(t, want, u)
	}

	// Empty and dot-segment values are rejected.
	_, err := r.URLFor("item", map[string]string{"id": ""}, nil)
	assert.ErrorIs(t, err, ErrRouteEmptyParam)
	_, err = r.URLFor("item", map[string]string{"id": ".."}, nil)
	assert.ErrorIs(t, err, ErrRouteUnsafeParam)
	_, err = r.URLFor("item", map[string]string{"id": "."}, nil)
	assert.ErrorIs(t, err, ErrRouteUnsafeParam)
}

func TestNamedRouteEscapedSlashRoundTrip(t *testing.T) {
	// Round-tripping a parameter that contains an escaped slash requires
	// UseRawPath so matching happens on the escaped path.
	r := New()
	r.UseRawPath = true
	r.UnescapePathValues = true
	var gotID string
	require.NoError(t, r.NamedGET("item", "/items/:id", func(c *Context) {
		gotID = c.Param("id")
		c.Status(http.StatusOK)
	}))

	u, err := r.URLFor("item", map[string]string{"id": "a/b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/items/a%2Fb", u)

	w := performNamedRequest(r, http.MethodGet, u)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "a/b", gotID)

	// A literal "%2F" value is encoded once more and unescapes back to "%2F".
	u, err = r.URLFor("item", map[string]string{"id": "%2F"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/items/%252F", u)
	w = performNamedRequest(r, http.MethodGet, u)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "%2F", gotID)
}

func TestNamedRouteParamErrors(t *testing.T) {
	r := New()
	require.NoError(t, r.NamedGET("item", "/items/:id", func(c *Context) {}))

	_, err := r.URLFor("item", nil, nil)
	assert.ErrorIs(t, err, ErrRouteMissingParam)

	_, err = r.URLFor("item", map[string]string{"id": "1", "extra": "x"}, nil)
	assert.ErrorIs(t, err, ErrRouteExtraParam)

	_, err = r.URLFor("no.such.route", nil, nil)
	assert.ErrorIs(t, err, ErrRouteNameNotFound)
}

func TestNamedRouteConflictLeavesNoResidue(t *testing.T) {
	r := New()
	require.NoError(t, r.NamedGET("item", "/items/:id", func(c *Context) {
		c.String(http.StatusOK, "get:"+c.Param("id"))
	}))

	// Same name on a different method must not overwrite the registration.
	err := r.NamedPOST("item", "/other/:id", func(c *Context) {})
	assert.ErrorIs(t, err, ErrRouteNameConflict)
	// Empty name is rejected before anything is registered.
	assert.ErrorIs(t, r.NamedGET("", "/empty", func(c *Context) {}), ErrRouteEmptyName)

	// The failed registration did not insert the route...
	w := performNamedRequest(r, http.MethodPost, "/other/1")
	assert.Equal(t, http.StatusNotFound, w.Code)
	// ...the old route still matches...
	w = performNamedRequest(r, http.MethodGet, "/items/7")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "get:7", w.Body.String())
	// ...and the name still resolves to the original route.
	u, err := r.URLFor("item", map[string]string{"id": "7"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/items/7", u)
	// No residual name from the empty-name attempt.
	_, err = r.URLFor("", nil, nil)
	assert.ErrorIs(t, err, ErrRouteNameNotFound)
}

func TestNamedRouteMultipleEngines(t *testing.T) {
	r1 := New()
	r2 := New()
	require.NoError(t, r1.NamedGET("home", "/one", func(c *Context) {}))
	require.NoError(t, r2.NamedGET("home", "/two", func(c *Context) {}))

	u1, err := r1.URLFor("home", nil, nil)
	require.NoError(t, err)
	u2, err := r2.URLFor("home", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "/one", u1)
	assert.Equal(t, "/two", u2)
}

func TestNamedRouteUnnamedCoexistence(t *testing.T) {
	r := New()
	r.GET("/plain/:id", func(c *Context) { c.String(http.StatusOK, "plain:"+c.Param("id")) })
	require.NoError(t, r.NamedGET("named", "/named/:id", func(c *Context) {}))

	// Unnamed routes keep matching and are not addressable by name.
	w := performNamedRequest(r, http.MethodGet, "/plain/1")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "plain:1", w.Body.String())
	_, err := r.URLFor("/plain/:id", map[string]string{"id": "1"}, nil)
	assert.ErrorIs(t, err, ErrRouteNameNotFound)

	// Redirects still work for named routes.
	w = performNamedRequest(r, http.MethodGet, "/named/1/")
	assert.Equal(t, http.StatusMovedPermanently, w.Code)
}

func TestNamedRouteConcurrentURLFor(t *testing.T) {
	r := New()
	require.NoError(t, r.NamedGET("item", "/items/:id/files/*path", func(c *Context) {}))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				u, err := r.URLFor("item",
					map[string]string{"id": "a/b", "path": "x/y"},
					url.Values{"k": {"v1", "v2"}})
				assert.NoError(t, err)
				assert.Equal(t, "/items/a%2Fb/files/x/y?k=v1&k=v2", u)
			}
		}()
	}
	wg.Wait()
}