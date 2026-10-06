package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ts = Timestamp(time.Date(2026, 9, 27, 14, 3, 11, 402113000, time.UTC))

// minimalRequestRecord has every nullable field nil and every list empty,
// so its encoding shows how "does not apply" is written.
func minimalRequestRecord(callID string) RequestRecord {
	return RequestRecord{
		Schema:      Schema,
		Record:      "request",
		CallID:      callID,
		Session:     Session{Status: "absent"},
		ReceivedAt:  ts,
		Outcome:     RequestOutcome{Result: "forwarded"},
		Stages:      []RequestStage{{Name: "tracing", Decision: "not_run"}},
		ToolResults: []ToolResult{},
		Findings:    []Finding{},
	}
}

func minimalResponseRecord(callID string) ResponseRecord {
	return ResponseRecord{
		Schema:    Schema,
		Record:    "response",
		CallID:    callID,
		Session:   Session{Status: "absent"},
		StartedAt: ts,
		Outcome:   ResponseOutcome{Result: "unanswered", Status: 502},
		Stages:    []ResponseStage{{Name: "tracing", StartedAt: ts, Observation: "completed"}},
		ToolCalls: []ToolCall{{ToolCallID: "toolu_1", ToolName: "bash", Outcome: "not_delivered"}},
		Findings:  []Finding{},
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(b) == 0 {
		return nil
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("sink output does not end with a newline: %q", b)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func newFileSink(t *testing.T, path string) *LogSink {
	t.Helper()
	s, err := NewLogSink(path)
	if err != nil {
		t.Fatalf("NewLogSink(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLogSink_OneLinePerRecordInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s := newFileSink(t, path)

	for _, r := range []Record{minimalRequestRecord("ak_0000000000000001"), minimalResponseRecord("ak_0000000000000001"), minimalRequestRecord("ak_0000000000000002")} {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	lines := readLines(t, path)
	want := []struct{ record, callID string }{
		{"request", "ak_0000000000000001"},
		{"response", "ak_0000000000000001"},
		{"request", "ak_0000000000000002"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not one JSON object: %v: %s", i, err, line)
		}
		if m["record"] != want[i].record || m["call_id"] != want[i].callID {
			t.Fatalf("line %d = %s/%s, want %s/%s", i, m["record"], m["call_id"], want[i].record, want[i].callID)
		}
		if m["schema"] != "agentkeeper.trace/v1" {
			t.Fatalf("line %d schema = %v, want agentkeeper.trace/v1", i, m["schema"])
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(line)); err != nil || compact.String() != line {
			t.Fatalf("line %d is not compact JSON: %s", i, line)
		}
	}
}

func TestLogSink_EveryFieldPresentWithNullAndEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s := newFileSink(t, path)
	if err := s.Write(minimalRequestRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(minimalResponseRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	var req, resp map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &req)
	_ = json.Unmarshal([]byte(lines[1]), &resp)

	assertKeys(t, "request", req, "schema", "record", "call_id", "agent_id", "session", "received_at", "duration_us",
		"model", "stream", "size_bytes", "outcome", "stages", "tool_results", "findings")
	assertKeys(t, "request.session", asObject(t, "session", req["session"]), "id", "status", "source", "source_name")
	assertKeys(t, "request.outcome", asObject(t, "outcome", req["outcome"]), "result", "status", "stage", "reason")
	stage := asObject(t, "request.stages[0]", asArray(t, "request.stages", req["stages"])[0])
	assertKeys(t, "request.stages[0]", stage, "name", "started_at", "duration_us", "decision", "detail")

	for _, k := range []string{"agent_id", "model", "size_bytes"} {
		if v, ok := req[k]; !ok || v != nil {
			t.Fatalf("request.%s = %#v, want null", k, v)
		}
	}
	for _, k := range []string{"started_at", "duration_us", "detail"} {
		if v := stage[k]; v != nil {
			t.Fatalf("not_run stage %s = %#v, want null", k, v)
		}
	}
	if n := len(asArray(t, "request.tool_results", req["tool_results"])); n != 0 {
		t.Fatalf("request.tool_results has %d entries, want []", n)
	}
	if n := len(asArray(t, "request.findings", req["findings"])); n != 0 {
		t.Fatalf("request.findings has %d entries, want []", n)
	}

	assertKeys(t, "response", resp, "schema", "record", "call_id", "agent_id", "session", "started_at", "first_byte_us",
		"duration_us", "outcome", "stages", "streamed", "response_id", "response_model", "stop_reason", "tokens",
		"tool_calls", "findings")
	assertKeys(t, "response.outcome", asObject(t, "outcome", resp["outcome"]), "result", "upstream_status", "status")
	rstage := asObject(t, "response.stages[0]", asArray(t, "response.stages", resp["stages"])[0])
	assertKeys(t, "response.stages[0]", rstage, "name", "started_at", "duration_us", "observation")
	tc := asObject(t, "response.tool_calls[0]", asArray(t, "response.tool_calls", resp["tool_calls"])[0])
	assertKeys(t, "response.tool_calls[0]", tc, "tool_call_id", "tool_name", "server_side", "argument_names",
		"size_bytes", "fingerprint", "relayed_at", "outcome", "recorded")
	for _, k := range []string{"argument_names", "size_bytes", "fingerprint", "relayed_at"} {
		if v := tc[k]; v != nil {
			t.Fatalf("tool_calls[0].%s = %#v, want null", k, v)
		}
	}
	for _, k := range []string{"agent_id", "first_byte_us", "response_id", "response_model", "stop_reason", "tokens"} {
		if v, ok := resp[k]; !ok || v != nil {
			t.Fatalf("response.%s = %#v, want null", k, v)
		}
	}
	if v := asObject(t, "outcome", resp["outcome"])["upstream_status"]; v != nil {
		t.Fatalf("response.outcome.upstream_status = %#v, want null", v)
	}
}

func TestLogSink_TimestampsHaveSixFractionalDigitsAndZ(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s := newFileSink(t, path)
	r := minimalResponseRecord("ak_0000000000000001")
	r.StartedAt = Timestamp(time.Date(2026, 9, 27, 16, 3, 11, 0, time.FixedZone("CEST", 2*60*60)))
	if err := s.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(readLines(t, path)[0]), &m)

	tsPattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)
	if got, _ := m["started_at"].(string); got != "2026-09-27T14:03:11.000000Z" || !tsPattern.MatchString(got) {
		t.Fatalf("started_at = %q, want 2026-09-27T14:03:11.000000Z", got)
	}
}

func TestLogSink_ConcurrentWritersNeverInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s := newFileSink(t, path)

	const writers, perWriter = 16, 50
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				r := minimalResponseRecord("ak_0000000000000001")
				// Make lines long enough that an unsynchronized write would tear.
				r.ToolCalls = make([]ToolCall, 20)
				for i := range r.ToolCalls {
					r.ToolCalls[i] = ToolCall{ToolCallID: strings.Repeat("t", 64), ToolName: "bash", Outcome: "delivered"}
				}
				if err := s.Write(r); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	lines := readLines(t, path)
	if len(lines) != writers*perWriter {
		t.Fatalf("got %d lines, want %d", len(lines), writers*perWriter)
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON (interleaved write?): %.200s", i, line)
		}
	}
}

func TestLogSink_AppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	const existing = `{"schema":"agentkeeper.trace/v1","record":"request","call_id":"ak_old"}` + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o640); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	s := newFileSink(t, path)
	if err := s.Write(minimalRequestRecord("ak_0000000000000002")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (existing + appended)", len(lines))
	}
	if lines[0]+"\n" != existing {
		t.Fatalf("existing line was changed: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"ak_0000000000000002"`) {
		t.Fatalf("appended line = %s, want the new record", lines[1])
	}
}

func TestLogSink_CreatesFileWithoutWorldPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	_ = newFileSink(t, path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("NewLogSink did not create %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm&^0o640 != 0 {
		t.Fatalf("file mode = %o, want at most 0640", perm)
	}
}

func TestNewLogSink_UnwritablePathFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "records.jsonl")
	s, err := NewLogSink(path)
	if err == nil {
		_ = s.Close()
		t.Fatalf("NewLogSink(%q) = nil error, want an error at construction", path)
	}
}

func TestLogSink_EmptyPathWritesToStdoutAndCloseLeavesItOpen(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	s, err := NewLogSink("")
	if err != nil {
		t.Fatalf(`NewLogSink(""): %v`, err)
	}
	if err := s.Write(minimalRequestRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("still-open\n")); err != nil {
		t.Fatalf("stdout was closed by LogSink.Close: %v", err)
	}
	os.Stdout = orig
	_ = w.Close()

	out, _ := io.ReadAll(r)
	sc := bufio.NewScanner(bytes.NewReader(out))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 || lines[1] != "still-open" {
		t.Fatalf("stdout = %q, want one record line followed by our marker", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil || m["call_id"] != "ak_0000000000000001" {
		t.Fatalf("stdout line = %s, want the request record", lines[0])
	}
}
