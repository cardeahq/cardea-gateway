package trace

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

// --- Anthropic SSE fixtures ---------------------------------------------------

func sseEvent(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func messageStartEvent(id, model string, inputTokens int) string {
	return sseEvent("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, id, model, inputTokens))
}

func textStartEvent(index int) string {
	return sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index))
}

func textDeltaEvent(index int, text string) string {
	return sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, jsonString(text)))
}

func toolStartEvent(index int, blockType, id, name string) string {
	return sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":%q,"id":%q,"name":%q,"input":{}}}`, index, blockType, id, name))
}

func argsDeltaEvent(index int, partial string) string {
	return sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, jsonString(partial)))
}

func blockStopEvent(index int) string {
	return sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func messageDeltaEvent(stopReason string, outputTokens int) string {
	return sseEvent("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stopReason, outputTokens))
}

func messageStopEvent() string { return sseEvent("message_stop", `{"type":"message_stop"}`) }

const (
	responseID      = "msg_01XFDUDYJgAACzvnptvVoYEL"
	responseModel   = "claude-sonnet-5-20260801"
	webSearchCanon  = `{"query":"golang release"}`
	bashCanon       = `{"command":"ls -la"}`
	webSearchStopAt = 8  // index in standardStream of web_search's content_block_stop
	bashStartAt     = 11 // index in standardStream of bash's content_block_start
	bashStopAt      = 15 // index in standardStream of bash's content_block_stop
	messageDeltaAt  = 16 // index in standardStream of message_delta
)

// standardStream is a recorded-shape Anthropic stream: text, a
// server_tool_use (web_search) with its result block, then a tool_use
// (bash), ending with stop reason tool_use. One element per SSE event.
func standardStream() []string {
	return []string{
		messageStartEvent(responseID, responseModel, 1834),
		sseEvent("ping", `{"type": "ping"}`),
		textStartEvent(0),
		textDeltaEvent(0, "Let me check."),
		blockStopEvent(0),
		toolStartEvent(1, "server_tool_use", "srvtoolu_01", "web_search"),
		argsDeltaEvent(1, `{"que`),
		argsDeltaEvent(1, `ry": "golang release"}`),
		blockStopEvent(1), // webSearchStopAt
		sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[]}}`),
		blockStopEvent(2),
		toolStartEvent(3, "tool_use", "toolu_01B2", "bash"), // bashStartAt
		argsDeltaEvent(3, ""),
		argsDeltaEvent(3, `{"command": `),
		argsDeltaEvent(3, `"ls -la"}`),
		blockStopEvent(3),                  // bashStopAt
		messageDeltaEvent("tool_use", 212), // messageDeltaAt
		messageStopEvent(),
	}
}

// standardJSONBody is the non-streamed form of standardStream.
const standardJSONBody = `{"id":"` + responseID + `","type":"message","role":"assistant","model":"` + responseModel + `",` +
	`"content":[{"type":"text","text":"Let me check."},` +
	`{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{"query": "golang release"}},` +
	`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[]},` +
	`{"type":"tool_use","id":"toolu_01B2","name":"bash","input":{"command": "ls -la"}}],` +
	`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1834,"output_tokens":212}}`

var t0 = time.Date(2026, 9, 27, 14, 3, 11, 0, time.UTC)

func sseHeader() http.Header {
	return http.Header{"Content-Type": {"text/event-stream"}}
}

func jsonHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}}
}

func startTap(tr *Tracer, status int, h http.Header) *ResponseTap {
	tap := tr.NewResponseTap()
	tap.Start(status, h)
	return tap
}

func feed(tap *ResponseTap, chunks []string, at time.Time) {
	for _, c := range chunks {
		tap.Write([]byte(c), at)
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func splitLines(s string) []string {
	return strings.SplitAfter(s, "\n")
}

type wantToolCall struct {
	id, name   string
	serverSide bool
	canonical  string // "" means argument fields are nil
	names      []string
	outcome    string
	recorded   bool
	relayedAt  *time.Time // nil means RelayedAt must be nil; use anyTime to skip the check
}

var anyTime = &time.Time{}

func checkToolCall(t *testing.T, got ToolCall, want wantToolCall) {
	t.Helper()
	if got.ToolCallID != want.id || got.ToolName != want.name || got.ServerSide != want.serverSide {
		t.Fatalf("tool call = {id %q, name %q, server_side %v}, want {id %q, name %q, server_side %v}",
			got.ToolCallID, got.ToolName, got.ServerSide, want.id, want.name, want.serverSide)
	}
	if got.Outcome != want.outcome {
		t.Fatalf("tool call %s Outcome = %q, want %q", want.id, got.Outcome, want.outcome)
	}
	if got.Recorded != want.recorded {
		t.Fatalf("tool call %s Recorded = %v, want %v", want.id, got.Recorded, want.recorded)
	}
	if want.canonical == "" {
		if got.ArgumentNames != nil || got.SizeBytes != nil || got.Fingerprint != nil {
			t.Fatalf("tool call %s argument fields = {names %v, size %v, fp %v}, want all nil",
				want.id, got.ArgumentNames, deref(got.SizeBytes), deref(got.Fingerprint))
		}
	} else {
		if !slices.Equal(got.ArgumentNames, want.names) || got.ArgumentNames == nil {
			t.Fatalf("tool call %s ArgumentNames = %#v, want %#v", want.id, got.ArgumentNames, want.names)
		}
		if got.SizeBytes == nil || *got.SizeBytes != int64(len(want.canonical)) {
			t.Fatalf("tool call %s SizeBytes = %v, want %d", want.id, deref(got.SizeBytes), len(want.canonical))
		}
		if fp := expectedFingerprint(testKey, want.canonical); got.Fingerprint == nil || *got.Fingerprint != fp {
			t.Fatalf("tool call %s Fingerprint = %v, want %s", want.id, deref(got.Fingerprint), fp)
		}
	}
	switch {
	case want.relayedAt == anyTime:
	case want.relayedAt == nil:
		if got.RelayedAt != nil {
			t.Fatalf("tool call %s RelayedAt = %v, want nil", want.id, time.Time(*got.RelayedAt))
		}
	default:
		if got.RelayedAt == nil || !time.Time(*got.RelayedAt).Equal(*want.relayedAt) {
			t.Fatalf("tool call %s RelayedAt = %v, want %v", want.id, got.RelayedAt, *want.relayedAt)
		}
	}
}

func webSearchCall(at *time.Time) wantToolCall {
	return wantToolCall{id: "srvtoolu_01", name: "web_search", serverSide: true, canonical: webSearchCanon, names: []string{"query"}, outcome: "delivered", recorded: true, relayedAt: at}
}

func bashCall(at *time.Time) wantToolCall {
	return wantToolCall{id: "toolu_01B2", name: "bash", canonical: bashCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: at}
}

func checkStandardInfo(t *testing.T, info ResponseInfo, streamed bool, at time.Time) {
	t.Helper()
	if info.Streamed != streamed {
		t.Fatalf("Streamed = %v, want %v", info.Streamed, streamed)
	}
	if !equalPtr(info.ResponseID, ptr(responseID)) {
		t.Fatalf("ResponseID = %v, want %s", deref(info.ResponseID), responseID)
	}
	if !equalPtr(info.ResponseModel, ptr(responseModel)) {
		t.Fatalf("ResponseModel = %v, want %s", deref(info.ResponseModel), responseModel)
	}
	if !equalPtr(info.StopReason, ptr("tool_use")) {
		t.Fatalf("StopReason = %v, want tool_use", deref(info.StopReason))
	}
	if info.Tokens == nil || *info.Tokens != (Tokens{Sent: 1834, Received: 212}) {
		t.Fatalf("Tokens = %+v, want {Sent:1834 Received:212}", deref(info.Tokens))
	}
	if info.Failed || info.Unread {
		t.Fatalf("Failed = %v, Unread = %v; want both false", info.Failed, info.Unread)
	}
	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2 (text and web_search_tool_result blocks are not tool calls): %+v", len(info.ToolCalls), info.ToolCalls)
	}
	checkToolCall(t, info.ToolCalls[0], webSearchCall(&at))
	checkToolCall(t, info.ToolCalls[1], bashCall(&at))
}

// --- SSE mode -----------------------------------------------------------------

func TestTap_SSE_SameResultWhateverTheWriteBoundaries(t *testing.T) {
	tr := newTestTracer(t, nil)
	whole := strings.Join(standardStream(), "")

	tests := []struct {
		name   string
		chunks []string
	}{
		{"whole stream in one write", []string{whole}},
		{"one event per write", standardStream()},
		{"one line per write", splitLines(whole)},
		{"byte by byte", splitEvery(whole, 1)},
		{"split mid-line every 7 bytes", splitEvery(whole, 7)},
		{"split mid-event every 64 bytes", splitEvery(whole, 64)},
	}

	var reference ResponseInfo
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tap := startTap(tr, http.StatusOK, sseHeader())
			feed(tap, tt.chunks, t0)
			info := tap.Result()

			checkStandardInfo(t, info, true, t0)
			if i == 0 {
				reference = info
				return
			}
			if !reflect.DeepEqual(info, reference) {
				t.Fatalf("ResponseInfo differs from the whole-stream result:\n got %+v\nwant %+v", info, reference)
			}
		})
	}
}

func TestTap_SSE_RelayedAtIsTheTimeOfTheWriteCarryingBlockStop(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, sseHeader())

	events := standardStream()
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	for i, e := range events {
		tap.Write([]byte(e), at(i))
	}
	info := tap.Result()

	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
	}
	webAt, bashAt := at(webSearchStopAt), at(bashStopAt)
	checkToolCall(t, info.ToolCalls[0], webSearchCall(&webAt))
	checkToolCall(t, info.ToolCalls[1], bashCall(&bashAt))
}

func TestTap_SSE_DisconnectBeforeABlockClosesMakesItNotDelivered(t *testing.T) {
	tr := newTestTracer(t, nil)
	events := standardStream()

	t.Run("open when the agent disconnected", func(t *testing.T) {
		tap := startTap(tr, http.StatusOK, sseHeader())
		feed(tap, events[:bashStartAt+3], t0) // bash opened, two deltas relayed, not closed
		tap.Disconnected()
		info := tap.Result()

		if len(info.ToolCalls) != 2 {
			t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
		}
		checkToolCall(t, info.ToolCalls[0], webSearchCall(&t0))
		got := info.ToolCalls[1]
		if got.ToolCallID != "toolu_01B2" || got.ToolName != "bash" {
			t.Fatalf("second tool call = %s/%s, want toolu_01B2/bash", got.ToolCallID, got.ToolName)
		}
		if got.Outcome != "not_delivered" {
			t.Fatalf("Outcome = %q, want not_delivered", got.Outcome)
		}
		if got.RelayedAt != nil || got.ArgumentNames != nil || got.SizeBytes != nil || got.Fingerprint != nil {
			t.Fatalf("not_delivered tool call has relayed_at/argument fields set: %+v", got)
		}
	})

	t.Run("opened after the agent disconnected", func(t *testing.T) {
		tap := startTap(tr, http.StatusOK, sseHeader())
		feed(tap, events[:bashStartAt], t0)
		tap.Disconnected()
		feed(tap, events[bashStartAt:], t0)
		info := tap.Result()

		if len(info.ToolCalls) != 2 {
			t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
		}
		checkToolCall(t, info.ToolCalls[0], webSearchCall(&t0))
		if got := info.ToolCalls[1].Outcome; got != "not_delivered" {
			t.Fatalf("tool call opened after Disconnected: Outcome = %q, want not_delivered", got)
		}
	})
}

func TestTap_SSE_StreamEndingEarly(t *testing.T) {
	tr := newTestTracer(t, nil)
	events := standardStream()

	t.Run("after the last tool call closed", func(t *testing.T) {
		tap := startTap(tr, http.StatusOK, sseHeader())
		feed(tap, events[:messageDeltaAt], t0)
		info := tap.Result()

		if info.StopReason != nil {
			t.Fatalf("StopReason = %q, want nil: message_delta never came", *info.StopReason)
		}
		if !equalPtr(info.ResponseID, ptr(responseID)) || !equalPtr(info.ResponseModel, ptr(responseModel)) {
			t.Fatalf("ResponseID/ResponseModel = %v/%v, want them from message_start", deref(info.ResponseID), deref(info.ResponseModel))
		}
		if len(info.ToolCalls) != 2 {
			t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
		}
		checkToolCall(t, info.ToolCalls[1], bashCall(&t0))
	})

	t.Run("in the middle of a tool call", func(t *testing.T) {
		tap := startTap(tr, http.StatusOK, sseHeader())
		feed(tap, events[:bashStartAt+2], t0)
		info := tap.Result()

		if info.StopReason != nil {
			t.Fatalf("StopReason = %q, want nil", *info.StopReason)
		}
		if len(info.ToolCalls) != 2 {
			t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
		}
		if got := info.ToolCalls[1].Outcome; got != "not_delivered" {
			t.Fatalf("tool call still open at the end of the stream: Outcome = %q, want not_delivered", got)
		}
		if info.ToolCalls[1].RelayedAt != nil {
			t.Fatal("tool call still open at the end of the stream has a RelayedAt")
		}
	})

	t.Run("before message_start", func(t *testing.T) {
		tap := startTap(tr, http.StatusOK, sseHeader())
		info := tap.Result()

		if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil {
			t.Fatalf("identifiers = %v/%v/%v, want all nil", deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason))
		}
		if len(info.ToolCalls) != 0 {
			t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
		}
	})
}

func TestTap_SSE_NonObjectArgumentsHaveEmptyArgumentNames(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, sseHeader())
	feed(tap, []string{
		messageStartEvent(responseID, responseModel, 10),
		toolStartEvent(0, "tool_use", "toolu_arr", "sum"),
		argsDeltaEvent(0, `[1, 2]`),
		blockStopEvent(0),
		messageDeltaEvent("tool_use", 5),
	}, t0)
	info := tap.Result()

	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	got := info.ToolCalls[0]
	if got.ArgumentNames == nil || len(got.ArgumentNames) != 0 {
		t.Fatalf("ArgumentNames = %#v, want an empty, non-nil slice for non-object arguments", got.ArgumentNames)
	}
	if got.SizeBytes == nil || *got.SizeBytes != int64(len(`[1,2]`)) {
		t.Fatalf("SizeBytes = %v, want %d", deref(got.SizeBytes), len(`[1,2]`))
	}
	if !got.Recorded {
		t.Fatal("Recorded = false, want true")
	}
}

func TestTap_SSE_ArgumentNamesAreSorted(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, sseHeader())
	feed(tap, []string{
		messageStartEvent(responseID, responseModel, 10),
		toolStartEvent(0, "tool_use", "toolu_w", "write_file"),
		argsDeltaEvent(0, `{"path": "/tmp/x", "content": "hi", "mode": "0644"}`),
		blockStopEvent(0),
	}, t0)
	info := tap.Result()

	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	want := []string{"content", "mode", "path"}
	if got := info.ToolCalls[0].ArgumentNames; !slices.Equal(got, want) {
		t.Fatalf("ArgumentNames = %v, want %v", got, want)
	}
}

func gzipChunks(t *testing.T, events []string) [][]byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	var chunks [][]byte
	take := func() {
		chunks = append(chunks, bytes.Clone(buf.Bytes()))
		buf.Reset()
	}
	for _, e := range events {
		if _, err := zw.Write([]byte(e)); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := zw.Flush(); err != nil {
			t.Fatalf("gzip flush: %v", err)
		}
		take()
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	take()
	return chunks
}

func TestTap_SSE_GzipGivesTheSameResult(t *testing.T) {
	tr := newTestTracer(t, nil)

	plain := startTap(tr, http.StatusOK, sseHeader())
	feed(plain, standardStream(), t0)
	want := plain.Result()

	h := sseHeader()
	h.Set("Content-Encoding", "gzip")
	tap := startTap(tr, http.StatusOK, h)
	chunks := gzipChunks(t, standardStream())
	before := make([]time.Time, len(chunks))
	for i, c := range chunks {
		before[i] = time.Now().Truncate(time.Microsecond)
		tap.Write(c, time.Now())
	}
	got := tap.Result()
	after := time.Now()

	if len(got.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(got.ToolCalls))
	}
	for i, stopAt := range []int{webSearchStopAt, bashStopAt} {
		ra := got.ToolCalls[i].RelayedAt
		if ra == nil {
			t.Fatalf("tool call %d RelayedAt = nil, want the time its content_block_stop was decoded", i)
		}
		ts := time.Time(*ra)
		if ts.Before(before[stopAt]) || ts.After(after) {
			t.Fatalf("tool call %d RelayedAt = %v, want no earlier than the write carrying its compressed content_block_stop (%v) and no later than Result (%v)",
				i, ts, before[stopAt], after)
		}
	}

	for i := range got.ToolCalls {
		got.ToolCalls[i].RelayedAt = nil
		want.ToolCalls[i].RelayedAt = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gzip ResponseInfo (ignoring RelayedAt) differs from identity:\n got %+v\nwant %+v", got, want)
	}
}

func TestTap_SSE_InvalidArgumentsFailTheToolCallOnly(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, sseHeader())
	feed(tap, []string{
		messageStartEvent(responseID, responseModel, 1834),
		toolStartEvent(0, "tool_use", "toolu_bad", "bash"),
		argsDeltaEvent(0, `{"command": `),
		blockStopEvent(0),
		toolStartEvent(1, "tool_use", "toolu_01B2", "bash"),
		argsDeltaEvent(1, `{"command": "ls -la"}`),
		blockStopEvent(1),
		messageDeltaEvent("tool_use", 212),
		messageStopEvent(),
	}, t0)
	info := tap.Result()

	if !info.Failed {
		t.Fatal("Failed = false, want true: arguments that are not valid JSON are a tracing failure")
	}
	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
	}
	bad := info.ToolCalls[0]
	if bad.ToolCallID != "toolu_bad" || bad.Recorded {
		t.Fatalf("invalid tool call = %+v, want toolu_bad with Recorded false", bad)
	}
	if bad.SizeBytes != nil || bad.Fingerprint != nil || bad.ArgumentNames != nil {
		t.Fatalf("invalid tool call has argument fields set: %+v", bad)
	}
	checkToolCall(t, info.ToolCalls[1], bashCall(&t0))
	if !equalPtr(info.StopReason, ptr("tool_use")) {
		t.Fatalf("StopReason = %v, want tool_use: parsing goes on after a failed tool call", deref(info.StopReason))
	}
}

// --- JSON mode ----------------------------------------------------------------

func TestTap_JSON_MatchesTheStreamedForm(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, jsonHeader())
	chunks := splitEvery(standardJSONBody, 100)
	var last time.Time
	for i, c := range chunks {
		last = t0.Add(time.Duration(i) * time.Second)
		tap.Write([]byte(c), last)
	}
	info := tap.Result()

	checkStandardInfo(t, info, false, last)

	streamed := startTap(tr, http.StatusOK, sseHeader())
	feed(streamed, standardStream(), t0)
	s := streamed.Result()
	for i := range info.ToolCalls {
		if *info.ToolCalls[i].Fingerprint != *s.ToolCalls[i].Fingerprint {
			t.Fatalf("tool call %d fingerprint: JSON %s, SSE %s; want equal", i, *info.ToolCalls[i].Fingerprint, *s.ToolCalls[i].Fingerprint)
		}
	}
}

func TestTap_JSON_GzipGivesTheSameResult(t *testing.T) {
	tr := newTestTracer(t, nil)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(standardJSONBody))
	_ = zw.Close()

	h := jsonHeader()
	h.Set("Content-Encoding", "gzip")
	tap := startTap(tr, http.StatusOK, h)
	for _, c := range splitEvery(buf.String(), 50) {
		tap.Write([]byte(c), t0)
	}
	checkStandardInfo(t, tap.Result(), false, t0)
}

func TestTap_JSON_ErrorBodyHasNoToolCallsOrIdentifiers(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusBadRequest, jsonHeader())
	tap.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`), t0)
	info := tap.Result()

	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
	}
	if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil {
		t.Fatalf("identifiers = %v/%v/%v, want all nil", deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason))
	}
}

