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

// Error kinds reported by UriPlan. They are stored on UriParamError.Kind so
// callers can branch on the failure reason without parsing error strings.
const (
	// UriErrorMissing means a field marked required had no matching route
	// parameter.
	UriErrorMissing = "missing"

	// UriErrorInvalid means the raw route value could not be converted to the
	// destination field type (syntax error or integer range overflow, or the
	// field's custom converter returned an error).
	UriErrorInvalid = "invalid"

	// UriErrorConflict means two fields of the planned struct bind the same
	// route parameter name (through a primary name or an alias). It is only
	// returned while compiling the plan.
	UriErrorConflict = "duplicate"
)

// UriParamError is the explicit error returned by a compiled UriPlan. Every
// instance carries the route parameter name (the key declared in the router
// path, or an accepted alias) in Param.
type UriParamError struct {
	// Param is the route parameter name the error refers to.
	Param string

	// Kind is one of UriErrorMissing, UriErrorInvalid and UriErrorConflict.
	Kind string

	// Field is the destination struct field name, useful for logging.
	Field string

	// Err wraps the underlying conversion error for UriErrorInvalid.
	Err error
}

func (e *UriParamError) Error() string {
	switch e.Kind {
	case UriErrorMissing:
		return fmt.Sprintf("uri: required route parameter %q is missing", e.Param)
	case UriErrorConflict:
		return fmt.Sprintf("uri: route parameter %q is bound by more than one field", e.Param)
	default:
		if e.Err != nil {
			return fmt.Sprintf("uri: cannot convert route parameter %q: %v", e.Param, e.Err)
		}
		return fmt.Sprintf("uri: invalid route parameter %q", e.Param)
	}
}

// Unwrap exposes the underlying conversion error, so errors.Is and errors.As
// keep working on custom converter failures.
func (e *UriParamError) Unwrap() error { return e.Err }

