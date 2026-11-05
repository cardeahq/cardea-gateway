package policy

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// specs/0004-technical, Format readers (gemini rows); functional rule 12
// and criteria 31-34.

func geminiReq(action, op, body string) Request {
	return Request{
		CallID:    "ak_7f3c2b1d9e8a4f60",
		Method:    http.MethodPost,
		Path:      "/v1beta/models/gemini-2.5-pro:" + action,
		Headers:   http.Header{},
		Body:      []byte(body),
		Provider:  "gemini",
		Format:    "gemini",
		Operation: op,
	}
}

func TestBuildInputGemini_Generate(t *testing.T) {
	r := geminiReq("generateContent", "messages", `{"systemInstruction":{"parts":[{"text":"be"},{"text":"nice"}]},"contents":[`+
		`{"role":"user","parts":[{"text":"look"}]},`+
		`{"role":"model","parts":[{"functionCall":{"name":"bash","args":{"command":"ls"}}}]},`+
		`{"role":"user","parts":[{"functionResponse":{"id":"fc_1","name":"bash","response":{"out":"a.go"}}}]}],`+
		`"tools":[{"functionDeclarations":[{"name":"bash","description":"run","parameters":{"type":"OBJECT"}}]}],`+
		`"generationConfig":{"temperature":0.3},"safetySettings":[]}`)
	in, raw := buildJSON(t, r)

	if in["model"] != "gemini-2.5-pro" {
		t.Fatalf("model = %v, want gemini-2.5-pro from the URL", in["model"])
	}
	if in["stream"] != false {
		t.Fatalf("stream = %v, want false for generateContent", in["stream"])
	}
	if in["system"] != "be\nnice" {
		t.Fatalf("system = %v, want the systemInstruction text parts joined with a newline", in["system"])
	}
	msgs := normalize(t, in["messages"]).([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want 3", msgs)
	}
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if !slices.Equal(roles, []string{"user", "assistant", "user"}) {
		t.Fatalf("roles = %v, want user, assistant, user (model becomes assistant)", roles)
	}
	if got, want := msgs[0], mustJSON(t, `{"role":"user","content":[{"type":"text","text":"look"}]}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages[0] = %v, want %v", got, want)
	}
	// A function call with no id carries no id key.
	if got, want := normalize(t, in["tool_calls"]), mustJSON(t, `[{"name":"bash","arguments":{"command":"ls"}}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("tool_calls = %v, want %v", got, want)
	}
	results := normalize(t, in["tool_results"]).([]any)
	if len(results) != 1 {
		t.Fatalf("tool_results = %v, want 1", results)
	}
	tr := results[0].(map[string]any)
	text, _ := tr["text"].(string)
	if tr["tool_call_id"] != "fc_1" || tr["tool_name"] != "bash" || mustJSON(t, text) == nil ||
		!reflect.DeepEqual(mustJSON(t, text), mustJSON(t, `{"out":"a.go"}`)) {
		t.Fatalf("tool_result = %v, want id fc_1, name bash, and the response serialized as text", tr)
	}
	if tr["size_bytes"] != float64(len(text)) {
		t.Fatalf("size_bytes = %v, want %d (the serialized response)", tr["size_bytes"], len(text))
	}
	if got, want := normalize(t, in["tools"]), mustJSON(t, `[{"name":"bash","description":"run","parameters":{"type":"OBJECT"}}]`); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if got, want := normalize(t, in["parameters"]), mustJSON(t, `{"generationConfig":{"temperature":0.3},"safetySettings":[]}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
	if strings.Contains(raw, "null") {
		t.Fatalf("input document contains null\n%s", raw)
	}
}

func TestBuildInputGemini_StreamIsTrueExactlyForStreamGenerateContent(t *testing.T) {
	body := `{"stream":false,"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	in, _ := buildJSON(t, geminiReq("streamGenerateContent", "messages", body))
	if in["stream"] != true {
		t.Fatalf("stream = %v, want true for streamGenerateContent whatever the body says", in["stream"])
	}
}

func TestBuildInputGemini_BinaryPartsCarryNoData(t *testing.T) {
	r := geminiReq("generateContent", "messages", `{"contents":[{"role":"user","parts":[`+
		`{"inlineData":{"mimeType":"image/png","data":"SECRETBYTES"}},`+
		`{"fileData":{"mimeType":"application/pdf","fileUri":"https://files.example.com/SECRETURI"}}]}]}`)
	in, raw := buildJSON(t, r)
	if strings.Contains(raw, "SECRETBYTES") || strings.Contains(raw, "SECRETURI") {
		t.Fatalf("input carries binary data or a file reference\n%s", raw)
	}
	parts := normalize(t, in["messages"]).([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want 2", parts)
	}
	for _, p := range parts {
		pm := p.(map[string]any)
		if len(pm) != 1 || (pm["type"] != "image" && pm["type"] != "file") {
			t.Fatalf("part = %v, want only a type of image or file", pm)
		}
	}
}

func TestBuildInputGemini_SnakeCaseIsReadAlike(t *testing.T) {
	camel := `{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1}}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"r":2}}}]}]}`
	snake := `{"system_instruction":{"parts":[{"text":"s"}]},"contents":[{"role":"model","parts":[{"function_call":{"name":"f","args":{"a":1}}}]},{"role":"user","parts":[{"function_response":{"name":"f","response":{"r":2}}}]}]}`
	a, _ := buildJSON(t, geminiReq("generateContent", "messages", camel))
	b, _ := buildJSON(t, geminiReq("generateContent", "messages", snake))
	for _, k := range []string{"system", "messages", "tool_calls", "tool_results"} {
		if got, want := normalize(t, b[k]), normalize(t, a[k]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: snake_case %v, camelCase %v; want equal", k, got, want)
		}
	}
}

func TestBuildInputGemini_BothSpellingsInOneObjectIsAnError(t *testing.T) {
	const marker = "LEAKMARKER-5521"
	for name, body := range map[string]string{
		"systemInstruction twice": `{"systemInstruction":{"parts":[{"text":"` + marker + `"}]},"system_instruction":{"parts":[]},"contents":[]}`,
		"functionCall twice":      `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"` + marker + `"},"function_call":{"name":"g"}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildInput(geminiReq("generateContent", "messages", body))
			if err == nil {
				t.Fatal("buildInput = nil error, want an error: the Gateway and Gemini could read different values")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q carries body content", err)
			}
		})
	}
}

func TestBuildInputGemini_CountTokens(t *testing.T) {
	t.Run("with contents", func(t *testing.T) {
		in, _ := buildJSON(t, geminiReq("countTokens", "count_tokens", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"hi"}]}]`); !reflect.DeepEqual(got, want) {
			t.Fatalf("messages = %v, want %v", got, want)
		}
	})
	t.Run("with generateContentRequest", func(t *testing.T) {
		in, _ := buildJSON(t, geminiReq("countTokens", "count_tokens", `{"generateContentRequest":{"model":"models/gemini-2.5-pro",`+
			`"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`))
		if in["system"] != "sys" {
			t.Fatalf("system = %v, want sys from generateContentRequest", in["system"])
		}
		if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"hi"}]}]`); !reflect.DeepEqual(got, want) {
			t.Fatalf("messages = %v, want %v", got, want)
		}
		op := normalize(t, in["operation"]).(map[string]any)
		if op["name"] != "count_tokens" {
			t.Fatalf("operation.name = %v, want count_tokens", op["name"])
		}
	})
}

func TestBuildInputGemini_EmbedIsOneUserMessage(t *testing.T) {
	t.Run("embedContent", func(t *testing.T) {
		in, _ := buildJSON(t, geminiReq("embedContent", "embeddings", `{"content":{"parts":[{"text":"a"},{"text":"b"}]},"taskType":"RETRIEVAL_DOCUMENT"}`))
		if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]`); !reflect.DeepEqual(got, want) {
			t.Fatalf("messages = %v, want %v", got, want)
		}
		checkEmptyToolGroups(t, in)
		if got, want := normalize(t, in["parameters"]), mustJSON(t, `{"taskType":"RETRIEVAL_DOCUMENT"}`); !reflect.DeepEqual(got, want) {
			t.Fatalf("parameters = %v, want %v", got, want)
		}
	})
	t.Run("batchEmbedContents", func(t *testing.T) {
		in, _ := buildJSON(t, geminiReq("batchEmbedContents", "embeddings",
			`{"requests":[{"model":"models/text-embedding-004","content":{"parts":[{"text":"a"}]}},{"model":"models/text-embedding-004","content":{"parts":[{"text":"b"}]}}]}`))
		if got, want := normalize(t, in["messages"]), mustJSON(t, `[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]`); !reflect.DeepEqual(got, want) {
			t.Fatalf("messages = %v, want %v", got, want)
		}
		checkEmptyToolGroups(t, in)
	})
}

