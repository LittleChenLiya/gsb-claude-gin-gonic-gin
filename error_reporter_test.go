// Copyright 2026 Manu Martinez-Almeida. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrappedError is the controlled fixture used instead of fmt.Errorf("%w") so
// the tests do not depend on the exact allocation behavior of the stdlib.
type wrappedError struct {
	msg   string
	cause error
}

func (e *wrappedError) Error() string { return e.msg + ": " + e.cause.Error() }
func (e *wrappedError) Unwrap() error { return e.cause }

func wrap(msg string, cause error) error {
	return &wrappedError{msg: msg, cause: cause}
}

func newTestReporterContext(t *testing.T) (*Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := CreateTestContext(w)
	return c, w
}

// TestReporterSameCauseTwoStages covers the core acceptance case: the same
// cause wrapped at two different layers is reported as one cause with two
// stage references, while equal-but-distinct errors keep two identities.
func TestReporterSameCauseTwoStages(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter()

	cause := errors.New("disk full")
	// Layer 1 (private) and layer 2 (public) both wrap the same cause object.
	c.Error(wrap("service failed", cause))
	c.Error(wrap("handler failed", cause)).SetType(ErrorTypePublic)
	// Equal text, distinct objects: must remain a second identity.
	c.Error(errors.New("disk full")).SetType(ErrorTypePublic)

	before := len(c.Errors)
	report := reporter.Report(c)

	// Reporting is read-only: errors are neither cleared nor mutated.
	assert.Len(t, c.Errors, before)
	assert.Len(t, c.Errors, 3)

	groups := report.Causes()
	require.Len(t, groups, 2)

	shared := groups[0]
	assert.Same(t, cause, shared.Cause())
	require.Len(t, shared.Stages, 2)
	assert.Equal(t, StagePrivate, shared.Stages[0].Stage)
	assert.Equal(t, 0, shared.Stages[0].Index)
	assert.Equal(t, StagePublic, shared.Stages[1].Stage)
	assert.Equal(t, 1, shared.Stages[1].Index)
	// Each wrapping keeps its own chain information.
	assert.Equal(t, "service failed: disk full", shared.Stages[0].RegisteredError().Error())
	assert.Equal(t, "handler failed: disk full", shared.Stages[1].RegisteredError().Error())
	assert.Len(t, shared.Stages[0].Chain(), 2)
	assert.Len(t, shared.Stages[1].Chain(), 2)
	// The shared cause is reachable at the end of every chain.
	assert.Same(t, cause, shared.Stages[0].Chain()[1])
	assert.Same(t, cause, shared.Stages[1].Chain()[1])

	separate := groups[1]
	assert.NotSame(t, cause, separate.Cause())
	require.Len(t, separate.Stages, 1)
	assert.Equal(t, StagePublic, separate.Stages[0].Stage)
}

// TestReporterPublicOnlyAuthorizedLabels checks external output never leaks
// internal messages or cause structure.
func TestReporterPublicOnlyAuthorizedLabels(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter()

	cause := errors.New("db password = hunter2")
	c.Error(wrap("repo failed", cause)) // private: hidden
	c.Error(wrap("bind failed", cause)).SetType(ErrorTypeBind)
	c.Error(wrap("handler failed", cause)).SetType(ErrorTypePublic)

	report := reporter.Report(c)
	public := report.Public()
	require.Len(t, public, 1)
	assert.Equal(t, "request failed", public[0].Label)

	body, err := json.Marshal(public)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "hunter2")
	assert.NotContains(t, string(body), "repo failed")
	assert.NotContains(t, string(body), "bind failed")
	assert.JSONEq(t, `[{"label":"request failed"}]`, string(body))

	// Internal view keeps the cause structure for all three registrations.
	internal := report.Internal()
	require.Len(t, internal, 1)
	assert.Equal(t, "db password = hunter2", internal[0].Cause)
	require.Len(t, internal[0].Stages, 3)
	assert.Equal(t, StagePrivate, internal[0].Stages[0].Stage)
	assert.Equal(t, StageBind, internal[0].Stages[1].Stage)
	assert.Equal(t, StagePublic, internal[0].Stages[2].Stage)
}

