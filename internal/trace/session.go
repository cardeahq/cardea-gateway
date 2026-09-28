package trace

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const maxSessionLen = 128

type source struct {
	kind    string // "header", "body_field" or "baggage"
	name    string
	pattern *regexp.Regexp
}

// session tries each source for agentID in order; the first that yields a
// value decides. An invalid value is dropped here and never stored.
func (t *Tracer) session(r *http.Request, body []byte, agentID string) Session {
	var parsed any
	var parsedOK, parseTried bool

	for _, src := range t.sourcesFor(agentID) {
		var value string
		var found, valid bool
		switch src.kind {
		case "header":
			value = r.Header.Get(src.name)
			found, valid = value != "", true
		case "body_field":
			if !parseTried {
				parseTried = true
				dec := json.NewDecoder(bytes.NewReader(body))
				dec.UseNumber()
				parsedOK = dec.Decode(&parsed) == nil
			}
			if parsedOK {
				value, found, valid = bodyField(parsed, src.name)
			}
		case "baggage":
			value, found, valid = baggageValue(r.Header.Values("Baggage"), src.name)
		}
		if !found {
			continue
		}
		if valid && src.pattern != nil {
			m := src.pattern.FindStringSubmatch(value)
			if m == nil {
				continue
			}
			value = m[1]
		}

		kind, name := src.kind, src.name
		s := Session{Status: "invalid", Source: &kind, SourceName: &name}
		if valid && validSessionID(value) {
			s.Status = "claimed"
			s.ID = &value
		}
		return s
	}
	return Session{Status: "absent"}
}

// bodyField follows a dot-separated path of object keys. A missing path is
// not found; a present value that is not a string is found but invalid.
func bodyField(v any, path string) (value string, found, valid bool) {
	for key := range strings.SplitSeq(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return "", false, false
		}
		if v, ok = obj[key]; !ok {
			return "", false, false
		}
	}
	s, ok := v.(string)
	return s, true, ok
}

// baggageValue reads key from W3C baggage headers: comma-separated
// key=value members, properties after ";" dropped, values percent-encoded.
// The first member with the key wins.
func baggageValue(headers []string, key string) (value string, found, valid bool) {
	for _, h := range headers {
		for member := range strings.SplitSeq(h, ",") {
			member, _, _ = strings.Cut(member, ";")
			k, v, ok := strings.Cut(member, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			decoded, err := url.PathUnescape(strings.TrimSpace(v))
			if err != nil {
				return "", true, false
			}
			return decoded, decoded != "", true
		}
	}
	return "", false, false
}

func validSessionID(s string) bool {
	if len(s) < 1 || len(s) > maxSessionLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
