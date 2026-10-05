// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package binding

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// URIParams is a read-only source of URI (route) parameters.
//
// gin.Params implements this interface directly, so a precompiled URIPlan
// can be bound against the parameters produced by the router without
// building an intermediate map and without re-parsing the URL:
//
//	plan, err := binding.CompileURIPlan(&GetUserRequest{})
//	if err != nil {
//	    // plan conflict: two fields bind the same route parameter
//	}
//	var req GetUserRequest
//	if err := plan.Bind(c.Params, &req); err != nil {
//	    // req is left untouched; the error carries the route parameter name
//	}
type URIParams interface {
	// Get returns the value of the first parameter with the given name.
	Get(name string) (string, bool)
}

// URIPlan is a precompiled, explicit binding plan for URI (route) parameters.
//
// A plan is compiled once from a target struct type and can be bound any
// number of times. Compilation walks the struct fields and their "uri" tags,
// so a route parameter is converted at most once per bind and no URL parsing
// happens at bind time.
//
// Struct fields are matched by the "uri" tag value (defaulting to the field
// name). The tag supports the options "required", "default=<value>",
// "alias=<name>" (which may be repeated) and
// "parser=encoding.TextUnmarshaler". Nested (anonymous or named) struct
// fields are walked recursively; pointer fields are allocated on demand.
//
// If two fields (including aliases) resolve to the same route parameter name,
// CompileURIPlan rejects the plan with a conflict error.
//
// At bind time every matched parameter is converted into a scratch value
// first; the target struct is only written after all conversions succeed,
// so a failed bind never leaves a half-populated struct.
type URIPlan struct {
	typ    reflect.Type
	leaves []*uriPlanLeaf
}

// uriPlanLeaf is a single bindable field resolved at compile time.
type uriPlanLeaf struct {
	path  []int
	field reflect.StructField

	name    string
	aliases []string

	required   bool
	hasDefault bool
	defaultVal string

	hasMin bool
	hasMax bool
	min    int64
	max    int64

	kind     reflect.Kind // scalar kind; slice/array store the element kind
	bitSize  int
	isSlice  bool
	isArray  bool
	arrayLen int

	custom     bool // field implements BindUnmarshaler
	elemCustom bool // slice/array element implements BindUnmarshaler
	text       bool // field (or slice element) uses encoding.TextUnmarshaler via parser tag
	timeType   bool
	duration   bool

	timeFmt string
	timeUTC bool
	timeLoc string
}

