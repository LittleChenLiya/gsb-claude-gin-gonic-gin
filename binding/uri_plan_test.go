// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package binding

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pageParams struct {
	ID   int    `uri:"id,required"`
	Page int    `uri:"page,required"`
	Note string `uri:"note"`
}

func lookupFromMap(m map[string]string) ParamLookup {
	return ParamLookup(func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	})
}

func TestUriPlanHappyPathCommitsAll(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	dst := pageParams{ID: 1, Page: 2, Note: "keep"}
	err = plan.Bind(&dst, lookupFromMap(map[string]string{
		"id":   "12",
		"page": "3",
	}))
	require.NoError(t, err)
	assert.Equal(t, 12, dst.ID)
	assert.Equal(t, 3, dst.Page)
	assert.Equal(t, "keep", dst.Note)
}

func TestUriPlanInvalidInputLeavesTargetUntouched(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	dst := pageParams{ID: 7, Page: 9, Note: "orig"}
	err = plan.Bind(&dst, lookupFromMap(map[string]string{
		"id":   "12",
		"page": "bad",
	}))
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, UriErrorInvalid, pe.Kind)
	assert.Equal(t, "page", pe.Param)
	assert.Equal(t, "Page", pe.Field)
	assert.Contains(t, pe.Error(), "page")

	assert.Equal(t, pageParams{ID: 7, Page: 9, Note: "orig"}, dst)
}

func TestUriPlanMissingRequiredKeepsTarget(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	dst := pageParams{ID: 5, Page: 6}
	err = plan.Bind(&dst, lookupFromMap(map[string]string{
		"id": "12",
	}))
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, UriErrorMissing, pe.Kind)
	assert.Equal(t, "page", pe.Param)
	assert.Equal(t, 5, dst.ID)
	assert.Equal(t, 6, dst.Page)
}

func TestUriPlanIntegerRangeErrorCarriesParamName(t *testing.T) {
	type small struct {
		N int8 `uri:"n,required"`
	}
	plan, err := CompileUriPlan(small{})
	require.NoError(t, err)

	var dst small
	err = plan.Bind(&dst, lookupFromMap(map[string]string{"n": "999"}))
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, UriErrorInvalid, pe.Kind)
	assert.Equal(t, "n", pe.Param)
	assert.Contains(t, pe.Error(), "out of range")
	assert.Contains(t, pe.Error(), "n")
	assert.True(t, errors.Is(pe.Err, strconv.ErrRange))
	assert.Equal(t, int8(0), dst.N)
}

type countingParam struct {
	v     int
	calls int
}

func (c *countingParam) UnmarshalParam(s string) error {
	c.calls++
	if s == "fail" {
		return errCountingCustom
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	c.v = n
	return nil
}

// onceParam counts how often its converter runs across a single Bind. The plan
// guarantees each matched parameter is converted at most once.
type onceParam struct {
	calls int
}

func (o *onceParam) UnmarshalParam(string) error {
	o.calls++
	return nil
}

var errCountingCustom = errors.New("custom converter failure")

type customParams struct {
	A countingParam `uri:"a,required"`
	B int           `uri:"b,required"`
}

func TestUriPlanCustomConverterErrorCarriesParamName(t *testing.T) {
	plan, err := CompileUriPlan(customParams{})
	require.NoError(t, err)

	dst := customParams{}
	err = plan.Bind(&dst, lookupFromMap(map[string]string{
		"a": "fail",
		"b": "42",
	}))
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "a", pe.Param)
	assert.Equal(t, UriErrorInvalid, pe.Kind)
	assert.ErrorIs(t, err, errCountingCustom)
	assert.Contains(t, err.Error(), "a")
	assert.Equal(t, 0, dst.B)
}

func TestUriPlanCustomFailurePreservesEarlierFields(t *testing.T) {
	type t1 struct {
		X int           `uri:"x,required"`
		A countingParam `uri:"a,required"`
	}
	plan, err := CompileUriPlan(t1{})
	require.NoError(t, err)

	dst := t1{X: 100}
	err = plan.Bind(&dst, lookupFromMap(map[string]string{"x": "5", "a": "fail"}))
	require.Error(t, err)
	assert.Equal(t, 100, dst.X)
}

type conflictSameName struct {
	ID     int `uri:"id,required"`
	Other  int `uri:"id"`
	Unused int `uri:"unused"`
}

func TestUriPlanCompileRejectsDuplicatePrimaryName(t *testing.T) {
	_, err := CompileUriPlan(conflictSameName{})
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, UriErrorConflict, pe.Kind)
	assert.Equal(t, "id", pe.Param)
}

func TestUriPlanCompileRejectsAliasCollision(t *testing.T) {
	type aliasConflict struct {
		A int `uri:"a,required"`
		B int `uri:"b,required,alias=a"`
	}
	_, err := CompileUriPlan(aliasConflict{})
	require.Error(t, err)

	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, UriErrorConflict, pe.Kind)
	assert.Equal(t, "a", pe.Param)
}

func TestUriPlanAliasIsAccepted(t *testing.T) {
	type aliasOK struct {
		A int `uri:"a,required,alias=article|article_id"`
	}
	plan, err := CompileUriPlan(aliasOK{})
	require.NoError(t, err)

	var d1 aliasOK
	require.NoError(t, plan.Bind(&d1, lookupFromMap(map[string]string{"a": "1", "article": "2"})))
	assert.Equal(t, 1, d1.A)

	var d2 aliasOK
	require.NoError(t, plan.Bind(&d2, lookupFromMap(map[string]string{"article": "3"})))
	assert.Equal(t, 3, d2.A)

	var d3 aliasOK
	require.NoError(t, plan.Bind(&d3, lookupFromMap(map[string]string{"article_id": "4"})))
	assert.Equal(t, 4, d3.A)

	var d4 aliasOK
	err = plan.Bind(&d4, lookupFromMap(map[string]string{}))
	require.Error(t, err)
	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "a", pe.Param)
}

