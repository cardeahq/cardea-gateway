package trace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- OpenAI-style (Mistral) response fixtures ---------------------------------
//
// specs/0003-technical, Reading other formats; functional criteria 32, 33.

const (
	mistralID        = "cmpl-e5cc70bb28c444948073e77776eb30ef"
	mistralModel     = "mistral-large-latest"
	mistralCallID    = "call_k3Xq9bT2"
	mistralFinishAt  = 2 // index in mistralToolStream of the finish_reason chunk
	mistralPromptTok = 120
	mistralOutputTok = 30
)

func sseData(data string) string { return "data: " + data + "\n\n" }

// mistralToolStream is a recorded-shape Mistral chat stream: a role
// chunk, one tool call whose arguments arrive in a single delta (as
// Mistral sends them), a finish_reason chunk carrying usage, then [DONE].
func mistralToolStream() []string {
	return []string{
		sseData(`{"id":"` + mistralID + `","object":"chat.completion.chunk","created":1760000000,"model":"` + mistralModel + `","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		sseData(`{"id":"` + mistralID + `","object":"chat.completion.chunk","created":1760000000,"model":"` + mistralModel + `","choices":[{"index":0,"delta":{"tool_calls":[{"id":"` + mistralCallID + `","type":"function","index":0,"function":{"name":"bash","arguments":"{\"command\": \"ls -la\"}"}}]},"finish_reason":null}]}`),
		sseData(`{"id":"` + mistralID + `","object":"chat.completion.chunk","created":1760000000,"model":"` + mistralModel + `","choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"total_tokens":150,"completion_tokens":30}}`),
		sseData(`[DONE]`),
	}
}

// fragmentedToolStream is the same tool call as mistralToolStream, with
// its arguments split across deltas the way other OpenAI-style servers
// send them: only the first delta carries id and name.
func fragmentedToolStream() []string {
	return []string{
		sseData(`{"id":"` + mistralID + `","model":"` + mistralModel + `","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + mistralCallID + `","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`),
		sseData(`{"id":"` + mistralID + `","model":"` + mistralModel + `","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"comm"}}]},"finish_reason":null}]}`),
		sseData(`{"id":"` + mistralID + `","model":"` + mistralModel + `","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\": \"ls -la\"}"}}]},"finish_reason":null}]}`),
		sseData(`{"id":"` + mistralID + `","model":"` + mistralModel + `","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"total_tokens":150,"completion_tokens":30}}`),
		sseData(`[DONE]`),
	}
}

const mistralJSONBody = `{"id":"` + mistralID + `","object":"chat.completion","created":1760000000,"model":"` + mistralModel + `",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + mistralCallID + `","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls -la\"}"}}]},"finish_reason":"tool_calls"}],` +
	`"usage":{"prompt_tokens":120,"total_tokens":150,"completion_tokens":30}}`

func mistralBashCall(at *time.Time) wantToolCall {
	return wantToolCall{id: mistralCallID, name: "bash", canonical: bashCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: at}
}

func checkMistralInfo(t *testing.T, info ResponseInfo, streamed bool, at time.Time) {
	t.Helper()
	if info.Streamed != streamed {
		t.Fatalf("Streamed = %v, want %v", info.Streamed, streamed)
	}
	if !equalPtr(info.ResponseID, ptr(mistralID)) {
		t.Fatalf("ResponseID = %v, want %s", deref(info.ResponseID), mistralID)
	}
	if !equalPtr(info.ResponseModel, ptr(mistralModel)) {
		t.Fatalf("ResponseModel = %v, want %s", deref(info.ResponseModel), mistralModel)
	}
	if !equalPtr(info.StopReason, ptr("tool_calls")) {
		t.Fatalf("StopReason = %v, want tool_calls (the provider's own value, untranslated)", deref(info.StopReason))
	}
	if info.Tokens == nil || *info.Tokens != (Tokens{Sent: mistralPromptTok, Received: mistralOutputTok}) {
		t.Fatalf("Tokens = %+v, want {Sent:%d Received:%d}", deref(info.Tokens), mistralPromptTok, mistralOutputTok)
	}
	if info.Failed || info.Unread {
		t.Fatalf("Failed = %v, Unread = %v; want both false", info.Failed, info.Unread)
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1: %+v", len(info.ToolCalls), info.ToolCalls)
	}
	checkToolCall(t, info.ToolCalls[0], mistralBashCall(&at))
}

func startOpenAITap(tr *Tracer, h http.Header) *ResponseTap {
	return startTapFor(tr, "openai", "messages", http.StatusOK, h)
}

// --- OpenAI-style response, SSE ------------------------------------------------

func TestTapOpenAI_SSE_ToolCallAndUsage(t *testing.T) {
	// Criterion 32, streamed.
	tr := newTestTracer(t, nil)
	tap := startOpenAITap(tr, sseHeader())
	events := mistralToolStream()
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	for i, e := range events {
		tap.Write([]byte(e), at(i))
	}
	// relayed_at is the write that carried finish_reason, which closes the call.
	checkMistralInfo(t, tap.Result(), true, at(mistralFinishAt))
}

func TestTapOpenAI_SSE_ByteByByteGivesTheSameResult(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, splitEvery(strings.Join(mistralToolStream(), ""), 1), t0)
	checkMistralInfo(t, tap.Result(), true, t0)
}

func TestTapOpenAI_SSE_FragmentedArgumentsGiveTheSameFingerprint(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, fragmentedToolStream(), t0)
	checkMistralInfo(t, tap.Result(), true, t0)
}

func TestTapOpenAI_SSE_ToolCallStillOpenAtDoneIsClosedThen(t *testing.T) {
	tr := newTestTracer(t, nil)
	events := mistralToolStream()
	// Drop the finish_reason chunk: the call is closed by [DONE] instead.
	stream := []string{events[0], events[1], events[3]}
	tap := startOpenAITap(tr, sseHeader())
	doneAt := t0.Add(5 * time.Second)
	tap.Write([]byte(stream[0]), t0)
	tap.Write([]byte(stream[1]), t0)
	tap.Write([]byte(stream[2]), doneAt)
	info := tap.Result()

	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	checkToolCall(t, info.ToolCalls[0], mistralBashCall(&doneAt))
}

func TestTapOpenAI_SSE_StreamCutBeforeFinishReasonLeavesItNotDelivered(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, mistralToolStream()[:mistralFinishAt], t0)
	info := tap.Result()

	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: mistralCallID, name: "bash", outcome: "not_delivered", relayedAt: nil})
}

func TestTapOpenAI_SSE_DisconnectBeforeFinishReasonLeavesItNotDelivered(t *testing.T) {
	tr := newTestTracer(t, nil)
	events := mistralToolStream()
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, events[:mistralFinishAt], t0)
	tap.Disconnected()
	feed(tap, events[mistralFinishAt:], t0)
	info := tap.Result()

	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	if got := info.ToolCalls[0].Outcome; got != "not_delivered" {
		t.Fatalf("Outcome = %q, want not_delivered: the agent left before the call was closed", got)
	}
}

func TestTapOpenAI_SSE_ToolCallsAreKeyedByChoiceAndIndex(t *testing.T) {
	// Two choices, each with a tool call at index 0: two distinct calls,
	// and stop_reason comes from choice 0.
	tr := newTestTracer(t, nil)
	stream := []string{
		sseData(`{"id":"c","model":"m","choices":[` +
			`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls -la\"}"}}]},"finish_reason":null},` +
			`{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"read","arguments":"{\"path\": \"/tmp\"}"}}]},"finish_reason":null}]}`),
		sseData(`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"length"}]}`),
		sseData(`[DONE]`),
	}
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, stream, t0)
	info := tap.Result()

	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2: %+v", len(info.ToolCalls), info.ToolCalls)
	}
	ids := map[string]bool{}
	for _, tc := range info.ToolCalls {
		if tc.ToolCallID != nil {
			ids[*tc.ToolCallID] = true
		}
		if tc.Outcome != "delivered" {
			t.Fatalf("tool call %v Outcome = %q, want delivered", deref(tc.ToolCallID), tc.Outcome)
		}
	}
	if !ids["call_a"] || !ids["call_b"] {
		t.Fatalf("tool call ids = %v, want call_a and call_b", ids)
	}
	if !equalPtr(info.StopReason, ptr("tool_calls")) {
		t.Fatalf("StopReason = %v, want tool_calls from choice 0", deref(info.StopReason))
	}
}

func TestTapOpenAI_SSE_NoUsageGivesNilTokens(t *testing.T) {
	tr := newTestTracer(t, nil)
	stream := []string{
		sseData(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		sseData(`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		sseData(`[DONE]`),
	}
	tap := startOpenAITap(tr, sseHeader())
	feed(tap, stream, t0)
	info := tap.Result()
	if info.Tokens != nil {
		t.Fatalf("Tokens = %+v, want nil: the stream carried no usage", info.Tokens)
	}
	if !equalPtr(info.StopReason, ptr("stop")) {
		t.Fatalf("StopReason = %v, want stop", deref(info.StopReason))
	}
	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
	}
}