// CompileURIPlan compiles an explicit binding plan for the struct type of obj.
// obj must be a pointer to a struct (or a struct value).
//
// Compilation fails when:
//   - obj is not a struct type,
//   - the "min"/"max" tags are malformed,
//   - two fields (or aliases) bind the same route parameter name.
func CompileURIPlan(obj any) (*URIPlan, error) {
	if obj == nil {
		return nil, errors.New("uri plan: obj must be a pointer to struct")
	}
	typ := reflect.TypeOf(obj)
	for typ != nil && typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("uri plan: obj must be a pointer to struct, got %T", obj)
	}

	p := &URIPlan{typ: typ}
	used := make(map[string]string)
	if err := p.compile(typ, nil, used); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *URIPlan) compile(typ reflect.Type, prefix []int, used map[string]string) error {
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous { // unexported
			continue
		}

		tag := sf.Tag.Get("uri")
		if tag == "-" {
			continue
		}

		name, opts := head(tag, ",")
		if name == "" { // default to the field name
			name = sf.Name
		}

		var aliases []string
		var required, hasDefault bool
		var def, parser string
		for len(opts) > 0 {
			var opt string
			opt, opts = head(opts, ",")
			if opt == "required" {
				required = true
				continue
			}
			k, v := head(opt, "=")
			switch k {
			case "default":
				hasDefault = true
				def = v
			case "alias":
				aliases = append(aliases, v)
			case "parser":
				parser = v
			}
		}

		path := make([]int, 0, len(prefix)+1)
		path = append(path, prefix...)
		path = append(path, i)

		ft := sf.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		custom := implementsBindUnmarshaler(sf.Type)
		text := parser == "encoding.TextUnmarshaler"

		// Struct fields without their own converter are walked recursively,
		// mirroring the nested-struct support of the default URI binder.
		// time.Time is a leaf scalar, not a nested group.
		if ft.Kind() == reflect.Struct && !custom && !text && ft != reflect.TypeOf(time.Time{}) {
			if err := p.compile(ft, path, used); err != nil {
				return err
			}
			continue
		}

		leaf := &uriPlanLeaf{
			path:       path,
			field:      sf,
			name:       name,
			aliases:    aliases,
			required:   required,
			hasDefault: hasDefault,
			defaultVal: def,
			custom:     custom,
			text:       text,
			timeFmt:    sf.Tag.Get("time_format"),
			timeUTC:    parseBoolTag(sf.Tag.Get("time_utc")),
			timeLoc:    sf.Tag.Get("time_location"),
		}
		if err := leaf.initType(sf.Type); err != nil {
			return err
		}
		if err := leaf.initRange(sf.Tag); err != nil {
			return err
		}

		for _, n := range append([]string{name}, aliases...) {
			if prev, ok := used[n]; ok {
				return fmt.Errorf("uri plan: duplicate route parameter %q for fields %s and %s", n, prev, sf.Name)
			}
			used[n] = sf.Name
		}
		p.leaves = append(p.leaves, leaf)
	}
	return nil
}

func (l *uriPlanLeaf) initType(t reflect.Type) error {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice:
		l.isSlice = true
		l.kind = t.Elem().Kind()
		l.bitSize = bitSizeOfKind(t.Elem().Kind())
		return l.initElem(t.Elem())
	case reflect.Array:
		l.isArray = true
		l.arrayLen = t.Len()
		l.kind = t.Elem().Kind()
		l.bitSize = bitSizeOfKind(t.Elem().Kind())
		return l.initElem(t.Elem())
	case reflect.Struct:
		if l.custom || l.text {
			return nil
		}
		if t == reflect.TypeOf(time.Time{}) {
			l.timeType = true
			return nil
		}
		return fmt.Errorf("uri plan: field %s: unsupported struct type %s", l.field.Name, t)
	default:
		l.kind = t.Kind()
		l.bitSize = bitSizeOfKind(t.Kind())
		if t == reflect.TypeOf(time.Duration(0)) {
			l.duration = true
		}
	}
	return nil
}

// initElem validates the slice/array element type and records any
// element-level converter.
func (l *uriPlanLeaf) initElem(elem reflect.Type) error {
	if elem == reflect.TypeOf(time.Time{}) {
		l.timeType = true
		return nil
	}
	if elem == reflect.TypeOf(time.Duration(0)) {
		l.duration = true
		return nil
	}
	if implementsBindUnmarshaler(elem) {
		l.elemCustom = true
		return nil
	}
	if elem.Kind() == reflect.Struct {
		return fmt.Errorf("uri plan: field %s: unsupported slice element type %s", l.field.Name, elem)
	}
	return nil
}

func (l *uriPlanLeaf) initRange(tag reflect.StructTag) error {
	if v := tag.Get("min"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("uri plan: field %s: invalid min tag %q: %w", l.field.Name, v, err)
		}
		l.hasMin = true
		l.min = n
	}
	if v := tag.Get("max"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("uri plan: field %s: invalid max tag %q: %w", l.field.Name, v, err)
		}
		l.hasMax = true
		l.max = n
	}
	return nil
}

