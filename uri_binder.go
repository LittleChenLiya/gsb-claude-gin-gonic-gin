// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"fmt"

	"github.com/gin-gonic/gin/binding"
)

// UriBinder is an explicit, precompiled binder for route (path) parameters.
//
// Unlike ShouldBindUri, which reflects over the destination struct on every
// request and builds an intermediate map, a UriBinder is compiled once from a
// fixed destination struct type (typically at package initialisation) and then
// reused. Binding converts the route Params matched by the router directly;
// the request URL is not re-parsed.
//
// Semantics on failure are atomic: required parameters, type conversion and
// integer range checks all run against a private copy of the destination, and
// the original struct is only overwritten when every check passes.
type UriBinder struct {
	plan *binding.UriPlan
}

// NewUriBinder compiles an explicit route-parameter binder for the given
// destination type. dest may be a value or a pointer of the target struct.
// Compilation errors (for example two fields binding the same parameter name)
// are returned here, before any request is served.
func NewUriBinder(dest any) (*UriBinder, error) {
	plan, err := binding.CompileUriPlan(dest)
	if err != nil {
		return nil, err
	}
	return &UriBinder{plan: plan}, nil
}

// MustNewUriBinder is like NewUriBinder but panics when compilation fails. It
// is intended for package-level initialisation with a compile-time-known type.
func MustNewUriBinder(dest any) *UriBinder {
	b, err := NewUriBinder(dest)
	if err != nil {
		panic(fmt.Sprintf("gin: invalid uri binder: %v", err))
	}
	return b
}

// Bind converts the already matched route parameters into dest. dest must be a
// pointer to the struct type the binder was compiled for. It never re-parses
// the URL; only the Params provided here are consulted.
func (b *UriBinder) Bind(dest any, params Params) error {
	return b.plan.Bind(dest, params.Get)
}

// BindContext binds the route parameters carried by c.
func (b *UriBinder) BindContext(c *Context, dest any) error {
	return b.plan.Bind(dest, c.Params.Get)
}