// --- Other content types and encodings -----------------------------------------

func TestTap_OtherContentTypeIsNotParsed(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := startTap(tr, http.StatusOK, http.Header{"Content-Type": {"text/plain"}})
	feed(tap, standardStream(), t0)
	info := tap.Result()

	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none for a text/plain response", info.ToolCalls)
	}
	if info.Failed {
		t.Fatal("Failed = true, want false: an unparsed content type is not a failure")
	}
}

func TestTap_UnknownEncodingFails(t *testing.T) {
	tr := newTestTracer(t, nil)
	h := sseHeader()
	h.Set("Content-Encoding", "br")
	tap := startTap(tr, http.StatusOK, h)
	feed(tap, []string{"\x1b\x2f\x00\xf8 not really brotli"}, t0)
	info := tap.Result()

	if !info.Failed {
		t.Fatal("Failed = false, want true for an encoding tracing cannot read")
	}
	if len(info.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
	}
}

// --- Read limit (criteria 30, 31) ------------------------------------------------

const testReadLimit = 512

func newLimitedTracer(t *testing.T) *Tracer {
	return newTestTracer(t, func(c *config.TracingConfig) { c.MaxReadBytes = testReadLimit })
}

func TestTap_ReadLimit_LargeStreamedArguments(t *testing.T) {
	tr := newLimitedTracer(t)
	bigArgs := `{"content": "` + strings.Repeat("a", 800) + `"}`

	events := []string{
		messageStartEvent(responseID, responseModel, 1834),
		toolStartEvent(0, "tool_use", "toolu_big", "write_file"),
	}
	for _, part := range splitEvery(bigArgs, 40) { // every event stays under the limit
		events = append(events, argsDeltaEvent(0, part))
	}
	events = append(events,
		blockStopEvent(0),
		toolStartEvent(1, "tool_use", "toolu_01B2", "bash"),
		argsDeltaEvent(1, `{"command": "ls -la"}`),
		blockStopEvent(1),
		messageDeltaEvent("tool_use", 212),
	)

	tap := startTap(tr, http.StatusOK, sseHeader())
	for i, e := range events {
		tap.Write([]byte(e), t0.Add(time.Duration(i)*time.Second))
	}
	info := tap.Result()

	if info.Failed {
		t.Fatal("Failed = true, want false: hitting the read limit is not a failure")
	}
	if len(info.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(info.ToolCalls))
	}
	bigStop := t0.Add(time.Duration(len(events)-5) * time.Second)
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: "toolu_big", name: "write_file", outcome: "delivered", recorded: false, relayedAt: &bigStop})
	checkToolCall(t, info.ToolCalls[1], bashCall(anyTime))
}