// Bind converts the route parameters provided by params into obj using the
// precompiled plan. obj must be a non-nil pointer to the struct type the plan
// was compiled for.
//
// Every matched parameter is converted at most once. All conversions are
// performed against a scratch value; obj is only updated after every
// conversion succeeds. On any error obj is left untouched and the error
// carries the route parameter name.
func (p *URIPlan) Bind(params URIParams, obj any) error {
	rv, err := p.targetValue(obj)
	if err != nil {
		return err
	}

	scratch := reflect.New(p.typ).Elem()
	committed := make([]bool, len(p.leaves))

	for i, leaf := range p.leaves {
		val, found := p.lookup(params, leaf)
		if !found {
			if leaf.required {
				return fmt.Errorf("uri param %q is required", leaf.name)
			}
			if leaf.hasDefault {
				val = leaf.defaultVal
			} else {
				continue // absent parameters never overwrite existing target values
			}
		}

		out, err := ensureField(scratch, leaf.path)
		if err != nil {
			return err
		}
		if err := convertURILeaf(val, leaf, out); err != nil {
			return fmt.Errorf("uri param %q: %w", leaf.name, err)
		}
		committed[i] = true
	}

	for i, leaf := range p.leaves {
		if !committed[i] {
			continue
		}
		src, err := ensureField(scratch, leaf.path)
		if err != nil {
			return err
		}
		dst, err := ensureField(rv, leaf.path)
		if err != nil {
			return err
		}
		dst.Set(src)
	}
	return nil
}

func (p *URIPlan) targetValue(obj any) (reflect.Value, error) {
	if obj == nil {
		return reflect.Value{}, errors.New("uri plan: obj must be a non-nil pointer to struct")
	}
	rv := reflect.ValueOf(obj)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return reflect.Value{}, errors.New("uri plan: obj must be a non-nil pointer to struct")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("uri plan: obj must be a pointer to struct, got %s", rv.Type())
	}
	if rv.Type() != p.typ {
		return reflect.Value{}, fmt.Errorf("uri plan: obj type %s does not match plan type %s", rv.Type(), p.typ)
	}
	return rv, nil
}

func (p *URIPlan) lookup(params URIParams, leaf *uriPlanLeaf) (string, bool) {
	if v, ok := params.Get(leaf.name); ok {
		return v, true
	}
	for _, alias := range leaf.aliases {
		if v, ok := params.Get(alias); ok {
			return v, true
		}
	}
	return "", false
}

// ensureField navigates v along path, allocating nil pointer fields on demand.
func ensureField(v reflect.Value, path []int) (reflect.Value, error) {
	for _, i := range path {
		v = v.Field(i)
		for v.Kind() == reflect.Ptr {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}, fmt.Errorf("uri plan: cannot allocate pointer field at index %v", path)
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
	}
	return v, nil
}

func convertURILeaf(val string, leaf *uriPlanLeaf, out reflect.Value) error {
	if leaf.custom {
		return out.Addr().Interface().(BindUnmarshaler).UnmarshalParam(val)
	}
	if leaf.text {
		if u, ok := out.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return u.UnmarshalText([]byte(val))
		}
	}

	if leaf.isSlice || leaf.isArray {
		vals := strings.Split(val, ",")
		if leaf.isArray && len(vals) != leaf.arrayLen {
			return fmt.Errorf("%q is not valid value for %s", vals, out.Type())
		}
		var arr reflect.Value
		if leaf.isSlice {
			arr = reflect.MakeSlice(out.Type(), len(vals), len(vals))
		} else {
			arr = reflect.New(out.Type()).Elem()
		}
		for i, s := range vals {
			if err := convertURILeafElem(s, leaf, arr.Index(i)); err != nil {
				return err
			}
		}
		out.Set(arr)
		return nil
	}
	return convertURIScalar(val, leaf, out)
}

// convertURILeafElem converts a single slice/array element, honoring
// element-level custom converters.
func convertURILeafElem(val string, leaf *uriPlanLeaf, out reflect.Value) error {
	if leaf.elemCustom {
		return out.Addr().Interface().(BindUnmarshaler).UnmarshalParam(val)
	}
	if leaf.text {
		if u, ok := out.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return u.UnmarshalText([]byte(val))
		}
	}
	return convertURIScalar(val, leaf, out)
}