// TestReporterUnauthorizedStageOmittedFromPublic checks a stage without an
// authorized label is omitted from public output even when flagged public
// through a custom label set.
func TestReporterUnauthorizedStageOmittedFromPublic(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter(WithStageLabels(map[Stage]string{StagePublic: "request failed"}))

	c.Error(errors.New("secret bind detail")).SetType(ErrorTypeBind)
	c.Error(errors.New("secret")).SetType(ErrorTypePublic)

	report := reporter.Report(c)
	_, bindAuthorized := reporter.Label(StageBind)
	assert.False(t, bindAuthorized)
	public := report.Public()
	require.Len(t, public, 1)
	assert.Equal(t, "request failed", public[0].Label)
}

// TestReporterIdentitySemantics checks pointer identity versus value identity.
func TestReporterIdentitySemantics(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter()

	ptrA := errors.New("same text")
	ptrB := errors.New("same text")
	const valueErr sentinelErr = "value same text"

	c.Error(wrap("w", ptrA))
	c.Error(wrap("w", ptrB))
	// A comparable sentinel used twice is the same value and merges.
	c.Error(wrap("w", valueErr))
	c.Error(wrap("w", valueErr))

	report := reporter.Report(c)
	groups := report.Causes()
	require.Len(t, groups, 3, "distinct pointers stay apart; equal sentinel values merge")
	assert.Len(t, groups[0].Stages, 1)
	assert.Len(t, groups[1].Stages, 1)
	assert.Len(t, groups[2].Stages, 2)
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

// cycleError builds an Unwrap chain that points back into itself.
type cycleError struct {
	name string
	next error
}

func (e *cycleError) Error() string { return e.name }
func (e *cycleError) Unwrap() error { return e.next }

// TestReporterCycleDoesNotHang makes sure a cyclic unwrap chain terminates and
// is annotated with a diagnostic.
func TestReporterCycleDoesNotHang(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter()

	a := &cycleError{name: "a"}
	b := &cycleError{name: "b"}
	cc := &cycleError{name: "c"}
	a.next = b
	b.next = cc
	cc.next = b // b -> c -> b

	c.Error(a).SetType(ErrorTypePrivate)

	// A healthy chain finishes in microseconds; fail loudly if it ever hangs.
	done := make(chan *ErrorReport, 1)
	go func() {
		done <- reporter.Report(c)
	}()
	var report *ErrorReport
	select {
	case report = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Report hung on cyclic chain")
	}

	groups := report.Causes()
	require.Len(t, groups, 1)
	stages := groups[0].Stages
	require.Len(t, stages, 1)
	cycle := stages[0].Cycle()
	require.NotNil(t, cycle)
	assert.Equal(t, 1, cycle.BackToDepth)
	assert.Equal(t, 3, cycle.StartDepth)
	assert.Contains(t, cycle.Message, "cycle")
	// Traversal stopped: chain holds a, b, c only.
	assert.Len(t, stages[0].Chain(), 3)

	// Diagnostic survives into the internal view.
	internal := report.Internal()
	require.NotNil(t, internal[0].Stages[0].Cycle)
}

// TestReporterCycleRegistrationDoesNotHang makes sure registering a cyclic
// chain through Context.Error cannot hang either (it must not use an
// unguarded errors.As).
func TestReporterCycleRegistrationDoesNotHang(t *testing.T) {
	c, _ := newTestReporterContext(t)

	a := &cycleError{name: "a"}
	b := &cycleError{name: "b"}
	a.next = b
	b.next = a

	done := make(chan struct{})
	go func() {
		_ = c.Error(a)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Context.Error hung on a cyclic chain")
	}
	require.Len(t, c.Errors, 1)
}

// TestReporterSelfLoop checks the trivial one-node cycle too.
func TestReporterSelfLoop(t *testing.T) {
	c, _ := newTestReporterContext(t)
	a := &cycleError{name: "self"}
	a.next = a
	c.Error(a)

	report := NewErrorReporter().Report(c)
	stages := report.Causes()[0].Stages
	require.Len(t, stages, 1)
	cycle := stages[0].Cycle()
	require.NotNil(t, cycle)
	assert.Equal(t, 0, cycle.BackToDepth)
	assert.Equal(t, 1, cycle.StartDepth)
	assert.Len(t, stages[0].Chain(), 1)
}

// TestReporterDoesNotClearErrors asserts repeated reports do not clear state.
func TestReporterDoesNotClearErrors(t *testing.T) {
	c, _ := newTestReporterContext(t)
	reporter := NewErrorReporter()
	c.Error(errors.New("one"))
	c.Error(errors.New("two"))

	reporter.Report(c)
	reporter.Report(c)
	assert.Equal(t, []string{"one", "two"}, c.Errors.Errors())
}

// TestReporterConcurrentRequestsNoSharedState exercises the reporter with
// concurrent httptest requests: deduplication state must never leak between
// requests, even though they share one reporter instance.
func TestReporterConcurrentRequestsNoSharedState(t *testing.T) {
	reporter := NewErrorReporter()
	router := New()

	type result struct {
		groups int
		stages int
		public int
		errors int
		label  string
	}

	var (
		resultsMu sync.Mutex
		results   []result
	)

	router.Use(func(c *Context) {
		c.Next()

		report := reporter.Report(c)
		res := result{groups: len(report.Causes()), errors: len(c.Errors)}
		stages := 0
		for _, group := range report.Causes() {
			stages += len(group.Stages)
		}
		res.stages = stages
		if public := report.Public(); len(public) > 0 {
			res.public = len(public)
			res.label = public[0].Label
		}
		resultsMu.Lock()
		results = append(results, res)
		resultsMu.Unlock()
	})

	// Each request gets its own cause objects: two wrappings of one cause
	// (private + public), plus a content-equal but distinct error.
	router.GET("/boom", func(c *Context) {
		cause := errors.New("disk full")
		c.Error(wrap("service failed", cause))
		c.Error(wrap("handler failed", cause)).SetType(ErrorTypePublic)
		c.Error(errors.New("disk full")).SetType(ErrorTypePublic)
		c.Status(http.StatusAccepted)
	})

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/boom", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			assert.Equal(t, http.StatusAccepted, w.Code)
		}()
	}
	wg.Wait()

	require.Len(t, results, n)
	for _, res := range results {
		assert.Equal(t, 2, res.groups, "no cross-request or in-request over-merge")
		assert.Equal(t, 3, res.stages)
		assert.Equal(t, 2, res.public, "both public registrations surface")
		assert.Equal(t, 3, res.errors, "Errors kept after read-only Report")
		assert.Equal(t, "request failed", res.label)
	}
}

