package routing

import (
	"fmt"
	"strings"
)

// Route is one entry a call can be resolved to: a configured provider in
// list mode, or the sole provider in single mode.
type Route struct {
	Provider string   // configured provider name
	Kind     string   // "anthropic", "mistral" or "gemini"
	Formats  []Format // non-empty
	Patterns []string // model patterns; empty in single mode
	Single   bool     // true only for the route built by NewSingleTable
}

// Rejection names why Resolve could not choose a route.
type Rejection string

const (
	RejectNone           Rejection = ""
	RejectUnknownFormat  Rejection = "unknown_format"
	RejectFormatMismatch Rejection = "format_mismatch"
	RejectNoProvider     Rejection = "no_provider"
	RejectUnsupportedOp  Rejection = "unsupported_operation"
	RejectNotServed      Rejection = "format_not_served" // single mode: the route does not speak the format
)

// Table resolves a call's format and model to a Route.
type Table struct {
	routes   []Route
	defaults map[Format]string

	single      bool
	singleRoute Route
}

// NewTable builds a list-mode table. It errors when a default names no
// route or names a route that does not speak that format.
func NewTable(routes []Route, defaults map[Format]string) (*Table, error) {
	t := &Table{routes: routes, defaults: defaults}
	for format, name := range defaults {
		r, ok := t.routeByName(name)
		if !ok {
			return nil, fmt.Errorf("routing.defaults[%s] names unknown provider %q", format, name)
		}
		if !speaksFormat(r, format) {
			return nil, fmt.Errorf("routing.defaults[%s] names provider %q, which does not speak %s", format, name, format)
		}
	}
	return t, nil
}

// NewSingleTable builds a single-mode table whose one route serves every
// call in its formats.
func NewSingleTable(r Route) *Table {
	return &Table{single: true, singleRoute: r}
}

// Resolve applies specs/0009-technical, Design -> Resolution. rej is
// RejectNone exactly when r is usable.
func (t *Table) Resolve(f Format, model string, found bool) (r Route, rej Rejection) {
	if f == FormatUnknown {
		return Route{}, RejectUnknownFormat
	}
	if t.single {
		if !speaksFormat(t.singleRoute, f) {
			return Route{}, RejectNotServed
		}
		return t.singleRoute, RejectNone
	}
	if found {
		anyMatch := false
		for _, route := range t.routes {
			if !matchesPattern(route.Patterns, model) {
				continue
			}
			anyMatch = true
			if speaksFormat(route, f) {
				return route, RejectNone
			}
		}
		if anyMatch {
			return Route{}, RejectFormatMismatch
		}
	}
	if name, ok := t.defaults[f]; ok {
		if route, ok := t.routeByName(name); ok {
			return route, RejectNone
		}
	}
	return Route{}, RejectNoProvider
}

// Routes returns every configured route, in list order. In single mode it
// returns the single route.
func (t *Table) Routes() []Route {
	if t.single {
		return []Route{t.singleRoute}
	}
	return append([]Route(nil), t.routes...)
}

// DefaultFormats returns the formats for which provider is the default.
func (t *Table) DefaultFormats(provider string) []Format {
	var out []Format
	for f, name := range t.defaults {
		if name == provider {
			out = append(out, f)
		}
	}
	return out
}

// Single reports whether t was built by NewSingleTable.
func (t *Table) Single() bool { return t.single }

func (t *Table) routeByName(name string) (Route, bool) {
	for _, r := range t.routes {
		if r.Provider == name {
			return r, true
		}
	}
	return Route{}, false
}

func speaksFormat(r Route, f Format) bool {
	for _, rf := range r.Formats {
		if rf == f {
			return true
		}
	}
	return false
}

// matchesPattern reports whether model matches one of patterns: exactly,
// or as a prefix when the pattern ends in "*".
func matchesPattern(patterns []string, model string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(model, prefix) {
				return true
			}
		} else if p == model {
			return true
		}
	}
	return false
}