func TestTap_ReadLimit_OversizedEventIsSkipped(t *testing.T) {
	tr := newLimitedTracer(t)
	tap := startTap(tr, http.StatusOK, sseHeader())
	feed(tap, []string{
		messageStartEvent(responseID, responseModel, 1834),
		toolStartEvent(0, "tool_use", "toolu_big", "write_file"),
		argsDeltaEvent(0, `{"content": "`+strings.Repeat("a", 600)+`"}`), // one event over the limit
		blockStopEvent(0),
		messageDeltaEvent("tool_use", 212),
	}, t0)
	info := tap.Result()

	if info.Failed {
		t.Fatal("Failed = true, want false: an oversized event is a limit, not a failure")
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: "toolu_big", name: "write_file", outcome: "delivered", recorded: false, relayedAt: &t0})
	if !equalPtr(info.StopReason, ptr("tool_use")) {
		t.Fatalf("StopReason = %v, want tool_use: events after the skipped one are still read", deref(info.StopReason))
	}
}

func TestTap_ReadLimit_LargeNonStreamedBody(t *testing.T) {
	body := `{"id":"` + responseID + `","type":"message","role":"assistant","model":"` + responseModel + `",` +
		`"content":[{"type":"text","text":"` + strings.Repeat("x", 2000) + `"},` +
		`{"type":"tool_use","id":"toolu_01B2","name":"bash","input":{"command":"ls -la"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1834,"output_tokens":212}}`

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	if gz.Len() >= testReadLimit {
		t.Fatalf("test setup: compressed body is %d bytes, want it under the %d-byte limit", gz.Len(), testReadLimit)
	}

	tests := []struct {
		name     string
		encoding string
		wire     []byte
	}{
		{"plain", "", []byte(body)},
		{"gzip body that inflates past the limit", "gzip", gz.Bytes()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newLimitedTracer(t)
			h := jsonHeader()
			if tt.encoding != "" {
				h.Set("Content-Encoding", tt.encoding)
			}
			tap := startTap(tr, http.StatusOK, h)
			for _, c := range splitEvery(string(tt.wire), 100) {
				tap.Write([]byte(c), t0)
			}
			info := tap.Result()

			if !info.Unread {
				t.Fatal("Unread = false, want true for a non-streamed body over the read limit")
			}
			if info.Failed {
				t.Fatal("Failed = true, want false: hitting the read limit is not a failure")
			}
			if len(info.ToolCalls) != 0 {
				t.Fatalf("ToolCalls = %+v, want none", info.ToolCalls)
			}
			if info.ResponseID != nil || info.ResponseModel != nil || info.StopReason != nil || info.Tokens != nil {
				t.Fatalf("ResponseID/ResponseModel/StopReason/Tokens = %v/%v/%v/%v, want all nil",
					deref(info.ResponseID), deref(info.ResponseModel), deref(info.StopReason), deref(info.Tokens))
			}
		})
	}
}

func TestTap_ReadLimit_NonStreamedBodyUnderTheLimitIsRead(t *testing.T) {
	tr := newLimitedTracer(t)
	body := `{"id":"` + responseID + `","model":"` + responseModel + `","content":[{"type":"tool_use","id":"toolu_01B2","name":"bash","input":{"command":"ls -la"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`
	if len(body) >= testReadLimit {
		t.Fatalf("test setup: body is %d bytes, want it under %d", len(body), testReadLimit)
	}
	tap := startTap(tr, http.StatusOK, jsonHeader())
	tap.Write([]byte(body), t0)
	info := tap.Result()

	if info.Unread {
		t.Fatal("Unread = true, want false for a body under the read limit")
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(info.ToolCalls))
	}
	checkToolCall(t, info.ToolCalls[0], bashCall(&t0))
}
