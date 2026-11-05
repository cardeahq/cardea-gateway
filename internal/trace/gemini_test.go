package trace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Gemini response fixtures -------------------------------------------------
//
// specs/0003-technical, Reading other formats; functional criteria 34, 35.

const (
	geminiResponseID = "kM3oaJ-XCJ2vz7IP2f6v0Q0"
	geminiModel      = "gemini-2.5-pro-002"
	weatherCanon     = `{"city":"Paris"}`
)

// geminiResponses are the GenerateContentResponse objects of one streamed
// call: text, then a function call with the finish reason and final usage.
func geminiResponses() []string {
	return []string{
		`{"candidates":[{"content":{"parts":[{"text":"Let me check."}],"role":"model"},"index":0}],` +
			`"usageMetadata":{"promptTokenCount":20,"totalTokenCount":20},"modelVersion":"` + geminiModel + `","responseId":"` + geminiResponseID + `"}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city": "Paris"}}}],"role":"model"},"finishReason":"STOP","index":0}],` +
			`"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"thoughtsTokenCount":4,"totalTokenCount":32},"modelVersion":"` + geminiModel + `","responseId":"` + geminiResponseID + `"}`,
	}
}

// geminiSingleResponse is the non-streamed generateContent body for the
// same call.
const geminiSingleResponse = `{"candidates":[{"content":{"parts":[{"text":"Let me check."},{"functionCall":{"name":"get_weather","args":{"city": "Paris"}}}],"role":"model"},"finishReason":"STOP","index":0}],` +
	`"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"thoughtsTokenCount":4,"totalTokenCount":32},"modelVersion":"` + geminiModel + `","responseId":"` + geminiResponseID + `"}`

func weatherCall(at *time.Time) wantToolCall {
	return wantToolCall{id: noID, name: "get_weather", canonical: weatherCanon, names: []string{"city"}, outcome: "delivered", recorded: true, relayedAt: at}
}

func checkGeminiInfo(t *testing.T, info ResponseInfo, streamed bool, at time.Time) {
	t.Helper()
	if info.Streamed != streamed {
		t.Fatalf("Streamed = %v, want %v", info.Streamed, streamed)
	}
	if !equalPtr(info.ResponseID, ptr(geminiResponseID)) {
		t.Fatalf("ResponseID = %v, want %s", deref(info.ResponseID), geminiResponseID)
	}
	if !equalPtr(info.ResponseModel, ptr(geminiModel)) {
		t.Fatalf("ResponseModel = %v, want %s", deref(info.ResponseModel), geminiModel)
	}
	if !equalPtr(info.StopReason, ptr("STOP")) {
		t.Fatalf("StopReason = %v, want STOP (the provider's own value)", deref(info.StopReason))
	}
	// received = candidatesTokenCount + thoughtsTokenCount.
	if info.Tokens == nil || *info.Tokens != (Tokens{Sent: 20, Received: 12}) {
		t.Fatalf("Tokens = %+v, want {Sent:20 Received:12}", deref(info.Tokens))
	}
	if info.Failed || info.Unread {
		t.Fatalf("Failed = %v, Unread = %v; want both false", info.Failed, info.Unread)
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1: %+v", len(info.ToolCalls), info.ToolCalls)
	}
	checkToolCall(t, info.ToolCalls[0], weatherCall(&at))
}

func startGeminiTap(tr *Tracer, h http.Header) *ResponseTap {
	return startTapFor(tr, "gemini", "messages", http.StatusOK, h)
}

func TestTapGemini_AltSSEStream(t *testing.T) {
	// Criterion 34.
	tr := newTestTracer(t, nil)
	tap := startGeminiTap(tr, sseHeader())
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	for i, r := range geminiResponses() {
		tap.Write([]byte(sseData(r)), at(i))
	}
	// The function call opens and closes in the response that carries it.
	checkGeminiInfo(t, tap.Result(), true, at(1))
}

func TestTapGemini_AltSSEByteByByte(t *testing.T) {
	tr := newTestTracer(t, nil)
	var all string
	for _, r := range geminiResponses() {
		all += sseData(r)
	}
	tap := startGeminiTap(tr, sseHeader())
	feed(tap, splitEvery(all, 1), t0)
	checkGeminiInfo(t, tap.Result(), true, t0)
}

func TestTapGemini_JSONArrayStream(t *testing.T) {
	// streamGenerateContent without alt=sse answers one JSON array of
	// responses; relayed_at is the last relayed write, as for any
	// non-streamed body.
	tr := newTestTracer(t, nil)
	body := "[" + strings.Join(geminiResponses(), ",\r\n") + "]"
	tap := startGeminiTap(tr, jsonHeader())
	var last time.Time
	for i, c := range splitEvery(body, 50) {
		last = t0.Add(time.Duration(i) * time.Second)
		tap.Write([]byte(c), last)
	}
	checkGeminiInfo(t, tap.Result(), false, last)
}

