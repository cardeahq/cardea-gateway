package trace

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Provider formats are tested in internal/provider; these tests cover what
// the tap does whatever the format.

var t0 = time.Date(2026, 9, 27, 14, 3, 11, 0, time.UTC)

// lineFormat is a toy format: a body or event is one command per line,
// "id X", "start N ID NAME ARGS", "args N PARTIAL", "end N", "panic".
type lineFormat struct{}

func (lineFormat) ReadBody(body []byte, out *ResponseSink) { lineEvent(body, out) }

func (lineFormat) NewStreamReader() StreamReader { return lineReader{} }

type lineReader struct{}

func (lineReader) Event(data []byte, out *ResponseSink) { lineEvent(data, out) }

func lineEvent(data []byte, out *ResponseSink) {
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		index := func() int { return int(f[1][0] - '0') }
		switch f[0] {
		case "id":
			out.SetIdentifiers(&f[1], nil)
		case "start":
			out.StartToolCall(index(), f[2], f[3], false, json.RawMessage(f[4]))
		case "args":
			out.AppendToolArguments(index(), f[2])
		case "end":
			out.EndToolCall(index())
		case "panic":
			panic("boom")
		}
	}
}

func sse(data string) string { return "data: " + data + "\n\n" }

func TestTap_NilFormatRelaysUnread(t *testing.T) {
	tr := newTestTracer(t, nil)
	for _, ct := range []string{"text/event-stream", "application/json"} {
		tap := tr.NewResponseTap(nil)
		tap.Start(http.StatusOK, http.Header{"Content-Type": {ct}})
		tap.Write([]byte(sse("id R")), t0)
		info := tap.Result()

		if info.Streamed != (ct == "text/event-stream") {
			t.Fatalf("%s: Streamed = %v", ct, info.Streamed)
		}
		if info.ResponseID != nil || info.Failed || info.Unread || len(info.ToolCalls) != 0 {
			t.Fatalf("%s: info = %+v, want nothing read and no failure", ct, info)
		}
	}
}

func TestTap_FormatReportsThroughTheSink(t *testing.T) {
	tr := newTestTracer(t, nil)
	t1 := t0.Add(time.Second)

	streamed := tr.NewResponseTap(lineFormat{})
	streamed.Start(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}})
	streamed.Write([]byte(sse("id R")+sse("start 0 call_1 bash {}")+sse(`args 0 {"a":1}`)), t0)
	streamed.Write([]byte(sse("end 0")+sse("start 1 call_2 never-ends {}")), t1)
	s := streamed.Result()

	if !s.Streamed || s.ResponseID == nil || *s.ResponseID != "R" || len(s.ToolCalls) != 2 {
		t.Fatalf("streamed info = %+v, want R with two tool calls", s)
	}
	if tc := s.ToolCalls[0]; tc.Outcome != "delivered" || !tc.Recorded || tc.RelayedAt == nil || !time.Time(*tc.RelayedAt).Equal(t1) {
		t.Fatalf("closed tool call = %+v, want delivered and recorded at the write carrying its end", tc)
	}
	if tc := s.ToolCalls[1]; tc.Outcome != "not_delivered" {
		t.Fatalf("unclosed tool call outcome = %q, want not_delivered", tc.Outcome)
	}

	body := tr.NewResponseTap(lineFormat{})
	body.Start(http.StatusOK, http.Header{"Content-Type": {"application/json"}})
	body.Write([]byte("id R\nstart 0 call_1 "), t0)
	body.Write([]byte("bash {\"a\":1}\nend 0\n"), t1)
	b := body.Result()

	if b.Streamed || len(b.ToolCalls) != 1 {
		t.Fatalf("body info = %+v, want one tool call, not streamed", b)
	}
	if tc := b.ToolCalls[0]; !tc.Recorded || *tc.Fingerprint != *s.ToolCalls[0].Fingerprint || !time.Time(*tc.RelayedAt).Equal(t1) {
		t.Fatalf("body tool call = %+v, want the streamed fingerprint, relayed at the last write", tc)
	}
}

func TestTap_ReaderPanicFailsTracingAndTheOpenCall(t *testing.T) {
	tr := newTestTracer(t, nil)
	tap := tr.NewResponseTap(lineFormat{})
	tap.Start(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}})
	feedAll := sse("start 0 call_1 bash {}") + sse("panic") + sse(`args 0 {"a":1}`) + sse("end 0") + sse("id R")
	tap.Write([]byte(feedAll), t0)
	info := tap.Result()

	if !info.Failed || info.Err == nil {
		t.Fatal("Failed = false, want true after the reader panicked")
	}
	if len(info.ToolCalls) != 1 || info.ToolCalls[0].Outcome != "delivered" || info.ToolCalls[0].Recorded {
		t.Fatalf("tool calls = %+v, want call_1 delivered but not recorded", info.ToolCalls)
	}
	if info.ResponseID == nil || *info.ResponseID != "R" {
		t.Fatal("events after the panic were not read")
	}
}
