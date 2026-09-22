// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeoutStatusCode is the HTTP status code written by the default
// timeout handler when a request exceeds its deadline.
const DefaultTimeoutStatusCode = http.StatusServiceUnavailable

// TimeoutOptions configures the TimeoutWith middleware.
type TimeoutOptions struct {
	// Timeout is the maximum duration the remaining handler chain is
	// allowed to run before the request is aborted. It must be > 0.
	Timeout time.Duration

	// Handler is invoked when the deadline is exceeded and the handler
	// chain has not written any response yet. If nil, a default handler
	// aborts with a unified JSON body {"error":"request timeout"} and
	// status 503 (Service Unavailable).
	Handler HandlerFunc
}

// Timeout returns a middleware that limits how long the remaining handler
// chain may run and propagates cancellation to downstream code.
//
// Semantics:
//   - The request context is wrapped with context.WithTimeout, so binding,
//     streaming loops and background callbacks that select on
//     c.Request.Context().Done() observe both the deadline and client
//     disconnects.
//   - When the deadline is exceeded, the rest of the chain is abandoned and
//     a unified JSON error is written, unless the chain already started
//     writing a response; an already written status code is never
//     overridden.
//   - Responses are buffered until the chain returns or calls Flush. After
//     the first Flush (SSE, long-polling) the status and headers are
//     committed and stream through; a later timeout can then only cancel
//     the context, not change the committed response.
//   - Panics inside the chain are re-thrown in the middleware goroutine so
//     a Recovery middleware registered before Timeout recovers them. A
//     Recovery registered after Timeout recovers them inside the chain.
//   - The timeout timer is always stopped when the middleware returns and
//     the internal goroutine never blocks on completion.
//
// Handlers that keep working after the deadline MUST stop when
// c.Request.Context() is done; otherwise they race with the reuse of the
// *Context for the next request.
func Timeout(timeout time.Duration) HandlerFunc {
	return TimeoutWith(TimeoutOptions{Timeout: timeout})
}

// TimeoutWith returns a Timeout middleware configured with opts.
func TimeoutWith(opts TimeoutOptions) HandlerFunc {
	if opts.Timeout <= 0 {
		panic("gin: timeout must be greater than zero")
	}
	timeoutHandler := opts.Handler
	if timeoutHandler == nil {
		timeoutHandler = defaultTimeoutHandler
	}
	return func(c *Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), opts.Timeout)
		defer cancel() // always releases the timer

		origWriter := c.Writer
		tw := newTimeoutWriter(origWriter)
		c.Writer = tw
		c.Request = c.Request.WithContext(ctx)

		// Buffered so the handler goroutine can always report and exit,
		// even after the middleware has already given up on it.
		done := make(chan any, 1)
		go func() {
			defer func() {
				done <- recover()
			}()
			c.Next()
		}()

		select {
		case rec := <-done:
			c.Writer = origWriter
			if rec != nil {
				// Re-panic so a Recovery middleware registered before
				// Timeout handles it in this goroutine.
				panic(rec)
			}
			tw.FlushBuffer()
		case <-ctx.Done():
			c.Abort()
			c.Writer = origWriter
			if ctx.Err() == context.DeadlineExceeded {
				if tw.Written() {
					// The chain already wrote a response: commit it as-is
					// and never override its status code.
					tw.FlushBuffer()
				} else {
					timeoutHandler(c)
				}
			}
			// On client disconnect (context.Canceled) there is nobody left
			// to answer; just abort and clean up.
		}
	}
}

func defaultTimeoutHandler(c *Context) {
	c.AbortWithStatusJSON(DefaultTimeoutStatusCode, H{"error": "request timeout"})
}

// timeoutWriter buffers the response written by the handler chain until the
// middleware decides whether to commit it. Once Flush is called (streaming
// responses) everything passes through to the underlying writer directly.
type timeoutWriter struct {
	ResponseWriter

	mu      sync.Mutex
	header  http.Header
	buf     bytes.Buffer
	status  int
	size    int
	flushed bool
}

var _ ResponseWriter = (*timeoutWriter)(nil)

func newTimeoutWriter(w ResponseWriter) *timeoutWriter {
	return &timeoutWriter{
		ResponseWriter: w,
		header:         make(http.Header),
		status:         defaultStatus,
		size:           noWritten,
	}
}

func (w *timeoutWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		return w.ResponseWriter.Header()
	}
	return w.header
}

func (w *timeoutWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.writeHeaderLocked(code)
}

func (w *timeoutWriter) writeHeaderLocked(code int) {
	if code > 0 && w.status != code {
		if w.writtenLocked() {
			debugPrint("[WARNING] Headers were already written. Wanted to override status code %d with %d", w.status, code)
			return
		}
		w.status = code
	}
}

// WriteHeaderNow only records that the handler committed a status; the
// bytes reach the client when the buffer is flushed.
func (w *timeoutWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	w.writeHeaderNowLocked()
}

func (w *timeoutWriter) writeHeaderNowLocked() {
	if !w.writtenLocked() {
		w.size = 0
	}
}

func (w *timeoutWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		n, err := w.ResponseWriter.Write(data)
		w.size = w.ResponseWriter.Size()
		return n, err
	}
	w.writeHeaderNowLocked()
	n, err := w.buf.Write(data)
	w.size += n
	return n, err
}

func (w *timeoutWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		n, err := w.ResponseWriter.WriteString(s)
		w.size = w.ResponseWriter.Size()
		return n, err
	}
	w.writeHeaderNowLocked()
	n, err := io.WriteString(&w.buf, s)
	w.size += n
	return n, err
}

func (w *timeoutWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		return w.ResponseWriter.Status()
	}
	return w.status
}

func (w *timeoutWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		return w.ResponseWriter.Size()
	}
	return w.size
}

func (w *timeoutWriter) writtenLocked() bool {
	return w.size != noWritten
}

func (w *timeoutWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writtenLocked()
}

// Flush commits the buffered status, headers and body to the underlying
// writer and switches to pass-through mode for streaming responses.
func (w *timeoutWriter) Flush() {
	w.FlushBuffer()
	w.ResponseWriter.Flush()
}

// FlushBuffer writes the buffered status line, headers and body to the
// underlying writer. It is idempotent.
func (w *timeoutWriter) FlushBuffer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushed {
		return
	}
	w.flushed = true
	dst := w.ResponseWriter.Header()
	for k, vv := range w.header {
		dst[k] = append([]string(nil), vv...)
	}
	if w.writtenLocked() {
		w.ResponseWriter.WriteHeader(w.status)
		w.ResponseWriter.WriteHeaderNow()
	}
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.size = w.ResponseWriter.Size()
	}
	w.buf.Reset()
}

// Hijack switches the writer to pass-through mode and hijacks the
// underlying connection. Hijacked connections are no longer buffered and
// are never touched after the middleware returns.
func (w *timeoutWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	w.flushed = true
	w.mu.Unlock()
	return w.ResponseWriter.Hijack()
}
