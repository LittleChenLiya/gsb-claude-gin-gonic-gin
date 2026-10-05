// Copyright 2026 Manu Martinez-Almeida. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin/codec/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrorReportSameCauseTwoStages verifies that when the same cause is wrapped
// by two separate wrappers and both are registered, the report groups them into a
// single cause while retaining both stage references.
func TestErrorReportSameCauseTwoStages(t *testing.T) {
	cause := errors.New("root cause")
	wrap1 := fmt.Errorf("wrap1: %w", cause)
	wrap2 := fmt.Errorf("wrap2: %w", cause)

	errs := errorMsgs{
		{Err: wrap1, Type: ErrorTypePrivate},
		{Err: wrap2, Type: ErrorTypePrivate},
	}

	report := NewErrorReport(errs)

	require.Len(t, report.Causes, 1, "expected exactly one cause identity")
	causeGroup := report.Causes[0]
	assert.Equal(t, "root cause", causeGroup.Message)
	require.Len(t, causeGroup.Stages, 2, "expected two stage references")

	// Each stage retains its own chain.
	assert.Equal(t, []string{"wrap1: root cause", "root cause"}, causeGroup.Stages[0].Chain)
	assert.Equal(t, []string{"wrap2: root cause", "root cause"}, causeGroup.Stages[1].Chain)

	// Stage indices reflect registration order.
	assert.Equal(t, 0, causeGroup.Stages[0].Index)
	assert.Equal(t, 1, causeGroup.Stages[1].Index)

	// No diagnostics for a healthy chain.
	assert.Empty(t, report.Diagnostics)
}

// TestErrorReportSameCauseChain verifies that a multi-layer wrapper chain
// (wrap2 -> wrap1 -> cause) registered once retains the full chain structure.
func TestErrorReportSameCauseChain(t *testing.T) {
	cause := errors.New("root cause")
	wrap1 := fmt.Errorf("wrap1: %w", cause)
	wrap2 := fmt.Errorf("wrap2: %w", wrap1)

	errs := errorMsgs{
		{Err: wrap2, Type: ErrorTypePrivate},
	}

	report := NewErrorReport(errs)

	require.Len(t, report.Causes, 1)
	causeGroup := report.Causes[0]
	require.Len(t, causeGroup.Stages, 1)
	assert.Equal(t, []string{"wrap2: wrap1: root cause", "wrap1: root cause", "root cause"},
		causeGroup.Stages[0].Chain)
	assert.Empty(t, report.Diagnostics)
}

// TestErrorReportDistinctIdentities verifies that two errors with identical text
// remain two distinct cause identities (pointer identity, not content identity).
func TestErrorReportDistinctIdentities(t *testing.T) {
	errs := errorMsgs{
		{Err: errors.New("same text"), Type: ErrorTypePrivate},
		{Err: errors.New("same text"), Type: ErrorTypePrivate},
	}

	report := NewErrorReport(errs)

	require.Len(t, report.Causes, 2, "identical text must not merge distinct error objects")
	assert.Equal(t, "same text", report.Causes[0].Message)
	assert.Equal(t, "same text", report.Causes[1].Message)
	assert.Len(t, report.Causes[0].Stages, 1)
	assert.Len(t, report.Causes[1].Stages, 1)
}

// cyclicError is an error whose Unwrap chain loops back to itself.
type cyclicError struct {
	msg  string
	next error
}

func (e *cyclicError) Error() string { return e.msg }
func (e *cyclicError) Unwrap() error { return e.next }

// TestErrorReportCycle verifies that a circular unwrap chain terminates instead
// of hanging and that the report records a diagnostic.
func TestErrorReportCycle(t *testing.T) {
	a := &cyclicError{msg: "a"}
	b := &cyclicError{msg: "b", next: a}
	a.next = b

	errs := errorMsgs{
		{Err: a, Type: ErrorTypePrivate},
	}

	done := make(chan *ErrorReport, 1)
	go func() {
		done <- NewErrorReport(errs)
	}()

	select {
	case report := <-done:
		require.NotEmpty(t, report.Diagnostics, "cycle must be marked with a diagnostic")
		assert.Contains(t, report.Diagnostics[0], "cycle")
		// The chain must have stopped, not looped forever.
		require.Len(t, report.Causes, 1)
		assert.NotEmpty(t, report.Causes[0].Stages[0].Chain)
	case <-time.After(2 * time.Second):
		t.Fatal("circular unwrap chain caused the report to hang")
	}
}