func TestUriPlanUntaggedFieldUsesFieldName(t *testing.T) {
	type byName struct {
		Page int
	}
	plan, err := CompileUriPlan(byName{})
	require.NoError(t, err)

	var d byName
	require.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"Page": "8"})))
	assert.Equal(t, 8, d.Page)
}

func TestUriPlanIgnoredField(t *testing.T) {
	type withSkip struct {
		A    int `uri:"a,required"`
		Skip int `uri:"-"`
	}
	plan, err := CompileUriPlan(withSkip{})
	require.NoError(t, err)

	d := withSkip{Skip: 99}
	require.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"a": "1"})))
	assert.Equal(t, 1, d.A)
	assert.Equal(t, 99, d.Skip)
}

func TestUriPlanReusesPlanAcrossBinds(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := pageParams{}
			assert.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"id": "1", "page": "2"})))
			assert.Equal(t, 1, d.ID)
			assert.Equal(t, 2, d.Page)
		}()
	}
	wg.Wait()
}

func TestUriPlanUnsupportedTypeRejectedAtCompile(t *testing.T) {
	type bad struct {
		S []int `uri:"s"`
	}
	_, err := CompileUriPlan(bad{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S")
}

func TestUriPlanDestinationTypeMismatch(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	type other struct {
		ID   int `uri:"id"`
		Page int `uri:"page"`
	}
	var d other
	err = plan.Bind(&d, lookupFromMap(map[string]string{"id": "1", "page": "2"}))
	require.Error(t, err)
}

func TestUriPlanBindParamsHelper(t *testing.T) {
	plan, err := CompileUriPlan(pageParams{})
	require.NoError(t, err)

	var d pageParams
	require.NoError(t, plan.BindParams(&d, map[string]string{"id": "21", "page": "4"}))
	assert.Equal(t, 21, d.ID)
	assert.Equal(t, 4, d.Page)
}

func TestUriPlanDurationPointerEmbedded(t *testing.T) {
	type inner struct {
		P *int `uri:"p"`
	}
	type all struct {
		inner
		D time.Duration `uri:"d,required"`
	}
	plan, err := CompileUriPlan(all{})
	require.NoError(t, err)

	var d all
	require.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"d": "1500ms", "p": "77"})))
	assert.Equal(t, 1500*time.Millisecond, d.D)
	require.NotNil(t, d.P)
	assert.Equal(t, 77, *d.P)

	var d2 all
	old := 3
	d2.P = &old
	err = plan.Bind(&d2, lookupFromMap(map[string]string{"d": "bad", "p": "77"}))
	require.Error(t, err)
	require.NotNil(t, d2.P)
	assert.Equal(t, 3, *d2.P)
}

func TestUriPlanTextUnmarshaler(t *testing.T) {
	type withText struct {
		T time.Time `uri:"t,required,parser=encoding.TextUnmarshaler"`
	}
	plan, err := CompileUriPlan(withText{})
	require.NoError(t, err)

	var d withText
	err = plan.Bind(&d, lookupFromMap(map[string]string{"t": "not-a-time"}))
	require.Error(t, err)
	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "t", pe.Param)
	assert.True(t, strings.Contains(pe.Error(), "t"))
}

func TestUriPlanLookupConsultedOncePerParameter(t *testing.T) {
	type t2 struct {
		X int `uri:"x,required"`
		Y int `uri:"y,required"`
	}
	plan, err := CompileUriPlan(t2{})
	require.NoError(t, err)

	hits := map[string]int{}
	lookup := ParamLookup(func(name string) (string, bool) {
		hits[name]++
		if name == "x" {
			return "10", true
		}
		if name == "y" {
			return "20", true
		}
		return "", false
	})
	var d t2
	require.NoError(t, plan.Bind(&d, lookup))
	assert.Equal(t, 10, d.X)
	assert.Equal(t, 20, d.Y)
	assert.Equal(t, 1, hits["x"])
	assert.Equal(t, 1, hits["y"])
}

func TestUriPlanCustomConverterRunsOncePerParameter(t *testing.T) {
	type onceStruct struct {
		A onceParam `uri:"a,required"`
		B int       `uri:"b,required"`
	}
	plan, err := CompileUriPlan(onceStruct{})
	require.NoError(t, err)

	var d onceStruct
	require.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"a": "1", "b": "2"})))
	assert.Equal(t, 1, d.A.calls)

	// Even on repeated reuse of the plan, each Bind converts the param once.
	var d2 onceStruct
	require.NoError(t, plan.Bind(&d2, lookupFromMap(map[string]string{"a": "9", "b": "2"})))
	assert.Equal(t, 1, d2.A.calls)
}

func TestUriPlanBoolFloatUint(t *testing.T) {
	type mix struct {
		F bool    `uri:"f,required"`
		X uint8   `uri:"x,required"`
		Z float64 `uri:"z,required"`
	}
	plan, err := CompileUriPlan(mix{})
	require.NoError(t, err)

	var d mix
	require.NoError(t, plan.Bind(&d, lookupFromMap(map[string]string{"f": "true", "x": "250", "z": "1.5"})))
	assert.True(t, d.F)
	assert.Equal(t, uint8(250), d.X)
	assert.Equal(t, 1.5, d.Z)

	var d2 mix
	err = plan.Bind(&d2, lookupFromMap(map[string]string{"f": "maybe", "x": "250", "z": "1.5"}))
	require.Error(t, err)
	var pe *UriParamError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "f", pe.Param)
}
