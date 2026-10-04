// Copyright 2025 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin/render"
	"github.com/go-playground/validator/v10"
)

// MIMEProblemJSON is the RFC 7807 problem details media type.
const MIMEProblemJSON = "application/problem+json"

// DefaultProblemTypeBase is the default base URI used for problem types.
const DefaultProblemTypeBase = "https://gin-gonic.com/problems/"

// BindingError is a stable, machine-readable representation of a single
// field-level binding error.
type BindingError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// BindingErrorResponse is the stable machine structure returned to clients
// when request binding fails with one or more field-level errors.
type BindingErrorResponse struct {
	Status int            `json:"status"`
	Errors []BindingError `json:"errors"`
}

// ProblemDetails is an RFC 7807 problem details document. It is used when the
// client negotiates the application/problem+json media type.
type ProblemDetails struct {
	Type   string         `json:"type,omitempty"`
	Title  string         `json:"title"`
	Status int            `json:"status"`
	Detail string         `json:"detail,omitempty"`
	Errors []BindingError `json:"errors,omitempty"`
}

// BindingErrorAdapter converts binding errors into stable, machine-readable
// HTTP responses. It performs content negotiation based on the Accept header
// and supports both a plain JSON representation and an RFC 7807 problem
// details representation.
//
// The adapter reuses the error produced during the binding phase and never
// reads the request body again. Unexpected errors are reported as a fixed
// server error without exposing internal types or raw request data.
type BindingErrorAdapter struct {
	// ProblemTypeBase is prepended to the problem type URI.
	ProblemTypeBase string
	// IncludeMessages controls whether human-readable messages are included.
	IncludeMessages bool
	// Logger is called with internal errors for debugging. May be nil.
	Logger func(err error)
}

// NewBindingErrorAdapter returns a new BindingErrorAdapter with default
// settings.
func NewBindingErrorAdapter() *BindingErrorAdapter {
	return &BindingErrorAdapter{
		ProblemTypeBase: DefaultProblemTypeBase,
		IncludeMessages: true,
	}
}

// DefaultBindingErrorAdapter is the default adapter used by HandleBindingError.
var DefaultBindingErrorAdapter = NewBindingErrorAdapter()

// HandleBindingError writes a structured error response for the given binding
// error using the default adapter and aborts the request. It returns the HTTP
// status code that was written.
func HandleBindingError(c *Context, obj any, err error) int {
	return DefaultBindingErrorAdapter.Handle(c, obj, err)
}

// Handle writes a structured error response for the given binding error and
// aborts the request so no further handler can modify the response. It returns
// the HTTP status code that was written, or 0 if no response was written.
func (a *BindingErrorAdapter) Handle(c *Context, obj any, err error) int {
	if err == nil {
		return 0
	}
	// Do not attempt to write again if the response has already been committed.
	if c.Writer.Written() {
		return c.Writer.Status()
	}

	format := a.negotiate(c)
	if format == "" {
		c.AbortWithStatusJSON(http.StatusNotAcceptable, H{
			"status": http.StatusNotAcceptable,
			"error":  "not acceptable",
		})
		return http.StatusNotAcceptable
	}

	status, errs := a.classify(obj, err)

	if format == MIMEProblemJSON {
		c.Abort()
		c.Header("Content-Type", MIMEProblemJSON)
		c.Render(status, render.JSON{Data: a.problemDetails(status, errs)})
	} else {
		c.AbortWithStatusJSON(status, BindingErrorResponse{
			Status: status,
			Errors: errs,
		})
	}

	return status
}

// classify maps a binding error to an HTTP status code and a list of stable
// field errors.
func (a *BindingErrorAdapter) classify(obj any, err error) (int, []BindingError) {
	var verrs validator.ValidationErrors
	if errors.As(err, &verrs) {
		return http.StatusBadRequest, a.fieldErrors(obj, verrs)
	}

	if isBodyParseError(err) {
		return http.StatusBadRequest, []BindingError{{
			Field:   "",
			Code:    "invalid_json",
			Message: "invalid request body",
		}}
	}

	if a.Logger != nil {
		a.Logger(err)
	}
	return http.StatusInternalServerError, []BindingError{{
		Field:   "",
		Code:    "internal_error",
		Message: "internal server error",
	}}
}

// fieldErrors converts validator field errors to stable BindingError values,
// preserving the full JSON path including nested fields and array indices.
func (a *BindingErrorAdapter) fieldErrors(obj any, verrs validator.ValidationErrors) []BindingError {
	t := structTypeOf(obj)
	result := make([]BindingError, 0, len(verrs))
	for _, fe := range verrs {
		path := fieldPath(t, fe.Namespace())
		if path == "" {
			path = lowerCamel(fe.Field())
		}
		msg := ""
		if a.IncludeMessages {
			msg = messageFor(fe.Tag(), fe.Param())
		}
		result = append(result, BindingError{
			Field:   path,
			Code:    fe.Tag(),
			Message: msg,
		})
	}
	return result
}

// problemDetails builds an RFC 7807 problem details document for the given
// status and field errors.
func (a *BindingErrorAdapter) problemDetails(status int, errs []BindingError) ProblemDetails {
	base := a.ProblemTypeBase
	if base == "" {
		base = DefaultProblemTypeBase
	}

	var typ, title, detail string
	switch status {
	case http.StatusBadRequest:
		if len(errs) > 0 && errs[0].Code == "invalid_json" {
			typ = "invalid-json"
			title = "Invalid JSON"
			detail = "invalid request body"
		} else {
			typ = "validation-failed"
			title = "Validation failed"
			detail = "one or more fields are invalid"
		}
	case http.StatusInternalServerError:
		typ = "internal-error"
		title = "Internal server error"
		detail = "internal server error"
	default:
		typ = "error"
		title = "Error"
		detail = "request error"
	}

	return ProblemDetails{
		Type:   base + typ,
		Title:  title,
		Status: status,
		Detail: detail,
		Errors: errs,
	}
}

