package trace

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

const sessionUUID = "8d0c7a3e-5b1f-4c2a-9e6d-1f2a3b4c5d6e"

type wantSession struct {
	status     string
	id         string // "" means nil
	source     string // "" means nil
	sourceName string // "" means nil
}

func checkSession(t *testing.T, got Session, want wantSession) {
	t.Helper()
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	if got.Status != want.status {
		t.Fatalf("Session.Status = %q, want %q", got.Status, want.status)
	}
	if !equalPtr(got.ID, opt(want.id)) {
		t.Fatalf("Session.ID = %v, want %v", deref(got.ID), deref(opt(want.id)))
	}
	if !equalPtr(got.Source, opt(want.source)) {
		t.Fatalf("Session.Source = %v, want %v", deref(got.Source), deref(opt(want.source)))
	}
	if !equalPtr(got.SourceName, opt(want.sourceName)) {
		t.Fatalf("Session.SourceName = %v, want %v", deref(got.SourceName), deref(opt(want.sourceName)))
	}
}

// sessionList is a test-only grouping: sources for the named Agents, or
// for every other agent when Default is set.
type sessionList struct {
	Agents  []string
	Default bool
	Sources []config.SessionSource
}

func defaultList(s ...config.SessionSource) sessionList {
	return sessionList{Default: true, Sources: s}
}

func agentList(agents []string, s ...config.SessionSource) sessionList {
	return sessionList{Agents: agents, Sources: s}
}

// splitLists maps lists onto NewTracer's inputs: the default sources and
// the per-agent sources.
func splitLists(lists []sessionList) ([]config.SessionSource, map[string][]config.SessionSource) {
	var defaults []config.SessionSource
	agents := map[string][]config.SessionSource{}
	for _, l := range lists {
		if l.Default {
			defaults = l.Sources
		}
		for _, a := range l.Agents {
			agents[a] = l.Sources
		}
	}
	return defaults, agents
}

func newListsTracer(t *testing.T, lists ...sessionList) *Tracer {
	t.Helper()
	defaults, agents := splitLists(lists)
	return newTestTracerWith(t, func(c *config.TracingConfig) { c.DefaultSessionSources = defaults }, agents)
}

func bodyWithUserID(v string) string {
	b, _ := json.Marshal(v)
	return `{"model":"m","metadata":{"user_id":` + string(b) + `},"messages":[]}`
}