var (
	bindUnmarshalerType = reflect.TypeOf((*BindUnmarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	timeDurationType    = reflect.TypeOf(time.Nanosecond)
)

// ParamLookup returns the raw value of a route parameter and whether it was
// present. It is satisfied by gin.Params.Get, so the plan converts the already
// matched Params directly and never re-parses the request URL.
type ParamLookup func(name string) (value string, ok bool)

// UriPlan is a precompiled binding plan for a fixed destination struct type.
// Compile it once per struct (typically at package init) and reuse it for every
// request; Bind is allocation-light and performs no further reflection on the
// struct's tags.
type UriPlan struct {
	typ    reflect.Type
	fields []*uriFieldPlan
	// owners maps every accepted parameter name (primary names and aliases) to
	// the owning field, so name collisions are rejected at compile time.
	owners map[string]string
}

type uriFieldKind int

const (
	uriKindString uriFieldKind = iota
	uriKindInt
	uriKindUint
	uriKindBool
	uriKindFloat
	uriKindDuration
	uriKindBind // field type (or *field type) implements BindUnmarshaler
	uriKindText // field type (or *field type) implements encoding.TextUnmarshaler
)

// uriFieldPlan describes how one struct field is populated.
type uriFieldPlan struct {
	// index is the reflect field index path through (possibly embedded)
	// structs.
	index []int
	// embedPtr[i] reports whether index[i] is an embedded pointer-struct field
	// that must be allocated before walking deeper. Its length is len(index)-1.
	embedPtr []bool
	// ptrDepth is the number of pointer levels wrapping the leaf field type.
	ptrDepth int

	primary string
	aliases []string

	required bool
	kind     uriFieldKind
	bits     int // strconv bit size for int/uint/float kinds
	// forceText is set by the parser="encoding.TextUnmarshaler" tag option.
	forceText bool
}

// CompileUriPlan precompiles a binding plan for the given destination type.
// dest may be a value or a pointer of the struct; only its type is inspected.
// Duplicate parameter names (primary names or aliases) and unsupported field
// types are rejected here, before any request is served.
func CompileUriPlan(dest any) (*UriPlan, error) {
	t := reflect.TypeOf(dest)
	if t == nil {
		return nil, errors.New("uri: cannot compile plan from untyped nil target")
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("uri: plan target must be a struct or a pointer to struct, got %s", t.Kind())
	}
	p := &UriPlan{typ: t, owners: map[string]string{}}
	if err := p.compileStruct(t, nil, nil); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *UriPlan) compileStruct(t reflect.Type, index []int, embedPtr []bool) error {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous {
			continue // unexported, non-embedded field
		}
		if sf.Tag.Get("uri") == "-" {
			continue
		}

		fIndex := appendIndex(index, i)
		ft := sf.Type

		// Walk into anonymous embedded structs, including pointer-embedded
		// structs, mirroring the classic uri binder.
		if sf.Anonymous {
			at := ft
			embeddedPtr := false
			if at.Kind() == reflect.Ptr && at.Elem().Kind() == reflect.Struct {
				at = at.Elem()
				embeddedPtr = true
			}
			if at.Kind() == reflect.Struct {
				nextEmbed := embedPtr
				if embeddedPtr {
					nextEmbed = append(append([]bool{}, embedPtr...), true)
				}
				if err := p.compileStruct(at, fIndex, nextEmbed); err != nil {
					return err
				}
				continue
			}
		}

		if err := p.compileField(sf, fIndex, embedPtr); err != nil {
			return err
		}
	}
	return nil
}

func (p *UriPlan) compileField(sf reflect.StructField, index []int, embedPtr []bool) error {
	tag := sf.Tag.Get("uri")
	name, opts := head(tag, ",")
	if name == "" {
		name = sf.Name // no tag or empty name: bind by the Go field name
	}
	fp := &uriFieldPlan{
		index:    append([]int{}, index...),
		embedPtr: append([]bool{}, embedPtr...),
		primary:  name,
	}

	for len(opts) > 0 {
		var opt string
		opt, opts = head(opts, ",")
		if opt == "" {
			continue
		}
		key, val := head(opt, "=")
		switch key {
		case "required":
			fp.required = true
		case "alias":
			if val != "" {
				fp.aliases = strings.Split(val, "|")
			}
		case "parser":
			if val == "encoding.TextUnmarshaler" {
				fp.forceText = true
			}
		}
	}

	ft := sf.Type
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
		fp.ptrDepth++
	}
	ptrType := reflect.PointerTo(ft)

	switch {
	case fp.forceText:
		if !ptrType.Implements(textUnmarshalerType) {
			return fmt.Errorf("uri: field %q declares parser %q but %s does not implement encoding.TextUnmarshaler",
				sf.Name, "encoding.TextUnmarshaler", sf.Type)
		}
		fp.kind = uriKindText
	case ptrType.Implements(bindUnmarshalerType):
		fp.kind = uriKindBind
	case ptrType.Implements(textUnmarshalerType):
		fp.kind = uriKindText
	default:
		if err := fp.resolveScalarKind(sf, ft); err != nil {
			return err
		}
	}

	// Reserve the primary name and every alias; same-name collisions abort the
	// whole plan at compile time.
	if err := p.reserve(fp.primary, sf.Name); err != nil {
		return err
	}
	for _, alias := range fp.aliases {
		if alias == fp.primary {
			return &UriParamError{Param: alias, Kind: UriErrorConflict, Field: sf.Name,
				Err: errors.New("alias duplicates the field primary parameter name")}
		}
		if err := p.reserve(alias, sf.Name); err != nil {
			return err
		}
	}

	p.fields = append(p.fields, fp)
	return nil
}

