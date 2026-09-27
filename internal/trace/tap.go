package trace

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

type tapMode int

const (
	modeNone tapMode = iota
	modeSSE
	modeJSON
)

// sseLineSlack lets a line carry its "data: " prefix on top of a payload
// of exactly the read limit.
const sseLineSlack = 64

// ResponseTap reads one call's response as it is relayed. Not safe for
// concurrent use; the pipeline calls it from the relaying goroutine only.
// In gzip SSE mode a parser goroutine shares its state, guarded by mu.
type ResponseTap struct {
	key          []byte
	maxReadBytes int64

	mu           sync.Mutex
	mode         tapMode
	status       int
	disconnected bool
	lastWrite    time.Time
	errs         []error

	// Gzip SSE: bytes are piped to a parser goroutine.
	gzip bool
	pw   *io.PipeWriter
	done chan struct{}

	// SSE parsing.
	line      []byte
	lineOver  bool
	data      []byte
	eventOver bool
	calls     []*toolCallState
	open      map[int]*toolCallState

	responseID    *string
	responseModel *string
	stopReason    *string
	tokens        *Tokens

	// JSON mode.
	body     bytes.Buffer
	bodyOver bool
}

type toolCallState struct {
	call            ToolCall
	args            []byte
	startInput      json.RawMessage
	overLimit       bool
	failed          bool
	afterDisconnect bool
	closed          bool
}

// ResponseInfo is what tracing contributes to a response record.
type ResponseInfo struct {
	Streamed      bool
	ResponseID    *string
	ResponseModel *string
	StopReason    *string
	Tokens        *Tokens
	ToolCalls     []ToolCall
	Failed        bool // tracing's observation is "failed"; hitting the read limit does not set it
	Unread        bool // non-streamed body past the read limit: finding "tool_calls_unread"
	// Err says why Failed is set. It never contains response content.
	Err error
}

func (t *ResponseTap) Start(status int, h http.Header) {
	t.status = status
	switch enc := h.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		t.gzip = true
	default:
		t.fail(fmt.Errorf("response content encoding %q cannot be read", enc))
		return
	}

	mediaType, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	switch mediaType {
	case "text/event-stream":
		t.mode = modeSSE
		t.open = map[int]*toolCallState{}
		if t.gzip {
			t.startGzipParser()
		}
	case "application/json":
		t.mode = modeJSON
	}
}

func (t *ResponseTap) Write(p []byte, relayedAt time.Time) {
	switch t.mode {
	case modeSSE:
		if t.gzip {
			_, _ = t.pw.Write(p)
			return
		}
		t.feedSSE(p, relayedAt)
	case modeJSON:
		t.lastWrite = relayedAt
		if t.bodyOver {
			return
		}
		if int64(t.body.Len()+len(p)) > t.maxReadBytes {
			t.bodyOver = true
			t.body = bytes.Buffer{}
			return
		}
		t.body.Write(p)
	}
}

func (t *ResponseTap) Disconnected() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.disconnected = true
}