func TestTapGemini_SingleJSONBody(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startGeminiTap(tr, jsonHeader())
	tap.Write([]byte(geminiSingleResponse), t0)
	checkGeminiInfo(t, tap.Result(), false, t0)
}

func TestTapGemini_AllThreeShapesGiveTheSameFingerprint(t *testing.T) {
	tr := newTestTracer(t, nil)

	sse := startGeminiTap(tr, sseHeader())
	for _, r := range geminiResponses() {
		sse.Write([]byte(sseData(r)), t0)
	}
	arr := startGeminiTap(tr, jsonHeader())
	arr.Write([]byte("["+strings.Join(geminiResponses(), ",")+"]"), t0)
	one := startGeminiTap(tr, jsonHeader())
	one.Write([]byte(geminiSingleResponse), t0)

	want := expectedFingerprint(testKey, weatherCanon)
	for name, tap := range map[string]*ResponseTap{"alt=sse": sse, "array": arr, "single": one} {
		info := tap.Result()
		if len(info.ToolCalls) != 1 || !equalPtr(info.ToolCalls[0].Fingerprint, &want) {
			t.Fatalf("%s: tool calls = %+v, want one with fingerprint %s", name, info.ToolCalls, want)
		}
	}
}

func TestTapGemini_FunctionCallIDIsKeptWhenPresent(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fc_01","name":"get_weather","args":{"city":"Paris"}}}],"role":"model"},"finishReason":"STOP"}]}`
	tap := startGeminiTap(tr, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()
	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	if !equalPtr(info.ToolCalls[0].ToolCallID, ptr("fc_01")) {
		t.Fatalf("ToolCallID = %v, want fc_01", deref(info.ToolCalls[0].ToolCallID))
	}
}

func TestTapGemini_FunctionCallWithoutIDHasNilToolCallID(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startGeminiTap(tr, jsonHeader())
	tap.Write([]byte(geminiSingleResponse), t0)
	info := tap.Result()
	if len(info.ToolCalls) != 1 || info.ToolCalls[0].ToolCallID != nil {
		t.Fatalf("tool calls = %+v, want one with a nil ToolCallID", info.ToolCalls)
	}
}

func TestTapGemini_StreamCutAfterTextHasNoToolCall(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startGeminiTap(tr, sseHeader())
	tap.Write([]byte(sseData(geminiResponses()[0])), t0)
	info := tap.Result()
	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none: the function call was never relayed", info.ToolCalls)
	}
	if info.StopReason != nil {
		t.Fatalf("StopReason = %v, want nil: no finishReason was seen", deref(info.StopReason))
	}
}

func TestTapGemini_CountTokensAndEmbedGiveNullDetails(t *testing.T) {
	tr := newTestTracer(t, nil)
	for op, body := range map[string]string{
		"count_tokens": `{"totalTokens":31,"promptTokensDetails":[{"modality":"TEXT","tokenCount":31}]}`,
		"embeddings":   `{"embedding":{"values":[0.1,0.2,0.3]}}`,
	} {
		t.Run(op, func(t *testing.T) {
			tap := startTapFor(tr, "gemini", op, http.StatusOK, jsonHeader())
			tap.Write([]byte(body), t0)
			info := tap.Result()
			if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil || info.Tokens != nil {
				t.Fatalf("details = %v/%v/%v/%v, want all nil",
					deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason), deref(info.Tokens))
			}
			if len(info.ToolCalls) != 0 || info.Failed {
				t.Fatalf("ToolCalls = %+v, Failed = %v; want none, false", info.ToolCalls, info.Failed)
			}
		})
	}
}

// --- Gemini request ------------------------------------------------------------

func newGeminiRequest(t *testing.T, action, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:"+action, strings.NewReader(body))
}

var geminiMessages = RequestOptions{Format: "gemini", Operation: "messages", Model: ptr("gemini-2.5-pro")}

