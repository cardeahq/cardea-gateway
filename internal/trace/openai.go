package trace

import (
	"encoding/json"
	"fmt"
	"time"
)

// newOpenAIToolResults reads the tool-role messages after the last
// assistant message: each tool result is its own message in this format.
func newOpenAIToolResults(messages []json.RawMessage) ([]toolResultBlock, error) {
	type openAIMessage struct {
		Role       string          `json:"role"`
		ToolCallID json.RawMessage `json:"tool_call_id"`
		Content    json.RawMessage `json:"content"`
	}
	decoded := make([]openAIMessage, len(messages))
	lastAssistant := -1
	for i, raw := range messages {
		if json.Unmarshal(raw, &decoded[i]) != nil {
			return nil, nil
		}
		if decoded[i].Role == "assistant" {
			lastAssistant = i
		}
	}

	var out []toolResultBlock
	for i := lastAssistant + 1; i < len(decoded); i++ {
		m := decoded[i]
		if m.Role != "tool" {
			continue
		}
		var id string
		if json.Unmarshal(m.ToolCallID, &id) != nil || id == "" {
			return nil, fmt.Errorf("message %d: tool message has no tool_call_id string", i)
		}
		out = append(out, toolResultBlock{id: &id, content: m.Content})
	}
	return out, nil
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type openAIFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// openAIChunk is one streamed chunk, or (with message instead of delta) a
// whole non-streamed response.
type openAIChunk struct {
	ID      *string `json:"id"`
	Model   *string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			ToolCalls []struct {
				Index    *int           `json:"index"`
				ID       string         `json:"id"`
				Function openAIFunction `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			ToolCalls []struct {
				ID       string         `json:"id"`
				Function openAIFunction `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

// argumentsString returns the function arguments, which this format sends
// as a JSON string holding JSON.
func argumentsString(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	return []byte(s), true
}

func (t *ResponseTap) handleOpenAIChunk(data []byte, at time.Time) {
	var touched []*toolCallState
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading a response chunk panicked: %v", p))
			for _, tc := range touched {
				tc.failed = true
				tc.args = nil
			}
		}
	}()

	if string(data) == "[DONE]" {
		for _, tc := range t.calls {
			if !tc.closed {
				delete(t.open, tc.key)
				t.closeToolCall(tc, tc.key.index, at)
			}
		}
		return
	}
	var ch openAIChunk
	if json.Unmarshal(data, &ch) != nil {
		return
	}
	if t.responseID == nil {
		t.responseID = ch.ID
	}
	if t.responseModel == nil {
		t.responseModel = ch.Model
	}
	if ch.Usage != nil {
		t.tokens = &Tokens{Sent: ch.Usage.PromptTokens, Received: ch.Usage.CompletionTokens}
	}

	for _, choice := range ch.Choices {
		for i, d := range choice.Delta.ToolCalls {
			key := toolKey{choice: choice.Index, index: i}
			if d.Index != nil {
				key.index = *d.Index
			}
			tc := t.open[key]
			if tc == nil {
				id := d.ID
				tc = &toolCallState{
					key:             key,
					call:            ToolCall{ToolCallID: &id, ToolName: d.Function.Name},
					afterDisconnect: t.disconnected,
				}
				t.open[key] = tc
				t.calls = append(t.calls, tc)
			}
			touched = append(touched, tc)
			if tc.overLimit || tc.failed {
				continue
			}
			fragment, ok := argumentsString(d.Function.Arguments)
			if !ok {
				tc.failed = true
				tc.args = nil
				t.fail(fmt.Errorf("tool call index %d: arguments are not a JSON string", key.index))
				continue
			}
			if int64(len(tc.args)+len(fragment)) > t.maxReadBytes {
				tc.overLimit = true
				tc.args = nil
				continue
			}
			tc.args = append(tc.args, fragment...)
		}
		if choice.FinishReason == nil {
			continue
		}
		if choice.Index == 0 {
			t.stopReason = choice.FinishReason
		}
		for _, tc := range t.calls {
			if !tc.closed && tc.key.choice == choice.Index {
				delete(t.open, tc.key)
				t.closeToolCall(tc, tc.key.index, at)
			}
		}
	}
}

func (t *ResponseTap) parseOpenAIJSON(body []byte, info *ResponseInfo) {
	var resp openAIChunk
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	info.ResponseID, info.ResponseModel = resp.ID, resp.Model
	if resp.Usage != nil {
		info.Tokens = &Tokens{Sent: resp.Usage.PromptTokens, Received: resp.Usage.CompletionTokens}
		if t.operation == "embeddings" {
			info.Tokens.Received = 0
		}
	}
	if len(resp.Choices) > 0 {
		info.StopReason = resp.Choices[0].FinishReason
	}
	for _, choice := range resp.Choices {
		for i, call := range choice.Message.ToolCalls {
			id := call.ID
			tc := ToolCall{ToolCallID: &id, ToolName: call.Function.Name}
			if t.disconnected {
				tc.Outcome = "not_delivered"
				info.ToolCalls = append(info.ToolCalls, tc)
				continue
			}
			ts := Timestamp(t.lastWrite)
			tc.RelayedAt = &ts
			tc.Outcome = "delivered"
			args, ok := argumentsString(call.Function.Arguments)
			if !ok {
				t.fail(fmt.Errorf("choice %d tool call %d: arguments are not a JSON string", choice.Index, i))
			} else if err := t.recordArguments(&tc, args); err != nil {
				t.fail(fmt.Errorf("choice %d tool call %d: arguments are %w", choice.Index, i, err))
			}
			info.ToolCalls = append(info.ToolCalls, tc)
		}
	}
}