// Result closes the tap and must be called exactly once, after the relay
// has ended.
func (t *ResponseTap) Result() ResponseInfo {
	if t.pw != nil {
		_ = t.pw.Close()
		<-t.done
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	info := ResponseInfo{ToolCalls: []ToolCall{}}
	switch t.mode {
	case modeSSE:
		info.Streamed = true
		info.ResponseID, info.ResponseModel, info.StopReason, info.Tokens = t.responseID, t.responseModel, t.stopReason, t.tokens
		for _, tc := range t.calls {
			if !tc.closed {
				notDelivered(tc)
			}
			info.ToolCalls = append(info.ToolCalls, tc.call)
		}
	case modeJSON:
		t.parseJSON(&info)
	}
	info.Failed = len(t.errs) > 0
	info.Err = errors.Join(t.errs...)
	return info
}

func (t *ResponseTap) fail(err error) {
	t.errs = append(t.errs, err)
}

// --- SSE ------------------------------------------------------------------

func (t *ResponseTap) startGzipParser() {
	pr, pw := io.Pipe()
	t.pw = pw
	t.done = make(chan struct{})
	go func() {
		defer close(t.done)
		defer func() {
			if p := recover(); p != nil {
				t.failLocked(fmt.Errorf("gzip response parser panicked: %v", p))
				_, _ = io.Copy(io.Discard, pr)
			}
		}()
		zr, err := gzip.NewReader(pr)
		if err != nil {
			t.failLocked(fmt.Errorf("gzip response cannot be read: %w", err))
			_, _ = io.Copy(io.Discard, pr)
			return
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := zr.Read(buf)
			if n > 0 {
				t.feedSSE(buf[:n], time.Now())
			}
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			if err != nil {
				t.failLocked(fmt.Errorf("gzip response cannot be read: %w", err))
				_, _ = io.Copy(io.Discard, pr)
				return
			}
		}
	}()
}

func (t *ResponseTap) failLocked(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fail(err)
}

// feedSSE splits p into lines and dispatches each event when the blank
// line ending it arrives.
func (t *ResponseTap) feedSSE(p []byte, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !t.lineOver {
			if int64(len(t.line)+len(chunk)) > t.maxReadBytes+sseLineSlack {
				t.lineOver = true
				t.line = nil
			} else {
				t.line = append(t.line, chunk...)
			}
		}
		if i < 0 {
			return
		}
		p = p[i+1:]
		t.endLine(at)
	}
}

func (t *ResponseTap) endLine(at time.Time) {
	line := bytes.TrimSuffix(t.line, []byte("\r"))
	over := t.lineOver
	t.line, t.lineOver = t.line[:0], false

	if over {
		t.eventOver = true
		return
	}
	if len(line) == 0 {
		t.endEvent(at)
		return
	}
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok || t.eventOver {
		return
	}
	payload = bytes.TrimPrefix(payload, []byte(" "))
	if len(t.data) > 0 {
		t.data = append(t.data, '\n')
	}
	if int64(len(t.data)+len(payload)) > t.maxReadBytes {
		t.eventOver = true
		t.data = t.data[:0]
		return
	}
	t.data = append(t.data, payload...)
}

func (t *ResponseTap) endEvent(at time.Time) {
	data, over := t.data, t.eventOver
	t.data, t.eventOver = t.data[:0], false

	if over {
		// The skipped event cannot be read to find its block; content
		// blocks do not interleave, so it belongs to whichever tool call
		// is open.
		for _, tc := range t.open {
			tc.overLimit = true
			tc.args = nil
		}
		return
	}
	if len(data) > 0 {
		t.handleEvent(data, at)
	}
}

func (t *ResponseTap) handleEvent(data []byte, at time.Time) {
	var ev streamEvent
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading %s event panicked: %v", ev.Type, p))
			if ev.Index != nil {
				if tc := t.open[*ev.Index]; tc != nil {
					tc.failed = true
					tc.args = nil
				}
			}
		}
	}()
	if json.Unmarshal(data, &ev) != nil {
		return
	}

	switch ev.Type {
	case "message_start":
		t.responseID, t.responseModel = ev.Message.ID, ev.Message.Model
		if u := ev.Message.Usage; u != nil {
			t.tokens = &Tokens{Sent: u.sent(), Received: u.OutputTokens}
		}
	case "content_block_start":
		if ev.Index == nil || !isToolUse(ev.ContentBlock.Type) {
			return
		}
		tc := &toolCallState{
			call: ToolCall{
				ToolCallID: ev.ContentBlock.ID,
				ToolName:   ev.ContentBlock.Name,
				ServerSide: ev.ContentBlock.Type == "server_tool_use",
			},
			startInput:      ev.ContentBlock.Input,
			afterDisconnect: t.disconnected,
		}
		t.open[*ev.Index] = tc
		t.calls = append(t.calls, tc)
	case "content_block_delta":
		if ev.Index == nil || ev.Delta.Type != "input_json_delta" {
			return
		}
		tc := t.open[*ev.Index]
		if tc == nil || tc.overLimit || tc.failed {
			return
		}
		if int64(len(tc.args)+len(ev.Delta.PartialJSON)) > t.maxReadBytes {
			tc.overLimit = true
			tc.args = nil
			return
		}
		tc.args = append(tc.args, ev.Delta.PartialJSON...)
	case "content_block_stop":
		if ev.Index == nil {
			return
		}
		tc := t.open[*ev.Index]
		if tc == nil {
			return
		}
		delete(t.open, *ev.Index)
		t.closeToolCall(tc, *ev.Index, at)
	case "message_delta":
		if ev.Delta.StopReason != nil {
			t.stopReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			if t.tokens == nil {
				t.tokens = &Tokens{}
			}
			t.tokens.Received = ev.Usage.OutputTokens
		}
	}
}

