package binding

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uriTestParams is a minimal URIParams implementation for tests.
type uriTestParams map[string]string

func (p uriTestParams) Get(name string) (string, bool) {
	v, ok := p[name]
	return v, ok
}

func TestCompileURIPlanDuplicateParam(t *testing.T) {
	type dup struct {
		ID  int `uri:"id"`
		ID2 int `uri:"id"`
	}
	plan, err := CompileURIPlan(&dup{})
	require.Nil(t, plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate route parameter "id"`)
}

func TestCompileURIPlanDuplicateAlias(t *testing.T) {
	type dup struct {
		ID  int `uri:"id,alias=uid"`
		ID2 int `uri:"uid"`
	}
	_, err := CompileURIPlan(&dup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate route parameter "uid"`)
}

func TestCompileURIPlanDuplicateAliasOfAlias(t *testing.T) {
	type dup struct {
		A int `uri:"a,alias=x"`
		B int `uri:"b,alias=x"`
	}
	_, err := CompileURIPlan(&dup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate route parameter "x"`)
}

func TestCompileURIPlanInvalidTarget(t *testing.T) {
	_, err := CompileURIPlan(42)
	require.Error(t, err)
	_, err = CompileURIPlan(nil)
	require.Error(t, err)
}

func TestCompileURIPlanInvalidRangeTag(t *testing.T) {
	type req struct {
		Page int `uri:"page" min:"abc"`
	}
	_, err := CompileURIPlan(&req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "min")

	type req2 struct {
		Page int `uri:"page" max:"xyz"`
	}
	_, err = CompileURIPlan(&req2{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max")
}

func TestURIPlanBindAtomic(t *testing.T) {
	type req struct {
		ID   int    `uri:"id"`
		Page int    `uri:"page"`
		Name string `uri:"name,default=anon"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	// id=12, page=bad: the conversion fails and the original target values
	// are fully preserved.
	obj := req{ID: 5, Page: 10}
	err = plan.Bind(uriTestParams{"id": "12", "page": "bad"}, &obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "page"`)
	assert.Equal(t, 5, obj.ID)
	assert.Equal(t, 10, obj.Page)
	assert.Equal(t, "", obj.Name)

	// all values valid: everything is committed together, defaults apply.
	obj = req{ID: 5, Page: 10}
	err = plan.Bind(uriTestParams{"id": "12", "page": "3"}, &obj)
	require.NoError(t, err)
	assert.Equal(t, 12, obj.ID)
	assert.Equal(t, 3, obj.Page)
	assert.Equal(t, "anon", obj.Name)
}

func TestURIPlanBindAbsentKeepsTarget(t *testing.T) {
	type req struct {
		ID   int `uri:"id"`
		Page int `uri:"page"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	obj := req{ID: 5, Page: 10}
	err = plan.Bind(uriTestParams{"id": "12"}, &obj)
	require.NoError(t, err)
	assert.Equal(t, 12, obj.ID)
	assert.Equal(t, 10, obj.Page) // page absent: original value kept
}

func TestURIPlanRequired(t *testing.T) {
	type req struct {
		ID int `uri:"id,required"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	err = plan.Bind(uriTestParams{}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "id" is required`)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"id": "7"}, &obj))
	assert.Equal(t, 7, obj.ID)
}