// --- One conversation, three formats ------------------------------------------------

// The same conversation as an Anthropic Messages body, an OpenAI-style chat
// body and a Gemini generate body (functional rule 12: the shape is the
// same for every format).
const (
	sameAnthropic = `{"model":"m","stream":false,"system":"be brief","messages":[` +
		`{"role":"user","content":"list files"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"list_files","input":{"dir":"src"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"{\"files\":[\"a.go\"]}"}]}]}`
	sameOpenAI = `{"model":"m","stream":false,"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"list files"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_files","arguments":"{\"dir\":\"src\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"files\":[\"a.go\"]}"}]}`
	sameGemini = `{"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[` +
		`{"role":"user","parts":[{"text":"list files"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"list_files","args":{"dir":"src"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"list_files","response":{"files":["a.go"]}}}]}]}`
)

func TestBuildInput_OneConversationGivesTheSameShapeInEveryFormat(t *testing.T) {
	reqs := map[string]Request{
		"anthropic": inputReq(sameAnthropic),
		"openai":    openAIReq("/v1/chat/completions", "messages", sameOpenAI),
		"gemini":    geminiReq("generateContent", "messages", sameGemini),
	}
	type view struct {
		System      any
		TextsByRole any
		ToolCalls   any
		ToolResults any
	}
	views := map[string]view{}
	for name, r := range reqs {
		in, _ := buildJSON(t, r)
		// Non-system messages, as (role, text parts) pairs; system and
		// developer messages are only kept in messages by the OpenAI-style
		// format, so they are compared through the system key instead.
		var texts []any
		for _, m := range normalize(t, in["messages"]).([]any) {
			mm := m.(map[string]any)
			if mm["role"] == "system" || mm["role"] == "developer" {
				continue
			}
			var ts []any
			for _, p := range mm["content"].([]any) {
				pm := p.(map[string]any)
				if pm["type"] == "text" {
					ts = append(ts, pm["text"])
				}
			}
			texts = append(texts, []any{mm["role"], ts})
		}
		var results []any
		for _, tr := range normalize(t, in["tool_results"]).([]any) {
			trm := tr.(map[string]any)
			results = append(results, []any{trm["tool_call_id"], trm["tool_name"], trm["size_bytes"], mustJSON(t, trm["text"].(string))})
		}
		views[name] = view{System: in["system"], TextsByRole: texts, ToolCalls: normalize(t, in["tool_calls"]), ToolResults: results}
	}
	want := views["anthropic"]
	if want.System != "be brief" {
		t.Fatalf("anthropic system = %v, want be brief", want.System)
	}
	for _, name := range []string{"openai", "gemini"} {
		if got := views[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s view = %+v\nanthropic view = %+v\nwant equal", name, got, want)
		}
	}
}

