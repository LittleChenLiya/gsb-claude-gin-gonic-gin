// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func performTimeoutRequest(engine *Engine, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestTimeoutElapsed(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))
	engine.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/slow", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
}

func TestTimeoutNotElapsed(t *testing.T) {
	engine := New()
	engine.Use(Timeout(time.Second))
	engine.GET("/fast", func(c *Context) {
		c.Header("X-Custom", "value")
		c.String(http.StatusCreated, "ok")
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/fast", nil))
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "ok", w.Body.String())
	assert.Equal(t, "value", w.Header().Get("X-Custom"))
}

func TestTimeoutCustomResponse(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50*time.Millisecond,
		WithTimeoutStatus(http.StatusGatewayTimeout),
		WithTimeoutResponse(func(c *Context) {
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, H{"error": "deadline exceeded"})
		}),
	))
	engine.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/slow", nil))
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.JSONEq(t, `{"error":"deadline exceeded"}`, w.Body.String())
}

func TestTimeoutCancellationPropagates(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))

	errCh := make(chan error, 1)
	engine.GET("/slow", func(c *Context) {
		// Downstream work (binding, streaming, background callbacks)
		// observes the deadline through the request context.
		<-c.Request.Context().Done()
		errCh <- c.Request.Context().Err()
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/slow", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.ErrorIs(t, <-errCh, context.DeadlineExceeded)
}

func TestTimeoutClientDisconnect(t *testing.T) {
	engine := New()
	engine.Use(Timeout(time.Minute))

	started := make(chan struct{})
	observed := make(chan error, 1)
	engine.GET("/watch", func(c *Context) {
		close(started)
		<-c.Request.Context().Done()
		observed <- c.Request.Context().Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/watch", nil).WithContext(ctx)

	served := make(chan struct{})
	go func() {
		engine.ServeHTTP(httptest.NewRecorder(), req)
		close(served)
	}()

	<-started
	cancel()

	select {
	case err := <-observed:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not observe client disconnect")
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("middleware did not return after client disconnect")
	}
}

func TestTimeoutCommittedResponseNotOverwritten(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))
	engine.GET("/commit", func(c *Context) {
		c.String(http.StatusCreated, "committed")
		// Keep working after committing; the timeout fires while the
		// handler is still busy.
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/commit", nil))
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "committed", w.Body.String())
}

func TestTimeoutCommittedHeaderOnlyNotOverwritten(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))
	engine.GET("/header", func(c *Context) {
		c.Status(http.StatusAccepted)
		c.Writer.WriteHeaderNow()
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/header", nil))
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestTimeoutConcurrent(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))
	engine.GET("/fast", func(c *Context) {
		c.String(http.StatusOK, "fast")
	})
	engine.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})

	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/fast", nil))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "fast", w.Body.String())
		}()
		go func() {
			defer wg.Done()
			w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/slow", nil))
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
		}()
	}
	wg.Wait()
}

func TestTimeoutPanicRecoveredByOuterRecovery(t *testing.T) {
	engine := New()
	engine.Use(Recovery(), Timeout(time.Second))
	engine.GET("/panic", func(c *Context) {
		panic("boom")
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/panic", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTimeoutNestedGroup(t *testing.T) {
	engine := New()
	api := engine.Group("/api", Timeout(50*time.Millisecond))
	v1 := api.Group("/v1")
	v1.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})
	v1.GET("/fast", func(c *Context) {
		c.String(http.StatusOK, "fast")
	})

	w := performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/api/v1/slow", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())

	w = performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/api/v1/fast", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTimeoutGoroutineReleased(t *testing.T) {
	engine := New()
	engine.Use(Timeout(50 * time.Millisecond))

	returned := make(chan struct{})
	engine.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
		close(returned)
	})

	performTimeoutRequest(engine, httptest.NewRequest(http.MethodGet, "/slow", nil))
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler goroutine was not released after timeout")
	}
}
