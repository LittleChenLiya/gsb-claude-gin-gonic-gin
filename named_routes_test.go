// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func performNamedRequest(router *Engine, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	router.ServeHTTP(w, req)
	return w
}

// newRoundTripEngine builds an engine configured so that generated URLs
// containing escaped slashes round-trip back to the same route.
func newRoundTripEngine() *Engine {
	router := New()
	router.UseRawPath = true
	router.UnescapePathValues = true
	return router
}

func TestNamedRouteURLForNestedGroups(t *testing.T) {
	router := New()
	api := router.Group("/api")
	v1 := api.Group("/v1")
	require.NoError(t, v1.NamedGET("user.show", "/users/:id", func(c *Context) {}))
	require.NoError(t, v1.NamedGET("file.download", "/files/*filepath", func(c *Context) {}))
	require.NoError(t, router.NamedGET("home", "/", func(c *Context) {}))

	u, err := router.URLFor("user.show", Params{{Key: "id", Value: "42"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/users/42", u)

	u, err = router.URLFor("file.download", Params{{Key: "filepath", Value: "a/b/c.txt"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/files/a/b/c.txt", u)

	u, err = router.URLFor("home", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "/", u)
}

func TestNamedRouteRoundTrip(t *testing.T) {
	router := newRoundTripEngine()
	v1 := router.Group("/api").Group("/v1")
	require.NoError(t, v1.NamedGET("user.show", "/users/:id", func(c *Context) {
		c.String(http.StatusOK, c.Param("id"))
	}))
	require.NoError(t, v1.NamedGET("file.download", "/files/*filepath", func(c *Context) {
		c.String(http.StatusOK, c.Param("filepath"))
	}))

	params := []struct {
		value string
		want  string
	}{
		{"simple", "/api/v1/users/simple"},
		{"a/b", "/api/v1/users/a%2Fb"},
		{"中文", "/api/v1/users/%E4%B8%AD%E6%96%87"},
		{"%2F", "/api/v1/users/%252F"},
		{"a b?c", "/api/v1/users/a%20b%3Fc"},
		{"..", "/api/v1/users/%2E%2E"},
	}
	for _, tc := range params {
		u, err := router.URLFor("user.show", Params{{Key: "id", Value: tc.value}}, nil)
		require.NoError(t, err, "value %q", tc.value)
		assert.Equal(t, tc.want, u, "value %q", tc.value)

		w := performNamedRequest(router, u)
		require.Equal(t, http.StatusOK, w.Code, "GET %s", u)
		assert.Equal(t, tc.value, w.Body.String(), "GET %s", u)
	}

	catchAll := []struct {
		value string
		want  string
	}{
		{"a/b/c.txt", "/api/v1/files/a/b/c.txt"},
		{"中文/文件.txt", "/api/v1/files/%E4%B8%AD%E6%96%87/%E6%96%87%E4%BB%B6.txt"},
		{"..", "/api/v1/files/%2E%2E"},
		{"a/../../b", "/api/v1/files/a/%2E%2E/%2E%2E/b"},
		{"100%/done", "/api/v1/files/100%25/done"},
	}
	for _, tc := range catchAll {
		u, err := router.URLFor("file.download", Params{{Key: "filepath", Value: tc.value}}, nil)
		require.NoError(t, err, "value %q", tc.value)
		assert.Equal(t, tc.want, u, "value %q", tc.value)

		w := performNamedRequest(router, u)
		require.Equal(t, http.StatusOK, w.Code, "GET %s", u)
		// Gin's catch-all parameter value keeps its leading slash.
		assert.Equal(t, "/"+tc.value, w.Body.String(), "GET %s", u)
	}
}

func TestNamedRouteRoundTripDefaultConfig(t *testing.T) {
	// Without UseRawPath, values that do not need escaping inside a segment
	// still round-trip, because URL.Path is already unescaped.
	router := New()
	require.NoError(t, router.NamedGET("user.show", "/users/:id", func(c *Context) {
		c.String(http.StatusOK, c.Param("id"))
	}))

	u, err := router.URLFor("user.show", Params{{Key: "id", Value: "中文"}}, nil)
	require.NoError(t, err)
	w := performNamedRequest(router, u)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "中文", w.Body.String())
}

func TestNamedRouteQueryValues(t *testing.T) {
	router := New()
	require.NoError(t, router.NamedGET("search", "/search", func(c *Context) {
		c.String(http.StatusOK, c.Query("q")+"|"+c.DefaultQuery("page", "")+"|"+c.QueryArray("tag")[0]+","+c.QueryArray("tag")[1])
	}))

	query := url.Values{}
	query.Add("tag", "b")
	query.Add("tag", "a")
	query.Add("q", "中文")
	u, err := router.URLFor("search", nil, query)
	require.NoError(t, err)
	// Keys are sorted; repeated keys keep all values in order.
	assert.Equal(t, "/search?q=%E4%B8%AD%E6%96%87&tag=b&tag=a", u)

	w := performNamedRequest(router, u)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "中文||b,a", w.Body.String())
}

func TestNamedRouteErrors(t *testing.T) {
	router := New()
	require.NoError(t, router.NamedGET("user.show", "/users/:id", func(c *Context) {}))
	require.NoError(t, router.NamedGET("file.download", "/files/*filepath", func(c *Context) {}))

	_, err := router.URLFor("user.show", nil, nil)
	assert.ErrorIs(t, err, ErrURLMissingParam)

	_, err = router.URLFor("user.show", Params{{Key: "id", Value: "1"}, {Key: "extra", Value: "x"}}, nil)
	assert.ErrorIs(t, err, ErrURLExtraParams)

	_, err = router.URLFor("user.show", Params{{Key: "id", Value: ""}}, nil)
	assert.ErrorIs(t, err, ErrURLEmptyParam)

	_, err = router.URLFor("file.download", Params{{Key: "filepath", Value: ""}}, nil)
	assert.ErrorIs(t, err, ErrURLEmptyParam)

	_, err = router.URLFor("no.such.route", nil, nil)
	assert.ErrorIs(t, err, ErrRouteNotFound)

	assert.ErrorIs(t, router.NamedGET("", "/x", func(c *Context) {}), ErrRouteNameEmpty)
	assert.ErrorIs(t, router.NamedHandle("bad.method", "get", "/x", func(c *Context) {}), ErrInvalidHTTPMethod)
}

func TestNamedRouteConflict(t *testing.T) {
	router := New()
	require.NoError(t, router.NamedGET("item", "/items/:id", func(c *Context) {
		c.String(http.StatusOK, "old")
	}))

	// Same name, different method: still a conflict, names are unique per Engine.
	err := router.NamedPOST("item", "/other/:id", func(c *Context) {})
	assert.ErrorIs(t, err, ErrRouteNameConflict)

	// Same name, different path: conflict, and the failed registration must
	// leave neither a route in the tree nor a name in the index.
	err = router.NamedGET("item", "/other/:id", func(c *Context) {})
	assert.ErrorIs(t, err, ErrRouteNameConflict)

	w := performNamedRequest(router, "/other/1")
	assert.Equal(t, http.StatusNotFound, w.Code)

	// The original route and name are untouched.
	w = performNamedRequest(router, "/items/1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "old", w.Body.String())

	u, err := router.URLFor("item", Params{{Key: "id", Value: "1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/items/1", u)

	// A name that failed validation is not reserved either.
	assert.ErrorIs(t, router.NamedGET("", "/tmp", func(c *Context) {}), ErrRouteNameEmpty)
	_, err = router.URLFor("", nil, nil)
	assert.ErrorIs(t, err, ErrRouteNotFound)
}

func TestNamedRouteMultiEngine(t *testing.T) {
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
	router := New()
	router.GET("/plain/:id", func(c *Context) { c.String(http.StatusOK, "plain") })
	require.NoError(t, router.NamedGET("named", "/named/:id", func(c *Context) { c.String(http.StatusOK, "named") }))

	w := performNamedRequest(router, "/plain/1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "plain", w.Body.String())

	// Trailing-slash redirect still works for named routes.
	w = performNamedRequest(router, "/named/1/")
	assert.Equal(t, http.StatusMovedPermanently, w.Code)
}

func TestNamedRouteConcurrentURLFor(t *testing.T) {
	router := New()
	require.NoError(t, router.NamedGET("user.show", "/users/:id", func(c *Context) {}))
	require.NoError(t, router.NamedGET("file.download", "/files/*filepath", func(c *Context) {}))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				u, err := router.URLFor("user.show", Params{{Key: "id", Value: "a/b"}}, url.Values{"x": {"1"}})
				if err != nil || u != "/users/a%2Fb?x=1" {
					t.Errorf("unexpected result: %q, %v", u, err)
					return
				}
				u, err = router.URLFor("file.download", Params{{Key: "filepath", Value: "a/../b"}}, nil)
				if err != nil || u != "/files/a/%2E%2E/b" {
					t.Errorf("unexpected result: %q, %v", u, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNamedRouteErrorUnwrap(t *testing.T) {
	router := New()
	require.NoError(t, router.NamedGET("a", "/a", func(c *Context) {}))
	err := router.NamedGET("a", "/b", func(c *Context) {})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRouteNameConflict))
	assert.Contains(t, err.Error(), "a")
}
