// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin/binding"
	codecjson "github.com/gin-gonic/gin/codec/json"
	"github.com/gin-gonic/gin/render"
	"github.com/go-playground/validator/v10"
)

// Media type for the RFC 7807 / RFC 9457 Problem Details format.
const MIMEProblemDetails = "application/problem+json"

// Stable error codes and fixed messages reported to clients. They never change
// and never leak implementation details or raw request data.
const (
	bindErrorTypeValidation    = "validation_error"
	bindErrorTypeMalformed     = "malformed_request_body"
	bindErrorTypeInternal      = "internal_server_error"
	bindErrorTypeNotAcceptable = "not_acceptable"

	bindErrorMessageMalformed = "The request body could not be parsed."
	bindErrorMessageInternal  = "An internal server error occurred."

	bindProblemTypeBlank       = "about:blank"
	bindProblemTitleValidation = "Request Validation Failed"
	bindProblemTitleMalformed  = "Malformed Request Body"
	bindProblemTitleInternal   = "Internal Server Error"
)

// BindingFieldError is the stable, machine readable representation of a single
// failed field constraint. It intentionally never contains the submitted value
// so internal Go types and raw request data stay private.
type BindingFieldError struct {
	// Field is the complete path from the request root to the failing field,
	// using request (JSON) names, dots for nesting and bracket indices for
	// arrays, e.g. "items[0].price".
	Field string `json:"field"`
	// Rule is the failed validation tag, e.g. "required" or "gt".
	Rule string `json:"rule"`
	// Param is the raw rule parameter, e.g. "0" for "gt=0". Empty otherwise.
	Param string `json:"param,omitempty"`
}

// BindingErrorResponse is the JSON envelope returned for failed binding.
type BindingErrorResponse struct {
	Error  string              `json:"error"`
	Fields []BindingFieldError `json:"fields,omitempty"`
}

// ProblemDetails is the RFC 7807 representation of a binding failure.
type ProblemDetails struct {
	Type   string              `json:"type"`
	Title  string              `json:"title"`
	Status int                 `json:"status"`
	Detail string              `json:"detail,omitempty"`
	Errors []BindingFieldError `json:"errors,omitempty"`
}

// RespondBindingError converts an error already returned by the binding phase
// into an explicit, stable error response.
//
// It never re-reads the request body: the error produced by ShouldBind* is the
// only input. The format is selected by HTTP content negotiation on Accept:
//
//   - application/json (the default) renders BindingErrorResponse
//   - application/problem+json renders RFC 7807 ProblemDetails
//
// An Accept header that cannot be satisfied results in 406 Not Acceptable.
//
// Validation errors and malformed request bodies are reported with 400; any
// other, unexpected error is reported with a fixed 500 envelope that exposes
// neither internal error types nor raw request data.
//
// After the response is written the handler chain is aborted, so later handlers
// cannot rewrite it. If a response was already written the chain is aborted and
// nothing more is written. The boolean result is true whenever the caller can
// stop processing (a response was rendered, skipped or negotiation failed).
func (c *Context) RespondBindingError(obj any, err error) bool {
	if err == nil {
		return false
	}
	if c.Writer.Written() {
		c.Abort()
		return true
	}

	status, errorType, fields := classifyBindingError(obj, err)

	format := negotiateBindingErrorFormat(c.requestHeader("Accept"))
	if format == "" {
		c.Abort()
		c.Data(http.StatusNotAcceptable, binding.MIMEJSON,
			[]byte(`{"error":"`+bindErrorTypeNotAcceptable+`"}`))
		return true
	}

	c.Abort()

	switch format {
	case MIMEProblemDetails:
		problem := ProblemDetails{
			Type:   bindProblemTypeBlank,
			Status: status,
			Errors: fields,
		}
		switch errorType {
		case bindErrorTypeValidation:
			problem.Title = bindProblemTitleValidation
			problem.Detail = "One or more fields failed validation."
		case bindErrorTypeMalformed:
			problem.Title = bindProblemTitleMalformed
			problem.Detail = bindErrorMessageMalformed
		default:
			problem.Title = bindProblemTitleInternal
			problem.Detail = bindErrorMessageInternal
		}
		c.Render(status, problemDetailsRender{Data: problem})
	default:
		code := bindErrorTypeInternal
		switch errorType {
		case bindErrorTypeValidation:
			code = bindErrorTypeValidation
		case bindErrorTypeMalformed:
			code = bindErrorTypeMalformed
		}
		c.Render(status, render.JSON{Data: BindingErrorResponse{
			Error:  code,
			Fields: fields,
		}})
	}
	return true
}

