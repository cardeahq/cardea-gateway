package policy

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// specs/0004-technical, Format readers (openai rows); functional rule 12.

func openAIReq(path, op, body string) Request {
	return Request{
		CallID:    "ak_7f3c2b1d9e8a4f60",
		Method:    http.MethodPost,
		Path:      path,
		Headers:   http.Header{"X-Team": {"blue"}},
		Body:      []byte(body),
		Provider:  "mistral",
		Format:    "openai",
		Operation: op,
	}
}

func TestBuildInputOpenAI_ChatGolden(t *testing.T) {
	r := openAIReq("/v1/chat/completions", "messages", `{"model":"mistral-large-latest","stream":true,"temperature":0.3,"messages":[`+
		`{"role":"system","content":"be"},`+
		`{"role":"developer","content":[{"type":"text","text":"nice"}]},`+
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"input_audio","input_audio":{"data":"BBBB","format":"wav"}}]},`+
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},`+
		`{"role":"tool","tool_call_id":"call_1","content":"a.go"}],`+
		`"tools":[{"type":"function","function":{"name":"bash","description":"run","parameters":{"type":"object"}}}]}`)
	golden := `{
		"call": {"id":"ak_7f3c2b1d9e8a4f60","size_bytes":%SIZE%,"headers":{"x-team":["blue"]}},
		"provider": "mistral",
		"operation": {"name":"messages","method":"POST","path":"/v1/chat/completions"},
		"model": "mistral-large-latest",
		"stream": true,
		"parameters": {"temperature":0.3},
		"system": "be\nnice",
		"messages": [
			{"role":"system","content":[{"type":"text","text":"be"}]},
			{"role":"developer","content":[{"type":"text","text":"nice"}]},
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image"},{"type":"audio"}]},
			{"role":"assistant","content":[{"type":"tool_call","id":"call_1","name":"bash","arguments":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_call_id":"call_1","is_error":false,"size_bytes":4,"text":"a.go"}]}
		],
		"tools": [{"name":"bash","description":"run","parameters":{"type":"object"}}],
		"tool_calls": [{"id":"call_1","name":"bash","arguments":{"command":"ls"}}],
		"tool_results": [{"tool_call_id":"call_1","tool_name":"bash","is_error":false,"size_bytes":4,"text":"a.go"}]
	}`
	in, raw := buildJSON(t, r)
	golden = strings.ReplaceAll(golden, "%SIZE%", itoa(len(r.Body)))
	if got, want := normalize(t, in), mustJSON(t, golden); !reflect.DeepEqual(got, want) {
		t.Fatalf("input document mismatch\n got: %s\nwant: %s", raw, golden)
	}
	for _, leak := range []string{"AAAA", "BBBB"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("input carries binary data %q: image and audio parts carry no data\n%s", leak, raw)
		}
	}
}

func TestBuildInputOpenAI_ArgumentsThatAreNotJSONStayAString(t *testing.T) {
	r := openAIReq("/v1/chat/completions", "messages", `{"model":"m","stream":false,"messages":[`+
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"rm -rf {"}}]}]}`)
	in, _ := buildJSON(t, r)
	calls := normalize(t, in["tool_calls"]).([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want 1", calls)
	}
	if got := calls[0].(map[string]any)["arguments"]; got != "rm -rf {" {
		t.Fatalf("arguments = %#v, want the raw string", got)
	}
}

func TestBuildInputOpenAI_ToolResultWithTextPartsAndAnUnknownCall(t *testing.T) {
	r := openAIReq("/v1/chat/completions", "messages", `{"model":"m","stream":false,"messages":[`+
		`{"role":"tool","tool_call_id":"call_gone","content":[{"type":"text","text":"a"},{"type":"text","text":"bc"}]}]}`)
	in, _ := buildJSON(t, r)
	got := normalize(t, in["tool_results"])
	want := mustJSON(t, `[{"tool_call_id":"call_gone","is_error":false,"size_bytes":4,"text":"a\nbc"}]`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool_results = %v, want %v (text parts joined; tool_name omitted on a miss)", got, want)
	}
}

func TestBuildInputOpenAI_FIMIsOneUserMessage(t *testing.T) {
	r := openAIReq("/v1/fim/completions", "completion", `{"model":"codestral-latest","prompt":"def f(","suffix":"return x","stream":false,"max_tokens":64}`)
	in, _ := buildJSON(t, r)

	if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"def f("},{"type":"text","text":"return x"}]}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	checkEmptyToolGroups(t, in)
	if _, ok := in["system"]; ok {
		t.Fatal("system present for a FIM request, want it omitted")
	}
	if got, want := normalize(t, in["parameters"]), mustJSON(t, `{"max_tokens":64}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
	if in["model"] != "codestral-latest" {
		t.Fatalf("model = %v, want codestral-latest", in["model"])
	}
}

func TestBuildInputOpenAI_FIMWithoutSuffix(t *testing.T) {
	r := openAIReq("/v1/fim/completions", "completion", `{"model":"codestral-latest","prompt":"def f(","stream":false}`)
	in, _ := buildJSON(t, r)
	if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"def f("}]}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func TestBuildInputOpenAI_EmbeddingsAreOneUserMessage(t *testing.T) {
	r := openAIReq("/v1/embeddings", "embeddings", `{"model":"mistral-embed","input":["first",[101,2023],"second"],"encoding_format":"float"}`)
	in, _ := buildJSON(t, r)

	// Token-ID inputs carry no text and are skipped.
	if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	checkEmptyToolGroups(t, in)
	if got, want := normalize(t, in["parameters"]), mustJSON(t, `{"encoding_format":"float"}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
}

func TestBuildInputOpenAI_EmbeddingsWithAStringInput(t *testing.T) {
	r := openAIReq("/v1/embeddings", "embeddings", `{"model":"mistral-embed","input":"only one"}`)
	in, _ := buildJSON(t, r)
	if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"only one"}]}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

func checkEmptyToolGroups(t *testing.T, in map[string]any) {
	t.Helper()
	for _, k := range []string{"tools", "tool_calls", "tool_results"} {
		v, ok := in[k]
		if !ok {
			t.Fatalf("key %q absent, want present and empty", k)
		}
		if got := normalize(t, v); !reflect.DeepEqual(got, []any{}) {
			t.Fatalf("%s = %v, want []", k, got)
		}
	}
}
