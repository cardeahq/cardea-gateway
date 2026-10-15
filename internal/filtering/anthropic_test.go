package filtering

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- buildTarget --------------------------------------------------------------

func TestBuildTarget_MatchesBothAnthropicConversationRoutes(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		top, isTarget, err := buildTarget("anthropic", path, body)
		if err != nil || !isTarget || top == nil {
			t.Fatalf("path %s: top=%v isTarget=%v err=%v, want a matched target", path, top, isTarget, err)
		}
	}
}

func TestBuildTarget_NonTargetBodiesReturnUnchangedWithNoError(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		path     string
		body     string
	}{
		{"wrong provider", "openai", "/v1/messages", `{"model":"m","messages":[]}`},
		{"wrong path", "anthropic", "/v1/chat/completions", `{"model":"m","messages":[]}`},
		{"non-JSON body on an unrelated path", "anthropic", "/v1/chat/completions", "not json at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top, isTarget, err := buildTarget(tt.provider, tt.path, []byte(tt.body))
			if isTarget || err != nil || top != nil {
				t.Fatalf("top=%v isTarget=%v err=%v, want nil/false/nil", top, isTarget, err)
			}
		})
	}
}

// --- redactSystem ---------------------------------------------------------------

func TestScan_RedactSystem_PlainStringIsRedacted(t *testing.T) {
	sc := newScan()

	out, err := sc.redactSystem(json.RawMessage(`"contact ` + exEmail + ` now"`))
	if err != nil {
		t.Fatalf("redactSystem: %v", err)
	}
	if strings.Contains(string(out), exEmail) {
		t.Fatalf("redactSystem output still has the real value: %s", out)
	}
	if len(placeholders(out)) != 1 {
		t.Fatalf("placeholders = %v, want 1", placeholders(out))
	}
}

func TestScan_RedactSystem_OnlyTextBlocksAreTouched(t *testing.T) {
	sc := newScan()

	raw := json.RawMessage(`[{"type":"text","text":"a ` + exEmail + ` b"},{"type":"other","text":"c ` + exEmail + ` d"}]`)
	out, err := sc.redactSystem(raw)
	if err != nil {
		t.Fatalf("redactSystem: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(out, &blocks); err != nil {
		t.Fatalf("output is not a JSON array: %v: %s", err, out)
	}
	if strings.Contains(blocks[0]["text"].(string), exEmail) {
		t.Fatalf("a text block was not redacted: %v", blocks[0])
	}
	if !strings.Contains(blocks[1]["text"].(string), exEmail) {
		t.Fatalf("a non-text block was redacted, want it byte-identical: %v", blocks[1])
	}
}

func TestScan_RedactSystem_NilWhenAbsent(t *testing.T) {
	sc := newScan()
	out, err := sc.redactSystem(nil)
	if err != nil || out != nil {
		t.Fatalf("redactSystem(nil) = %v, %v, want nil, nil", out, err)
	}
}

// --- redactBlock ------------------------------------------------------------------

func TestScan_RedactBlock_TextBlockTextIsRedacted(t *testing.T) {
	sc := newScan()
	out, err := sc.redactBlock(json.RawMessage(`{"type":"text","text":"hi ` + exEmail + `"}`))
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	if strings.Contains(string(out), exEmail) {
		t.Fatalf("text block was not redacted: %s", out)
	}
}

func TestScan_RedactBlock_ToolUseInputRedactedAtDepthAndInArraysKeysUntouched(t *testing.T) {
	sc := newScan()
	// depth 3: input.a.b.c is the leaf string; input.list[0] is an array
	// element; the map key itself looks like an email and must survive.
	raw := json.RawMessage(`{"type":"tool_use","id":"t1","name":"bash","input":{"a":{"b":{"c":"deep ` + exEmail + `"}},"list":["in list ` + exEmail + `"],"` + exEmail + `":"key looks like an email too"}}`)
	out, err := sc.redactBlock(raw)
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	var block map[string]any
	if err := json.Unmarshal(out, &block); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out)
	}
	input := block["input"].(map[string]any)
	if strings.Contains(fmtAny(input["a"]), "person@example.com") && !strings.Contains(fmtAny(input["a"]), "[email:") {
		t.Fatalf("nested string value was not redacted: %v", input["a"])
	}
	if !strings.Contains(fmtAny(input["list"]), "[email:") {
		t.Fatalf("array element string value was not redacted: %v", input["list"])
	}
	found := false
	for k := range input {
		if k == exEmail {
			found = true
		}
	}
	if !found {
		t.Fatalf("a map key that looks like PII was altered, want it untouched: %v", input)
	}
}

