package filtering

import (
	"encoding/json"
	"testing"
)

// specs/0006-technical, Formats and operations (Gemini); functional rule 3
// and criteria 20-22.

func TestRedactGenerate_Criterion20_SystemInstructionTextAndFunctionResponse(t *testing.T) {
	body := `{"systemInstruction":{"parts":[{"text":"admin is ` + exEmail + `"}]},"contents":[` +
		`{"role":"user","parts":[{"text":"write to ` + exEmail + `"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"owner":"` + exEmail + `"}}}]}]}`
	res := redactFor(t, "gemini", "messages", body)
	v := decodeAny(t, res.Body)

	checkRedacted(t, atString(t, v, "systemInstruction", "parts", 0, "text"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "contents", 0, "parts", 0, "text"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "contents", 2, "parts", 0, "functionResponse", "response", "owner"), exEmail, CategoryEmail, 1)
	if res.Counts[CategoryEmail] != 3 {
		t.Fatalf("Counts = %v, want email:3", res.Counts)
	}
	if atString(t, v, "contents", 1, "role") != "model" || atString(t, v, "contents", 2, "parts", 0, "functionResponse", "name") != "lookup" {
		t.Fatal("role or function name changed")
	}
}

func TestRedactGenerate_SnakeCaseSpellingsAreScanned(t *testing.T) {
	body := `{"system_instruction":{"parts":[{"text":"` + exEmail + `"}]},"contents":[` +
		`{"role":"model","parts":[{"function_call":{"name":"f","args":{"to":"` + exEmail + `"}}}]},` +
		`{"role":"user","parts":[{"function_response":{"name":"f","response":{"r":"` + exEmail + `"}}}]}]}`
	res := redactFor(t, "gemini", "messages", body)
	v := decodeAny(t, res.Body)

	checkRedacted(t, atString(t, v, "system_instruction", "parts", 0, "text"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "contents", 0, "parts", 0, "function_call", "args", "to"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "contents", 1, "parts", 0, "function_response", "response", "r"), exEmail, CategoryEmail, 1)
}

func TestRedactGenerate_FunctionArgsAndResponseAtDepth(t *testing.T) {
	body := `{"contents":[` +
		`{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":{"b":{"c":["x","` + exSSNUS + `"]}},"n":12345678901234567890}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"a":{"b":{"c":"key ` + exAPIKeyGitHub + `"}}}}}]}]}`
	res := redactFor(t, "gemini", "messages", body)
	v := decodeAny(t, res.Body)

	checkRedacted(t, atString(t, v, "contents", 0, "parts", 0, "functionCall", "args", "a", "b", "c", 1), exSSNUS, CategorySSN, 1)
	if atString(t, v, "contents", 0, "parts", 0, "functionCall", "args", "a", "b", "c", 0) != "x" {
		t.Fatal("an unmatched array element changed")
	}
	if n := at(t, v, "contents", 0, "parts", 0, "functionCall", "args", "n"); n.(json.Number).String() != "12345678901234567890" {
		t.Fatalf("n = %v, want the large integer preserved exactly", n)
	}
	checkRedacted(t, atString(t, v, "contents", 1, "parts", 0, "functionResponse", "response", "a", "b", "c"), exAPIKeyGitHub, CategoryAPIKey, 1)
}

func TestRedactGenerate_OutOfScopeMembersAreByteIdentical(t *testing.T) {
	inline := `{"inlineData":{"mimeType":"text/plain","data":"` + exEmail + `"}}`
	file := `{"fileData":{"mimeType":"application/pdf","fileUri":"https://files.example.com/` + exEmail + `"}}`
	code := `{"executableCode":{"language":"PYTHON","code":"print('` + exEmail + `')"}}`
	tools := `[{"functionDeclarations":[{"name":"send","description":"mail ` + exEmail + `","parameters":{"type":"OBJECT"}}]}]`
	safety := `[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]`
	gen := `{"temperature":0.1,"stopSequences":["` + exEmail + `"]}`
	cached := `"cachedContents/` + exEmail + `"`
	body := `{"contents":[{"role":"user","parts":[` + inline + `,` + file + `,` + code + `,{"text":"hi"}]}],` +
		`"tools":` + tools + `,"safetySettings":` + safety + `,"generationConfig":` + gen + `,"cachedContent":` + cached + `}`
	res := redactFor(t, "gemini", "messages", body)

	// Nothing in scope matched, so the whole body comes back as sent.
	if string(res.Body) != body || res.Counts != nil {
		t.Fatalf("Result = %s (counts %v), want the original bytes", res.Body, res.Counts)
	}
}

func TestRedactGenerate_OutOfScopePartsSurviveARedaction(t *testing.T) {
	inline := `{"inlineData":{"mimeType":"text/plain","data":"` + exEmail + `"}}`
	tools := `[{"functionDeclarations":[{"name":"send","description":"mail ` + exEmail + `"}]}]`
	body := `{"contents":[{"role":"user","parts":[` + inline + `,{"text":"mail ` + exEmail + `"}]}],"tools":` + tools + `}`
	res := redactFor(t, "gemini", "messages", body)

	if got := rawMember(t, res.Body, "tools"); got != tools {
		t.Fatalf("tools = %s, want byte-identical", got)
	}
	var contents []struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal([]byte(rawMember(t, res.Body, "contents")), &contents); err != nil {
		t.Fatal(err)
	}
	if string(contents[0].Parts[0]) != inline {
		t.Fatalf("inlineData part = %s, want byte-identical", contents[0].Parts[0])
	}
	checkRedacted(t, atString(t, decodeAny(t, contents[0].Parts[1]), "text"), exEmail, CategoryEmail, 1)
}