// negotiate selects a supported media type based on the Accept header. It
// returns an empty string when no supported type is acceptable.
func (a *BindingErrorAdapter) negotiate(c *Context) string {
	accept := c.GetHeader("Accept")
	if accept == "" || accept == "*/*" {
		return MIMEJSON
	}

	type candidate struct {
		mime  string
		q     float64
		order int
	}
	var candidates []candidate

	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		mime := part
		q := 1.0
		if semi := strings.IndexByte(part, ';'); semi >= 0 {
			mime = strings.TrimSpace(part[:semi])
			for _, param := range strings.Split(part[semi+1:], ";") {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(param, "q=") {
					if v, err := strconv.ParseFloat(strings.TrimPrefix(param, "q="), 64); err == nil {
						q = v
					}
				}
			}
		}

		// q=0 explicitly means "not acceptable".
		if q == 0 {
			continue
		}

		switch mime {
		case MIMEJSON, MIMEProblemJSON:
			candidates = append(candidates, candidate{mime: mime, q: q, order: len(candidates)})
		case "application/*", "*/*":
			// Wildcard accepts both; prefer the default JSON representation.
			candidates = append(candidates, candidate{mime: MIMEJSON, q: q, order: len(candidates)})
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	best := candidates[0]
	for _, cand := range candidates[1:] {
		if cand.q > best.q || (cand.q == best.q && cand.order < best.order) {
			best = cand
		}
	}
	return best.mime
}

// isBodyParseError reports whether err is a request body parse error (JSON
// syntax/type error or truncated body).
func isBodyParseError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return true
	}
	return isDecodeErrorType(err)
}

// isDecodeErrorType is a best-effort fallback for JSON libraries other than
// encoding/json (e.g. sonic, go-json, jsoniter).
func isDecodeErrorType(err error) bool {
	if err == nil {
		return false
	}
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil {
		return false
	}
	name := t.Name()
	return strings.Contains(name, "Syntax") ||
		strings.Contains(name, "Unmarshal") ||
		strings.Contains(name, "Decode")
}

// fieldPath converts a validator namespace (e.g. "Order.Items[0].Price") to a
// JSON field path (e.g. "items[0].price") using the struct's json tags.
func fieldPath(t reflect.Type, namespace string) string {
	segments := strings.Split(namespace, ".")
	if len(segments) <= 1 {
		return ""
	}

	var b strings.Builder
	current := t

	for _, seg := range segments[1:] {
		fieldName, index := splitSegment(seg)
		field, ok := lookupField(current, fieldName)
		name := lowerCamel(fieldName)
		if ok {
			name = jsonName(field)
		}

		// A dot separates a field from the preceding part, except the first.
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(name)

		if index >= 0 {
			b.WriteString(fmt.Sprintf("[%d]", index))
			if ok {
				current = elemType(field.Type)
			}
		} else if ok {
			current = derefType(field.Type)
		}
	}

	return b.String()
}

// splitSegment splits a namespace segment like "Items[0]" into ("Items", 0).
func splitSegment(seg string) (string, int) {
	i := strings.IndexByte(seg, '[')
	if i < 0 {
		return seg, -1
	}
	idxStr := strings.TrimSuffix(seg[i+1:], "]")
	idx, _ := strconv.Atoi(idxStr)
	return seg[:i], idx
}

// lookupField finds a struct field by name, returning false if the current
// type is not a struct.
func lookupField(t reflect.Type, name string) (reflect.StructField, bool) {
	if t == nil || t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	return t.FieldByName(name)
}

// jsonName returns the JSON tag name for a field, falling back to lowerCamel.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return lowerCamel(f.Name)
	}
	name := strings.SplitN(tag, ",", 2)[0]
	if name == "" {
		return lowerCamel(f.Name)
	}
	return name
}

// lowerCamel converts "Items" to "items".
func lowerCamel(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// derefType dereferences pointer types.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// elemType returns the element type of a slice/array, dereferencing pointers.
func elemType(t reflect.Type) reflect.Type {
	t = derefType(t)
	if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		t = derefType(t.Elem())
	}
	return t
}

// structTypeOf returns the underlying struct type of obj.
func structTypeOf(obj any) reflect.Type {
	if obj == nil {
		return nil
	}
	return derefType(reflect.TypeOf(obj))
}

// messageFor returns a human-readable message for a validator tag.
func messageFor(tag, param string) string {
	switch tag {
	case "required":
		return "is required"
	case "gt":
		return "must be greater than " + param
	case "gte":
		return "must be greater than or equal to " + param
	case "lt":
		return "must be less than " + param
	case "lte":
		return "must be less than or equal to " + param
	case "min":
		return "must be at least " + param
	case "max":
		return "must be at most " + param
	case "len":
		return "must have length " + param
	case "eq":
		return "must be equal to " + param
	case "ne":
		return "must not be equal to " + param
	case "oneof":
		return "must be one of " + param
	case "email":
		return "must be a valid email address"
	case "url", "uri":
		return "must be a valid URL"
	case "uuid":
		return "must be a valid UUID"
	case "boolean":
		return "must be a boolean"
	case "numeric":
		return "must be numeric"
	case "alpha":
		return "must be alphabetic"
	case "alphanum":
		return "must be alphanumeric"
	case "contains":
		return "must contain " + param
	case "excludes":
		return "must not contain " + param
	case "startswith":
		return "must start with " + param
	case "endswith":
		return "must end with " + param
	default:
		return "failed " + tag + " validation"
	}
}