func (fp *uriFieldPlan) resolveScalarKind(sf reflect.StructField, ft reflect.Type) error {
	switch ft.Kind() {
	case reflect.String:
		fp.kind = uriKindString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if ft == timeDurationType {
			fp.kind = uriKindDuration
			return nil
		}
		fp.kind = uriKindInt
		switch ft.Kind() {
		case reflect.Int:
			fp.bits = 0
		case reflect.Int8:
			fp.bits = 8
		case reflect.Int16:
			fp.bits = 16
		case reflect.Int32:
			fp.bits = 32
		default:
			fp.bits = 64
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		fp.kind = uriKindUint
		switch ft.Kind() {
		case reflect.Uint:
			fp.bits = 0
		case reflect.Uint8:
			fp.bits = 8
		case reflect.Uint16:
			fp.bits = 16
		case reflect.Uint32:
			fp.bits = 32
		default:
			fp.bits = 64
		}
	case reflect.Bool:
		fp.kind = uriKindBool
	case reflect.Float32, reflect.Float64:
		fp.kind = uriKindFloat
		if ft.Kind() == reflect.Float32 {
			fp.bits = 32
		} else {
			fp.bits = 64
		}
	default:
		return fmt.Errorf("uri: field %q of type %s cannot be bound from a route parameter; "+
			"use a scalar field, or implement binding.BindUnmarshaler / encoding.TextUnmarshaler",
			sf.Name, sf.Type)
	}
	return nil
}

func (p *UriPlan) reserve(name, field string) error {
	if name == "" {
		return fmt.Errorf("uri: field %q has an empty parameter name", field)
	}
	if owner, ok := p.owners[name]; ok {
		return &UriParamError{
			Param: name,
			Kind:  UriErrorConflict,
			Field: field,
			Err:   fmt.Errorf("already bound by field %q", owner),
		}
	}
	p.owners[name] = field
	return nil
}

// pick returns the matched parameter name and raw value. The primary name wins
// over aliases.
func (fp *uriFieldPlan) pick(lookup ParamLookup) (name, raw string, ok bool) {
	if raw, ok = lookup(fp.primary); ok {
		return fp.primary, raw, true
	}
	for _, alias := range fp.aliases {
		if raw, ok = lookup(alias); ok {
			return alias, raw, true
		}
	}
	return fp.primary, "", false
}

type uriConverted struct {
	val reflect.Value // addressable leaf-typed value holding the result
	err error
}

// Bind converts the route parameters resolved by lookup and writes them into
// dest, which must be a pointer to the struct type the plan was compiled for.
//
// Conversion runs into a private copy of the destination: the original struct
// is only overwritten once every required parameter is present and every
// conversion succeeds, so a failure can never leave the struct half-populated.
// Within one Bind call each matched route parameter is converted at most once.
func (p *UriPlan) Bind(dest any, lookup ParamLookup) error {
	if lookup == nil {
		return errors.New("uri: nil parameter lookup")
	}
	dv := reflect.ValueOf(dest)
	if dv.Kind() != reflect.Ptr || dv.IsNil() {
		return errors.New("uri: destination must be a non-nil pointer to struct")
	}
	target := dv.Elem()
	if target.Kind() != reflect.Struct || target.Type() != p.typ {
		return fmt.Errorf("uri: destination type %s does not match planned type %s", target.Type(), p.typ)
	}

	// Private working copy; the caller struct stays untouched until commit.
	shadow := reflect.New(p.typ).Elem()
	shadow.Set(target)

	// Allocate embedded pointer-struct paths once, copying the target's data
	// in, so fields sharing an embedded struct build on the same working copy
	// and the target is never mutated before commit.
	alloc := &uriEmbeddedAlloc{paths: map[string]bool{}}
	for _, fp := range p.fields {
		fp.initEmbedded(shadow, alloc)
	}

	cache := make(map[string]*uriConverted, len(p.fields))

	// Fields are processed in declaration order, so error order is stable.
	for _, fp := range p.fields {
		name, raw, present := fp.pick(lookup)
		if !present {
			if fp.required {
				return &UriParamError{Param: fp.primary, Kind: UriErrorMissing, Field: p.owners[fp.primary]}
			}
			continue
		}

		entry := cache[name]
		if entry == nil {
			tmp := reflect.New(p.leafType(fp)).Elem()
			err := fp.convert(raw, tmp)
			entry = &uriConverted{val: tmp, err: err}
			cache[name] = entry
		}
		if entry.err != nil {
			return &UriParamError{
				Param: name,
				Kind:  UriErrorInvalid,
				Field: p.owners[name],
				Err:   entry.err,
			}
		}

		leaf := fp.walkAlloc(shadow)
		leaf.Set(entry.val)
	}

	// All checks passed: commit the whole struct atomically.
	target.Set(shadow)
	return nil
}

// leafType rebuilds the pointer-free leaf type of a planned field.
func (p *UriPlan) leafType(fp *uriFieldPlan) reflect.Type {
	t := p.typ.FieldByIndex(fp.index).Type
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// uriEmbeddedAlloc records which embedded pointer-struct paths have already
// been allocated within one Bind, so fields sharing an embedded struct reuse
// the same working copy.
type uriEmbeddedAlloc struct {
	paths map[string]bool
}

// initEmbedded allocates the embedded pointer-struct segments of the field
// path on the working copy, copying pre-existing data in.
func (fp *uriFieldPlan) initEmbedded(shadow reflect.Value, alloc *uriEmbeddedAlloc) {
	v := shadow
	for seg, idx := range fp.index {
		if seg >= len(fp.embedPtr) || !fp.embedPtr[seg] {
			v = v.Field(idx)
			continue
		}
		f := v.Field(idx)
		key := strings.Builder{}
		for i := 0; i <= seg; i++ {
			if i > 0 {
				key.WriteByte('.')
			}
			fmt.Fprintf(&key, "%d", fp.index[i])
		}
		if f.IsNil() {
			f.Set(reflect.New(f.Type().Elem()))
		} else if !alloc.paths[key.String()] {
			nv := reflect.New(f.Type().Elem())
			nv.Elem().Set(f.Elem())
			f.Set(nv)
		}
		alloc.paths[key.String()] = true
		v = f.Elem()
	}
}

// walkAlloc follows the field index path on the already initialised working
// copy, allocating fresh pointer leaves, and returns the settable leaf value.
func (fp *uriFieldPlan) walkAlloc(shadow reflect.Value) reflect.Value {
	v := shadow
	for seg, idx := range fp.index {
		if seg < len(fp.embedPtr) && fp.embedPtr[seg] {
			v = v.Field(idx).Elem()
			continue
		}
		v = v.Field(idx)
	}
	for i := 0; i < fp.ptrDepth; i++ {
		nv := reflect.New(v.Type().Elem())
		v.Set(nv)
		v = nv.Elem()
	}
	return v
}

// convert turns one raw route value into the leaf value. It runs at most once
// per matched parameter and Bind call.
func (fp *uriFieldPlan) convert(raw string, v reflect.Value) error {
	switch fp.kind {
	case uriKindBind:
		return v.Addr().Interface().(BindUnmarshaler).UnmarshalParam(raw)
	case uriKindText:
		return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw))
	case uriKindString:
		v.SetString(raw)
		return nil
	case uriKindDuration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	case uriKindInt:
		val := raw
		if strings.TrimSpace(val) == "" {
			val = "0"
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, fp.bits)
		if err != nil {
			return uriConvertError(raw, v.Type(), err)
		}
		v.SetInt(n)
		return nil
	case uriKindUint:
		val := raw
		if strings.TrimSpace(val) == "" {
			val = "0"
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, fp.bits)
		if err != nil {
			return uriConvertError(raw, v.Type(), err)
		}
		v.SetUint(n)
		return nil
	case uriKindBool:
		val := raw
		if strings.TrimSpace(val) == "" {
			val = "false"
		}
		b, err := strconv.ParseBool(strings.TrimSpace(val))
		if err != nil {
			return uriConvertError(raw, v.Type(), err)
		}
		v.SetBool(b)
		return nil
	case uriKindFloat:
		val := raw
		if strings.TrimSpace(val) == "" {
			val = "0"
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(val), fp.bits)
		if err != nil {
			return uriConvertError(raw, v.Type(), err)
		}
		v.SetFloat(f)
		return nil
	default:
		return errUnknownType
	}
}

// uriConvertError turns an overflow into an explicit range message while
// keeping the strconv range sentinel in the chain; syntax errors are returned
// as-is. Either way the caller wraps the parameter name.
func uriConvertError(raw string, typ reflect.Type, err error) error {
	if errors.Is(err, strconv.ErrRange) {
		return fmt.Errorf("value %q out of range for %s: %w", raw, typ, strconv.ErrRange)
	}
	return err
}

// BindParams is a convenience wrapper around Bind for callers that already
// hold the route parameters as a simple map (for example in unit tests). Prefer
// Bind with gin.Params.Get on the hot path.
func (p *UriPlan) BindParams(dest any, params map[string]string) error {
	return p.Bind(dest, ParamLookup(func(name string) (string, bool) {
		v, ok := params[name]
		return v, ok
	}))
}

func appendIndex(base []int, i int) []int {
	// Preallocate a fresh backing array so sibling paths never share storage.
	out := make([]int, 0, len(base)+1)
	out = append(out, base...)
	out = append(out, i)
	return out
}