// --- Rules see every format (engine) ------------------------------------------------

func evalRules(t *testing.T, e *Engine, r Request) []string {
	t.Helper()
	res, err := e.Evaluate(context.Background(), r)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res.Rules
}

func TestEvaluate_Criterion31_ForbiddenTextIsDeniedInEveryFormat(t *testing.T) {
	e, _ := newEngine(t, `deny contains "falcon" if {
	some m in input.messages
	some part in m.content
	part.type == "text"
	contains(part.text, "Project Falcon")
}`)
	const text = "status of Project Falcon?"
	reqs := map[string]Request{
		"anthropic messages": msgReq(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"` + text + `"}]}`),
		"mistral chat":       openAIReq("/v1/chat/completions", "messages", `{"model":"mistral-large-latest","messages":[{"role":"user","content":"`+text+`"}]}`),
		"gemini generate":    geminiReq("generateContent", "messages", `{"contents":[{"role":"user","parts":[{"text":"`+text+`"}]}]}`),
		"mistral embeddings": openAIReq("/v1/embeddings", "embeddings", `{"model":"mistral-embed","input":["unrelated","`+text+`"]}`),
		"mistral fim":        openAIReq("/v1/fim/completions", "completion", `{"model":"codestral-latest","prompt":"# `+text+`"}`),
		"gemini embed":       geminiReq("embedContent", "embeddings", `{"content":{"parts":[{"text":"`+text+`"}]}}`),
	}
	for name, r := range reqs {
		t.Run(name, func(t *testing.T) {
			if got := evalRules(t, e, r); !slices.Equal(got, []string{"falcon"}) {
				t.Fatalf("Rules = %v, want [falcon]", got)
			}
		})
	}
	// And the rule is real: the same calls without the text are allowed.
	clean := openAIReq("/v1/chat/completions", "messages", `{"model":"mistral-large-latest","messages":[{"role":"user","content":"hello"}]}`)
	if got := evalRules(t, e, clean); len(got) != 0 {
		t.Fatalf("Rules = %v for a clean request, want none", got)
	}
}

func TestEvaluate_Criterion32_ToolCallArgumentsInMistralAndGemini(t *testing.T) {
	e, _ := newEngine(t, `deny contains "force-delete" if {
	some c in input.tool_calls
	c.name == "delete_repo"
	c.arguments.force == true
}`)
	tests := []struct {
		name string
		r    Request
		want []string
	}{
		{"mistral, force true", openAIReq("/v1/chat/completions", "messages", `{"model":"mistral-large-latest","messages":[`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"delete_repo","arguments":"{\"repo\":\"core\",\"force\":true}"}}]}]}`), []string{"force-delete"}},
		{"mistral, force false", openAIReq("/v1/chat/completions", "messages", `{"model":"mistral-large-latest","messages":[`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"delete_repo","arguments":"{\"repo\":\"core\",\"force\":false}"}}]}]}`), nil},
		{"gemini, force true", geminiReq("generateContent", "messages", `{"contents":[`+
			`{"role":"model","parts":[{"functionCall":{"name":"delete_repo","args":{"repo":"core","force":true}}}]}]}`), []string{"force-delete"}},
		{"gemini, another tool", geminiReq("generateContent", "messages", `{"contents":[`+
			`{"role":"model","parts":[{"functionCall":{"name":"list_repos","args":{"force":true}}}]}]}`), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalRules(t, e, tt.r); !slices.Equal(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Fatalf("Rules = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_Criterion33_OperationNameIsFormatNeutral(t *testing.T) {
	e, _ := newEngine(t, `deny contains "no-embeddings" if { input.operation.name == "embeddings" }`)
	for name, r := range map[string]Request{
		"mistral embeddings": openAIReq("/v1/embeddings", "embeddings", `{"model":"mistral-embed","input":"x"}`),
		"gemini embed":       geminiReq("embedContent", "embeddings", `{"content":{"parts":[{"text":"x"}]}}`),
		"gemini batch embed": geminiReq("batchEmbedContents", "embeddings", `{"requests":[{"content":{"parts":[{"text":"x"}]}}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if got := evalRules(t, e, r); !slices.Equal(got, []string{"no-embeddings"}) {
				t.Fatalf("Rules = %v, want [no-embeddings]", got)
			}
		})
	}
	if got := evalRules(t, e, openAIReq("/v1/chat/completions", "messages", `{"model":"m","messages":[]}`)); len(got) != 0 {
		t.Fatalf("Rules = %v for a chat request, want none", got)
	}
}

func TestEvaluate_Criterion34_ModelsSeeNoConversationKeys(t *testing.T) {
	e, _ := newEngine(t, `deny contains "models" if { input.operation.name == "models" }
deny contains "has-conversation" if { input.messages }
deny contains "has-model" if { input.model }`)
	for _, format := range []string{"anthropic", "openai", "gemini"} {
		t.Run(format, func(t *testing.T) {
			r := Request{CallID: "c", Method: http.MethodGet, Path: "/v1/models", Headers: http.Header{}, Provider: format, Format: format, Operation: "models"}
			if got := evalRules(t, e, r); !slices.Equal(got, []string{"models"}) {
				t.Fatalf("Rules = %v, want only [models]", got)
			}
		})
	}
}

func TestEvaluate_PairWithNoReaderIsAnInternalFailure(t *testing.T) {
	e, _ := newEngine(t, `deny contains "x" if { input.model == "never" }`)
	r := msgReq(`{"model":"m","messages":[]}`)
	r.Format = "cohere"
	_, err := e.Evaluate(context.Background(), r)
	if err == nil {
		t.Fatal("Evaluate = nil error, want a failure")
	}
	if k := evalKind(t, err); k != "internal" {
		t.Fatalf("Kind = %q, want internal", k)
	}
}
