// Copyright 2026 Gin Core Team. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Errors returned by named route registration and URLFor. Use errors.Is to
// match them.
var (
	// ErrRouteNameConflict is returned when a route name is already
	// registered on the Engine. The conflicting route is not registered.
	ErrRouteNameConflict = errors.New("gin: route name already registered")
	// ErrRouteEmptyName is returned when registering a route with an empty name.
	ErrRouteEmptyName = errors.New("gin: route name can not be empty")
	// ErrRouteNameNotFound is returned by URLFor for an unknown route name.
	ErrRouteNameNotFound = errors.New("gin: named route not found")
	// ErrRouteMissingParam is returned by URLFor when a path parameter
	// required by the route is missing.
	ErrRouteMissingParam = errors.New("gin: missing path parameter")
	// ErrRouteExtraParam is returned by URLFor when a parameter is supplied
	// that the route does not declare.
	ErrRouteExtraParam = errors.New("gin: unexpected path parameter")
	// ErrRouteEmptyParam is returned by URLFor when a regular :param
	// receives an empty value.
	ErrRouteEmptyParam = errors.New("gin: empty path parameter")
	// ErrRouteUnsafeParam is returned by URLFor when a parameter value is a
	// dot segment ("." or "..") that could escape the route prefix.
	ErrRouteUnsafeParam = errors.New("gin: unsafe dot-segment path parameter")
)

// routeSegment is one parsed segment of a named route's full path.
type routeSegment struct {
	literal  string // literal segment text, empty for parameter segments
	param    string // parameter name for :param and *param segments
	catchAll bool   // true for *param segments
}

// namedRoute is the immutable reverse-lookup record of a named route. It is
// built once at registration time and never mutated afterwards, so URLFor can
// read it concurrently without locking.
type namedRoute struct {
	name     string
	fullPath string
	segments []routeSegment
}

// parseNamedRoute validates the route name and parses the absolute path into
// segments for URL generation.
func parseNamedRoute(name, fullPath string) (*namedRoute, error) {
	if name == "" {
		return nil, ErrRouteEmptyName
	}
	route := &namedRoute{name: name, fullPath: fullPath}
	for _, seg := range strings.Split(fullPath, "/")[1:] {
		switch {
		case strings.HasPrefix(seg, ":"):
			route.segments = append(route.segments, routeSegment{param: seg[1:]})
		case strings.HasPrefix(seg, "*"):
			route.segments = append(route.segments, routeSegment{param: seg[1:], catchAll: true})
		default:
			route.segments = append(route.segments, routeSegment{literal: seg})
		}
	}
	return route, nil
}

// build renders the route path with the given parameters. Every value is
// escaped exactly once with url.PathEscape, so Unicode, '%', spaces and '?'
// are encoded a single time and never double-encoded.
func (route *namedRoute) build(params map[string]string) (string, error) {
	var sb strings.Builder
	used := make(map[string]struct{}, len(route.segments))
	for _, seg := range route.segments {
		sb.WriteByte('/')
		if seg.param == "" {
			sb.WriteString(seg.literal)
			continue
		}
		value, ok := params[seg.param]
		if !ok {
			return "", fmt.Errorf("%w %q for route %q", ErrRouteMissingParam, seg.param, route.name)
		}
		used[seg.param] = struct{}{}
		if seg.catchAll {
			// A catch-all value may span multiple segments; each segment is
			// escaped individually and dot segments are rejected so the
			// generated URL can never escape the route prefix.
			for i, part := range strings.Split(value, "/") {
				if part == "." || part == ".." {
					return "", fmt.Errorf("%w %q for route %q", ErrRouteUnsafeParam, seg.param, route.name)
				}
				if i > 0 {
					sb.WriteByte('/')
				}
				sb.WriteString(url.PathEscape(part))
			}
			continue
		}
		if value == "" {
			return "", fmt.Errorf("%w %q for route %q", ErrRouteEmptyParam, seg.param, route.name)
		}
		if value == "." || value == ".." {
			return "", fmt.Errorf("%w %q for route %q", ErrRouteUnsafeParam, seg.param, route.name)
		}
		sb.WriteString(url.PathEscape(value))
	}
	return sb.String(), route.checkExtra(params, used)
}

