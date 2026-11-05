package filtering

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// specs/0006-technical, Formats and operations; functional rule 3 and
// criteria 17-19, 23.

func redactFor(t *testing.T, format, op, body string) Result {
	t.Helper()
	res, err := testEngine().Redact(context.Background(), Request{CallID: "c1", Body: []byte(body), Format: format, Operation: op})
	if err != nil {
		t.Fatalf("Redact(%s/%s): %v", format, op, err)
	}
	return res
}

func redactErr(t *testing.T, format, op, body string) *FilterError {
	t.Helper()
	res, err := testEngine().Redact(context.Background(), Request{CallID: "c1", Body: []byte(body), Format: format, Operation: op})
	var fe *FilterError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a *FilterError", err)
	}
	if res.Body != nil || res.Counts != nil {
		t.Fatalf("Result = %+v on error, want the zero value: never a partially redacted body", res)
	}
	return fe
}

// decodeAny decodes body for navigation with at.
func decodeAny(t *testing.T, body []byte) any {
	t.Helper()
	var v any
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatalf("redacted body is not JSON: %v\n%s", err, body)
	}
	return v
}

// at walks v by object keys (string) and array indexes (int).
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %v: %T is not an object", path, v)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("at %v: %T is not an array long enough", path, v)
			}
			v = a[k]
		}
	}
	return v
}

func atString(t *testing.T, v any, path ...any) string {
	t.Helper()
	s, ok := at(t, v, path...).(string)
	if !ok {
		t.Fatalf("at %v: %T, want a string", path, at(t, v, path...))
	}
	return s
}

// checkRedacted fails unless s holds exactly n placeholders of cat and none
// of the real value.
func checkRedacted(t *testing.T, s, real string, cat Category, n int) {
	t.Helper()
	if strings.Contains(s, real) {
		t.Fatalf("%q still holds the real value %q", s, real)
	}
	ph := placeholders([]byte(s))
	if len(ph) != n {
		t.Fatalf("%q: placeholders = %v, want %d", s, ph, n)
	}
	for _, p := range ph {
		if p != string(cat) {
			t.Fatalf("%q: placeholder category %s, want %s", s, p, cat)
		}
	}
}

// rawMember returns the raw bytes of a top-level member of body.
func rawMember(t *testing.T, body []byte, key string) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	return string(top[key])
}

// --- OpenAI-style chat ---------------------------------------------------------------

func TestRedactChat_EveryRoleAndShapeOfContentIsScanned(t *testing.T) {
	body := `{"model":"mistral-large-latest","messages":[` +
		`{"role":"system","content":"sys ` + exEmail + `"},` +
		`{"role":"developer","content":"dev ` + exEmail + `"},` +
		`{"role":"user","content":"user ` + exEmail + `"},` +
		`{"role":"user","content":[{"type":"text","text":"part ` + exEmail + `"}]},` +
		`{"role":"assistant","content":"asst ` + exEmail + `"},` +
		`{"role":"tool","tool_call_id":"call_1","content":"tool ` + exEmail + `"}]}`
	res := redactFor(t, "openai", "messages", body)
	v := decodeAny(t, res.Body)

	checkRedacted(t, atString(t, v, "messages", 0, "content"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "messages", 1, "content"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "messages", 2, "content"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "messages", 3, "content", 0, "text"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "messages", 4, "content"), exEmail, CategoryEmail, 1)
	checkRedacted(t, atString(t, v, "messages", 5, "content"), exEmail, CategoryEmail, 1)
	if res.Counts[CategoryEmail] != 6 {
		t.Fatalf("Counts = %v, want email:6", res.Counts)
	}
}

func TestRedactChat_ToolCallArgumentsStayAStringHoldingValidJSON(t *testing.T) {
	args := `{"a":{"b":{"c":"mail ` + exEmail + `"}},"n":12345678901234567890,"keep":"plain"}`
	body := `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"send","arguments":` + jsonStr(args) + `}}]}]}`
	res := redactFor(t, "openai", "messages", body)
	v := decodeAny(t, res.Body)

	tc := at(t, v, "messages", 0, "tool_calls", 0)
	gotArgs, ok := at(t, tc, "function", "arguments").(string)
	if !ok {
		t.Fatalf("arguments = %T, want a string: the format carries arguments as a JSON string", at(t, tc, "function", "arguments"))
	}
	parsed := decodeAny(t, []byte(gotArgs))
	checkRedacted(t, atString(t, parsed, "a", "b", "c"), exEmail, CategoryEmail, 1)
	if n := at(t, parsed, "n"); n.(json.Number).String() != "12345678901234567890" {
		t.Fatalf("n = %v, want the large integer preserved exactly", n)
	}
	if atString(t, parsed, "keep") != "plain" {
		t.Fatal("an unmatched argument value changed")
	}
	if atString(t, tc, "id") != "call_1" || atString(t, tc, "type") != "function" || atString(t, tc, "function", "name") != "send" {
		t.Fatalf("tool call id/type/name changed: %v", tc)
	}
	if at(t, v, "messages", 0, "content") != nil {
		t.Fatalf("content = %v, want null left as is", at(t, v, "messages", 0, "content"))
	}
}