// classifyBindingError maps a binding error onto a stable status/type and, for
// validation failures, a list of structured field errors.
func classifyBindingError(obj any, err error) (status int, errorType string, fields []BindingFieldError) {
	var validationErrs validator.ValidationErrors
	if errors.As(err, &validationErrs) {
		return http.StatusBadRequest, bindErrorTypeValidation,
			convertValidationErrors(obj, validationErrs)
	}

	var sliceErrs binding.SliceValidationError
	if errors.As(err, &sliceErrs) {
		for i, subErr := range sliceErrs {
			var subValidation validator.ValidationErrors
			if errors.As(subErr, &subValidation) {
				converted := convertValidationErrors(obj, subValidation)
				prefix := "[" + strconv.Itoa(i) + "]"
				for j := range converted {
					converted[j].Field = prefix + converted[j].Field
				}
				fields = append(fields, converted...)
			}
		}
		if len(fields) > 0 {
			return http.StatusBadRequest, bindErrorTypeValidation, fields
		}
	}

	if isMalformedBodyError(err) {
		return http.StatusBadRequest, bindErrorTypeMalformed, nil
	}

	// Unexpected error: only the fixed server error ever reaches the client.
	return http.StatusInternalServerError, bindErrorTypeInternal, nil
}

// isMalformedBodyError recognizes request-body decoding failures. The JSON
// codecs return the standard library error types (or wrap them), so they are
// matched without depending on the codec in use. EOF from an empty body is a
// client fault as well.
func isMalformedBodyError(err error) bool {
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
	// Unknown-field errors (DisallowUnknownFields) are plain string errors.
	if strings.Contains(err.Error(), "json: unknown field") {
		return true
	}
	return false
}

// convertValidationErrors turns validator field errors into the stable wire
// structure, translating Go struct field paths to request field paths (JSON
// names) while preserving nesting and array indices.
func convertValidationErrors(obj any, errs validator.ValidationErrors) []BindingFieldError {
	rootType := reflect.TypeOf(obj)
	out := make([]BindingFieldError, 0, len(errs))
	for _, fe := range errs {
		out = append(out, BindingFieldError{
			Field: requestFieldPath(rootType, fe),
			Rule:  fe.Tag(),
			Param: fe.Param(),
		})
	}
	return out
}

// requestFieldPath maps a validator namespace like "Order.Items[0].Price" to
// the request path, e.g. "items[0].price".
func requestFieldPath(rootType reflect.Type, fe validator.FieldError) string {
	structField := fe.StructField()
	if rootType == nil {
		return fallbackFieldPath(structField)
	}
	for rootType.Kind() == reflect.Ptr {
		rootType = rootType.Elem()
	}

	// StructNamespace is the root-struct-name prefixed version of StructField,
	// e.g. "CreateOrderRequest.Items[0].Price" vs "Items[0].Price". Strip the
	// first segment to keep the path relative to the bound object.
	rest := structField
	if ns := fe.StructNamespace(); ns != "" {
		if idx := strings.IndexByte(ns, '.'); idx >= 0 {
			rest = ns[idx+1:]
		}
	}
	return translatePath(rootType, rest)
}

// translatePath walks the validator field path against the root struct type and
// resolves each field segment to its JSON name; index segments pass through.
func translatePath(rootType reflect.Type, path string) string {
	if path == "" {
		return ""
	}
	var b strings.Builder
	current := rootType
	for _, seg := range splitFieldPath(path) {
		if seg.index {
			// Move from []T / *[...]T to T so following fields keep resolving.
			current = derefType(current)
			if current != nil && (current.Kind() == reflect.Slice ||
				current.Kind() == reflect.Array) {
				current = current.Elem()
			}
			b.WriteByte('[')
			b.WriteString(seg.name)
			b.WriteByte(']')
			continue
		}

		jsonName := ""
		if current != nil {
			current = derefType(current)
			if current.Kind() == reflect.Struct {
				if sf, ok := current.FieldByName(seg.name); ok {
					jsonName = jsonNameFromTag(sf)
					current = sf.Type
				} else {
					current = nil
				}
			} else {
				current = nil
			}
		}
		if jsonName == "" {
			jsonName = fallbackName(seg.name)
		}

		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(jsonName)
	}
	return b.String()
}

type pathSegment struct {
	name  string
	index bool
}