func (route *namedRoute) checkExtra(params map[string]string, used map[string]struct{}) error {
	for key := range params {
		if _, ok := used[key]; !ok {
			return fmt.Errorf("%w %q for route %q", ErrRouteExtraParam, key, route.name)
		}
	}
	return nil
}

// URLFor returns the URL of the route registered under name, with path
// parameters substituted and query encoded. It only reads the immutable
// named-route index, so it is safe for concurrent use once registration has
// completed (register all routes before the Engine starts serving requests).
//
// Regular :param values occupy exactly one path segment; embedded slashes are
// escaped as %2F. Catch-all *param values may contain slashes and are escaped
// segment by segment; "." and ".." segments are rejected with
// ErrRouteUnsafeParam. Query values are encoded with url.Values.Encode, which
// sorts keys and preserves repeated values for the same key.
//
// Note on matching generated URLs: by default Gin matches on the decoded
// URL.Path, so a generated %2F arrives as a literal slash and will not match
// a single :param segment. To round-trip parameters containing slashes, set
// Engine.UseRawPath = true (with Engine.UnescapePathValues = true, the
// default) so matching uses the escaped path and parameters are unescaped
// back to their original values.
func (engine *Engine) URLFor(name string, params map[string]string, query url.Values) (string, error) {
	route, ok := engine.namedRoutes[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrRouteNameNotFound, name)
	}
	path, err := route.build(params)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return path, nil
}

// NamedHandle registers a named route for the given HTTP method and path,
// relative to the group's base path. The final absolute path (including all
// nested group prefixes) is the basis for URLFor generation.
//
// Names are unique per Engine across all HTTP methods. If name is empty or
// already registered, an error is returned and nothing is added to the route
// tree or the name index. It panics on an invalid HTTP method, like Handle.
func (group *RouterGroup) NamedHandle(name, httpMethod, relativePath string, handlers ...HandlerFunc) error {
	if matched := regEnLetter.MatchString(httpMethod); !matched {
		panic("http method " + httpMethod + " is not valid")
	}
	absolutePath := group.calculateAbsolutePath(relativePath)
	route, err := parseNamedRoute(name, absolutePath)
	if err != nil {
		return err
	}
	engine := group.engine
	if _, exists := engine.namedRoutes[name]; exists {
		return fmt.Errorf("%w: %q", ErrRouteNameConflict, name)
	}
	group.handle(httpMethod, relativePath, handlers)
	engine.namedRoutes[name] = route
	return nil
}

// NamedGET is a shortcut for NamedHandle(name, "GET", path, handlers).
func (group *RouterGroup) NamedGET(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodGet, relativePath, handlers...)
}

// NamedPOST is a shortcut for NamedHandle(name, "POST", path, handlers).
func (group *RouterGroup) NamedPOST(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPost, relativePath, handlers...)
}

// NamedPUT is a shortcut for NamedHandle(name, "PUT", path, handlers).
func (group *RouterGroup) NamedPUT(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPut, relativePath, handlers...)
}

// NamedPATCH is a shortcut for NamedHandle(name, "PATCH", path, handlers).
func (group *RouterGroup) NamedPATCH(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodPatch, relativePath, handlers...)
}

// NamedDELETE is a shortcut for NamedHandle(name, "DELETE", path, handlers).
func (group *RouterGroup) NamedDELETE(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodDelete, relativePath, handlers...)
}

// NamedHEAD is a shortcut for NamedHandle(name, "HEAD", path, handlers).
func (group *RouterGroup) NamedHEAD(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodHead, relativePath, handlers...)
}

// NamedOPTIONS is a shortcut for NamedHandle(name, "OPTIONS", path, handlers).
func (group *RouterGroup) NamedOPTIONS(name, relativePath string, handlers ...HandlerFunc) error {
	return group.NamedHandle(name, http.MethodOptions, relativePath, handlers...)
}