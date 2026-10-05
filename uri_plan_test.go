// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContextBindURIPlanBindsParamsWithoutURLParsing proves the explicit
// binder converts the router-produced Params directly: no URL is re-parsed
// and no intermediate map is built.
func TestContextBindURIPlanBindsParamsWithoutURLParsing(t *testing.T) {
	type req struct {
		ID   int `uri:"id"`
		Page int `uri:"page"`
	}
	plan, err := binding.CompileURIPlan(&req{})
	require.NoError(t, err)

	c, _ := CreateTestContext(httptest.NewRecorder())
	// Params as produced by the router; the request URL is irrelevant.
	c.Params = Params{
		{Key: "id", Value: "12"},
		{Key: "page", Value: "3"},
	}

	var obj req
	require.NoError(t, c.BindURIPlan(plan, &obj))
	assert.Equal(t, 12, obj.ID)
	assert.Equal(t, 3, obj.Page)
}

// TestContextBindURIPlanFailureAtomic checks that a failed conversion leaves
// the target struct untouched and reports the route parameter name.
func TestContextBindURIPlanFailureAtomic(t *testing.T) {
	type req struct {
		ID   int `uri:"id"`
		Page int `uri:"page"`
	}
	plan, err := binding.CompileURIPlan(&req{})
	require.NoError(t, err)

	c, _ := CreateTestContext(httptest.NewRecorder())
	c.Params = Params{
		{Key: "id", Value: "12"},
		{Key: "page", Value: "bad"},
	}

	obj := req{ID: 5, Page: 10}
	err = c.BindURIPlan(plan, &obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "page"`)
	assert.Equal(t, 5, obj.ID)
	assert.Equal(t, 10, obj.Page)
}

// TestContextBindURIPlanConflict checks that two fields binding the same
// route parameter are rejected at plan compile time, before any conversion.
func TestContextBindURIPlanConflict(t *testing.T) {
	type req struct {
		ID  int `uri:"id"`
		ID2 int `uri:"id"`
	}
	_, err := binding.CompileURIPlan(&req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate route parameter "id"`)
}

// TestShouldBindUriStillCompatible ensures the legacy binder keeps working.
func TestShouldBindUriStillCompatible(t *testing.T) {
	type req struct {
		Name string `uri:"name"`
	}
	c, _ := CreateTestContext(httptest.NewRecorder())
	c.Params = Params{{Key: "name", Value: "gin"}}

	var obj req
	require.NoError(t, c.ShouldBindUri(&obj))
	assert.Equal(t, "gin", obj.Name)
}