func TestURIPlanRange(t *testing.T) {
	type req struct {
		Page int `uri:"page" min:"1" max:"100"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	for _, bad := range []string{"200", "0", "-5"} {
		err = plan.Bind(uriTestParams{"page": bad}, &req{})
		require.Error(t, err, "page=%s should be out of range", bad)
		assert.Contains(t, err.Error(), `uri param "page"`)
	}

	for _, good := range []string{"1", "50", "100"} {
		var obj req
		require.NoError(t, plan.Bind(uriTestParams{"page": good}, &obj), "page=%s should be valid", good)
	}
}

func TestURIPlanRangeUint(t *testing.T) {
	type req struct {
		Age uint `uri:"age" min:"1" max:"120"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	err = plan.Bind(uriTestParams{"age": "0"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "age"`)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"age": "42"}, &obj))
	assert.Equal(t, uint(42), obj.Age)
}

// uriID is a custom type implementing BindUnmarshaler.
type uriID string

func (u *uriID) UnmarshalParam(val string) error {
	if val == "bad" {
		return errors.New("bad id")
	}
	*u = uriID("id:" + val)
	return nil
}

func TestURIPlanCustomConverterError(t *testing.T) {
	type req struct {
		ID uriID `uri:"id"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	err = plan.Bind(uriTestParams{"id": "bad"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "id"`)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"id": "42"}, &obj))
	assert.Equal(t, uriID("id:42"), obj.ID)
}

type countingID struct {
	calls int
	v     int
}

func (c *countingID) UnmarshalParam(val string) error {
	c.calls++
	n, err := strconv.Atoi(val)
	if err != nil {
		return err
	}
	c.v = n
	return nil
}

func TestURIPlanAliasAndConvertOnce(t *testing.T) {
	type req struct {
		ID countingID `uri:"id,alias=uid,alias=ID"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	obj := req{}
	err = plan.Bind(uriTestParams{"uid": "7"}, &obj)
	require.NoError(t, err)
	assert.Equal(t, 1, obj.ID.calls) // converted exactly once
	assert.Equal(t, 7, obj.ID.v)

	// primary name wins over aliases, still a single conversion
	obj = req{}
	err = plan.Bind(uriTestParams{"id": "9", "uid": "8"}, &obj)
	require.NoError(t, err)
	assert.Equal(t, 1, obj.ID.calls)
	assert.Equal(t, 9, obj.ID.v)
}

func TestURIPlanTime(t *testing.T) {
	type req struct {
		When time.Time `uri:"when" time_format:"2006-01-02" time_utc:"true"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"when": "2026-10-05"}, &obj))
	assert.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), obj.When)

	err = plan.Bind(uriTestParams{"when": "not-a-date"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "when"`)
}

func TestURIPlanTimeUnix(t *testing.T) {
	type req struct {
		When time.Time `uri:"when" time_format:"unix"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"when": "1791792000"}, &obj))
	assert.Equal(t, time.Unix(1791792000, 0), obj.When)
}

func TestURIPlanTextParser(t *testing.T) {
	type req struct {
		When time.Time `uri:"when,parser=encoding.TextUnmarshaler"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"when": "2026-10-05T00:00:00Z"}, &obj))
	assert.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), obj.When)

	err = plan.Bind(uriTestParams{"when": "not-a-date"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "when"`)
}

func TestURIPlanNested(t *testing.T) {
	type innerS struct {
		Age int `uri:"sage"`
	}
	type innerP struct {
		Age int `uri:"page"`
	}
	type req struct {
		Name string `uri:"name"`
		S    innerS
		P    *innerP
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	obj := req{}
	err = plan.Bind(uriTestParams{"name": "mike", "sage": "25", "page": "30"}, &obj)
	require.NoError(t, err)
	assert.Equal(t, "mike", obj.Name)
	assert.Equal(t, 25, obj.S.Age)
	require.NotNil(t, obj.P)
	assert.Equal(t, 30, obj.P.Age)

	// failure leaves nested targets untouched
	obj2 := req{}
	obj2.S.Age = 100
	err = plan.Bind(uriTestParams{"name": "mike", "sage": "bad"}, &obj2)
	require.Error(t, err)
	assert.Equal(t, "", obj2.Name) // nothing committed on failure
	assert.Equal(t, 100, obj2.S.Age)
}

func TestURIPlanEmbedded(t *testing.T) {
	type base struct {
		ID int `uri:"id"`
	}
	type req struct {
		base
		Page int `uri:"page"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"id": "12", "page": "3"}, &obj))
	assert.Equal(t, 12, obj.ID)
	assert.Equal(t, 3, obj.Page)
}

func TestURIPlanSlice(t *testing.T) {
	type req struct {
		IDs []int `uri:"ids"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"ids": "1,2,3"}, &obj))
	assert.Equal(t, []int{1, 2, 3}, obj.IDs)

	err = plan.Bind(uriTestParams{"ids": "1,x"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "ids"`)
}