func TestScan_RedactBlock_ToolUseInputPreservesLargeIntegers(t *testing.T) {
	sc := newScan()
	const bigInt = "9007199254740993" // beyond float64's exact integer range
	raw := json.RawMessage(`{"type":"tool_use","id":"t1","name":"x","input":{"n":` + bigInt + `}}`)
	out, err := sc.redactBlock(raw)
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	if !strings.Contains(string(out), bigInt) {
		t.Fatalf("output = %s, want the large integer preserved exactly (no float round-trip)", out)
	}
}

func TestScan_RedactBlock_ToolResultContentAsStringAndAsBlocks(t *testing.T) {
	sc := newScan()

	str, err := sc.redactBlock(json.RawMessage(`{"type":"tool_result","tool_use_id":"t1","content":"out ` + exEmail + `"}`))
	if err != nil {
		t.Fatalf("redactBlock (string content): %v", err)
	}
	if strings.Contains(string(str), exEmail) {
		t.Fatalf("tool_result string content was not redacted: %s", str)
	}

	blocks, err := sc.redactBlock(json.RawMessage(`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out ` + exEmail + `"}]}`))
	if err != nil {
		t.Fatalf("redactBlock (block content): %v", err)
	}
	if strings.Contains(string(blocks), exEmail) {
		t.Fatalf("tool_result block content was not redacted: %s", blocks)
	}
}

func TestScan_RedactBlock_ThinkingAndRedactedThinkingPassThroughUntouched(t *testing.T) {
	sc := newScan()
	for _, typ := range []string{"thinking", "redacted_thinking"} {
		raw := json.RawMessage(`{"type":"` + typ + `","text":"` + exEmail + `"}`)
		out, err := sc.redactBlock(raw)
		if err != nil {
			t.Fatalf("redactBlock(%s): %v", typ, err)
		}
		if string(out) != string(raw) {
			t.Fatalf("%s block = %s, want byte-identical to %s", typ, out, raw)
		}
	}
}

func TestScan_RedactBlock_ImageBlockPassesThroughUntouched(t *testing.T) {
	sc := newScan()
	raw := json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8gd29ybGQ="}}`)
	out, err := sc.redactBlock(raw)
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("image block = %s, want byte-identical to %s", out, raw)
	}
}

func TestScan_RedactBlock_UnrecognizedBlockTypePassesThroughUntouched(t *testing.T) {
	sc := newScan()
	raw := json.RawMessage(`{"type":"server_tool_use","id":"t1","input":{"q":"` + exEmail + `"}}`)
	out, err := sc.redactBlock(raw)
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("unrecognized block type = %s, want byte-identical to %s", out, raw)
	}
}

func TestScan_RedactBlock_UnrecognizedKeysOnAKnownBlockTypeArePreserved(t *testing.T) {
	sc := newScan()
	raw := json.RawMessage(`{"type":"text","text":"hi ` + exEmail + `","cache_control":{"type":"ephemeral"},"citations":[{"x":1}]}`)
	out, err := sc.redactBlock(raw)
	if err != nil {
		t.Fatalf("redactBlock: %v", err)
	}
	var block map[string]any
	if err := json.Unmarshal(out, &block); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out)
	}
	if _, ok := block["cache_control"]; !ok {
		t.Fatalf("cache_control was dropped: %v", block)
	}
	if _, ok := block["citations"]; !ok {
		t.Fatalf("citations was dropped: %v", block)
	}
}

