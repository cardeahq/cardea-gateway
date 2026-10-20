package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The JSON Schemas under schemas/ are the public contract for the records.
// These tests keep them and record.go from drifting apart: real records must
// validate, and the schema must name exactly the fields the structs encode.

func schemaPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	s, err := c.Compile(schemaPath(t, name))
	if err != nil {
		t.Fatalf("compiling %s: %v", name, err)
	}
	return s
}

func readSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(schemaPath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", name, err)
	}
	return doc
}

func schemaTS(us int) Timestamp {
	return Timestamp(time.Date(2026, 9, 27, 14, 3, 11, us*1000, time.UTC))
}

func schemaRecords() map[string]Record {
	claimed := Session{ID: ptr("8d0c7a3e-5b1f-4c2a-9e6d-1f2a3b4c5d6e"), Status: "claimed", Source: ptr("body_field"), SourceName: ptr("metadata.user_id")}
	absent := Session{Status: "absent"}
	return map[string]Record{
		"request forwarded, every stage and a tool result": RequestRecord{
			Schema: Schema, Record: "request", CallID: "ak_7f3c2b1d9e8a4f60", RemoteAddr: "203.0.113.42", AgentID: ptr("acme-bot"),
			Session: claimed, ReceivedAt: schemaTS(402113), DurationUS: 1794, Model: ptr("claude-sonnet-5"), Stream: true,
			SizeBytes: ptr(int64(48213)),
			Outcome:   RequestOutcome{Result: "forwarded"},
			Stages: []RequestStage{
				{Name: "identification", StartedAt: ptr(schemaTS(402200)), DurationUS: ptr(int64(41)), Decision: "allowed"},
				{Name: "policies", StartedAt: ptr(schemaTS(402640)), DurationUS: ptr(int64(903)), Decision: "allowed",
					Detail: &StageDetail{Revision: "r7", Rules: []string{}}},
			},
			ToolResults: []ToolResult{{ToolCallID: "toolu_01A9", IsError: false, SizeBytes: 5120, Fingerprint: ptr("fp_9c1e0b7d44a2f316")}},
			Findings:    []Finding{},
		},
		"request denied by policy, later stages not run": RequestRecord{
			Schema: Schema, Record: "request", CallID: "ak_0000000000000001", RemoteAddr: "203.0.113.42", AgentID: ptr("acme-bot"),
			Session: absent, ReceivedAt: schemaTS(1), DurationUS: 12, Model: nil, SizeBytes: ptr(int64(10)),
			Outcome: RequestOutcome{Result: "denied", Status: ptr(403), Stage: ptr("policies"), Reason: ptr("policy_denied")},
			Stages: []RequestStage{
				{Name: "policies", StartedAt: ptr(schemaTS(2)), DurationUS: ptr(int64(5)), Decision: "denied",
					Detail: &StageDetail{Revision: "r7", Rules: []string{"no-rm-rf"}}},
				{Name: "tracing", Decision: "not_run"},
			},
			ToolResults: []ToolResult{}, Findings: []Finding{},
		},
		"request refused before its body was read": RequestRecord{
			Schema: Schema, Record: "request", CallID: "ak_0000000000000002", RemoteAddr: "203.0.113.42", Session: absent, ReceivedAt: schemaTS(3),
			Outcome:     RequestOutcome{Result: "failed", Status: ptr(413), Stage: ptr("pipeline"), Reason: ptr("body_too_large")},
			Stages:      []RequestStage{},
			ToolResults: []ToolResult{}, Findings: []Finding{},
		},
		"response delivered with tokens and tool calls": ResponseRecord{
			Schema: Schema, Record: "response", CallID: "ak_7f3c2b1d9e8a4f60", RemoteAddr: "203.0.113.42", AgentID: ptr("acme-bot"), Session: claimed,
			StartedAt: schemaTS(403907), FirstByteUS: ptr(int64(620114)), DurationUS: 4180337,
			Outcome:  ResponseOutcome{Result: "delivered", UpstreamStatus: ptr(200), Status: 200},
			Stages:   []ResponseStage{{Name: "tracing", StartedAt: schemaTS(24021), DurationUS: 1840, Observation: "completed"}},
			Streamed: true, ResponseID: ptr("msg_01XFDUDYJgAACzvnptvVoYEL"), ResponseModel: ptr("claude-sonnet-5-20260801"),
			StopReason: ptr("tool_use"), Tokens: &Tokens{Sent: 1834, Received: 212},
			ToolCalls: []ToolCall{
				{ToolCallID: "toolu_01B2", ToolName: "bash", ArgumentNames: []string{"command"}, SizeBytes: ptr(int64(38)),
					Fingerprint: ptr("fp_03d8a6c2e91b7f40"), RelayedAt: ptr(schemaTS(571930)), Outcome: "delivered", Recorded: true},
				{ToolCallID: "toolu_01B3", ToolName: "read", Outcome: "not_delivered", Recorded: false},
			},
			Findings: []Finding{{Kind: "response_truncated"}},
		},
		"response unanswered": ResponseRecord{
			Schema: Schema, Record: "response", CallID: "ak_0000000000000003", RemoteAddr: "203.0.113.42", Session: absent, StartedAt: schemaTS(4), DurationUS: 10004213,
			Outcome: ResponseOutcome{Result: "unanswered", Status: 502},
			Stages:  []ResponseStage{}, ToolCalls: []ToolCall{}, Findings: []Finding{},
		},
	}
}