func convertURIScalar(val string, leaf *uriPlanLeaf, out reflect.Value) error {
	if leaf.timeType {
		return setURILeafTime(val, leaf, out)
	}
	if leaf.duration {
		d, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		out.Set(reflect.ValueOf(d))
		return nil
	}

	switch leaf.kind {
	case reflect.String:
		out.SetString(val)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if val == "" {
			val = "0"
		}
		iv, err := strconv.ParseInt(val, 10, leaf.bitSize)
		if err != nil {
			return err
		}
		if leaf.hasMin && iv < leaf.min {
			return fmt.Errorf("value %d is less than min %d", iv, leaf.min)
		}
		if leaf.hasMax && iv > leaf.max {
			return fmt.Errorf("value %d is greater than max %d", iv, leaf.max)
		}
		out.SetInt(iv)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if val == "" {
			val = "0"
		}
		uv, err := strconv.ParseUint(val, 10, leaf.bitSize)
		if err != nil {
			return err
		}
		if leaf.hasMin && leaf.min >= 0 && uv < uint64(leaf.min) {
			return fmt.Errorf("value %d is less than min %d", uv, leaf.min)
		}
		if leaf.hasMax && leaf.max >= 0 && uv > uint64(leaf.max) {
			return fmt.Errorf("value %d is greater than max %d", uv, leaf.max)
		}
		out.SetUint(uv)
		return nil
	case reflect.Float32, reflect.Float64:
		if val == "" {
			val = "0.0"
		}
		fv, err := strconv.ParseFloat(val, leaf.bitSize)
		if err != nil {
			return err
		}
		if leaf.hasMin && fv < float64(leaf.min) {
			return fmt.Errorf("value %g is less than min %d", fv, leaf.min)
		}
		if leaf.hasMax && fv > float64(leaf.max) {
			return fmt.Errorf("value %g is greater than max %d", fv, leaf.max)
		}
		out.SetFloat(fv)
		return nil
	case reflect.Bool:
		if val == "" {
			val = "false"
		}
		bv, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		out.SetBool(bv)
		return nil
	default:
		return errUnknownType
	}
}

func setURILeafTime(val string, leaf *uriPlanLeaf, out reflect.Value) error {
	timeFormat := leaf.timeFmt
	if timeFormat == "" {
		timeFormat = time.RFC3339
	}

	if val == "" {
		out.Set(reflect.ValueOf(time.Time{}))
		return nil
	}

	switch tf := strings.ToLower(timeFormat); tf {
	case "unix", "unixmilli", "unixmicro", "unixnano":
		tv, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return err
		}

		var t time.Time
		switch tf {
		case "unix":
			t = time.Unix(tv, 0)
		case "unixmilli":
			t = time.UnixMilli(tv)
		case "unixmicro":
			t = time.UnixMicro(tv)
		default:
			t = time.Unix(0, tv)
		}
		out.Set(reflect.ValueOf(t))
		return nil
	}

	l := time.Local
	if leaf.timeUTC {
		l = time.UTC
	}
	if leaf.timeLoc != "" {
		loc, err := time.LoadLocation(leaf.timeLoc)
		if err != nil {
			return err
		}
		l = loc
	}

	t, err := time.ParseInLocation(timeFormat, val, l)
	if err != nil {
		return err
	}
	out.Set(reflect.ValueOf(t))
	return nil
}

func bitSizeOfKind(k reflect.Kind) int {
	switch k {
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32:
		return 32
	case reflect.Int64, reflect.Uint64:
		return 64
	default: // Int, Uint, Float32/Float64 use the native/standard bit size
		return 0
	}
}

func parseBoolTag(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

func implementsBindUnmarshaler(t reflect.Type) bool {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	_, ok := reflect.New(t).Interface().(BindUnmarshaler)
	return ok
}
