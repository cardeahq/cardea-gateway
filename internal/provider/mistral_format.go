package provider

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// mistralFormat reads Mistral's chat completions responses, which follow
// the OpenAI-compatible shape, for tracing. Only the first choice is read:
// it is the one an agent acts on.
type mistralFormat struct{}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

func (u chatUsage) tokens() trace.Tokens {
	return trace.Tokens{Sent: u.PromptTokens, Received: u.CompletionTokens}
}

type chatToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON string holding the arguments' JSON text;
		// Mistral may also send the object itself.
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// arguments returns the JSON text of the call's arguments.
func (c chatToolCall) arguments() []byte {
	var s string
	if json.Unmarshal(c.Function.Arguments, &s) == nil {
		return []byte(s)
	}
	return c.Function.Arguments
}

type chatCompletion struct {
	ID      *string `json:"id"`
	Model   *string `json:"model"`
	Choices []struct {
		Message struct {
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func (mistralFormat) ReadBody(body []byte, out *trace.ResponseSink) {
	var resp chatCompletion
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	out.SetIdentifiers(resp.ID, resp.Model)
	if resp.Usage != nil {
		out.SetTokens(resp.Usage.tokens())
	}
	if len(resp.Choices) == 0 {
		return
	}
	choice := resp.Choices[0]
	if choice.FinishReason != nil {
		out.SetStopReason(*choice.FinishReason)
	}
	for i, tc := range choice.Message.ToolCalls {
		out.StartToolCall(i, tc.ID, tc.Function.Name, false, tc.arguments())
		out.EndToolCall(i)
	}
}

func (mistralFormat) NewStreamReader() trace.StreamReader {
	return &mistralStream{open: map[int]bool{}}
}

// mistralStream tracks the tool calls a stream has opened: chunks carry
// no event that closes one, so they all close when finish_reason arrives.
type mistralStream struct {
	open map[int]bool
}

type chatChunk struct {
	ID      *string `json:"id"`
	Model   *string `json:"model"`
	Choices []struct {
		Delta struct {
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func (s *mistralStream) Event(data []byte, out *trace.ResponseSink) {
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return // includes the closing "[DONE]"
	}
	out.SetIdentifiers(chunk.ID, chunk.Model)
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		for pos, tc := range choice.Delta.ToolCalls {
			index := pos
			if tc.Index != nil {
				index = *tc.Index
			}
			if !s.open[index] {
				if tc.ID == "" {
					continue // a fragment of a call whose start was never seen
				}
				s.open[index] = true
				out.StartToolCall(index, tc.ID, tc.Function.Name, false, nil)
			}
			if args := tc.arguments(); len(args) > 0 {
				out.AppendToolArguments(index, string(args))
			}
		}
		if choice.FinishReason != nil {
			out.SetStopReason(*choice.FinishReason)
			for _, index := range slices.Sorted(maps.Keys(s.open)) {
				out.EndToolCall(index)
			}
			clear(s.open)
		}
	}
	if chunk.Usage != nil {
		out.SetTokens(chunk.Usage.tokens())
	}
}
