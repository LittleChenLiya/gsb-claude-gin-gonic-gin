// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Errors returned by named route registration and URL generation.
// Use errors.Is to match them.
var (
	// ErrRouteNameEmpty is returned when a route is registered with an empty name.
	ErrRouteNameEmpty = errors.New("gin: route name must not be empty")
	// ErrRouteNameConflict is returned when a route name is already registered
	// on the same Engine, regardless of the HTTP method.
	ErrRouteNameConflict = errors.New("gin: route name already registered")
	// ErrInvalidHTTPMethod is returned when a named route is registered with
	// an invalid HTTP method.
	ErrInvalidHTTPMethod = errors.New("gin: invalid HTTP method")
	// ErrRouteNotFound is returned by URLFor when no route is registered with
	// the given name.
	ErrRouteNotFound = errors.New("gin: no route registered with the given name")
	// ErrURLMissingParam is returned by URLFor when a path parameter required
	// by the route template is missing.
	ErrURLMissingParam = errors.New("gin: missing path parameter")
	// ErrURLExtraParams is returned by URLFor when parameters are supplied that
	// the route template does not declare.
	ErrURLExtraParams = errors.New("gin: unexpected path parameters")
	// ErrURLEmptyParam is returned by URLFor when a path parameter is empty.
	// A normal :param always occupies exactly one path segment, and a
	// catch-all *param requires at least one segment, so empty values cannot
	// produce a valid URL.
	ErrURLEmptyParam = errors.New("gin: path parameter must not be empty")
)

// INamedRoutes defines the named-route registration interface.
//
// It is deliberately kept out of IRoutes: adding methods to IRoutes would
// break every type outside of Gin that implements it. Type assert when you
// only hold an IRoutes value:
//
//	if n, ok := routes.(gin.INamedRoutes); ok {
//		n.NamedGET("user.show", "/users/:id", handler)
//	}
//
// *gin.RouterGroup and *gin.Engine satisfy this interface, so the assertion
// is only needed when the concrete type has been erased.
type INamedRoutes interface {
	NamedHandle(name, httpMethod, relativePath string, handlers ...HandlerFunc) error
	NamedGET(name, relativePath string, handlers ...HandlerFunc) error
	NamedPOST(name, relativePath string, handlers ...HandlerFunc) error
	NamedPUT(name, relativePath string, handlers ...HandlerFunc) error
	NamedPATCH(name, relativePath string, handlers ...HandlerFunc) error
	NamedDELETE(name, relativePath string, handlers ...HandlerFunc) error
	NamedOPTIONS(name, relativePath string, handlers ...HandlerFunc) error
	NamedHEAD(name, relativePath string, handlers ...HandlerFunc) error
}

var (
	_ INamedRoutes = (*RouterGroup)(nil)
	_ INamedRoutes = (*Engine)(nil)
)

type segmentKind uint8

const (
	segmentLiteral segmentKind = iota
	segmentParam
	segmentCatchAll
)

// routeSegment is one segment of a parsed route path template.
type routeSegment struct {
	kind    segmentKind
	literal string // set for segmentLiteral
	name    string // set for segmentParam and segmentCatchAll
}

// namedRoute is the immutable URL-generation index entry for one named route.
// It is built once at registration time and never mutated afterwards, so
// concurrent URLFor calls can read it without any lock.
type namedRoute struct {
	path     string
	segments []routeSegment
}

// parseNamedRoute splits an absolute route path template into segments.
func parseNamedRoute(path string) *namedRoute {
	route := &namedRoute{path: path}
	for _, seg := range strings.Split(path, "/")[1:] {
		switch {
		case strings.HasPrefix(seg, ":"):
			route.segments = append(route.segments, routeSegment{kind: segmentParam, name: seg[1:]})
		case strings.HasPrefix(seg, "*"):
			route.segments = append(route.segments, routeSegment{kind: segmentCatchAll, name: seg[1:]})
		default:
			route.segments = append(route.segments, routeSegment{kind: segmentLiteral, literal: seg})
		}
	}
	return route
}

// escapePathSegment escapes one path segment for URL generation. Dot segments
// ("." and "..") are percent-encoded so a parameter value can never traverse
// out of the route prefix when the URL is resolved by a client.
func escapePathSegment(seg string) string {
	switch seg {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	}
	return url.PathEscape(seg)
}

