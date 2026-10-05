// Copyright 2026 Manu Martinez-Almeida. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
)

// Stage identifies the registration phase of an error stored on Context.Errors.
type Stage string

const (
	// StageBind is the phase in which Context.Bind registers errors (ErrorTypeBind).
	StageBind Stage = "bind"
	// StageRender is the phase in which rendering registers errors (ErrorTypeRender).
	StageRender Stage = "render"
	// StagePrivate is the default phase used by Context.Error (ErrorTypePrivate).
	StagePrivate Stage = "private"
	// StagePublic is the phase of errors explicitly flagged as ErrorTypePublic.
	// Only this phase is eligible for public output.
	StagePublic Stage = "public"
	// StageAny is used for errors that carry no known phase flag.
	StageAny Stage = "any"
)

// defaultStageLabels are the labels authorized for public output per phase.
var defaultStageLabels = map[Stage]string{
	StageBind:    "invalid request",
	StageRender:  "invalid response",
	StagePrivate: "internal server error",
	StagePublic:  "request failed",
	StageAny:     "error",
}

// maxUnwrapDepth bounds unwalking so a pathological chain can never hang a report.
const maxUnwrapDepth = 10_000

// ErrorReporter builds read-only reports from Context.Errors.
//
// A reporter is safe for concurrent use by multiple requests: it holds only
// immutable configuration (the authorized labels). All deduplication state is
// local to a single Report call, so concurrent requests never share it.
type ErrorReporter struct {
	labels map[Stage]string
}

// ReporterOption configures an ErrorReporter.
type ReporterOption func(*ErrorReporter)

// WithStageLabels overrides the authorized public labels per phase. Stages that
// are missing from the map are not authorized for public output.
func WithStageLabels(labels map[Stage]string) ReporterOption {
	return func(r *ErrorReporter) {
		custom := make(map[Stage]string, len(labels))
		for stage, label := range labels {
			custom[stage] = label
		}
		r.labels = custom
	}
}

// NewErrorReporter returns a reporter with the default authorized stage labels,
// optionally adjusted by the given options.
func NewErrorReporter(options ...ReporterOption) *ErrorReporter {
	r := &ErrorReporter{labels: make(map[Stage]string, len(defaultStageLabels))}
	for stage, label := range defaultStageLabels {
		r.labels[stage] = label
	}
	for _, option := range options {
		option(r)
	}
	return r
}

// Label reports the authorized public label for a stage.
func (r *ErrorReporter) Label(stage Stage) (string, bool) {
	label, ok := r.labels[stage]
	return label, ok
}

// Report organizes Context.Errors by error object identity and registration
// phase. Errors that wrap the same cause (through errors.Unwrap) are grouped
// together while every wrapping keeps its own phase reference and chain.
//
// Report is strictly read-only: it never clears or mutates Context.Errors.
func (r *ErrorReporter) Report(c *Context) *ErrorReport {
	report := &ErrorReport{}
	// groups is call-local state: it must never escape to another request.
	groups := make(map[any]*CauseGroup)
	var order []any

	for i, entry := range c.Errors {
		chain := walkChain(entry)
		stage := stageOf(entry.Type)
		label, authorized := r.labels[stage]
		ref := StageRef{
			Stage:      stage,
			Index:      i,
			Label:      label,
			authorized: authorized,
			entry:      entry,
			chain:      chain,
		}
		report.refs = append(report.refs, ref)

		// A malformed entry with no wrapped error cannot be grouped by cause;
		// give it an isolated group instead of merging on a nil identity.
		var rootID any
		if chain.root.err == nil {
			rootID = opaqueIdentity{id: atomic.AddUint64(&opaqueIdentityCounter, 1)}
		} else {
			rootID = chain.root.id
		}
		group, ok := groups[rootID]
		if !ok {
			group = &CauseGroup{root: chain.root.err}
			groups[rootID] = group
			order = append(order, rootID)
		}
		group.Stages = append(group.Stages, ref)
	}

	report.groups = make([]*CauseGroup, 0, len(order))
	for _, id := range order {
		report.groups = append(report.groups, groups[id])
	}
	return report
}