// TestErrorReportPublicFilter verifies that the public report only contains
// stages labeled ErrorTypePublic and strips internal chain/diagnostics.
func TestErrorReportPublicFilter(t *testing.T) {
	cause := errors.New("secret cause")
	errs := errorMsgs{
		{Err: fmt.Errorf("private wrap: %w", cause), Type: ErrorTypePrivate},
		{Err: fmt.Errorf("public wrap: %w", cause), Type: ErrorTypePublic, Meta: H{"code": 42}},
		{Err: errors.New("public standalone"), Type: ErrorTypePublic},
	}

	report := NewErrorReport(errs)
	require.Len(t, report.Causes, 2)

	pub := report.Public()

	// Only public-labeled stages survive.
	require.Len(t, pub.Causes, 2)
	for _, c := range pub.Causes {
		for _, st := range c.Stages {
			assert.Nil(t, st.Chain, "public report must strip unwrap chains")
			assert.Nil(t, st.Registered(), "public report must drop the registered error")
		}
	}

	// The public standalone error is present.
	var foundStandalone bool
	for _, c := range pub.Causes {
		for _, st := range c.Stages {
			if st.Message == "public standalone" {
				foundStandalone = true
			}
		}
	}
	assert.True(t, foundStandalone)

	// Diagnostics are internal-only.
	assert.Empty(t, pub.Diagnostics)
}

// TestErrorReportReadOnly verifies that building a report does not mutate or
// clear Context.Errors.
func TestErrorReportReadOnly(t *testing.T) {
	c, _ := CreateTestContext(httptest.NewRecorder())
	c.Error(errors.New("first"))
	c.Error(errors.New("second"))

	require.Len(t, c.Errors, 2)
	_ = c.ErrorReport()
	_ = c.ErrorReport().Public()
	assert.Len(t, c.Errors, 2, "reporting must not clear Errors")
	assert.Equal(t, "first", c.Errors[0].Err.Error())
	assert.Equal(t, "second", c.Errors[1].Err.Error())
}

// TestErrorReportConcurrent runs many concurrent HTTP requests through a gin
// engine. Each request wraps the same cause twice and builds a report. The test
// proves that per-request dedup state is never shared across requests.
func TestErrorReportConcurrent(t *testing.T) {
	gin := New()
	gin.HandleMethodNotAllowed = false
	gin.GET("/report", func(c *Context) {
		cause := errors.New("shared cause")
		c.Error(fmt.Errorf("wrap1: %w", cause))
		c.Error(fmt.Errorf("wrap2: %w", cause))

		report := c.ErrorReport()
		if len(report.Causes) != 1 || len(report.Causes[0].Stages) != 2 {
			c.String(http.StatusInternalServerError, "bad report: causes=%d stages=%d",
				len(report.Causes), len(report.Causes[0].Stages))
			return
		}
		c.String(http.StatusOK, "ok")
	})

	const numRequests = 80
	var wg sync.WaitGroup
	failures := make(chan string, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/report", nil)
			w := httptest.NewRecorder()
			gin.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				failures <- w.Body.String()
			}
		}()
	}
	wg.Wait()
	close(failures)

	for f := range failures {
		t.Errorf("concurrent request failed: %s", f)
	}
}

