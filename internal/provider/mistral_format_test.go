package provider

import (
	"net/http"
	"testing"
	"time"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

func readMistral(t *testing.T, contentType string, chunks ...string) trace.ResponseInfo {
	t.Helper()
	tap := newTestTracer(t, nil).NewResponseTap(mistralFormat{})
	tap.Start(http.StatusOK, http.Header{"Content-Type": {contentType}})
	for i, c := range chunks {
		tap.Write([]byte(c), t0.Add(time.Duration(i)*time.Second))
	}
	return tap.Result()
}

func chunk(data string) string { return "data: " + data + "\n\n" }

const mistralCanon = `{"command":"ls -la"}`

func checkMistralInfo(t *testing.T, info trace.ResponseInfo, stopReason string) {
	t.Helper()
	if !equalPtr(info.ResponseID, ptr("cmpl-1")) || !equalPtr(info.ResponseModel, ptr("mistral-large-latest")) {
		t.Fatalf("ResponseID/ResponseModel = %v/%v, want cmpl-1/mistral-large-latest", deref(info.ResponseID), deref(info.ResponseModel))
	}
	if !equalPtr(info.StopReason, ptr(stopReason)) {
		t.Fatalf("StopReason = %v, want %s", deref(info.StopReason), stopReason)
	}
	if info.Tokens == nil || *info.Tokens != (trace.Tokens{Sent: 11, Received: 7}) {
		t.Fatalf("Tokens = %v, want {Sent:11 Received:7}", deref(info.Tokens))
	}
	if info.Failed || info.Unread {
		t.Fatalf("Failed = %v (%v), Unread = %v; want both false", info.Failed, info.Err, info.Unread)
	}
}

func TestMistralFormat_Body(t *testing.T) {
	info := readMistral(t, "application/json",
		`{"id":"cmpl-1","object":"chat.completion","model":"mistral-large-latest",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls -la\"}"}}]},`+
			`"finish_reason":"tool_calls"}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)

	checkMistralInfo(t, info, "tool_calls")
	if len(info.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want one", info.ToolCalls)
	}
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: "call_1", name: "bash", canonical: mistralCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: &t0})
}

func TestMistralFormat_BodyWithObjectArguments(t *testing.T) {
	info := readMistral(t, "application/json",
		`{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"name":"bash","arguments":{"command":"ls -la"}}}]},"finish_reason":"tool_calls"}]}`)

	if len(info.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want one", info.ToolCalls)
	}
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: "call_1", name: "bash", canonical: mistralCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: &t0})
}

func TestMistralFormat_Stream(t *testing.T) {
	info := readMistral(t, "text/event-stream",
		chunk(`{"id":"cmpl-1","model":"mistral-large-latest","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check."},"finish_reason":null}]}`),
		chunk(`{"id":"cmpl-1","model":"mistral-large-latest","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\": "}}]},"finish_reason":null}]}`),
		chunk(`{"id":"cmpl-1","model":"mistral-large-latest","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"finish_reason":null}]}`),
		chunk(`{"id":"cmpl-1","model":"mistral-large-latest","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		chunk(`[DONE]`),
	)

	checkMistralInfo(t, info, "tool_calls")
	if !info.Streamed || len(info.ToolCalls) != 1 {
		t.Fatalf("Streamed = %v, ToolCalls = %+v; want streamed with one tool call", info.Streamed, info.ToolCalls)
	}
	finishedAt := t0.Add(3 * time.Second)
	checkToolCall(t, info.ToolCalls[0], wantToolCall{id: "call_1", name: "bash", canonical: mistralCanon, names: []string{"command"}, outcome: "delivered", recorded: true, relayedAt: &finishedAt})
}

func TestMistralFormat_StreamEndingBeforeFinishLeavesTheCallUndelivered(t *testing.T) {
	info := readMistral(t, "text/event-stream",
		chunk(`{"id":"cmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":null}]}`),
	)

	if len(info.ToolCalls) != 1 || info.ToolCalls[0].Outcome != "not_delivered" {
		t.Fatalf("ToolCalls = %+v, want call_1 not_delivered", info.ToolCalls)
	}
}

func TestMistralFormat_NoUsageLeavesTokensNil(t *testing.T) {
	for _, tt := range []struct{ contentType, body string }{
		{"application/json", `{"id":"cmpl-1","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`},
		{"text/event-stream", chunk(`{"id":"cmpl-1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`) + chunk(`[DONE]`)},
	} {
		info := readMistral(t, tt.contentType, tt.body)
		if info.Tokens != nil {
			t.Fatalf("%s: Tokens = %v, want nil", tt.contentType, *info.Tokens)
		}
		if !equalPtr(info.StopReason, ptr("stop")) || info.Failed {
			t.Fatalf("%s: StopReason = %v, Failed = %v; want stop, false", tt.contentType, deref(info.StopReason), info.Failed)
		}
	}
}

func TestMistralFormat_ErrorBodyReportsNothing(t *testing.T) {
	info := readMistral(t, "application/json", `{"object":"error","message":"Unauthorized","type":"invalid_request_error","code":null}`)

	if info.ResponseID != nil || info.StopReason != nil || info.Tokens != nil || len(info.ToolCalls) != 0 || info.Failed {
		t.Fatalf("info = %+v, want nothing recorded and no failure", info)
	}
}