// stageOf derives the registration phase from an Error's type flags.
func stageOf(typ ErrorType) Stage {
	switch {
	case typ&ErrorTypeBind != 0:
		return StageBind
	case typ&ErrorTypeRender != 0:
		return StageRender
	case typ&ErrorTypePublic != 0:
		return StagePublic
	case typ&ErrorTypePrivate != 0:
		return StagePrivate
	default:
		return StageAny
	}
}

// StageRef references one registered error and the phase in which it was added.
type StageRef struct {
	// Stage is the phase in which the error was registered.
	Stage Stage
	// Index is the position of the error in Context.Errors.
	Index int
	// Label is the authorized public label for the phase (empty if unauthorized).
	Label string

	authorized bool
	entry      *Error
	chain      *walkedChain
}

// RegisteredError returns the *Error registered on Context.Errors.
// The returned value must not be mutated.
func (s StageRef) RegisteredError() *Error { return s.entry }

// Chain returns the unwrap chain from the registered error down to its cause.
// If the chain contains a cycle, traversal stops at the first repeated error.
func (s StageRef) Chain() []error {
	nodes := s.chain.nodes
	out := make([]error, len(nodes))
	for i, node := range nodes {
		out[i] = node.err
	}
	return out
}

// Cycle returns the diagnostic for a cyclic unwrap chain, or nil if the
// chain is acyclic.
func (s StageRef) Cycle() *CycleDiagnostic { return s.chain.cycle }

// CauseGroup gathers every registration that unwraps to the same cause object.
type CauseGroup struct {
	// Stages lists the phase references, in registration order.
	Stages []StageRef

	root error
}

// Cause returns the deepest error shared by every stage reference.
func (g *CauseGroup) Cause() error { return g.root }

// Cycles returns diagnostics for every stage reference whose chain cycles.
func (g *CauseGroup) Cycles() []CycleDiagnostic {
	var out []CycleDiagnostic
	for _, ref := range g.Stages {
		if ref.chain.cycle != nil {
			out = append(out, *ref.chain.cycle)
		}
	}
	return out
}

// ErrorReport is the read-only result of organizing Context.Errors.
type ErrorReport struct {
	groups []*CauseGroup
	refs   []StageRef
}

// Causes returns the cause groups in order of first registration.
// The returned slice is a copy for read-only use.
func (r *ErrorReport) Causes() []CauseGroup {
	if len(r.groups) == 0 {
		return nil
	}
	out := make([]CauseGroup, len(r.groups))
	for i, group := range r.groups {
		stages := make([]StageRef, len(group.Stages))
		copy(stages, group.Stages)
		out[i] = CauseGroup{Stages: stages, root: group.root}
	}
	return out
}

// PublicError is a public-safe payload: it contains only an authorized label,
// never the underlying error message or cause chain.
type PublicError struct {
	Label string `json:"label"`
}

// Public returns the errors safe for external output, in registration order.
// Only errors registered in the public phase with an authorized label are
// included; internal messages and cause structure are never exposed.
func (r *ErrorReport) Public() []PublicError {
	var out []PublicError
	for _, ref := range r.refs {
		if ref.Stage == StagePublic && ref.authorized {
			out = append(out, PublicError{Label: ref.Label})
		}
	}
	return out
}

// InternalChainNode is one node of an internal cause chain.
type InternalChainNode struct {
	Depth int    `json:"depth"`
	Error string `json:"error"`
}

// CycleDiagnostic describes a cyclic errors.Unwrap chain that was cut short.
type CycleDiagnostic struct {
	StartDepth  int    `json:"startDepth"`
	BackToDepth int    `json:"backToDepth"`
	Message     string `json:"message"`
}

// InternalStage is the internal view of one registration.
type InternalStage struct {
	Stage Stage               `json:"stage"`
	Error string              `json:"error"`
	Chain []InternalChainNode `json:"chain"`
	Cycle *CycleDiagnostic    `json:"cycle,omitempty"`
}

// InternalCause is the internal view of one cause group.
type InternalCause struct {
	Cause  string          `json:"cause"`
	Stages []InternalStage `json:"stages"`
}

