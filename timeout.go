// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// DefaultTimeoutStatus is the HTTP status code written when a request
// exceeds its timeout and no response has been committed yet.
const DefaultTimeoutStatus = http.StatusServiceUnavailable

// timeoutOptions holds the configurable behavior of the Timeout middleware.
type timeoutOptions struct {
	status   int
	response HandlerFunc
}

// TimeoutOption customizes the Timeout middleware.
type TimeoutOption func(*timeoutOptions)

// WithTimeoutStatus sets the status code used for the unified timeout
// response. It is ignored once the handler has committed a response.
func WithTimeoutStatus(status int) TimeoutOption {
	return func(o *timeoutOptions) {
		o.status = status
	}
}

// WithTimeoutResponse replaces the default unified JSON timeout body.
// The handler runs on the original request context after the deadline
// expired, so it must not block. It is ignored once the handler has
// committed a response.
func WithTimeoutResponse(h HandlerFunc) TimeoutOption {
	return func(o *timeoutOptions) {
		o.response = h
	}
}

// Timeout returns a middleware that bounds the execution time of the
// remaining handler chain to d.
//
// Semantics:
//
//   - The remaining chain runs in a separate goroutine against a copy of
//     the Context whose Request carries a context.WithTimeout derived
//     from the incoming request context. Binding, streaming handlers and
//     background callbacks that honor c.Request.Context() observe the
//     cancellation when the deadline passes or the client disconnects.
//   - While the chain runs, all writes are buffered. On completion the
//     buffered status, headers and body are flushed to the real writer.
//   - On timeout, if the handler already committed a response (a status
//     or body was written), the committed response is flushed unchanged
//     and the status code is never overwritten. Otherwise a unified JSON
//     body {"error":"request timeout"} is written with
//     DefaultTimeoutStatus (or the status set via WithTimeoutStatus).
//   - Panics raised inside the chain are re-thrown on the request
//     goroutine so an outer Recovery middleware handles them. Register
//     Recovery before Timeout.
//   - The middleware applies to every route of the RouterGroup it is
//     attached to, including nested groups; the budget is per request
//     and starts when the middleware runs.
//   - Because the response is buffered, Timeout is not suitable for SSE
//     or long-polling endpoints: nothing reaches the client until the
//     handler returns or the deadline expires. Leave streaming routes
//     outside the timed group and have them watch
//     c.Request.Context().Done() themselves.
//   - Handlers that ignore context cancellation keep running in the
//     background after the timeout response is sent; always honor
//     c.Request.Context() to let the middleware reclaim its goroutine.
//     The internal timer is released as soon as the request finishes.
func Timeout(d time.Duration, opts ...TimeoutOption) HandlerFunc {
	o := timeoutOptions{status: DefaultTimeoutStatus}
	for _, opt := range opts {
		opt(&o)
	}
	if o.response == nil {
		status := o.status
		o.response = func(c *Context) {
			c.AbortWithStatusJSON(status, H{"error": "request timeout"})
		}
	}

	return func(c *Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()

		// Work on a copy that continues the current handler chain: the
		// goroutine below must never touch the original Context once
		// this handler returns. c.Copy() cannot be used here because it
		// resets the handler chain for out-of-request use.
		cp := &Context{
			Request:  c.Request.WithContext(ctx),
			engine:   c.engine,
			handlers: c.handlers,
			index:    c.index,
			fullPath: c.fullPath,
			sameSite: c.sameSite,
		}
		cp.Params = make(Params, len(c.Params))
		copy(cp.Params, c.Params)
		if c.Keys != nil {
			c.mu.RLock()
			cp.Keys = make(map[any]any, len(c.Keys))
			for k, v := range c.Keys {
				cp.Keys[k] = v
			}
			c.mu.RUnlock()
		}
		if c.Errors != nil {
			cp.Errors = make(errorMsgs, len(c.Errors))
			copy(cp.Errors, c.Errors)
		}

		buffer := newTimeoutWriter(c.Writer)
		cp.Writer = buffer

		panicChan := make(chan any, 1)
		done := make(chan struct{})
		go func() {
			defer func() {
				if p := recover(); p != nil {
					panicChan <- p
				}
				close(done)
			}()
			cp.Next()
		}()

		select {
		case <-done:
			select {
			case p := <-panicChan:
				// Re-panic on the request goroutine so Recovery
				// registered before Timeout can handle it.
				panic(p)
			default:
			}
			buffer.flushTo(c.Writer)
		case <-ctx.Done():
			if buffer.Written() {
				// A response was already committed: deliver it
				// unchanged, never overwrite the status code.
				buffer.flushTo(c.Writer)
				break
			}
			c.Abort()
			o.response(c)
		}
		// Stop the outer chain: the remaining handlers already ran (or
		// were abandoned) inside the goroutine above.
		c.Abort()
	}
}

// timeoutWriter buffers everything the handler chain writes so the
// Timeout middleware can decide whether to deliver the response or the
// unified timeout body.
type timeoutWriter struct {
	ResponseWriter
	header      http.Header
	body        bytes.Buffer
	code        int
	wroteHeader bool
}

func newTimeoutWriter(w ResponseWriter) *timeoutWriter {
	return &timeoutWriter{
		ResponseWriter: w,
		header:         make(http.Header),
		code:           http.StatusOK,
	}
}

func (w *timeoutWriter) Header() http.Header {
	return w.header
}

func (w *timeoutWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.code = code
}

func (w *timeoutWriter) WriteHeaderNow() {}

func (w *timeoutWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *timeoutWriter) WriteString(s string) (int, error) {
	return w.body.WriteString(s)
}

func (w *timeoutWriter) Status() int {
	return w.code
}

func (w *timeoutWriter) Size() int {
	return w.body.Len()
}

func (w *timeoutWriter) Written() bool {
	return w.wroteHeader || w.body.Len() > 0
}

// Flush is a no-op: buffered data only reaches the client via flushTo.
func (w *timeoutWriter) Flush() {}

// flushTo replays the buffered headers, status and body onto w.
func (w *timeoutWriter) flushTo(dst ResponseWriter) {
	dstHeader := dst.Header()
	for k, vs := range w.header {
		dstHeader[k] = vs
	}
	dst.WriteHeader(w.code)
	if w.body.Len() > 0 {
		_, _ = dst.Write(w.body.Bytes())
	}
}