func TestURIPlanArray(t *testing.T) {
	type req struct {
		IDs [3]int `uri:"ids"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"ids": "1,2,3"}, &obj))
	assert.Equal(t, [3]int{1, 2, 3}, obj.IDs)

	err = plan.Bind(uriTestParams{"ids": "1,2"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "ids"`)
}

func TestURIPlanSliceCustomElement(t *testing.T) {
	type req struct {
		IDs []uriID `uri:"ids"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"ids": "a,b"}, &obj))
	assert.Equal(t, []uriID{"id:a", "id:b"}, obj.IDs)

	err = plan.Bind(uriTestParams{"ids": "a,bad"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "ids"`)
}

func TestURIPlanSliceTime(t *testing.T) {
	type req struct {
		When []time.Time `uri:"when" time_format:"2006-01-02" time_utc:"true"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"when": "2026-10-05,2026-10-06"}, &obj))
	assert.Equal(t, []time.Time{
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
	}, obj.When)
}

func TestURIPlanSliceUnsupportedElement(t *testing.T) {
	type unsupported struct {
		X int
	}
	type req struct {
		Items []unsupported `uri:"items"`
	}
	_, err := CompileURIPlan(&req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported slice element type")
}

func TestURIPlanSliceParserTag(t *testing.T) {
	// parser tag on a slice field must not panic; elements use TextUnmarshaler.
	type req struct {
		When []time.Time `uri:"when,parser=encoding.TextUnmarshaler"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"when": "2026-10-05T00:00:00Z,2026-10-06T00:00:00Z"}, &obj))
	assert.Equal(t, []time.Time{
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
	}, obj.When)
}

func TestURIPlanPointer(t *testing.T) {
	type req struct {
		ID *int `uri:"id"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	obj := req{ID: new(int)}
	*obj.ID = 5
	require.NoError(t, plan.Bind(uriTestParams{"id": "42"}, &obj))
	require.NotNil(t, obj.ID)
	assert.Equal(t, 42, *obj.ID)

	// nil pointer is allocated on demand
	obj2 := req{}
	require.NoError(t, plan.Bind(uriTestParams{"id": "42"}, &obj2))
	require.NotNil(t, obj2.ID)
	assert.Equal(t, 42, *obj2.ID)
}

func TestURIPlanDuration(t *testing.T) {
	type req struct {
		D time.Duration `uri:"d"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{"d": "1h30m"}, &obj))
	assert.Equal(t, 90*time.Minute, obj.D)

	err = plan.Bind(uriTestParams{"d": "bad"}, &req{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uri param "d"`)
}

func TestURIPlanDefaults(t *testing.T) {
	type req struct {
		Page int `uri:"page,default=1"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	var obj req
	require.NoError(t, plan.Bind(uriTestParams{}, &obj))
	assert.Equal(t, 1, obj.Page)
}

func TestURIPlanTypeMismatch(t *testing.T) {
	type req struct {
		ID int `uri:"id"`
	}
	type other struct {
		ID int `uri:"id"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	err = plan.Bind(uriTestParams{"id": "1"}, &other{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match plan type")

	err = plan.Bind(uriTestParams{"id": "1"}, nil)
	require.Error(t, err)
}

func TestURIPlanReuse(t *testing.T) {
	type req struct {
		ID int `uri:"id,required"`
	}
	plan, err := CompileURIPlan(&req{})
	require.NoError(t, err)

	for i, p := range []uriTestParams{{"id": "1"}, {"id": "2"}, {"id": "3"}} {
		var obj req
		require.NoError(t, plan.Bind(p, &obj), "iteration %d", i)
		assert.Equal(t, i+1, obj.ID)
	}
}

func ExampleURIPlan() {
	type Page struct {
		ID   int    `uri:"id,required"`
		Page int    `uri:"page" min:"1" max:"100"`
		Name string `uri:"name,alias=username"`
	}

	plan, err := CompileURIPlan(&Page{})
	if err != nil {
		panic(err)
	}

	var p Page
	if err := plan.Bind(uriTestParams{"id": "12", "page": "3", "username": "alice"}, &p); err != nil {
		panic(err)
	}
	fmt.Println(p.ID, p.Page, p.Name)
	// Output: 12 3 alice
}