func TestRedactChat_ArgumentsThatAreNotJSONAreRedactedAsText(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"send","arguments":"to ` + exEmail + ` {"}}]}]}`
	res := redactFor(t, "openai", "messages", body)
	v := decodeAny(t, res.Body)
	got := atString(t, v, "messages", 0, "tool_calls", 0, "function", "arguments")
	checkRedacted(t, got, exEmail, CategoryEmail, 1)
	if !strings.HasPrefix(got, "to ") || !strings.HasSuffix(got, " {") {
		t.Fatalf("arguments = %q, want only the email replaced", got)
	}
}

func TestRedactChat_Criterion17_SystemToolAndArgumentsEachGetTheirOwnPlaceholder(t *testing.T) {
	args := `{"to":"` + exEmail + `"}`
	body := `{"model":"mistral-large-latest","messages":[` +
		`{"role":"system","content":"admin is ` + exEmail + `"},` +
		`{"role":"user","content":"mail them"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"send","arguments":` + jsonStr(args) + `}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"sent to ` + exEmail + `"}]}`
	res := redactFor(t, "openai", "messages", body)
	if strings.Contains(string(res.Body), exEmail) {
		t.Fatalf("body still holds the email: %s", res.Body)
	}
	v := decodeAny(t, res.Body)
	gotArgs := atString(t, v, "messages", 2, "tool_calls", 0, "function", "arguments")
	parsed := decodeAny(t, []byte(gotArgs)) // still valid JSON
	for _, s := range []string{
		atString(t, v, "messages", 0, "content"),
		atString(t, parsed, "to"),
		atString(t, v, "messages", 3, "content"),
	} {
		ph := placeholders([]byte(s))
		if len(ph) != 1 || ph[0] != string(CategoryEmail) {
			t.Fatalf("%q: placeholders = %v, want one email placeholder", s, ph)
		}
	}
}

func TestRedactChat_OutOfScopeMembersAreByteIdentical(t *testing.T) {
	tools := `[{"type":"function","function":{"name":"send","description":"mail ` + exEmail + `","parameters":{"type":"object"}}}]`
	responseFormat := `{"type":"json_schema","json_schema":{"name":"r","schema":{"description":"` + exEmail + `"}}}`
	toolChoice := `{"type":"function","function":{"name":"send"}}`
	imagePart := `{"type":"image_url","image_url":{"url":"https://img.example.com/` + exEmail + `.png"}}`
	audioPart := `{"type":"input_audio","input_audio":{"data":"` + exEmail + `","format":"wav"}}`
	body := `{"model":"m","tools":` + tools + `,"tool_choice":` + toolChoice + `,"response_format":` + responseFormat + `,"temperature":0.2,` +
		`"messages":[{"role":"user","name":"` + exEmail + `","content":[` + imagePart + `,` + audioPart + `,{"type":"text","text":"hi ` + exEmail + `"}]}]}`
	res := redactFor(t, "openai", "messages", body)

	if got := rawMember(t, res.Body, "tools"); got != tools {
		t.Fatalf("tools = %s, want byte-identical %s", got, tools)
	}
	if got := rawMember(t, res.Body, "response_format"); got != responseFormat {
		t.Fatalf("response_format = %s, want byte-identical", got)
	}
	if got := rawMember(t, res.Body, "tool_choice"); got != toolChoice {
		t.Fatalf("tool_choice = %s, want byte-identical", got)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawMember(t, res.Body, "messages")), &msgs); err != nil {
		t.Fatal(err)
	}
	if string(msgs[0]["name"]) != `"`+exEmail+`"` {
		t.Fatalf("name = %s, want copied untouched: only content and tool call arguments are scanned", msgs[0]["name"])
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(msgs[0]["content"], &parts); err != nil {
		t.Fatal(err)
	}
	if string(parts[0]) != imagePart || string(parts[1]) != audioPart {
		t.Fatalf("image/audio parts = %s / %s, want byte-identical", parts[0], parts[1])
	}
	checkRedacted(t, atString(t, decodeAny(t, parts[2]), "text"), exEmail, CategoryEmail, 1)
	if res.Counts[CategoryEmail] != 1 {
		t.Fatalf("Counts = %v, want email:1 (only the text part)", res.Counts)
	}
}

func TestRedactChat_BadShapesAreInputErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":                  `{"model":"m","messages":[`,
		"messages not an array":     `{"model":"m","messages":"hi"}`,
		"a message not an object":   `{"model":"m","messages":["hi"]}`,
		"duplicate key in message":  `{"model":"m","messages":[{"role":"user","content":"a","content":"b"}]}`,
		"duplicate key in top":      `{"model":"m","model":"n","messages":[]}`,
		"duplicate key in argument": `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","name":"g","arguments":"{}"}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if fe := redactErr(t, "openai", "messages", body); fe.Kind != "input" {
				t.Fatalf("Kind = %q, want input", fe.Kind)
			}
		})
	}
}

