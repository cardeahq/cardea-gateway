package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
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
// In compressed SSE mode a parser goroutine shares its state, guarded by mu.
type ResponseTap struct {
	key          []byte
	maxReadBytes int64
	format       ResponseFormat // nil: the response is relayed unread
	reader       StreamReader   // set in SSE mode when format is

	mu           sync.Mutex
	mode         tapMode
	status       int
	disconnected bool
	lastWrite    time.Time
	errs         []error

	// encoding is the response's content encoding, "" for identity. In
	// compressed SSE mode bytes are piped to a parser goroutine.
	encoding string
	pw       *io.PipeWriter
	done     chan struct{}

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
	switch enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding"))); {
	case enc == "" || enc == "identity":
	case decoders[enc] != nil:
		t.encoding = enc
	default:
		t.fail(fmt.Errorf("response content encoding %q cannot be read", enc))
		return
	}

	t.open = map[int]*toolCallState{}
	mediaType, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	switch mediaType {
	case "text/event-stream":
		t.mode = modeSSE
		if t.format == nil {
			return
		}
		t.reader = t.format.NewStreamReader()
		if t.encoding != "" {
			t.startDecoder()
		}
	case "application/json":
		if t.format != nil {
			t.mode = modeJSON
		}
	}
}

func (t *ResponseTap) Write(p []byte, relayedAt time.Time) {
	switch t.mode {
	case modeSSE:
		if t.reader == nil {
			return
		}
		if t.encoding != "" {
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
	case modeJSON:
		info.Unread = t.readBody()
	}
	info.ResponseID, info.ResponseModel, info.StopReason, info.Tokens = t.responseID, t.responseModel, t.stopReason, t.tokens
	for _, tc := range t.calls {
		if !tc.closed {
			notDelivered(tc)
		}
		info.ToolCalls = append(info.ToolCalls, tc.call)
	}
	info.Failed = len(t.errs) > 0
	info.Err = errors.Join(t.errs...)
	return info
}

func (t *ResponseTap) fail(err error) {
	t.errs = append(t.errs, err)
}

// --- SSE ------------------------------------------------------------------

func (t *ResponseTap) startDecoder() {
	pr, pw := io.Pipe()
	t.pw = pw
	t.done = make(chan struct{})
	go func() {
		defer close(t.done)
		defer func() {
			if p := recover(); p != nil {
				t.failLocked(fmt.Errorf("%s response parser panicked: %v", t.encoding, p))
				_, _ = io.Copy(io.Discard, pr)
			}
		}()
		zr, err := decoders[t.encoding](pr)
		if err != nil {
			t.failLocked(fmt.Errorf("%s response cannot be read: %w", t.encoding, err))
			_, _ = io.Copy(io.Discard, pr)
			return
		}
		defer func() { _ = zr.Close() }()
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
				t.failLocked(fmt.Errorf("%s response cannot be read: %w", t.encoding, err))
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
		t.readEvent(data, at)
	}
}

// readEvent hands one event to the format's reader. A panic in it fails
// tracing for the call; like an oversized event, it cannot say which tool
// call it was reading, so every open one is failed.
func (t *ResponseTap) readEvent(data []byte, at time.Time) {
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading a stream event panicked: %v", p))
			for _, tc := range t.open {
				tc.failed = true
				tc.args = nil
			}
		}
	}()
	t.reader.Event(data, &ResponseSink{tap: t, at: at})
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

// readBody hands the buffered body to the format and reports whether it
// was past the read limit, and so not read.
func (t *ResponseTap) readBody() (unread bool) {
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading the response body panicked: %v", p))
			t.responseID, t.responseModel, t.stopReason, t.tokens, t.calls = nil, nil, nil, nil, nil
		}
	}()

	if t.bodyOver {
		return true
	}
	body := t.body.Bytes()
	if t.encoding != "" {
		zr, err := decoders[t.encoding](bytes.NewReader(body))
		if err != nil {
			t.fail(fmt.Errorf("%s response cannot be read: %w", t.encoding, err))
			return false
		}
		body, err = io.ReadAll(io.LimitReader(zr, t.maxReadBytes+1))
		_ = zr.Close()
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.fail(fmt.Errorf("%s response cannot be read: %w", t.encoding, err))
			return false
		}
		if int64(len(body)) > t.maxReadBytes {
			return true
		}
	}
	t.format.ReadBody(body, &ResponseSink{tap: t, at: t.lastWrite})
	return false
}
