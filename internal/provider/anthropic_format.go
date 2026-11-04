package provider

import (
	"encoding/json"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// anthropicFormat reads Anthropic Messages responses for tracing.
type anthropicFormat struct{}

// messagesResponse is the part of a non-streamed Messages response tracing
// reads.
type messagesResponse struct {
	ID         *string `json:"id"`
	Model      *string `json:"model"`
	StopReason *string `json:"stop_reason"`
	Usage      *usage  `json:"usage"`
	Content    []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
}

// usage mirrors Anthropic's usage object; sent counts fresh and cached
// input alike, since all of it was transmitted and billed.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

func (u usage) sent() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// streamEvent is the union of the streamed event shapes tracing reads.
type streamEvent struct {
	Type    string `json:"type"`
	Index   *int   `json:"index"`
	Message struct {
		ID    *string `json:"id"`
		Model *string `json:"model"`
		Usage *usage  `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string  `json:"type"`
		PartialJSON string  `json:"partial_json"`
		StopReason  *string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
}

func isToolUse(blockType string) bool {
	return blockType == "tool_use" || blockType == "server_tool_use"
}

func (anthropicFormat) ReadBody(body []byte, out *trace.ResponseSink) {
	var resp messagesResponse
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	out.SetIdentifiers(resp.ID, resp.Model)
	if resp.StopReason != nil {
		out.SetStopReason(*resp.StopReason)
	}
	if resp.Usage != nil {
		out.SetTokens(trace.Tokens{Sent: resp.Usage.sent(), Received: resp.Usage.OutputTokens})
	}
	for i, block := range resp.Content {
		if !isToolUse(block.Type) {
			continue
		}
		out.StartToolCall(i, block.ID, block.Name, block.Type == "server_tool_use", block.Input)
		out.EndToolCall(i)
	}
}

func (anthropicFormat) NewStreamReader() trace.StreamReader { return &anthropicStream{} }

// anthropicStream keeps the usage message_start reported, since
// message_delta updates only the output count.
type anthropicStream struct {
	tokens trace.Tokens
}

func (s *anthropicStream) Event(data []byte, out *trace.ResponseSink) {
	var ev streamEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		out.SetIdentifiers(ev.Message.ID, ev.Message.Model)
		if u := ev.Message.Usage; u != nil {
			s.tokens = trace.Tokens{Sent: u.sent(), Received: u.OutputTokens}
			out.SetTokens(s.tokens)
		}
	case "content_block_start":
		if ev.Index != nil && isToolUse(ev.ContentBlock.Type) {
			out.StartToolCall(*ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name, ev.ContentBlock.Type == "server_tool_use", ev.ContentBlock.Input)
		}
	case "content_block_delta":
		if ev.Index != nil && ev.Delta.Type == "input_json_delta" {
			out.AppendToolArguments(*ev.Index, ev.Delta.PartialJSON)
		}
	case "content_block_stop":
		if ev.Index != nil {
			out.EndToolCall(*ev.Index)
		}
	case "message_delta":
		if ev.Delta.StopReason != nil {
			out.SetStopReason(*ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			s.tokens.Received = ev.Usage.OutputTokens
			out.SetTokens(s.tokens)
		}
	}
}
