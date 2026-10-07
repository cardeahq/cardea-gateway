package policy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// normalize round-trips v through JSON so goldens compare as plain JSON
// values, whatever Go types buildInput used.
func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("golden is not JSON: %v\n%s", err, s)
	}
	return out
}

func buildJSON(t *testing.T, r Request) (map[string]any, string) {
	t.Helper()
	in, err := buildInput("anthropic", r)
	if err != nil {
		t.Fatalf("buildInput: %v", err)
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return in, string(b)
}

func inputReq(body string) Request {
	return Request{
		CallID:  "ak_7f3c2b1d9e8a4f60",
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: http.Header{"X-Team": {"blue"}},
		Body:    []byte(body),
	}
}

func TestBuildInput_Golden(t *testing.T) {
	tests := []struct {
		name   string
		req    func() Request
		golden string
	}{
		{
			name: "string content, system blocks, parameters, identified agent and claimed session",
			req: func() Request {
				r := inputReq(`{"model":"claude-sonnet-5","max_tokens":4096,"temperature":0.2,"stream":true,` +
					`"system":[{"type":"text","text":"be"},{"type":"text","text":"nice"}],` +
					`"messages":[{"role":"user","content":"hello"}]}`)
				r.AgentID = "acme-bot"
				r.SessionID = "8d0c7a3e"
				return r
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"agent": {"id":"acme-bot"},
				"session": {"id":"8d0c7a3e"},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "claude-sonnet-5",
				"stream": true,
				"parameters": {"max_tokens":4096,"temperature":0.2},
				"system": "be\nnice",
				"messages": [{"role":"user","content":[{"type":"text","text":"hello"}]}],
				"tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "system as a plain string; no agent and no session means no keys",
			req: func() Request {
				return inputReq(`{"model":"m","stream":false,"system":"plain","messages":[]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m",
				"stream": false,
				"parameters": {},
				"system": "plain",
				"messages": [], "tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "no system prompt gives no system key; empty lists and parameters are present",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m",
				"stream": true,
				"parameters": {},
				"messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}],
				"tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "tool_use becomes tool_call; tool_result text is joined and sized; tool_name hit",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[` +
					`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls"}}]},` +
					`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a"},{"type":"text","text":"bc"}]}]}]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m", "stream": true, "parameters": {},
				"messages": [
					{"role":"assistant","content":[{"type":"tool_call","id":"toolu_1","name":"bash","arguments":{"command":"ls"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_call_id":"toolu_1","is_error":false,"size_bytes":4,"text":"a\nbc"}]}
				],
				"tools": [],
				"tool_calls": [{"id":"toolu_1","name":"bash","arguments":{"command":"ls"}}],
				"tool_results": [{"tool_call_id":"toolu_1","tool_name":"bash","is_error":false,"size_bytes":4,"text":"a\nbc"}]
			}`,
		},
		{
			name: "tool_result with string content and is_error; tool_name miss omits the key",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[` +
					`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_gone","is_error":true,"content":"boom"}]}]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m", "stream": true, "parameters": {},
				"messages": [
					{"role":"user","content":[{"type":"tool_result","tool_call_id":"toolu_gone","is_error":true,"size_bytes":4,"text":"boom"}]}
				],
				"tools": [], "tool_calls": [],
				"tool_results": [{"tool_call_id":"toolu_gone","is_error":true,"size_bytes":4,"text":"boom"}]
			}`,
		},
		{
			name: "image carries no data; unknown block types carry no payload",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[{"role":"user","content":[` +
					`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAABBBB"}},` +
					`{"type":"thinking","thinking":"private","signature":"sig"}]}]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m", "stream": true, "parameters": {},
				"messages": [{"role":"user","content":[{"type":"image"},{"type":"thinking"}]}],
				"tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "tools: schema becomes parameters; server tool carries its type; description omitted when absent",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[],"tools":[` +
					`{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},` +
					`{"type":"web_search_20250305","name":"web_search"}]}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m", "stream": true, "parameters": {},
				"messages": [],
				"tools": [
					{"name":"bash","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}},
					{"name":"web_search","type":"web_search_20250305"}
				],
				"tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "parameters are every other top-level field, unchanged",
			req: func() Request {
				return inputReq(`{"model":"m","stream":true,"messages":[],"system":"s","tools":[],` +
					`"max_tokens":10,"temperature":0.5,"metadata":{"user_id":"u"},"tool_choice":{"type":"auto"},"thinking":{"type":"enabled","budget_tokens":2048}}`)
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"messages","method":"POST","path":"/v1/messages"},
				"model": "m", "stream": true, "system": "s",
				"parameters": {"max_tokens":10,"temperature":0.5,"metadata":{"user_id":"u"},"tool_choice":{"type":"auto"},"thinking":{"type":"enabled","budget_tokens":2048}},
				"messages": [], "tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
		{
			name: "count_tokens is a conversation operation",
			req: func() Request {
				r := inputReq(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
				r.Path = "/v1/messages/count_tokens"
				return r
			},
			golden: `{
				"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
				"provider": "anthropic",
				"operation": {"name":"count_tokens","method":"POST","path":"/v1/messages/count_tokens"},
				"model": "m", "stream": false, "parameters": {},
				"messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}],
				"tools": [], "tool_calls": [], "tool_results": []
			}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req()
			in, raw := buildJSON(t, req)
			golden := strings.ReplaceAll(tt.golden, "%SIZE%", itoa(len(req.Body)))
			if got, want := normalize(t, in), mustJSON(t, golden); !reflect.DeepEqual(got, want) {
				gb, _ := json.MarshalIndent(got, "", "  ")
				wb, _ := json.MarshalIndent(want, "", "  ")
				t.Fatalf("input document mismatch\n got: %s\nwant: %s", gb, wb)
			}
			if strings.Contains(raw, "null") {
				t.Fatalf("input document contains null: a key that does not apply must be omitted\n%s", raw)
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestBuildInput_Operations(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantName string
	}{
		{"messages", "/v1/messages", "messages"},
		{"count_tokens", "/v1/messages/count_tokens", "count_tokens"},
		{"models list is other", "/v1/models", "other"},
		{"unknown path is other", "/something/else", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := inputReq(`{"model":"m","messages":[]}`)
			r.Path = tt.path
			in, _ := buildJSON(t, r)
			op := normalize(t, in["operation"]).(map[string]any)
			if op["name"] != tt.wantName || op["path"] != tt.path || op["method"] != "POST" {
				t.Fatalf("operation = %v, want name %q with the raw method and path", op, tt.wantName)
			}
		})
	}
}

func TestBuildInput_OtherOperationOmitsConversationGroupsAndDoesNotParseTheBody(t *testing.T) {
	bodies := []string{
		`this is not json {`,
		`{"model":"a","model":"b"}`,
		``,
	}
	for _, body := range bodies {
		r := inputReq(body)
		r.Method = http.MethodGet
		r.Path = "/v1/models"
		in, err := buildInput("anthropic", r)
		if err != nil {
			t.Fatalf("buildInput on an other path with body %q: %v, want no error", body, err)
		}
		for _, k := range []string{"model", "stream", "parameters", "system", "messages", "tools", "tool_calls", "tool_results"} {
			if _, ok := in[k]; ok {
				t.Fatalf("key %q present on an other operation; conversation groups must be absent", k)
			}
		}
		for _, k := range []string{"call", "provider", "operation"} {
			if _, ok := in[k]; !ok {
				t.Fatalf("key %q missing on an other operation; generic groups are always present", k)
			}
		}
		call := normalize(t, in["call"]).(map[string]any)
		if call["size_bytes"] != float64(len(body)) {
			t.Fatalf("call.size_bytes = %v, want %d", call["size_bytes"], len(body))
		}
	}
}

func TestBuildInput_OtherProviderHasOnlyGenericGroups(t *testing.T) {
	in, err := buildInput("openai", inputReq(`not json`))
	if err != nil {
		t.Fatalf("buildInput for another provider: %v", err)
	}
	if in["provider"] != "openai" {
		t.Fatalf("provider = %v, want openai", in["provider"])
	}
	for _, k := range []string{"model", "messages", "tools", "tool_calls", "tool_results", "parameters", "system", "stream"} {
		if _, ok := in[k]; ok {
			t.Fatalf("key %q present for a provider with no reader", k)
		}
	}
}

func TestBuildInput_InvalidConversationBodiesAreErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"count_tokens body that is not JSON", "/v1/messages/count_tokens", `}{`},
		{"messages body that is not JSON", "/v1/messages", `plain text`},
		{"messages is a string", "/v1/messages", `{"model":"m","messages":"x"}`},
		{"messages is an object", "/v1/messages", `{"model":"m","messages":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := inputReq(tt.body)
			r.Path = tt.path
			if _, err := buildInput("anthropic", r); err == nil {
				t.Fatal("buildInput = nil error, want an error (fail closed)")
			}
		})
	}
}

func TestBuildInput_HeadersLowerCasedAndCredentialsRemoved(t *testing.T) {
	const secret = "SECRET-CREDENTIAL-VALUE-5d2e"
	r := inputReq(`{"model":"m","messages":[]}`)
	r.Headers = http.Header{}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "Cookie", "Set-Cookie", "X-Gateway-Request-Id"} {
		r.Headers.Set(h, secret)
	}
	r.Headers.Add("X-Team", "blue")
	r.Headers.Add("X-Team", "green")
	r.Headers.Set("Content-Type", "application/json")

	in, raw := buildJSON(t, r)
	if strings.Contains(raw, secret) {
		t.Fatalf("a credential header value reached the input document: %s", raw)
	}
	headers := normalize(t, in["call"]).(map[string]any)["headers"].(map[string]any)
	want := map[string]any{"x-team": []any{"blue", "green"}, "content-type": []any{"application/json"}}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("call.headers = %v, want %v", headers, want)
	}
	for _, banned := range []string{"authorization", "proxy-authorization", "x-api-key", "api-key", "cookie", "set-cookie", "x-gateway-request-id"} {
		if _, ok := headers[banned]; ok {
			t.Fatalf("header %q reached the input", banned)
		}
	}
}

func TestBuildInput_LargeIntegersSurvive(t *testing.T) {
	_, raw := buildJSON(t, inputReq(`{"model":"m","messages":[],"max_tokens":9007199254740993}`))
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatalf("large integer lost precision in the input document: %s", raw)
	}
}

func TestBuildInput_CredentialsInBodyStayAvailableToRulesButNotInHeaders(t *testing.T) {
	// Conversation text is the point of the input; only headers are stripped.
	_, raw := buildJSON(t, inputReq(`{"model":"m","messages":[{"role":"user","content":"visible text"}]}`))
	if !strings.Contains(raw, "visible text") {
		t.Fatalf("conversation text missing from the input: %s", raw)
	}
}

func TestRejectDuplicateKeys(t *testing.T) {
	const keyMarker = "dupkey_marker"
	const valMarker = "dupval_marker"
	rejected := []struct {
		name string
		body string
	}{
		{"top-level key twice", `{"model":"a","model":"b","messages":[]}`},
		{"top-level key twice, same value", `{"model":"a","model":"a","messages":[]}`},
		{"inside a tool", `{"model":"m","messages":[],"tools":[{"name":"a","name":"b","input_schema":{}}]}`},
		{"inside nested tool-call arguments", `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"bash","input":{"command":"ls","command":"rm"}}]}]}`},
		{"inside an array element", `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","role":"assistant","content":"b"}]}`},
		{"deep in an input schema", `{"model":"m","messages":[],"tools":[{"name":"t","input_schema":{"properties":{"p":{"type":"string","type":"number"}}}}]}`},
		{"custom key and value are not echoed", `{"model":"m","messages":[],"` + keyMarker + `":"` + valMarker + `","` + keyMarker + `":"x"}`},
	}
	for _, tt := range rejected {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			err := rejectDuplicateKeys([]byte(tt.body))
			if err == nil {
				t.Fatal("rejectDuplicateKeys = nil, want an error")
			}
			for _, leaked := range []string{keyMarker, valMarker, "model", "command", "role"} {
				if strings.Contains(err.Error(), "\""+leaked+"\"") || strings.Contains(err.Error(), leaked+"\"") {
					t.Fatalf("error %q names a key or value (%q)", err.Error(), leaked)
				}
			}
			if strings.Contains(err.Error(), keyMarker) || strings.Contains(err.Error(), valMarker) {
				t.Fatalf("error %q carries body content", err.Error())
			}
		})
	}

	accepted := []struct {
		name string
		body string
	}{
		{"the same key in two different objects", `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}],"tools":[{"name":"a"},{"name":"b"}]}`},
		{"a key repeated in a parent and a child", `{"name":"x","tools":[{"name":"y"}]}`},
		{"the same key in sibling nested objects", `{"a":{"k":1},"b":{"k":2}}`},
		{"empty object", `{}`},
	}
	for _, tt := range accepted {
		t.Run("accepts "+tt.name, func(t *testing.T) {
			if err := rejectDuplicateKeys([]byte(tt.body)); err != nil {
				t.Fatalf("rejectDuplicateKeys = %v, want nil", err)
			}
		})
	}
}

func TestBuildInput_DuplicateKeysAreErrorsAndNeverLeakContent(t *testing.T) {
	const marker = "LEAKMARKER-8841"
	bodies := map[string]string{
		"model twice":          `{"model":"` + marker + `","model":"b","messages":[]}`,
		"inside tools":         `{"model":"m","messages":[],"tools":[{"name":"` + marker + `","name":"b"}]}`,
		"inside arguments":     `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"` + marker + `":1,"` + marker + `":2}}]}]}`,
		"inside array element": `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","role":"x","content":"` + marker + `"}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, err := buildInput("anthropic", inputReq(body))
			if err == nil {
				t.Fatal("buildInput = nil error, want an error for a repeated key")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q carries body content", err.Error())
			}
		})
	}
}