// --- OpenAI-style fill-in-the-middle --------------------------------------------------

func TestRedactFIM_Criterion18_PromptAndSuffix(t *testing.T) {
	body := `{"model":"codestral-latest","prompt":"key = '` + exAPIKeyOpenAI + `'","suffix":"ssn = '` + exSSNUS + `'","stop":["` + exEmail + `"],"max_tokens":64}`
	res := redactFor(t, "openai", "completion", body)
	v := decodeAny(t, res.Body)

	checkRedacted(t, atString(t, v, "prompt"), exAPIKeyOpenAI, CategoryAPIKey, 1)
	checkRedacted(t, atString(t, v, "suffix"), exSSNUS, CategorySSN, 1)
	if got := rawMember(t, res.Body, "stop"); got != `["`+exEmail+`"]` {
		t.Fatalf("stop = %s, want byte-identical: stop sequences are not scanned", got)
	}
}

func TestRedactFIM_PromptOnly(t *testing.T) {
	res := redactFor(t, "openai", "completion", `{"model":"codestral-latest","prompt":"`+exEmail+`"}`)
	checkRedacted(t, atString(t, decodeAny(t, res.Body), "prompt"), exEmail, CategoryEmail, 1)
}

func TestRedactFIM_NonStringPromptOrSuffixIsAnInputError(t *testing.T) {
	for name, body := range map[string]string{
		"prompt array":  `{"model":"m","prompt":["a"]}`,
		"prompt number": `{"model":"m","prompt":1}`,
		"suffix object": `{"model":"m","prompt":"a","suffix":{"x":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if fe := redactErr(t, "openai", "completion", body); fe.Kind != "input" {
				t.Fatalf("Kind = %q, want input", fe.Kind)
			}
		})
	}
}

// --- OpenAI-style embeddings --------------------------------------------------------

func TestRedactEmbeddings_StringInput(t *testing.T) {
	res := redactFor(t, "openai", "embeddings", `{"model":"mistral-embed","input":"mail `+exEmail+`"}`)
	checkRedacted(t, atString(t, decodeAny(t, res.Body), "input"), exEmail, CategoryEmail, 1)
}

func TestRedactEmbeddings_Criterion19_OnlyTheTextHoldingPIIChanges(t *testing.T) {
	body := `{"model":"mistral-embed","input":["first text","mail ` + exEmail + `","third text",[101,2023,102]],"encoding_format":"float"}`
	res := redactFor(t, "openai", "embeddings", body)
	var input []json.RawMessage
	if err := json.Unmarshal([]byte(rawMember(t, res.Body, "input")), &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 4 {
		t.Fatalf("len(input) = %d, want 4", len(input))
	}
	if string(input[0]) != `"first text"` || string(input[2]) != `"third text"` {
		t.Fatalf("unmatched inputs = %s, %s; want unchanged", input[0], input[2])
	}
	if string(input[3]) != `[101,2023,102]` {
		t.Fatalf("token-ID input = %s, want byte-identical", input[3])
	}
	var second string
	_ = json.Unmarshal(input[1], &second)
	checkRedacted(t, second, exEmail, CategoryEmail, 1)
	if rawMember(t, res.Body, "encoding_format") != `"float"` {
		t.Fatal("encoding_format changed")
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
