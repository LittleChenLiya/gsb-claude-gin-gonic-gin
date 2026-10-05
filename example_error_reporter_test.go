// Copyright 2026 Manu Martinez-Almeida. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
)

// reporterWrappedErr is a fixture error type with an Unwrap method.
type reporterWrappedErr struct {
	msg   string
	cause error
}

func (e *reporterWrappedErr) Error() string { return e.msg + ": " + e.cause.Error() }
func (e *reporterWrappedErr) Unwrap() error { return e.cause }

// reporterLoopErr is an error whose Unwrap points back into its own chain.
type reporterLoopErr struct {
	name string
	next error
}

func (e *reporterLoopErr) Error() string { return e.name }
func (e *reporterLoopErr) Unwrap() error { return e.next }

// ExampleErrorReporter_report demonstrates organizing Context.Errors by cause
// identity and registration phase, contrasting the internal chain with the
// public, label-only output.
func ExampleErrorReporter_report() {
	gin.SetMode(gin.TestMode)
	reporter := gin.NewErrorReporter()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	cause := errors.New("disk full")
	// The same cause is wrapped by two layers at two different phases.
	_ = c.Error(&reporterWrappedErr{msg: "service failed", cause: cause})
	_ = c.Error(&reporterWrappedErr{msg: "handler failed", cause: cause}).SetType(gin.ErrorTypePublic)
	// Equal text, different object: remains a distinct identity.
	_ = c.Error(errors.New("disk full")).SetType(gin.ErrorTypePublic)

	report := reporter.Report(c)

	// Public output: authorized labels only, no cause or message leaks.
	public, _ := json.Marshal(report.Public())
	fmt.Println("public:", string(public))

	// Internal output: causes, phases and unwrap chains are preserved.
	internal, _ := json.Marshal(report.Internal())
	fmt.Println("internal:", string(internal))

	// Reporting is read-only: Errors are retained.
	fmt.Println("errors retained:", len(c.Errors))

	// Output:
	// public: [{"label":"request failed"},{"label":"request failed"}]
	// internal: [{"cause":"disk full","stages":[{"stage":"private","error":"service failed: disk full","chain":[{"depth":0,"error":"service failed: disk full"},{"depth":1,"error":"disk full"}]},{"stage":"public","error":"handler failed: disk full","chain":[{"depth":0,"error":"handler failed: disk full"},{"depth":1,"error":"disk full"}]}]},{"cause":"disk full","stages":[{"stage":"public","error":"disk full","chain":[{"depth":0,"error":"disk full"}]}]}]
	// errors retained: 3
}

// ExampleErrorReporter_http shows one shared reporter serving concurrent-safe
// per-request reports behind an httptest server, returning only labels to the
// client.
func ExampleErrorReporter_http() {
	gin.SetMode(gin.TestMode)
	reporter := gin.NewErrorReporter()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Next()
		// Middleware reads the report after handlers ran; Errors stay intact.
		_ = reporter.Report(c)
	})
	router.GET("/orders", func(c *gin.Context) {
		cause := errors.New("database unreachable")
		_ = c.Error(&reporterWrappedErr{msg: "repository failed", cause: cause})
		publicErr := c.Error(&reporterWrappedErr{msg: "create order failed", cause: cause}).
			SetType(gin.ErrorTypePublic)
		_ = publicErr

		c.JSON(http.StatusInternalServerError, reporter.Report(c).Public())
	})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	fmt.Println("status:", w.Code)
	fmt.Println("body:", w.Body.String())

	// Output:
	// status: 500
	// body: [{"label":"request failed"}]
}

// ExampleErrorReporter_cycle shows a cyclic unwrap chain terminates and is
// diagnosed instead of hanging the report.
func ExampleErrorReporter_cycle() {
	gin.SetMode(gin.TestMode)

	a := &reporterLoopErr{name: "a"}
	b := &reporterLoopErr{name: "b"}
	a.next = b
	b.next = a // a -> b -> a

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_ = c.Error(a)

	report := gin.NewErrorReporter().Report(c)
	stages := report.Causes()[0].Stages
	cycle := stages[0].Cycle()

	fmt.Println("chain length:", len(stages[0].Chain()))
	fmt.Println("cycle:", cycle.Message)

	// Output:
	// chain length: 2
	// cycle: error chain contains a cycle: node at depth 2 wraps back to the error at depth 0
}