func (t *ResponseTap) closeToolCall(tc *toolCallState, index int, at time.Time) {
	tc.closed = true
	if tc.afterDisconnect {
		notDelivered(tc)
		return
	}
	ts := Timestamp(at)
	tc.call.RelayedAt = &ts
	tc.call.Outcome = "delivered"
	if tc.overLimit || tc.failed {
		return
	}
	args := tc.args
	if len(bytes.TrimSpace(args)) == 0 {
		args = tc.startInput
	}
	if err := t.recordArguments(&tc.call, args); err != nil {
		t.fail(fmt.Errorf("tool call index %d: arguments are %w", index, err))
	}
}

func notDelivered(tc *toolCallState) {
	tc.call.Outcome = "not_delivered"
	tc.call.RelayedAt = nil
	tc.call.ArgumentNames, tc.call.SizeBytes, tc.call.Fingerprint = nil, nil, nil
	tc.call.Recorded = false
}

func (t *ResponseTap) recordArguments(tc *ToolCall, args []byte) error {
	canonical, err := canonicalJSON(args)
	if err != nil {
		return err
	}
	size := int64(len(canonical))
	fp := fingerprint(t.key, canonical)
	tc.ArgumentNames = argumentNames(canonical)
	tc.SizeBytes = &size
	tc.Fingerprint = &fp
	tc.Recorded = true
	return nil
}

// --- JSON -----------------------------------------------------------------

func (t *ResponseTap) parseJSON(info *ResponseInfo) {
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading the response body panicked: %v", p))
			*info = ResponseInfo{ToolCalls: []ToolCall{}}
		}
	}()

	if t.bodyOver {
		info.Unread = true
		return
	}
	body := t.body.Bytes()
	if t.gzip {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.fail(fmt.Errorf("gzip response cannot be read: %w", err))
			return
		}
		body, err = io.ReadAll(io.LimitReader(zr, t.maxReadBytes+1))
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.fail(fmt.Errorf("gzip response cannot be read: %w", err))
			return
		}
		if int64(len(body)) > t.maxReadBytes {
			info.Unread = true
			return
		}
	}

	var resp messagesResponse
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	info.ResponseID, info.ResponseModel, info.StopReason = resp.ID, resp.Model, resp.StopReason
	if resp.Usage != nil {
		info.Tokens = &Tokens{Sent: resp.Usage.sent(), Received: resp.Usage.OutputTokens}
	}
	for i, block := range resp.Content {
		if !isToolUse(block.Type) {
			continue
		}
		tc := ToolCall{ToolCallID: block.ID, ToolName: block.Name, ServerSide: block.Type == "server_tool_use"}
		if t.disconnected {
			tc.Outcome = "not_delivered"
		} else {
			ts := Timestamp(t.lastWrite)
			tc.RelayedAt = &ts
			tc.Outcome = "delivered"
			if err := t.recordArguments(&tc, block.Input); err != nil {
				t.fail(fmt.Errorf("content block %d: tool call arguments are %w", i, err))
			}
		}
		info.ToolCalls = append(info.ToolCalls, tc)
	}
}
