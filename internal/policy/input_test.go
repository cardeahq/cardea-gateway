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
	in, err := buildInput(r)
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
		CallID:    "ak_7f3c2b1d9e8a4f60",
		Method:    http.MethodPost,
		Path:      "/v1/messages",
		Headers:   http.Header{"X-Team": {"blue"}},
		Body:      []byte(body),
		Provider:  "anthropic",
		Format:    "anthropic",
		Operation: "messages",
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
				r.Operation = "count_tokens"
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

func TestBuildInput_OperationNameIsTheRoutedOperation(t *testing.T) {
	// operation.name is r.Operation, the neutral name routing detected
	// (specs/0009), never derived from the path here.
	tests := []struct {
		name, format, method, path, op, body string
	}{
		{"anthropic messages", "anthropic", "POST", "/v1/messages", "messages", `{"model":"m","messages":[]}`},
		{"anthropic count_tokens", "anthropic", "POST", "/v1/messages/count_tokens", "count_tokens", `{"model":"m","messages":[]}`},
		{"anthropic models", "anthropic", "GET", "/v1/models", "models", ``},
		{"openai chat is messages", "openai", "POST", "/v1/chat/completions", "messages", `{"model":"m","messages":[]}`},
		{"openai fim is completion", "openai", "POST", "/v1/fim/completions", "completion", `{"model":"m","prompt":"x"}`},
		{"openai embeddings", "openai", "POST", "/v1/embeddings", "embeddings", `{"model":"m","input":"x"}`},
		{"gemini generate is messages", "gemini", "POST", "/v1beta/models/gemini-2.5-pro:generateContent", "messages", `{"contents":[]}`},
		{"gemini embed is embeddings", "gemini", "POST", "/v1beta/models/text-embedding-004:embedContent", "embeddings", `{"content":{"parts":[]}}`},
		{"gemini models", "gemini", "GET", "/v1beta/models", "models", ``},
		{"the path does not decide the name", "anthropic", "POST", "/v1/messages", "count_tokens", `{"model":"m","messages":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := inputReq(tt.body)
			r.Format, r.Method, r.Path, r.Operation = tt.format, tt.method, tt.path, tt.op
			in, _ := buildJSON(t, r)
			op := normalize(t, in["operation"]).(map[string]any)
			if op["name"] != tt.op || op["path"] != tt.path || op["method"] != tt.method {
				t.Fatalf("operation = %v, want name %q with the raw method and path", op, tt.op)
			}
		})
	}
}

func TestBuildInput_ModelsOperationOmitsConversationGroupsAndDoesNotParseTheBody(t *testing.T) {
	// Criterion 34, input side.
	bodies := []string{
		`this is not json {`,
		`{"model":"a","model":"b"}`,
		``,
	}
	for _, format := range []string{"anthropic", "openai", "gemini"} {
		for _, body := range bodies {
			r := inputReq(body)
			r.Format = format
			r.Method = http.MethodGet
			r.Path = "/v1/models"
			r.Operation = "models"
			in, err := buildInput(r)
			if err != nil {
				t.Fatalf("%s: buildInput on models with body %q: %v, want no error", format, body, err)
			}
			for _, k := range []string{"model", "stream", "parameters", "system", "messages", "tools", "tool_calls", "tool_results"} {
				if _, ok := in[k]; ok {
					t.Fatalf("%s: key %q present on a models operation; conversation groups must be absent", format, k)
				}
			}
			for _, k := range []string{"call", "provider", "operation"} {
				if _, ok := in[k]; !ok {
					t.Fatalf("%s: key %q missing on a models operation; generic groups are always present", format, k)
				}
			}
			call := normalize(t, in["call"]).(map[string]any)
			if call["size_bytes"] != float64(len(body)) {
				t.Fatalf("call.size_bytes = %v, want %d", call["size_bytes"], len(body))
			}
		}
	}
}

func TestBuildInput_InvalidConversationBodiesAreErrors(t *testing.T) {
	tests := []struct {
		name, format, path, op, body string
	}{
		{"anthropic count_tokens body that is not JSON", "anthropic", "/v1/messages/count_tokens", "count_tokens", `}{`},
		{"anthropic messages body that is not JSON", "anthropic", "/v1/messages", "messages", `plain text`},
		{"anthropic messages is a string", "anthropic", "/v1/messages", "messages", `{"model":"m","messages":"x"}`},
		{"anthropic messages is an object", "anthropic", "/v1/messages", "messages", `{"model":"m","messages":{}}`},
		{"openai chat body that is not JSON", "openai", "/v1/chat/completions", "messages", `{"model":`},
		{"openai messages is a string", "openai", "/v1/chat/completions", "messages", `{"model":"m","messages":"x"}`},
		{"openai fim body that is not JSON", "openai", "/v1/fim/completions", "completion", `nope`},
		{"openai embeddings body that is not JSON", "openai", "/v1/embeddings", "embeddings", `nope`},
		{"gemini generate body that is not JSON", "gemini", "/v1beta/models/gemini-2.5-pro:generateContent", "messages", `nope`},
		{"gemini contents is an object", "gemini", "/v1beta/models/gemini-2.5-pro:generateContent", "messages", `{"contents":{}}`},
		{"gemini embed body that is not JSON", "gemini", "/v1beta/models/text-embedding-004:embedContent", "embeddings", `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := inputReq(tt.body)
			r.Format, r.Path, r.Operation = tt.format, tt.path, tt.op
			if _, err := buildInput(r); err == nil {
				t.Fatal("buildInput = nil error, want an error (fail closed)")
			}
		})
	}
}

func TestBuildInput_ProviderIsTheRoutedProviderName(t *testing.T) {
	r := inputReq(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	r.Provider = "my-mistral"
	r.Format = "openai"
	r.Path = "/v1/chat/completions"
	in, err := buildInput(r)
	if err != nil {
		t.Fatalf("buildInput: %v", err)
	}
	if in["provider"] != "my-mistral" {
		t.Fatalf("provider = %v, want my-mistral", in["provider"])
	}
	if _, ok := in["messages"]; !ok {
		t.Fatal("messages absent: every format's conversation is read (specs/0009 rule 14)")
	}
}

func TestBuildInput_PairWithNoReaderIsAnError(t *testing.T) {
	tests := []struct{ format, op string }{
		{"", "messages"},
		{"cohere", "messages"},
		{"anthropic", ""},
		{"anthropic", "other"},
		{"anthropic", "completion"},
		{"anthropic", "embeddings"},
		{"openai", "count_tokens"},
		{"gemini", "completion"},
	}
	for _, tt := range tests {
		t.Run(tt.format+"/"+tt.op, func(t *testing.T) {
			r := inputReq(`{"model":"m","messages":[]}`)
			r.Format, r.Operation = tt.format, tt.op
			if _, err := buildInput(r); err == nil {
				t.Fatal("buildInput = nil error, want an error: a pair with no reader fails closed")
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
			_, err := buildInput(inputReq(body))
			if err == nil {
				t.Fatal("buildInput = nil error, want an error for a repeated key")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q carries body content", err.Error())
			}
		})
	}
}
