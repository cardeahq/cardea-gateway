package trace

import (
	"encoding/json"
	"time"
)

// ResponseFormat reads one provider's responses for tracing. The tap owns
// everything format-neutral (encodings, SSE framing, read limits,
// disconnects, timing, fingerprints); a format only reports what the
// provider's bodies say, through a ResponseSink.
type ResponseFormat interface {
	// ReadBody reads a whole non-streamed JSON body. A body it does not
	// recognize, such as an error body, reports nothing.
	ReadBody(body []byte, out *ResponseSink)
	// NewStreamReader returns the reader for one streamed response.
	NewStreamReader() StreamReader
}

// StreamReader reads one streamed response, one SSE event's data at a
// time, and may keep state across its events.
type StreamReader interface {
	Event(data []byte, out *ResponseSink)
}

// ResponseSink receives what a format reads from one response. Tool calls
// are keyed by an index the format chooses, unique among open calls.
type ResponseSink struct {
	tap *ResponseTap
	at  time.Time // when the bytes being read were relayed
}

// SetIdentifiers records the response's ID and model; a nil argument
// leaves the value already recorded.
func (s *ResponseSink) SetIdentifiers(id, model *string) {
	if id != nil {
		s.tap.responseID = id
	}
	if model != nil {
		s.tap.responseModel = model
	}
}

func (s *ResponseSink) SetStopReason(reason string) { s.tap.stopReason = &reason }

// SetTokens replaces the usage recorded so far.
func (s *ResponseSink) SetTokens(tokens Tokens) { s.tap.tokens = &tokens }

// StartToolCall opens a tool call. input is its arguments when no
// AppendToolArguments follows (a non-streamed body, or a stream that
// sends them whole up front).
func (s *ResponseSink) StartToolCall(index int, id, name string, serverSide bool, input json.RawMessage) {
	tc := &toolCallState{
		call:            ToolCall{ToolCallID: id, ToolName: name, ServerSide: serverSide},
		startInput:      input,
		afterDisconnect: s.tap.disconnected,
	}
	s.tap.open[index] = tc
	s.tap.calls = append(s.tap.calls, tc)
}

func (s *ResponseSink) AppendToolArguments(index int, partial string) {
	tc := s.tap.open[index]
	if tc == nil || tc.overLimit || tc.failed {
		return
	}
	if int64(len(tc.args)+len(partial)) > s.tap.maxReadBytes {
		tc.overLimit = true
		tc.args = nil
		return
	}
	tc.args = append(tc.args, partial...)
}

// EndToolCall closes a tool call: it is delivered at the time of the
// bytes being read.
func (s *ResponseSink) EndToolCall(index int) {
	tc := s.tap.open[index]
	if tc == nil {
		return
	}
	delete(s.tap.open, index)
	s.tap.closeToolCall(tc, index, s.at)
}