// --- OpenAI-style response, JSON -----------------------------------------------

func TestTapOpenAI_JSON_ToolCallAndUsage(t *testing.T) {
	// Criterion 32, non-streamed.
	tr := newTestTracer(t, nil)
	tap := startOpenAITap(tr, jsonHeader())
	chunks := splitEvery(mistralJSONBody, 64)
	var last time.Time
	for i, c := range chunks {
		last = t0.Add(time.Duration(i) * time.Second)
		tap.Write([]byte(c), last)
	}
	checkMistralInfo(t, tap.Result(), false, last)
}

func TestTapOpenAI_JSONAndSSEGiveTheSameFingerprint(t *testing.T) {
	tr := newTestTracer(t, nil)
	j := startOpenAITap(tr, jsonHeader())
	j.Write([]byte(mistralJSONBody), t0)
	s := startOpenAITap(tr, sseHeader())
	feed(s, mistralToolStream(), t0)

	ji, si := j.Result(), s.Result()
	if len(ji.ToolCalls) != 1 || len(si.ToolCalls) != 1 {
		t.Fatalf("tool calls: JSON %d, SSE %d; want 1 each", len(ji.ToolCalls), len(si.ToolCalls))
	}
	if !equalPtr(ji.ToolCalls[0].Fingerprint, si.ToolCalls[0].Fingerprint) {
		t.Fatalf("fingerprint: JSON %v, SSE %v; want equal", deref(ji.ToolCalls[0].Fingerprint), deref(si.ToolCalls[0].Fingerprint))
	}
}