// Internal returns the full diagnostic view: root cause messages, every phase
// reference and the unwrap chains (including cycle diagnostics). This view is
// intended for logs and trusted systems only.
func (r *ErrorReport) Internal() []InternalCause {
	if len(r.groups) == 0 {
		return nil
	}
	out := make([]InternalCause, 0, len(r.groups))
	for _, group := range r.groups {
		causeText := ""
		if group.root != nil {
			causeText = group.root.Error()
		}
		cause := InternalCause{
			Cause:  causeText,
			Stages: make([]InternalStage, 0, len(group.Stages)),
		}
		for _, ref := range group.Stages {
			stage := InternalStage{
				Stage: ref.Stage,
				Error: ref.entry.Error(),
				Chain: make([]InternalChainNode, 0, len(ref.chain.nodes)),
				Cycle: ref.chain.cycle,
			}
			for _, node := range ref.chain.nodes {
				stage.Chain = append(stage.Chain, InternalChainNode{
					Depth: node.depth,
					Error: node.err.Error(),
				})
			}
			cause.Stages = append(cause.Stages, stage)
		}
		out = append(out, cause)
	}
	return out
}

// identity kinds, kept in unexported key types so different kinds or types can
// never collapse into one another in a map.
type pointerIdentity struct {
	typ reflect.Type
	ptr uintptr
}

type valueIdentity struct {
	typ reflect.Type
	val any
}

type opaqueIdentity struct {
	typ reflect.Type
	id  uint64
}

var opaqueIdentityCounter uint64

// identityOf identifies an error by object identity: pointers by address,
// comparable values by type+value, anything else by a freshly minted id.
// Two errors with equal Error() text but different identity never compare equal.
func identityOf(err error) any {
	if err == nil {
		return nil
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return pointerIdentity{typ: value.Type(), ptr: value.Pointer()}
	}
	if value.Type().Comparable() {
		return valueIdentity{typ: value.Type(), val: err}
	}
	return opaqueIdentity{typ: value.Type(), id: atomic.AddUint64(&opaqueIdentityCounter, 1)}
}

type chainNode struct {
	err   error
	id    any
	depth int
}

type walkedChain struct {
	nodes []chainNode
	root  chainNode
	cycle *CycleDiagnostic
}

// asGinError is a cycle-safe replacement for errors.As used while registering
// an error. The standard library's errors.As/Is follow Unwrap without cycle
// detection, so a cyclic chain would hang registration itself.
//
// It mirrors errors.As semantics, including errors.Is-style multi-Unwrap
// support (an Unwrap() []error method), while tracking error identity.
func asGinError(err error, target **Error) bool {
	if err == nil {
		panic("errors: err cannot be nil")
	}
	visited := make(map[any]struct{})
	return asChain(err, target, visited)
}

func asChain(err error, target **Error, visited map[any]struct{}) bool {
	if err == nil {
		return false
	}
	id := identityOf(err)
	if _, seen := visited[id]; seen {
		return false
	}
	visited[id] = struct{}{}

	if ginErr, ok := err.(*Error); ok {
		*target = ginErr
		return true
	}
	switch next := err.(type) {
	case interface{ Unwrap() error }:
		if wrapped := next.Unwrap(); asChain(wrapped, target, visited) {
			return true
		}
	case interface{ Unwrap() []error }:
		for _, wrapped := range next.Unwrap() {
			if asChain(wrapped, target, visited) {
				return true
			}
		}
	}
	return false
}

// walkChain follows errors.Unwrap with per-chain visited tracking. A cycle is
// stopped at the first repeated node and recorded instead of hanging.
//
// Traversal starts at the user-registered error: gin's own *Error envelope is
// not considered part of the cause chain.
func walkChain(entry *Error) *walkedChain {
	chain := &walkedChain{}
	visited := make(map[any]int)

	current := entry.Err
	for depth := 0; current != nil; depth++ {
		if depth >= maxUnwrapDepth {
			break
		}
		id := identityOf(current)
		if backTo, seen := visited[id]; seen {
			chain.cycle = &CycleDiagnostic{
				StartDepth:  depth,
				BackToDepth: backTo,
				Message: fmt.Sprintf(
					"error chain contains a cycle: node at depth %d wraps back to the error at depth %d",
					depth, backTo,
				),
			}
			break
		}
		visited[id] = depth
		node := chainNode{err: current, id: id, depth: depth}
		chain.nodes = append(chain.nodes, node)
		chain.root = node

		next := errors.Unwrap(current)
		if next == nil {
			break
		}
		current = next
	}
	return chain
}