// NamedHandle registers a named route for the given HTTP method and path,
// relative to the group's base path. The final absolute path (including all
// nested group prefixes) is what URLFor later generates.
//
// Names are unique per Engine across all HTTP methods: registering the same
// name twice fails with ErrRouteNameConflict even if the methods differ.
// Registration is atomic with respect to the name index: if it fails, the
// route tree and the name index are both left untouched.
//
// Named routes must be registered before the Engine starts serving requests;
// see URLFor for the concurrency contract.
func (group *RouterGroup) NamedHandle(name, httpMethod, relativePath string, handlers ...HandlerFunc) error {
	if name == "" {
		return ErrRouteNameEmpty
	}
	if !regEnLetter.MatchString(httpMethod) {
		return fmt.Errorf("%w: %q", ErrInvalidHTTPMethod, httpMethod)
	}
	engine := group.engine
	if engine.namedRoutes == nil {
		engine.namedRoutes = make(map[string]*namedRoute)
	}
	// Check the name before touching the route tree, so a failed
	// registration never leaves a route in the tree without its name.
	if _, exists := engine.namedRoutes[name]; exists {
		return fmt.Errorf("%w: %q", ErrRouteNameConflict, name)
	}
	absolutePath := group.calculateAbsolutePath(relativePath)
	engine.addRoute(httpMethod, absolutePath, group.combineHandlers(handlers))
	engine.namedRoutes[name] = parseNamedRoute(absolutePath)
	return nil
}

// NamedGET is a shortcut for NamedHandle(name, http.MethodGet, ...).
func (group *RouterGroup) NamedGET(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodGet, relativePath, handlers...)
}

// NamedPOST is a shortcut for NamedHandle(name, http.MethodPost, ...).
func (group *RouterGroup) NamedPOST(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPost, relativePath, handlers...)
}

// NamedPUT is a shortcut for NamedHandle(name, http.MethodPut, ...).
func (group *RouterGroup) NamedPUT(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPut, relativePath, handlers...)
}

// NamedPATCH is a shortcut for NamedHandle(name, http.MethodPatch, ...).
func (group *RouterGroup) NamedPATCH(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPatch, relativePath, handlers...)
}

// NamedDELETE is a shortcut for NamedHandle(name, http.MethodDelete, ...).
func (group *RouterGroup) NamedDELETE(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodDelete, relativePath, handlers...)
}

// NamedOPTIONS is a shortcut for NamedHandle(name, http.MethodOptions, ...).
func (group *RouterGroup) NamedOPTIONS(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodOptions, relativePath, handlers...)
}

// NamedHEAD is a shortcut for NamedHandle(name, http.MethodHead, ...).
func (group *RouterGroup) NamedHEAD(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodHead, relativePath, handlers...)
}

// URLFor generates the URL for the route registered with the given name.
//
// params supplies the path parameters declared in the route template. A
// normal :param fills exactly one path segment; a slash inside its value is
// escaped as %2F so it stays data of that segment. A catch-all *param fills
// one or more segments: its value is split on "/" and each segment is escaped
// individually, and dot segments ("." and "..") are percent-encoded so the
// generated URL can never traverse out of the route prefix. Unicode, percent
// signs, spaces and question marks are escaped exactly once.
//
// query is encoded with url.Values.Encode: keys are sorted for a stable
// output order and repeated keys keep all of their values.
//
// URLFor fails with ErrRouteNotFound for an unknown name, ErrURLMissingParam
// for a missing parameter, ErrURLEmptyParam for an empty parameter value and
// ErrURLExtraParams for parameters the route does not declare.
//
// Register all named routes before the Engine starts serving requests. The
// name index is immutable afterwards, so URLFor only performs reads and is
// safe for concurrent use without any lock.
//
// Note on matching generated URLs: by default Gin matches on the already
// unescaped URL path, so a parameter value containing an escaped slash
// (%2F) would be seen as two segments and fail to match. To round-trip such
// URLs, enable UseRawPath (or UseEscapedPath) together with
// UnescapePathValues:
//
//	router := gin.New()
//	router.UseRawPath = true
//	router.UnescapePathValues = true
func (engine *Engine) URLFor(name string, params Params, query url.Values) (string, error) {
	route, ok := engine.namedRoutes[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrRouteNotFound, name)
	}

	values := make(map[string]string, len(params))
	for _, param := range params {
		values[param.Key] = param.Value
	}

	var b strings.Builder
	b.Grow(len(route.path) + 16)
	for _, seg := range route.segments {
		if seg.kind == segmentLiteral {
			b.WriteByte('/')
			b.WriteString(seg.literal)
			continue
		}
		value, ok := values[seg.name]
		if !ok {
			return "", fmt.Errorf("%w: %q required by route %q", ErrURLMissingParam, seg.name, name)
		}
		delete(values, seg.name)
		if value == "" {
			return "", fmt.Errorf("%w: %q of route %q", ErrURLEmptyParam, seg.name, name)
		}
		if seg.kind == segmentParam {
			b.WriteByte('/')
			b.WriteString(escapePathSegment(value))
			continue
		}
		for _, part := range strings.Split(value, "/") {
			b.WriteByte('/')
			b.WriteString(escapePathSegment(part))
		}
	}

	if len(values) > 0 {
		extra := make([]string, 0, len(values))
		for key := range values {
			extra = append(extra, key)
		}
		sort.Strings(extra)
		return "", fmt.Errorf("%w: %s not declared by route %q", ErrURLExtraParams, strings.Join(extra, ", "), name)
	}

	if len(query) > 0 {
		b.WriteByte('?')
		b.WriteString(query.Encode())
	}
	return b.String(), nil
}