func TestTapOpenAI_JSON_ArgumentsThatAreNotJSONAreNotRecordedAndFail(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
		`{"id":"call_bad","type":"function","function":{"name":"bash","arguments":"not json {"}},` +
		`{"id":"call_ok","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls -la\"}"}}]},"finish_reason":"tool_calls"}]}`
	tap := startOpenAITap(tr, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()

	if !info.Failed {
		t.Fatal("Failed = false, want true: arguments that are not valid JSON are a tracing failure")
	}
	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
	}
	bad := info.ToolCalls[0]
	if !equalPtr(bad.ToolCallID, ptr("call_bad")) || bad.Recorded {
		t.Fatalf("invalid tool call = %+v, want call_bad with Recorded false", bad)
	}
	if bad.SizeBytes != nil || bad.Fingerprint != nil || bad.ArgumentNames != nil {
		t.Fatalf("invalid tool call has argument fields set: %+v", bad)
	}
	checkToolCall(t, info.ToolCalls[1], wantToolCall{id: "call_ok", name: "bash", canonical: bashCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: anyTime})
}

func TestTapOpenAI_JSON_EmbeddingsGiveSentTokensOnly(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"id":"emb-7d2f","object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"mistral-embed",` +
		`"usage":{"prompt_tokens":12,"total_tokens":12,"completion_tokens":0}}`
	tap := startTapFor(tr, "openai", "embeddings", http.StatusOK, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()

	if info.Tokens == nil || *info.Tokens != (Tokens{Sent: 12, Received: 0}) {
		t.Fatalf("Tokens = %+v, want {Sent:12 Received:0}", deref(info.Tokens))
	}
	if !equalPtr(info.ResponseID, ptr("emb-7d2f")) || !equalPtr(info.ResponseModel, ptr("mistral-embed")) {
		t.Fatalf("ResponseID/ResponseModel = %v/%v, want emb-7d2f/mistral-embed", deref(info.ResponseID), deref(info.ResponseModel))
	}
	if info.StopReason != nil {
		t.Fatalf("StopReason = %v, want nil: embeddings have no stop reason", deref(info.StopReason))
	}
	if len(info.ToolCalls) != 0 || info.Failed {
		t.Fatalf("ToolCalls = %+v, Failed = %v; want none, false", info.ToolCalls, info.Failed)
	}
}

func TestTapOpenAI_JSON_FIMCompletionReadsLikeChat(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"id":"fim-1","object":"chat.completion","model":"codestral-latest","choices":[{"index":0,"message":{"role":"assistant","content":"return a + b"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":9,"total_tokens":15,"completion_tokens":6}}`
	tap := startTapFor(tr, "openai", "completion", http.StatusOK, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()

	if !equalPtr(info.ResponseID, ptr("fim-1")) || !equalPtr(info.StopReason, ptr("stop")) {
		t.Fatalf("ResponseID/StopReason = %v/%v, want fim-1/stop", deref(info.ResponseID), deref(info.StopReason))
	}
	if info.Tokens == nil || *info.Tokens != (Tokens{Sent: 9, Received: 6}) {
		t.Fatalf("Tokens = %+v, want {Sent:9 Received:6}", deref(info.Tokens))
	}
}

func TestTapOpenAI_JSON_ErrorBodyHasNoToolCallsOrIdentifiers(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"object":"error","message":"Unauthorized","type":"invalid_request_error","code":"1000"}`
	tap := startTapFor(tr, "openai", "messages", http.StatusUnauthorized, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()

	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
	}
	if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil {
		t.Fatalf("identifiers = %v/%v/%v, want all nil", deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason))
	}
}

// --- Dispatch ---------------------------------------------------------------------

func TestTap_ModelsGivesNullDetailsInEveryFormat(t *testing.T) {
	tr := newTestTracer(t, nil)
	bodies := map[string]string{
		"anthropic": `{"data":[{"type":"model","id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}],"has_more":false,"first_id":"claude-sonnet-5","last_id":"claude-sonnet-5"}`,
		"openai":    `{"object":"list","data":[{"id":"mistral-large-latest","object":"model"}]}`,
		"gemini":    `{"models":[{"name":"models/gemini-2.5-pro","version":"002"}]}`,
	}
	for format, body := range bodies {
		t.Run(format, func(t *testing.T) {
			tap := startTapFor(tr, format, "models", http.StatusOK, jsonHeader())
			tap.Write([]byte(body), t0)
			info := tap.Result()
			if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil || info.Tokens != nil {
				t.Fatalf("details = %v/%v/%v/%v, want all nil for a model listing",
					deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason), deref(info.Tokens))
			}
			if len(info.ToolCalls) != 0 || info.Failed {
				t.Fatalf("ToolCalls = %+v, Failed = %v; want none, false", info.ToolCalls, info.Failed)
			}
		})
	}
}

func TestTap_UnknownFormatOrOperationFails(t *testing.T) {
	tr := newTestTracer(t, nil)
	for _, tt := range []struct{ format, op string }{
		{"", "messages"},
		{"cohere", "messages"},
		{"anthropic", ""},
		{"anthropic", "batches"},
	} {
		t.Run(tt.format+"/"+tt.op, func(t *testing.T) {
			tap := startTapFor(tr, tt.format, tt.op, http.StatusOK, jsonHeader())
			tap.Write([]byte(mistralJSONBody), t0)
			if info := tap.Result(); !info.Failed {
				t.Fatal("Failed = false, want true: a (format, operation) pair with no reader marks the tap Failed")
			}
		})
	}
}

// --- OpenAI-style request ----------------------------------------------------------

func newOpenAIRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
}

var openAIMessages = RequestOptions{Format: "openai", Operation: "messages"}

func TestRequestOpenAI_ModelAndStream(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"model":"mistral-large-latest","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	info, err := tr.Request(newOpenAIRequest(t, "/v1/chat/completions", body), []byte(body), "", openAIMessages)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !equalPtr(info.Model, ptr("mistral-large-latest")) || !info.Stream {
		t.Fatalf("Model/Stream = %v/%v, want mistral-large-latest/true", deref(info.Model), info.Stream)
	}
}

func TestRequestOpenAI_NewToolResults(t *testing.T) {
	// Criterion 33.
	tr := newTestTracer(t, nil)
	tests := []struct {
		name    string
		body    string
		wantIDs []string
	}{
		{
			name: "two tool messages after the last assistant message; earlier ones are not new",
			body: `{"model":"m","messages":[` +
				`{"role":"user","content":"go"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_old","type":"function","function":{"name":"bash","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_old","content":"old"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"one"},` +
				`{"role":"tool","tool_call_id":"call_2","content":"two"}]}`,
			wantIDs: []string{"call_1", "call_2"},
		},
		{
			name: "trailing system messages are skipped",
			body: `{"model":"m","messages":[` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"one"},` +
				`{"role":"system","content":"reminder"}]}`,
			wantIDs: []string{"call_1"},
		},
		{
			name: "a user message after the tool results does not hide them",
			body: `{"model":"m","messages":[` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"one"},` +
				`{"role":"user","content":"and now?"}]}`,
			wantIDs: []string{"call_1"},
		},
		{
			name:    "no assistant message: every tool message is new",
			body:    `{"model":"m","messages":[{"role":"tool","tool_call_id":"call_9","content":"x"}]}`,
			wantIDs: []string{"call_9"},
		},
		{
			name: "last message is the assistant's: no new results",
			body: `{"model":"m","messages":[` +
				`{"role":"tool","tool_call_id":"call_0","content":"x"},` +
				`{"role":"assistant","content":"done"}]}`,
		},
		{
			name: "no tool messages",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := tr.Request(newOpenAIRequest(t, "/v1/chat/completions", tt.body), []byte(tt.body), "", openAIMessages)
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			var got []string
			for _, r := range info.ToolResults {
				got = append(got, deref(r.ToolCallID).(string))
			}
			if strings.Join(got, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("tool result IDs = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

func TestRequestOpenAI_ToolResultFields(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_s","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call_p","type":"function","function":{"name":"b","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_s","content":"hello"},` +
		`{"role":"tool","tool_call_id":"call_p","content":[ {"type":"text", "text":"boom"} ]}]}`

	info, err := tr.Request(newOpenAIRequest(t, "/v1/chat/completions", body), []byte(body), "", openAIMessages)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(info.ToolResults) != 2 {
		t.Fatalf("len(ToolResults) = %d, want 2: %+v", len(info.ToolResults), info.ToolResults)
	}
	const strCanonical = `"hello"`
	const partsCanonical = `[{"text":"boom","type":"text"}]`
	for i, want := range []struct {
		id, canonical string
	}{{"call_s", strCanonical}, {"call_p", partsCanonical}} {
		got := info.ToolResults[i]
		if !equalPtr(got.ToolCallID, ptr(want.id)) {
			t.Fatalf("[%d] ToolCallID = %v, want %s", i, deref(got.ToolCallID), want.id)
		}
		if got.IsError {
			t.Fatalf("[%d] IsError = true, want false: the format has no error flag", i)
		}
		if got.SizeBytes != int64(len(want.canonical)) {
			t.Fatalf("[%d] SizeBytes = %d, want %d (canonical form of content)", i, got.SizeBytes, len(want.canonical))
		}
		if fp := expectedFingerprint(testKey, want.canonical); got.Fingerprint == nil || *got.Fingerprint != fp {
			t.Fatalf("[%d] Fingerprint = %v, want %s", i, deref(got.Fingerprint), fp)
		}
	}
}

func TestRequestOpenAI_ToolCallIDMustBeANonEmptyString(t *testing.T) {
	const marker = "SECRET_TOOL_OUTPUT_MARKER"
	tr := newTestTracer(t, nil)
	for name, msg := range map[string]string{
		"missing":    `{"role":"tool","content":"` + marker + `"}`,
		"empty":      `{"role":"tool","tool_call_id":"","content":"` + marker + `"}`,
		"not string": `{"role":"tool","tool_call_id":7,"content":"` + marker + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}}]},` + msg + `]}`
			_, err := tr.Request(newOpenAIRequest(t, "/v1/chat/completions", body), []byte(body), "", openAIMessages)
			if err == nil {
				t.Fatal("Request = nil error, want a read failure")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q leaks tool result content", err)
			}
		})
	}
}

func TestRequestOpenAI_CompletionAndEmbeddingsReadModelAndStreamOnly(t *testing.T) {
	tr := newTestTracer(t, nil)
	tests := []struct {
		op, path, body string
		wantModel      string
		wantStream     bool
	}{
		{"completion", "/v1/fim/completions", `{"model":"codestral-latest","prompt":"def f(","suffix":"return x","stream":true}`, "codestral-latest", true},
		{"embeddings", "/v1/embeddings", `{"model":"mistral-embed","input":["a","b"]}`, "mistral-embed", false},
		// A messages member is not read outside the messages operation.
		{"embeddings", "/v1/embeddings", `{"model":"mistral-embed","input":"a","messages":[{"role":"tool","content":"no id"}]}`, "mistral-embed", false},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			info, err := tr.Request(newOpenAIRequest(t, tt.path, tt.body), []byte(tt.body), "", RequestOptions{Format: "openai", Operation: tt.op})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if !equalPtr(info.Model, ptr(tt.wantModel)) || info.Stream != tt.wantStream {
				t.Fatalf("Model/Stream = %v/%v, want %s/%v", deref(info.Model), info.Stream, tt.wantModel, tt.wantStream)
			}
			if len(info.ToolResults) != 0 {
				t.Fatalf("ToolResults = %+v, want none", info.ToolResults)
			}
		})
	}
}