func TestRequest_Session(t *testing.T) {
	absent := wantSession{status: "absent"}
	xSession := config.SessionSource{Header: "X-Session-Id"}
	userID := config.SessionSource{BodyField: "metadata.user_id"}
	baggage := config.SessionSource{Baggage: "session.id"}

	tests := []struct {
		name    string
		lists   []sessionList
		agentID string
		headers map[string]string
		body    string
		want    wantSession
	}{
		// Each source kind (criteria 17, 20).
		{
			name:    "header source",
			lists:   []sessionList{defaultList(xSession)},
			headers: map[string]string{"X-Session-Id": "s1"},
			want:    wantSession{"claimed", "s1", "header", "X-Session-Id"},
		},
		{
			name:    "empty header value counts as absent",
			lists:   []sessionList{defaultList(xSession)},
			headers: map[string]string{"X-Session-Id": ""},
			want:    absent,
		},
		{
			name:  "body field source",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID(sessionUUID),
			want:  wantSession{"claimed", sessionUUID, "body_field", "metadata.user_id"},
		},
		{
			name:  "missing body field counts as absent",
			lists: []sessionList{defaultList(userID)},
			body:  `{"model":"m","metadata":{},"messages":[]}`,
			want:  absent,
		},
		{
			name:  "body path through a non-object counts as absent",
			lists: []sessionList{defaultList(userID)},
			body:  `{"model":"m","metadata":"flat","messages":[]}`,
			want:  absent,
		},
		{
			name:  "non-JSON body counts as absent",
			lists: []sessionList{defaultList(userID)},
			body:  `not json`,
			want:  absent,
		},
		{
			name:  "non-string body field is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  `{"model":"m","metadata":{"user_id":12345},"messages":[]}`,
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:  "object body field is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  `{"model":"m","metadata":{"user_id":{"id":"s1"}},"messages":[]}`,
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:    "baggage source",
			lists:   []sessionList{defaultList(baggage)},
			headers: map[string]string{"baggage": "session.id=s9,other=x"},
			want:    wantSession{"claimed", "s9", "baggage", "session.id"},
		},
		{
			name:    "baggage member properties are dropped",
			lists:   []sessionList{defaultList(baggage)},
			headers: map[string]string{"baggage": "other=x,session.id=s9;ttl=30;flag"},
			want:    wantSession{"claimed", "s9", "baggage", "session.id"},
		},
		{
			name:    "baggage value is percent-decoded",
			lists:   []sessionList{defaultList(baggage)},
			headers: map[string]string{"baggage": "session.id=a%2Fb%3Dc"},
			want:    wantSession{"claimed", "a/b=c", "baggage", "session.id"},
		},
		{
			name:    "first baggage member with the key wins",
			lists:   []sessionList{defaultList(baggage)},
			headers: map[string]string{"baggage": "session.id=first,session.id=second"},
			want:    wantSession{"claimed", "first", "baggage", "session.id"},
		},
		{
			name:    "baggage without the key counts as absent",
			lists:   []sessionList{defaultList(baggage)},
			headers: map[string]string{"baggage": "other=x"},
			want:    absent,
		},

		// Order and fallback (criterion 19).
		{
			name:    "first source wins when both are present",
			lists:   []sessionList{defaultList(config.SessionSource{Header: "A"}, config.SessionSource{Header: "B"})},
			headers: map[string]string{"A": "from-a", "B": "from-b"},
			want:    wantSession{"claimed", "from-a", "header", "A"},
		},
		{
			name:    "falls back to the next source when the first is absent",
			lists:   []sessionList{defaultList(config.SessionSource{Header: "A"}, config.SessionSource{Header: "B"})},
			headers: map[string]string{"B": "from-b"},
			want:    wantSession{"claimed", "from-b", "header", "B"},
		},
		{
			name:    "an invalid value from the first source decides; no fallback",
			lists:   []sessionList{defaultList(config.SessionSource{Header: "A"}, config.SessionSource{Header: "B"})},
			headers: map[string]string{"A": strings.Repeat("x", 300), "B": "from-b"},
			want:    wantSession{"invalid", "", "header", "A"},
		},
		{
			name:  "no source yields a value",
			lists: []sessionList{defaultList(xSession, userID, baggage)},
			body:  `{"model":"m","messages":[]}`,
			want:  absent,
		},

		// Patterns (criterion 18).
		{
			name:  "pattern keeps only capture group 1",
			lists: []sessionList{defaultList(config.SessionSource{BodyField: "metadata.user_id", Pattern: `_session_([0-9a-f-]{36})$`})},
			body:  bodyWithUserID("user_abc_account_def_session_" + sessionUUID),
			want:  wantSession{"claimed", sessionUUID, "body_field", "metadata.user_id"},
		},
		{
			name:  "pattern non-match counts as absent",
			lists: []sessionList{defaultList(config.SessionSource{BodyField: "metadata.user_id", Pattern: `_session_([0-9a-f-]{36})$`})},
			body:  bodyWithUserID("user_abc_account_def"),
			want:  absent,
		},
		{
			name: "pattern non-match falls back to the next source",
			lists: []sessionList{defaultList(
				config.SessionSource{Header: "X-Session-Id", Pattern: `^sess-(.+)$`},
				config.SessionSource{Header: "X-Fallback"},
			)},
			headers: map[string]string{"X-Session-Id": "nope", "X-Fallback": "fb"},
			want:    wantSession{"claimed", "fb", "header", "X-Fallback"},
		},
		{
			name:    "pattern applies to headers too",
			lists:   []sessionList{defaultList(config.SessionSource{Header: "X-Session-Id", Pattern: `^sess-(.+)$`})},
			headers: map[string]string{"X-Session-Id": "sess-42"},
			want:    wantSession{"claimed", "42", "header", "X-Session-Id"},
		},

		// Validity (criterion 22).
		{
			name:    "128 characters is valid",
			lists:   []sessionList{defaultList(xSession)},
			headers: map[string]string{"X-Session-Id": strings.Repeat("a", 128)},
			want:    wantSession{"claimed", strings.Repeat("a", 128), "header", "X-Session-Id"},
		},
		{
			name:    "129 characters is invalid",
			lists:   []sessionList{defaultList(xSession)},
			headers: map[string]string{"X-Session-Id": strings.Repeat("a", 129)},
			want:    wantSession{"invalid", "", "header", "X-Session-Id"},
		},
		{
			name:  "every printable non-space ASCII character is valid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID("!~azAZ09-_.:/{}\"\\"),
			want:  wantSession{"claimed", "!~azAZ09-_.:/{}\"\\", "body_field", "metadata.user_id"},
		},
		{
			name:  "a newline is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID("abc\ndef"),
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:  "a space is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID("abc def"),
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:  "non-ASCII is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID("café"),
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:  "DEL is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID("abc\x7fdef"),
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},
		{
			name:  "an empty body field is invalid",
			lists: []sessionList{defaultList(userID)},
			body:  bodyWithUserID(""),
			want:  wantSession{"invalid", "", "body_field", "metadata.user_id"},
		},

		// Which list applies (criteria 20, 21).
		{
			name:    "an agent with its own list uses it, not the default",
			lists:   []sessionList{agentList([]string{"agent-x"}, xSession), defaultList(baggage)},
			agentID: "agent-x",
			headers: map[string]string{"X-Session-Id": "own", "baggage": "session.id=dflt"},
			want:    wantSession{"claimed", "own", "header", "X-Session-Id"},
		},
		{
			name:    "an agent with its own list does not fall back to the default",
			lists:   []sessionList{agentList([]string{"agent-x"}, xSession), defaultList(baggage)},
			agentID: "agent-x",
			headers: map[string]string{"baggage": "session.id=dflt"},
			want:    absent,
		},
		{
			name:    "an agent without its own list uses the default",
			lists:   []sessionList{agentList([]string{"agent-x"}, xSession), defaultList(baggage)},
			agentID: "agent-y",
			headers: map[string]string{"X-Session-Id": "own", "baggage": "session.id=s9,other=x"},
			want:    wantSession{"claimed", "s9", "baggage", "session.id"},
		},
		{
			name:    "a list shared by several agents applies to each",
			lists:   []sessionList{agentList([]string{"agent-x", "agent-y"}, xSession)},
			agentID: "agent-y",
			headers: map[string]string{"X-Session-Id": "shared"},
			want:    wantSession{"claimed", "shared", "header", "X-Session-Id"},
		},
		{
			name:    "no agent (identification disabled) uses the default",
			lists:   []sessionList{agentList([]string{"agent-x"}, xSession), defaultList(baggage)},
			agentID: "",
			headers: map[string]string{"X-Session-Id": "own", "baggage": "session.id=dflt"},
			want:    wantSession{"claimed", "dflt", "baggage", "session.id"},
		},
		{
			name:    "an agent without a list and no default reads no session",
			lists:   []sessionList{agentList([]string{"agent-x"}, xSession)},
			agentID: "agent-y",
			headers: map[string]string{"X-Session-Id": "s1"},
			want:    absent,
		},
		{
			name:    "no lists at all reads no session",
			agentID: "agent-x",
			headers: map[string]string{"X-Session-Id": "s1", "baggage": "session.id=s9"},
			body:    bodyWithUserID("s1"),
			want:    absent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newListsTracer(t, tt.lists...)
			body := tt.body
			if body == "" {
				body = `{"model":"m","messages":[]}`
			}
			r := newRequest(t, body)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			info, err := tr.Request(r, []byte(body), tt.agentID)
			if err != nil {
				t.Fatalf("Request: %v, want nil: a session never causes an error", err)
			}
			checkSession(t, info.Session, tt.want)
		})
	}
}