// splitFieldPath splits "Items[0].Price" into [Items, [0], Price].
func splitFieldPath(path string) []pathSegment {
	var segments []pathSegment
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		for {
			i := strings.IndexByte(part, '[')
			if i < 0 {
				if part != "" {
					segments = append(segments, pathSegment{name: part})
				}
				break
			}
			if i > 0 {
				segments = append(segments, pathSegment{name: part[:i]})
				part = part[i:]
				continue
			}
			j := strings.IndexByte(part, ']')
			if j < 0 {
				segments = append(segments, pathSegment{name: part[1:], index: true})
				break
			}
			segments = append(segments, pathSegment{name: part[1:j], index: true})
			part = part[j+1:]
		}
	}
	return segments
}

func jsonNameFromTag(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return f.Name
	}
	if tag != "" {
		if name := strings.Split(tag, ",")[0]; name != "" {
			return name
		}
	}
	return f.Name
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// fallbackName never emits package-qualified internal identifiers: only a
// lower-cased field name is used.
func fallbackName(name string) string {
	name = strings.TrimPrefix(name, "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return lcFirst(name)
}

func fallbackFieldPath(path string) string {
	var b strings.Builder
	for _, s := range splitFieldPath(path) {
		if s.index {
			b.WriteByte('[')
			b.WriteString(s.name)
			b.WriteByte(']')
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(fallbackName(s.name))
	}
	return b.String()
}

func lcFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// problemDetailsRender serializes JSON with the RFC 7807 content type.
type problemDetailsRender struct {
	Data any
}

func (r problemDetailsRender) Render(w http.ResponseWriter) error {
	r.WriteContentType(w)
	payload, err := codecjson.API.Marshal(r.Data)
	if err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func (r problemDetailsRender) WriteContentType(w http.ResponseWriter) {
	header := w.Header()
	if _, exists := header["Content-Type"]; !exists {
		header.Set("Content-Type", MIMEProblemDetails+"; charset=utf-8")
	}
}

// acceptEntry is one parsed media range from an Accept header.
type acceptEntry struct {
	typ     string
	subtype string
	q       float64
}

// negotiateBindingErrorFormat performs RFC 7231 content negotiation on the
// offered formats. It returns the chosen media type, or "" when nothing matches.
func negotiateBindingErrorFormat(acceptHeader string) string {
	offers := []string{binding.MIMEJSON, MIMEProblemDetails}

	header := strings.TrimSpace(acceptHeader)
	if header == "" {
		return offers[0]
	}
	entries := parseAcceptHeader(header)
	if len(entries) == 0 {
		return offers[0]
	}

	parsedOffers := make([]acceptEntry, 0, len(offers))
	for _, o := range offers {
		if e, ok := parseMediaRange(o, 1); ok {
			parsedOffers = append(parsedOffers, e)
		}
	}

	// Highest q wins. Equal q always keeps the earlier offer (JSON first),
	// independent of the order used by the client.
	bestOffer := ""
	bestQ := -1.0
	for _, accepted := range entries {
		if accepted.q <= 0 {
			continue
		}
		for oi, offer := range parsedOffers {
			if !mediaRangeMatches(accepted, offer) {
				continue
			}
			switch {
			case accepted.q > bestQ:
				bestQ = accepted.q
				bestOffer = offers[oi]
			case accepted.q == bestQ && bestOffer != offers[0] && offers[oi] == offers[0]:
				bestOffer = offers[0]
			}
		}
	}
	return bestOffer
}

func parseAcceptHeader(header string) []acceptEntry {
	var entries []acceptEntry
	for _, raw := range strings.Split(header, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ";")
		media := strings.TrimSpace(parts[0])
		q := 1.0
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") {
				if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
					q = v
				}
			}
		}
		if e, ok := parseMediaRange(media, q); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

func parseMediaRange(media string, q float64) (acceptEntry, bool) {
	idx := strings.IndexByte(media, '/')
	if idx <= 0 || idx == len(media)-1 {
		return acceptEntry{}, false
	}
	return acceptEntry{
		typ:     strings.TrimSpace(media[:idx]),
		subtype: strings.TrimSpace(media[idx+1:]),
		q:       q,
	}, true
}

func mediaRangeMatches(accept, offer acceptEntry) bool {
	if accept.typ != "*" && accept.typ != offer.typ {
		return false
	}
	if accept.subtype != "*" && accept.subtype != offer.subtype {
		return false
	}
	return true
}
