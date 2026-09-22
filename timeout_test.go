// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func performTimeoutRequest(router *Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	router.ServeHTTP(w, req)
	return w
}

func TestTimeoutNormalCompletion(t *testing.T) {
	router := New()
	router.Use(Timeout(time.Second))
	router.GET("/", func(c *Context) {
		c.Header("X-Test", "1")
		c.JSON(http.StatusOK, H{"msg": "ok"})
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1", w.Header().Get("X-Test"))
	assert.JSONEq(t, `{"msg":"ok"}`, w.Body.String())
}

func TestTimeoutExceededWritesUnifiedJSON(t *testing.T) {
	router := New()
	router.Use(Timeout(50 * time.Millisecond))
	router.GET("/", func(c *Context) {
		<-c.Request.Context().Done() // long-poll style: wait until canceled
	})

	start := time.Now()
	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, DefaultTimeoutStatusCode, w.Code)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestTimeoutCustomHandler(t *testing.T) {
	router := New()
	router.Use(TimeoutWith(TimeoutOptions{
		Timeout: 50 * time.Millisecond,
		Handler: func(c *Context) {
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, H{"error": "slow downstream"})
		},
	}))
	router.GET("/", func(c *Context) {
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.JSONEq(t, `{"error":"slow downstream"}`, w.Body.String())
}

func TestTimeoutDoesNotOverrideWrittenResponse(t *testing.T) {
	router := New()
	router.Use(Timeout(50 * time.Millisecond))
	router.GET("/", func(c *Context) {
		c.String(http.StatusCreated, "partial")
		<-c.Request.Context().Done() // keep working after committing
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "partial", w.Body.String())
}

func TestTimeoutClientDisconnect(t *testing.T) {
	router := New()
	router.Use(Timeout(time.Hour)) // deadline must not fire first
	started := make(chan struct{})
	canceled := make(chan struct{})
	router.GET("/", func(c *Context) {
		close(started)
		<-c.Request.Context().Done()
		close(canceled)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	<-started
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("middleware did not return after client disconnect")
	}
	<-canceled // cancellation propagated into the handler
	assert.Equal(t, 0, w.Body.Len(), "nothing may be written after a client disconnect")
}

func TestTimeoutStreamingFlushKeepsCommittedStatus(t *testing.T) {
	router := New()
	router.Use(Timeout(50 * time.Millisecond))
	router.GET("/events", func(c *Context) {
		c.Writer.WriteString("data: one\n\n")
		c.Writer.Flush() // SSE: commit headers and stream
		<-c.Request.Context().Done()
	})

	w := performTimeoutRequest(router, http.MethodGet, "/events")
	assert.Equal(t, http.StatusOK, w.Code, "timeout must not override a committed streaming response")
	assert.Equal(t, "data: one\n\n", w.Body.String())
}

func TestTimeoutNestedGroups(t *testing.T) {
	router := New()
	api := router.Group("/api", Timeout(50*time.Millisecond))
	v1 := api.Group("/v1")
	v1.GET("/ok", func(c *Context) {
		c.String(http.StatusOK, "ok")
	})
	v1.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})
	router.GET("/outside", func(c *Context) {
		c.String(http.StatusOK, "outside")
	})

	assert.Equal(t, http.StatusOK, performTimeoutRequest(router, http.MethodGet, "/api/v1/ok").Code)
	w := performTimeoutRequest(router, http.MethodGet, "/api/v1/slow")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
	assert.Equal(t, http.StatusOK, performTimeoutRequest(router, http.MethodGet, "/outside").Code)
}

func TestTimeoutPanicRecoveredByOuterRecovery(t *testing.T) {
	router := New()
	router.Use(RecoveryWithWriter(io.Discard), Timeout(time.Second))
	router.GET("/", func(c *Context) {
		panic("boom")
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTimeoutPanicRecoveredByInnerRecovery(t *testing.T) {
	router := New()
	router.Use(Timeout(time.Second), RecoveryWithWriter(io.Discard))
	router.GET("/", func(c *Context) {
		panic("boom")
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTimeoutConcurrentRequests(t *testing.T) {
	router := New()
	router.Use(Timeout(50 * time.Millisecond))
	router.GET("/fast", func(c *Context) {
		c.String(http.StatusOK, "fast")
	})
	router.GET("/slow", func(c *Context) {
		<-c.Request.Context().Done()
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := performTimeoutRequest(router, http.MethodGet, "/fast")
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "fast", w.Body.String())
		}()
		go func() {
			defer wg.Done()
			w := performTimeoutRequest(router, http.MethodGet, "/slow")
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
		}()
	}
	wg.Wait()
}

func TestTimeoutCancellationReachesBackgroundCallback(t *testing.T) {
	router := New()
	router.Use(Timeout(50 * time.Millisecond))
	finished := make(chan struct{})
	router.GET("/", func(c *Context) {
		ctx := c.Request.Context()
		go func() {
			defer close(finished)
			<-ctx.Done() // background callback observes the deadline
		}()
		<-ctx.Done()
	})

	w := performTimeoutRequest(router, http.MethodGet, "/")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine was not canceled; goroutine leak")
	}
}

func TestTimeoutInvalidOptionPanics(t *testing.T) {
	assert.Panics(t, func() { TimeoutWith(TimeoutOptions{}) })
	assert.Panics(t, func() { TimeoutWith(TimeoutOptions{Timeout: -time.Second}) })
	require.NotPanics(t, func() { Timeout(time.Millisecond) })
}

func TestTimeoutWriterInterface(t *testing.T) {
	var _ ResponseWriter = (*timeoutWriter)(nil)
	var _ http.Flusher = (*timeoutWriter)(nil)
}