// --- redactMessages ---------------------------------------------------------------

func TestScan_RedactMessages_EveryKeyOtherThanContentIsPreserved(t *testing.T) {
	sc := newScan()
	raw := json.RawMessage(`[{"role":"user","content":"hi ` + exEmail + `","cache_control":{"type":"ephemeral"}}]`)
	out, err := sc.redactMessages(raw)
	if err != nil {
		t.Fatalf("redactMessages: %v", err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("output is not a JSON array: %v: %s", err, out)
	}
	if msgs[0]["role"] != "user" {
		t.Fatalf("role was altered: %v", msgs[0])
	}
	if _, ok := msgs[0]["cache_control"]; !ok {
		t.Fatalf("an unrecognized message-level key was dropped: %v", msgs[0])
	}
	if strings.Contains(fmtAny(msgs[0]["content"]), exEmail) {
		t.Fatalf("content was not redacted: %v", msgs[0]["content"])
	}
}

// --- Integration: top-level fields outside system/messages are untouched --------

func TestEngine_Redact_TopLevelFieldsOutsideSystemAndMessagesAreByteIdentical(t *testing.T) {
	eng := testEngine()
	body := []byte(`{"model":"m","tools":[{"name":"x","description":"contact ` + exEmail + `"}],` +
		`"parameters":{"note":"` + exEmail + `"},"metadata":{"note":"` + exEmail + `"},` +
		`"messages":[{"role":"user","content":"hi"}]}`)

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts != nil {
		t.Fatalf("Counts = %v, want nil: nothing in system/messages matched", res.Counts)
	}
	for _, field := range []string{`"tools"`, `"parameters"`, `"metadata"`} {
		if !strings.Contains(string(res.Body), field) {
			t.Fatalf("top-level field %s missing from output: %s", field, res.Body)
		}
	}
	if !strings.Contains(string(res.Body), exEmail) {
		t.Fatalf("PII inside a non-scanned top-level field was altered: %s", res.Body)
	}
}

func TestEngine_Redact_PIIOnlyInToolCallAndToolResultIsStillRedacted(t *testing.T) {
	eng := testEngine()
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"please look this up"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"query":"` + exEmail + `"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"found ` + exSSNUS + `"}]}]}`)

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts[CategoryEmail] != 1 || res.Counts[CategorySSN] != 1 {
		t.Fatalf("Counts = %v, want email:1 ssn:1", res.Counts)
	}
	if strings.Contains(string(res.Body), exEmail) || strings.Contains(string(res.Body), exSSNUS) {
		t.Fatalf("Body = %s, want both occurrences redacted", res.Body)
	}
}

// --- redactText --------------------------------------------------------------------

// TestScan_RedactText_CannotFail pins the one change specs/0007 makes inside
// anthropic.go: with no cache left to write to, redactText has nothing left
// that can fail, so it returns a single value. Assigning its result to one
// variable only compiles if that is true — a two-value return here would be
// a compile error, which is exactly the property this spec asks for (see
// specs/0007-technical, Testing: "redactText has no error return any more").
// Every call site in this file and in anthropic.go relies on the same
// one-value signature.
func TestScan_RedactText_CannotFail(t *testing.T) {
	sc := newScan()
	redacted := sc.redactText("contact " + exEmail + " now")
	if strings.Contains(redacted, exEmail) {
		t.Fatalf("redactText output still has the real value: %s", redacted)
	}
	if len(placeholders([]byte(redacted))) != 1 {
		t.Fatalf("placeholders = %v, want 1", placeholders([]byte(redacted)))
	}
}

// --- helpers ---------------------------------------------------------------------

func newScan() *scan {
	return &scan{ctx: context.Background(), engine: testEngine(), counts: map[Category]int{}}
}

func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
