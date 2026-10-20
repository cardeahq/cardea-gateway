// Package identification classifies a proxied call's agent type from
// request headers, never denying a call.
package identification

import (
	"fmt"
	"net/http"
	"regexp"
)

// Unknown is the agent type attached to a call that matches no
// configured agent type, has no configured agent types to match
// against, or is otherwise unclassified. It is reserved: no agent-type
// file may use it as its own agent_id.
const Unknown = "unknown"

// MatchRule is one way to recognize an agent type from a single request
// header. Exactly one of Value, Prefix, Pattern is set.
type MatchRule struct {
	Header  string `yaml:"header"`
	Value   string `yaml:"value"`   // exact, case-sensitive match against the header's value
	Prefix  string `yaml:"prefix"`  // the header's value starts with this
	Pattern string `yaml:"pattern"` // RE2; the header's value matches this
}

// compiledRule is one MatchRule with its Pattern, if any, precompiled.
type compiledRule struct {
	header  string
	value   string
	prefix  string
	pattern *regexp.Regexp
}

func (r compiledRule) matches(req *http.Request) bool {
	v := req.Header.Get(r.header)
	switch {
	case r.pattern != nil:
		return r.pattern.MatchString(v)
	case r.prefix != "":
		return len(v) >= len(r.prefix) && v[:len(r.prefix)] == r.prefix
	default:
		return v == r.value
	}
}

// agentType is one loaded, compiled agent-type definition.
type agentType struct {
	id    string
	rules []compiledRule
}

// Registry holds every loaded agent type, in file-load order — also
// match precedence (functional spec rule 4): the first type any of whose
// rules match wins.
type Registry struct {
	types []agentType
}

// IDs returns every loaded agent type's ID, in no particular order. Used
// by gateway.buildStages to cross-validate tracing.sessions[].agents.
func (r *Registry) IDs() []string {
	ids := make([]string, len(r.types))
	for i, t := range r.types {
		ids[i] = t.id
	}
	return ids
}

// Identify returns the first matching agent type's ID, or Unknown.
func (r *Registry) Identify(req *http.Request) string {
	for _, t := range r.types {
		for _, rule := range t.rules {
			if rule.matches(req) {
				return t.id
			}
		}
	}
	return Unknown
}

func compileRule(rule MatchRule) (compiledRule, error) {
	set := 0
	for _, v := range []string{rule.Value, rule.Prefix, rule.Pattern} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return compiledRule{}, fmt.Errorf("header %q: exactly one of value/prefix/pattern must be set", rule.Header)
	}
	c := compiledRule{header: rule.Header, value: rule.Value, prefix: rule.Prefix}
	if rule.Pattern != "" {
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return compiledRule{}, fmt.Errorf("header %q: invalid pattern %q: %w", rule.Header, rule.Pattern, err)
		}
		c.pattern = re
	}
	return c, nil
}