func TestSchemas_RealRecordsValidate(t *testing.T) {
	record := compileSchema(t, "trace-record.v1.schema.json")
	line := compileSchema(t, "cardea-intake-line.v1.schema.json")

	for name, rec := range schemaRecords() {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			var asRecord, asLine any
			if err := json.Unmarshal(raw, &asRecord); err != nil {
				t.Fatal(err)
			}
			if err := record.Validate(asRecord); err != nil {
				t.Fatalf("record does not match trace-record.v1: %v\n%s", err, raw)
			}
			enveloped := `{"seq":7,"record":` + string(raw) + `}`
			if err := json.Unmarshal([]byte(enveloped), &asLine); err != nil {
				t.Fatal(err)
			}
			if err := line.Validate(asLine); err != nil {
				t.Fatalf("line does not match cardea-intake-line.v1: %v\n%s", err, enveloped)
			}
		})
	}
}

func TestSchemas_RejectWhatTheContractForbids(t *testing.T) {
	record := compileSchema(t, "trace-record.v1.schema.json")
	line := compileSchema(t, "cardea-intake-line.v1.schema.json")

	var good map[string]any
	raw, _ := json.Marshal(schemaRecords()["response unanswered"])
	if err := json.Unmarshal(raw, &good); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(m map[string]any)) any {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f(m)
		return m
	}

	tests := []struct {
		name string
		s    *jsonschema.Schema
		doc  any
	}{
		{"a field left out", record, mutate(func(m map[string]any) { delete(m, "tokens") })},
		{"a wrong schema version", record, mutate(func(m map[string]any) { m["schema"] = "cardea-gateway.trace/v2" })},
		{"a call id without its prefix", record, mutate(func(m map[string]any) { m["call_id"] = "7f3c" })},
		{"a timestamp without microseconds", record, mutate(func(m map[string]any) { m["started_at"] = "2026-09-27T14:03:11Z" })},
		{"a negative duration", record, mutate(func(m map[string]any) { m["duration_us"] = -1 })},
		{"an unknown record type", record, mutate(func(m map[string]any) { m["record"] = "tool" })},
		{"a line without its number", line, map[string]any{"record": good}},
		{"a line numbered 0", line, map[string]any{"seq": 0, "record": good}},
		{"a line whose record is incomplete", line, map[string]any{"seq": 1, "record": map[string]any{"record": "response"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.s.Validate(tt.doc); err == nil {
				t.Fatal("validation passed, want it to fail")
			}
		})
	}

	extra := mutate(func(m map[string]any) { m["a_field_added_in_v1_1"] = true })
	if err := record.Validate(extra); err != nil {
		t.Fatalf("a record with an added field must still validate (v1 may grow): %v", err)
	}
	unknownValue := mutate(func(m map[string]any) { m["outcome"].(map[string]any)["result"] = "a_value_added_later" })
	if err := record.Validate(unknownValue); err != nil {
		t.Fatalf("a record with a new value in a listed-values field must still validate: %v", err)
	}
}

// jsonFields returns the JSON field names a struct encodes, in order.
func jsonFields(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// walk follows keys through nested maps and, for a numeric-looking step
// "[0]", into the first element of an array.
func walk(t *testing.T, doc any, path ...string) map[string]any {
	t.Helper()
	cur := doc
	for _, p := range path {
		if p == "[0]" {
			arr, ok := cur.([]any)
			if !ok || len(arr) == 0 {
				t.Fatalf("schema path %v: not a non-empty array at [0]", path)
			}
			cur = arr[0]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: %q is not under an object", path, p)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("schema path %v: no %q", path, p)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("schema path %v is not an object", path)
	}
	return m
}

func TestSchemas_NameExactlyTheFieldsTheStructsEncode(t *testing.T) {
	doc := readSchema(t, "trace-record.v1.schema.json")
	defs := func(parts ...string) []string { return append([]string{"$defs"}, parts...) }

	tests := []struct {
		name string
		typ  reflect.Type
		path []string
	}{
		{"RequestRecord", reflect.TypeFor[RequestRecord](), defs("requestRecord")},
		{"ResponseRecord", reflect.TypeFor[ResponseRecord](), defs("responseRecord")},
		{"Session", reflect.TypeFor[Session](), defs("session")},
		{"Finding", reflect.TypeFor[Finding](), defs("finding")},
		{"RequestOutcome", reflect.TypeFor[RequestOutcome](), defs("requestRecord", "properties", "outcome")},
		{"RequestStage", reflect.TypeFor[RequestStage](), defs("requestRecord", "properties", "stages", "items")},
		{"StageDetail", reflect.TypeFor[StageDetail](), defs("requestRecord", "properties", "stages", "items", "properties", "detail", "oneOf", "[0]")},
		{"ToolResult", reflect.TypeFor[ToolResult](), defs("requestRecord", "properties", "tool_results", "items")},
		{"ResponseOutcome", reflect.TypeFor[ResponseOutcome](), defs("responseRecord", "properties", "outcome")},
		{"ResponseStage", reflect.TypeFor[ResponseStage](), defs("responseRecord", "properties", "stages", "items")},
		{"Tokens", reflect.TypeFor[Tokens](), defs("responseRecord", "properties", "tokens", "oneOf", "[0]")},
		{"ToolCall", reflect.TypeFor[ToolCall](), defs("responseRecord", "properties", "tool_calls", "items")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := walk(t, doc, tt.path...)
			var props []string
			for k := range walk(t, node, "properties") {
				props = append(props, k)
			}
			slices.Sort(props)
			var required []string
			for _, r := range node["required"].([]any) {
				required = append(required, r.(string))
			}
			slices.Sort(required)

			want := jsonFields(tt.typ)
			if !slices.Equal(props, want) {
				t.Fatalf("schema properties = %v, %s encodes %v: change schemas/trace-record.v1.schema.json with record.go", props, tt.name, want)
			}
			if !slices.Equal(required, want) {
				t.Fatalf("schema required = %v, want every field %v: a record always carries all its fields (null when not applicable)", required, want)
			}
		})
	}
}