// TestReporterConcurrentRequestsDistinctSameContent makes sure the same
// reporter used by parallel requests with identical content does not merge
// identities across request boundaries.
func TestReporterConcurrentRequestsDistinctSameContent(t *testing.T) {
	reporter := NewErrorReporter()
	router := New()

	var (
		countsMu sync.Mutex
		groupsN  []int
	)
	router.Use(func(c *Context) {
		c.Next()
		countsMu.Lock()
		groupsN = append(groupsN, len(reporter.Report(c).Causes()))
		countsMu.Unlock()
	})
	router.GET("/err", func(c *Context) {
		// Every request registers two errors with identical text but fresh
		// objects. A shared dedup map would collapse these over time.
		c.Error(errors.New("same message"))
		c.Error(errors.New("same message"))
		c.Status(http.StatusOK)
	})

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/err", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
		}()
	}
	wg.Wait()

	require.Len(t, groupsN, n)
	for _, n := range groupsN {
		assert.Equal(t, 2, n, "per-request dedup state must not leak or merge")
	}
}

// TestReporterReRegisteredGinError checks that re-passing an existing gin
// *Error to Context.Error preserves its type and still groups by its cause.
func TestReporterReRegisteredGinError(t *testing.T) {
	c, _ := newTestReporterContext(t)
	cause := errors.New("boom")
	first := c.Error(wrap("outer", cause)).SetType(ErrorTypeRender)
	// Re-register the exact same gin *Error object: errors.As-style detection
	// must find it and keep the render type.
	second := c.Error(first)
	assert.Same(t, first, second)
	assert.True(t, second.IsType(ErrorTypeRender))

	groups := NewErrorReporter().Report(c).Causes()
	require.Len(t, groups, 1)
	assert.Same(t, cause, groups[0].Cause())
	require.Len(t, groups[0].Stages, 2)
	assert.Equal(t, StageRender, groups[0].Stages[0].Stage)
	assert.Equal(t, StageRender, groups[0].Stages[1].Stage)
}

// TestReporterEmptyReport covers a context without errors.
func TestReporterEmptyReport(t *testing.T) {
	c, _ := newTestReporterContext(t)
	report := NewErrorReporter().Report(c)
	assert.Nil(t, report.Causes())
	assert.Nil(t, report.Public())
	assert.Nil(t, report.Internal())
}
