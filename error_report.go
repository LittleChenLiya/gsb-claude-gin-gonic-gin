// Copyright 2026 Manu Martinez-Almeida. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin/codec/json"
)

// ErrorStage is a read-only reference to a single error registration in Context.Errors.
// It preserves the registration's own message, type, meta and unwrap chain even when
// multiple registrations share the same root cause identity.
type ErrorStage struct {
	// Index is the position of the registered error in Context.Errors.
	Index int `json:"index"`
	// Message is the registered error's message.
	Message string `json:"message"`
	// Type is the registered error's type flags.
	Type ErrorType `json:"type"`
	// Meta is the registered error's metadata.
	Meta any `json:"meta,omitempty"`
	// Chain is the unwrap path from the registered error down to the root cause.
	// The first element is the registered error's message; the last is the cause's.
	Chain []string `json:"chain"`

	err *Error
}

// Registered returns the original *Error that produced this stage.
// It returns nil for stages produced from the public report.
func (s *ErrorStage) Registered() *Error {
	return s.err
}

// ErrorCause groups error stages that share the same root cause object identity.
// Two registrations are grouped together only when their unwrap chains terminate
// at the very same error object (pointer identity for pointer errors). Repeatedly
// wrapping the same cause therefore yields one cause with multiple stage references
// without losing any stage's own chain, type or meta.
type ErrorCause struct {
	// Cause is the root error at which all grouped stages terminate.
	// It is excluded from JSON output to avoid leaking internal structure.
	Cause error `json:"-"`
	// Message is the root cause's message.
	Message string `json:"message"`
	// Stages are the registrations sharing this cause identity, in registration order.
	Stages []*ErrorStage `json:"stages"`
}

// ErrorReport is an organized, read-only view of Context.Errors.
// It groups registrations by root-cause identity while preserving every
// registration stage's own unwrap chain, type and meta. Building a report never
// mutates Context.Errors, and every report owns its own dedup state, so reports
// built from concurrent requests never interfere with each other.
type ErrorReport struct {
	// Causes are the grouped error causes, ordered by first registration.
	Causes []*ErrorCause `json:"causes"`
	// Diagnostics collects non-fatal observations such as unwrap cycles.
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// errorIdentity returns a stable identity key for an error object.
// Pointer errors are identified by their pointer address, so two distinct errors
// with identical messages remain two identities. Value errors fall back to a
// type+content key.
func errorIdentity(err error) string {
	if err == nil {
		return ""
	}
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return fmt.Sprintf("p:%d", v.Pointer())
	default:
		return fmt.Sprintf("v:%T:%v", err, err)
	}
}

// unwrapChain follows the single-error unwrap path and returns the message of
// every error visited together with the terminal error object. If a cycle is
// detected, unwrapping stops immediately (so callers never hang) and the returned
// boolean is true.
func unwrapChain(err error) (chain []string, terminal error, cyclic bool) {
	visited := map[string]bool{}
	current := err
	for current != nil {
		id := errorIdentity(current)
		if visited[id] {
			return chain, current, true
		}
		visited[id] = true
		chain = append(chain, current.Error())
		next := errors.Unwrap(current)
		if next == nil {
			return chain, current, false
		}
		current = next
	}
	return nil, nil, false
}

// NewErrorReport organizes the given registered errors by root-cause identity
// and registration stage. It never mutates the input slice and owns all of its
// dedup state, making it safe to build reports from concurrent requests.
func NewErrorReport(errs []*Error) *ErrorReport {
	report := &ErrorReport{}
	causeByID := map[string]*ErrorCause{}

	for stageIdx, registered := range errs {
		if registered == nil {
			continue
		}
		stage := &ErrorStage{
			Index: stageIdx,
			err:   registered,
		}
		if registered.Err != nil {
			stage.Message = registered.Err.Error()
			stage.Type = registered.Type
			stage.Meta = registered.Meta
		}
		chain, causeErr, cyclic := unwrapChain(registered.Err)
		stage.Chain = chain
		if cyclic {
			report.Diagnostics = append(report.Diagnostics,
				fmt.Sprintf("unwrap cycle detected at error stage #%d", stageIdx))
		}
		if causeErr == nil {
			causeErr = registered.Err
		}

		causeID := errorIdentity(causeErr)
		cause, ok := causeByID[causeID]
		if !ok {
			cause = &ErrorCause{Cause: causeErr}
			if causeErr != nil {
				cause.Message = causeErr.Error()
			}
			causeByID[causeID] = cause
			report.Causes = append(report.Causes, cause)
		}
		cause.Stages = append(cause.Stages, stage)
	}
	return report
}

// ErrorReport builds an organized, read-only report of all errors attached to
// this context. Errors are grouped by root-cause identity while preserving each
// registration stage's own chain, type and meta. Building a report never clears
// Context.Errors, and each report owns its own dedup state, so concurrent requests
// never interfere.
func (c *Context) ErrorReport() *ErrorReport {
	return NewErrorReport(c.Errors)
}

// Public returns a read-only report containing only stages labeled ErrorTypePublic.
// The public report strips unwrap chains and diagnostics so that internal error
// structure is not leaked to clients. It never mutates the receiver.
func (r *ErrorReport) Public() *ErrorReport {
	pub := &ErrorReport{}
	for _, cause := range r.Causes {
		var stages []*ErrorStage
		for _, st := range cause.Stages {
			if st.err != nil && st.err.IsType(ErrorTypePublic) {
				stageCopy := *st
				stageCopy.Chain = nil
				stageCopy.err = nil
				stages = append(stages, &stageCopy)
			}
		}
		if len(stages) > 0 {
			causeCopy := *cause
			causeCopy.Stages = stages
			causeCopy.Cause = nil
			pub.Causes = append(pub.Causes, &causeCopy)
		}
	}
	return pub
}

// JSON returns the report as a JSON-serializable value.
func (r *ErrorReport) JSON() any {
	return r
}

// MarshalJSON implements the json.Marshaler interface.
func (r *ErrorReport) MarshalJSON() ([]byte, error) {
	type plainReport ErrorReport
	return json.API.Marshal((*plainReport)(r))
}

// String renders a human-readable view of the report for internal logging.
func (r *ErrorReport) String() string {
	var b strings.Builder
	stages := 0
	for _, cause := range r.Causes {
		stages += len(cause.Stages)
	}
	fmt.Fprintf(&b, "Error Report: %d cause(s), %d stage(s), %d diagnostic(s)\n",
		len(r.Causes), stages, len(r.Diagnostics))
	for i, cause := range r.Causes {
		fmt.Fprintf(&b, "  Cause #%d: %q\n", i, cause.Message)
		for _, st := range cause.Stages {
			typeName := "private"
			if st.Type&ErrorTypePublic > 0 {
				typeName = "public"
			}
			fmt.Fprintf(&b, "    Stage #%d [%s]: %q\n", st.Index, typeName, st.Message)
			if len(st.Chain) > 0 {
				fmt.Fprintf(&b, "      chain: %s\n", strings.Join(st.Chain, " -> "))
			}
			if st.Meta != nil {
				fmt.Fprintf(&b, "      meta: %v\n", st.Meta)
			}
		}
	}
	for i, d := range r.Diagnostics {
		fmt.Fprintf(&b, "  Diagnostic #%d: %s\n", i, d)
	}
	return b.String()
}
