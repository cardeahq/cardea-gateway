package provider

import (
	"bytes"
	"encoding/json"
)

// anthropicUsage mirrors Anthropic's usage object. Prompt caching splits
// input tokens across three fields (fresh, newly cached, served from
// cache); sentTokens sums all three since all count toward what was
// transmitted/billed for the turn.
type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

func (u anthropicUsage) sentTokens() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// ParseUsage parses an Anthropic /v1/messages response body and returns
// sent (input + cache) and received (output) token counts. It handles both
// a single non-streaming JSON response and an SSE-streamed response
// (stream: true), where input usage arrives on the message_start event and
// the final output_tokens arrives on the message_delta event. ok is false
// if no usage information could be found (e.g. an error response).
func ParseUsage(body []byte) (sent, received int64, ok bool) {
	var payload struct {
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		return payload.Usage.sentTokens(), payload.Usage.OutputTokens, true
	}
	return parseStreamedUsage(body)
}

// parseStreamedUsage scans an SSE response body for the message_start and
// message_delta events Anthropic emits when streaming. message_delta's
// usage.output_tokens is the running total, so the last one seen wins.
func parseStreamedUsage(body []byte) (sent, received int64, ok bool) {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		data, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !found {
			continue
		}

		var event struct {
			Type    string `json:"type"`
			Message struct {
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			sent = event.Message.Usage.sentTokens()
			ok = true
		case "message_delta":
			received = event.Usage.OutputTokens
			ok = true
		}
	}
	return sent, received, ok
}