func TestRequestGemini_FunctionResponseInTheLastUserEntryIsANewResult(t *testing.T) {
	// Criterion 35.
	tr := newTestTracer(t, nil)
	body := `{"contents":[` +
		`{"role":"user","parts":[{"text":"weather?"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"temp": 21, "unit": "C"}}}]}]}`

	info, err := tr.Request(newGeminiRequest(t, "generateContent", body), []byte(body), "", geminiMessages)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(info.ToolResults) != 1 {
		t.Fatalf("len(ToolResults) = %d, want 1: %+v", len(info.ToolResults), info.ToolResults)
	}
	got := info.ToolResults[0]
	const canonical = `{"temp":21,"unit":"C"}`
	if got.ToolCallID != nil {
		t.Fatalf("ToolCallID = %v, want nil: the function response has no id", deref(got.ToolCallID))
	}
	if got.IsError {
		t.Fatal("IsError = true, want false")
	}
	if got.SizeBytes != int64(len(canonical)) {
		t.Fatalf("SizeBytes = %d, want %d (canonical form of response)", got.SizeBytes, len(canonical))
	}
	if fp := expectedFingerprint(testKey, canonical); got.Fingerprint == nil || *got.Fingerprint != fp {
		t.Fatalf("Fingerprint = %v, want %s", deref(got.Fingerprint), fp)
	}
	if !equalPtr(info.Model, ptr("gemini-2.5-pro")) {
		t.Fatalf("Model = %v, want gemini-2.5-pro", deref(info.Model))
	}
}

func TestRequestGemini_WhichFunctionResponsesAreNew(t *testing.T) {
	tr := newTestTracer(t, nil)
	const fr = `{"functionResponse":{"id":"%s","name":"f","response":{"ok":true}}}`
	part := func(id string) string { return strings.Replace(fr, "%s", id, 1) }
	tests := []struct {
		name    string
		body    string
		wantIDs []string
	}{
		{
			name: "only the last entry's responses, with their ids",
			body: `{"contents":[{"role":"user","parts":[` + part("fc_old") + `]},{"role":"model","parts":[{"text":"ok"}]},` +
				`{"role":"user","parts":[` + part("fc_1") + `,{"text":"and"},` + part("fc_2") + `]}]}`,
			wantIDs: []string{"fc_1", "fc_2"},
		},
		{
			name:    "last entry with no role counts as user",
			body:    `{"contents":[{"parts":[` + part("fc_1") + `]}]}`,
			wantIDs: []string{"fc_1"},
		},
		{
			name: "last entry from the model has no new results",
			body: `{"contents":[{"role":"user","parts":[` + part("fc_1") + `]},{"role":"model","parts":[{"text":"done"}]}]}`,
		},
		{
			name: "no contents",
			body: `{"contents":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := tr.Request(newGeminiRequest(t, "generateContent", tt.body), []byte(tt.body), "", geminiMessages)
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

func TestRequestGemini_SnakeCaseIsReadAlike(t *testing.T) {
	tr := newTestTracer(t, nil)
	camel := `{"contents":[{"role":"user","parts":[{"functionResponse":{"id":"fc_1","name":"f","response":{"ok":true}}}]}]}`
	snake := `{"contents":[{"role":"user","parts":[{"function_response":{"id":"fc_1","name":"f","response":{"ok":true}}}]}]}`

	var fps []string
	for _, body := range []string{camel, snake} {
		info, err := tr.Request(newGeminiRequest(t, "generateContent", body), []byte(body), "", geminiMessages)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		if len(info.ToolResults) != 1 || info.ToolResults[0].Fingerprint == nil {
			t.Fatalf("ToolResults = %+v, want one with a fingerprint", info.ToolResults)
		}
		fps = append(fps, *info.ToolResults[0].Fingerprint)
	}
	if fps[0] != fps[1] {
		t.Fatalf("fingerprints camelCase %s, snake_case %s; want equal", fps[0], fps[1])
	}
}

func TestRequestGemini_BothSpellingsInOnePartIsAnError(t *testing.T) {
	const marker = "SECRET_FUNCTION_OUTPUT"
	tr := newTestTracer(t, nil)
	body := `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"v":"` + marker + `"}},` +
		`"function_response":{"name":"f","response":{"v":"other"}}}]}]}`
	_, err := tr.Request(newGeminiRequest(t, "generateContent", body), []byte(body), "", geminiMessages)
	if err == nil {
		t.Fatal("Request = nil error, want a read failure: the Gateway and Gemini could read different values")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error %q leaks content", err)
	}
}

func TestRequestGemini_CountTokensAndEmbeddingsReadModelOnly(t *testing.T) {
	tr := newTestTracer(t, nil)
	// A function response in the last entry is not read outside messages.
	body := `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}]}`
	for op, action := range map[string]string{"count_tokens": "countTokens", "embeddings": "embedContent"} {
		t.Run(op, func(t *testing.T) {
			info, err := tr.Request(newGeminiRequest(t, action, body), []byte(body), "", RequestOptions{Format: "gemini", Operation: op, Model: ptr("gemini-2.5-pro")})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if len(info.ToolResults) != 0 {
				t.Fatalf("ToolResults = %+v, want none", info.ToolResults)
			}
			if !equalPtr(info.Model, ptr("gemini-2.5-pro")) || info.Stream {
				t.Fatalf("Model/Stream = %v/%v, want gemini-2.5-pro/false", deref(info.Model), info.Stream)
			}
		})
	}
}