// TestErrorReportConcurrentDedup hammers NewErrorReport concurrently from many
// goroutines reading the same input slice. It proves the dedup state is owned by
// each report and never shared.
func TestErrorReportConcurrentDedup(t *testing.T) {
	errs := errorMsgs{
		{Err: errors.New("a"), Type: ErrorTypePrivate},
		{Err: errors.New("a"), Type: ErrorTypePrivate},
		{Err: errors.New("b"), Type: ErrorTypePublic},
	}

	const numGoroutines = 64
	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report := NewErrorReport(errs)
			if len(report.Causes) != 3 {
				errCh <- fmt.Errorf("expected 3 causes, got %d", len(report.Causes))
				return
			}
			pub := report.Public()
			if len(pub.Causes) != 1 {
				errCh <- fmt.Errorf("expected 1 public cause, got %d", len(pub.Causes))
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

// TestErrorReportJSON verifies the report serializes to JSON with the expected
// structure and that the public report excludes internal fields.
func TestErrorReportJSON(t *testing.T) {
	cause := errors.New("json cause")
	errs := errorMsgs{
		{Err: fmt.Errorf("wrap: %w", cause), Type: ErrorTypePrivate, Meta: H{"k": "v"}},
	}
	report := NewErrorReport(errs)

	data, err := json.API.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(data), "json cause")
	assert.Contains(t, string(data), "wrap: json cause")
	assert.Contains(t, string(data), "chain")

	// A report with a cycle must serialize its diagnostics.
	cycleErrs := errorMsgs{{Err: &cyclicError{msg: "cyc"}, Type: ErrorTypePrivate}}
	// Build a real cycle: a <-> b.
	ca := &cyclicError{msg: "ca"}
	cb := &cyclicError{msg: "cb", next: ca}
	ca.next = cb
	cycleErrs = errorMsgs{{Err: ca, Type: ErrorTypePrivate}}
	cycleReport := NewErrorReport(cycleErrs)
	cycleData, err := json.API.Marshal(cycleReport)
	require.NoError(t, err)
	assert.Contains(t, string(cycleData), "diagnostics")
	assert.Contains(t, string(cycleData), "cycle")

	// Public report must not leak the cause object or chain.
	pubData, err := json.API.Marshal(report.Public())
	require.NoError(t, err)
	assert.NotContains(t, string(pubData), "chain")
	assert.NotContains(t, string(pubData), "diagnostics")
}

// TestErrorReportString verifies the human-readable internal format renders.
func TestErrorReportString(t *testing.T) {
	cause := errors.New("string cause")
	errs := errorMsgs{
		{Err: fmt.Errorf("wrap: %w", cause), Type: ErrorTypePrivate},
	}
	report := NewErrorReport(errs)
	out := report.String()
	assert.Contains(t, out, "1 cause(s)")
	assert.Contains(t, out, "1 stage(s)")
	assert.Contains(t, out, "string cause")
	assert.Contains(t, out, "wrap: string cause")
	assert.Contains(t, out, "chain:")
}

// ExampleErrorReport_internal demonstrates the internal report: the same cause
// wrapped by two layers is reported as one cause with two stage references, each
// retaining its own unwrap chain. This view is intended for logging and debugging.
func ExampleErrorReport_internal() {
	cause := errors.New("connection refused")
	errs := errorMsgs{
		{Err: fmt.Errorf("dial tcp: %w", cause), Type: ErrorTypePrivate},
		{Err: fmt.Errorf("query failed: %w", fmt.Errorf("read: %w", cause)), Type: ErrorTypePrivate},
	}

	report := NewErrorReport(errs)
	fmt.Println(report.String())
	// Output:
	// Error Report: 1 cause(s), 2 stage(s), 0 diagnostic(s)
	//   Cause #0: "connection refused"
	//     Stage #0 [private]: "dial tcp: connection refused"
	//       chain: dial tcp: connection refused -> connection refused
	//     Stage #1 [private]: "query failed: read: connection refused"
	//       chain: query failed: read: connection refused -> read: connection refused -> connection refused
}

// ExampleErrorReport_external demonstrates the public report: only stages labeled
// ErrorTypePublic are retained, and unwrap chains are stripped so internal error
// structure is not leaked to clients.
func ExampleErrorReport_external() {
	cause := errors.New("db offline")
	errs := errorMsgs{
		{Err: fmt.Errorf("internal detail: %w", cause), Type: ErrorTypePrivate},
		{Err: errors.New("request timeout"), Type: ErrorTypePublic, Meta: H{"retryable": true}},
	}

	report := NewErrorReport(errs)
	pub := report.Public()
	for _, c := range pub.Causes {
		for _, st := range c.Stages {
			fmt.Printf("public: %s meta=%v\n", st.Message, st.Meta)
		}
	}
	// Output:
	// public: request timeout meta=map[retryable:true]
}