func TestRequest_InvalidSessionValueLeavesNoTrace(t *testing.T) {
	const marker = "INVALIDSESSIONMARKER"
	tests := []struct {
		name  string
		value string
	}{
		{"too long", marker + strings.Repeat("x", 300)},
		{"contains a newline", marker + "\nrest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newListsTracer(t, defaultList(config.SessionSource{BodyField: "metadata.user_id"}))
			body := bodyWithUserID(tt.value)
			info, err := tr.Request(newRequest(t, body), []byte(body), "")
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			raw, err := json.Marshal(info.Session)
			if err != nil {
				t.Fatalf("Marshal(Session): %v", err)
			}
			if strings.Contains(string(raw), marker) {
				t.Fatalf("Session %s carries part of an invalid value", raw)
			}
			checkSession(t, info.Session, wantSession{"invalid", "", "body_field", "metadata.user_id"})
		})
	}
}

func TestRequest_PatternRecordsOnlyTheCapturedPart(t *testing.T) {
	tr := newListsTracer(t, defaultList(config.SessionSource{BodyField: "metadata.user_id", Pattern: `_session_([0-9a-f-]{36})$`}))
	body := bodyWithUserID("user_abc123xyz_account_def456uvw_session_" + sessionUUID)
	info, err := tr.Request(newRequest(t, body), []byte(body), "")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	raw, _ := json.Marshal(info.Session)
	for _, leaked := range []string{"abc123xyz", "def456uvw"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("Session %s carries %q, which is outside the pattern's capture group", raw, leaked)
		}
	}
}