func TestRedactGenerate_Criterion22_CountTokens(t *testing.T) {
	t.Run("with contents", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"text":"ssn ` + exSSNUS + `"}]}]}`
		res := redactFor(t, "gemini", "count_tokens", body)
		checkRedacted(t, atString(t, decodeAny(t, res.Body), "contents", 0, "parts", 0, "text"), exSSNUS, CategorySSN, 1)
	})
	t.Run("wrapping a full generate request", func(t *testing.T) {
		body := `{"generateContentRequest":{"model":"models/gemini-2.5-pro",` +
			`"systemInstruction":{"parts":[{"text":"sys ` + exEmail + `"}]},` +
			`"contents":[{"role":"user","parts":[{"text":"ssn ` + exSSNUS + `"}]}]}}`
		res := redactFor(t, "gemini", "count_tokens", body)
		v := decodeAny(t, res.Body)
		checkRedacted(t, atString(t, v, "generateContentRequest", "contents", 0, "parts", 0, "text"), exSSNUS, CategorySSN, 1)
		checkRedacted(t, atString(t, v, "generateContentRequest", "systemInstruction", "parts", 0, "text"), exEmail, CategoryEmail, 1)
		if atString(t, v, "generateContentRequest", "model") != "models/gemini-2.5-pro" {
			t.Fatal("model changed")
		}
	})
	t.Run("wrapping a full generate request, snake_case", func(t *testing.T) {
		body := `{"generate_content_request":{"contents":[{"parts":[{"text":"ssn ` + exSSNUS + `"}]}]}}`
		res := redactFor(t, "gemini", "count_tokens", body)
		checkRedacted(t, atString(t, decodeAny(t, res.Body), "generate_content_request", "contents", 0, "parts", 0, "text"), exSSNUS, CategorySSN, 1)
	})
}

func TestRedactEmbed_EmbedContent(t *testing.T) {
	body := `{"model":"models/text-embedding-004","content":{"parts":[{"text":"mail ` + exEmail + `"}]},"taskType":"RETRIEVAL_DOCUMENT","title":"` + exEmail + `","outputDimensionality":256}`
	res := redactFor(t, "gemini", "embeddings", body)
	v := decodeAny(t, res.Body)
	checkRedacted(t, atString(t, v, "content", "parts", 0, "text"), exEmail, CategoryEmail, 1)
	if rawMember(t, res.Body, "title") != `"`+exEmail+`"` {
		t.Fatal("title changed: it is not scanned")
	}
	if rawMember(t, res.Body, "taskType") != `"RETRIEVAL_DOCUMENT"` || rawMember(t, res.Body, "outputDimensionality") != `256` {
		t.Fatal("taskType or outputDimensionality changed")
	}
}

func TestRedactEmbed_Criterion22_BatchEmbedSecondRequest(t *testing.T) {
	first := `{"model":"models/text-embedding-004","content":{"parts":[{"text":"nothing here"}]}}`
	body := `{"requests":[` + first + `,{"model":"models/text-embedding-004","content":{"parts":[{"text":"mail ` + exEmail + `"}]}}]}`
	res := redactFor(t, "gemini", "embeddings", body)
	var reqs []json.RawMessage
	if err := json.Unmarshal([]byte(rawMember(t, res.Body, "requests")), &reqs); err != nil {
		t.Fatal(err)
	}
	checkRedacted(t, atString(t, decodeAny(t, reqs[1]), "content", "parts", 0, "text"), exEmail, CategoryEmail, 1)
	if atString(t, decodeAny(t, reqs[0]), "content", "parts", 0, "text") != "nothing here" {
		t.Fatal("the first request's text changed")
	}
}

func TestRedactGemini_BothSpellingsOfOneFieldAreAnInputError(t *testing.T) {
	tests := []struct {
		name, op, body string
	}{
		{"functionCall and function_call in one part", "messages",
			`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"function_call":{"name":"f","args":{"to":"` + exEmail + `"}}}]}]}`},
		{"functionResponse and function_response in one part", "messages",
			`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}},"function_response":{"name":"f","response":{}}}]}]}`},
		{"systemInstruction and system_instruction", "messages",
			`{"systemInstruction":{"parts":[{"text":"a"}]},"system_instruction":{"parts":[{"text":"b"}]},"contents":[]}`},
		{"generateContentRequest and generate_content_request", "count_tokens",
			`{"generateContentRequest":{"contents":[]},"generate_content_request":{"contents":[]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if fe := redactErr(t, "gemini", tt.op, tt.body); fe.Kind != "input" {
				t.Fatalf("Kind = %q, want input", fe.Kind)
			}
		})
	}
}

func TestRedactGemini_BadShapesAreInputErrors(t *testing.T) {
	for name, tt := range map[string]struct{ op, body string }{
		"not JSON":                       {"messages", `{"contents":[`},
		"duplicate key":                  {"messages", `{"contents":[],"contents":[]}`},
		"contents not an array":          {"messages", `{"contents":{"parts":[]}}`},
		"parts not an array":             {"messages", `{"contents":[{"parts":"hi"}]}`},
		"embed requests not an array":    {"embeddings", `{"requests":{"content":{}}}`},
		"duplicate key inside a content": {"embeddings", `{"content":{"parts":[],"parts":[]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			if fe := redactErr(t, "gemini", tt.op, tt.body); fe.Kind != "input" {
				t.Fatalf("Kind = %q, want input", fe.Kind)
			}
		})
	}
}
