package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// newGeminiToolResults reads the function responses of the last contents
// entry, when it comes from the user. Keys are matched exactly: Gemini
// accepts both the camelCase and snake_case spelling, and a part carrying
// both could be read differently by the Gateway and by Gemini.
func newGeminiToolResults(body []byte) ([]toolResultBlock, error) {
	var req struct {
		Contents []json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Contents) == 0 {
		return nil, nil
	}
	var last struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	if json.Unmarshal(req.Contents[len(req.Contents)-1], &last) != nil {
		return nil, nil
	}
	if last.Role != "user" && last.Role != "" {
		return nil, nil
	}

	var out []toolResultBlock
	for i, raw := range last.Parts {
		var part map[string]json.RawMessage
		if json.Unmarshal(raw, &part) != nil {
			continue
		}
		camel, hasCamel := part["functionResponse"]
		snake, hasSnake := part["function_response"]
		if hasCamel && hasSnake {
			return nil, fmt.Errorf("last contents entry part %d: both functionResponse and function_response are set", i)
		}
		fr := camel
		if hasSnake {
			fr = snake
		}
		if !hasCamel && !hasSnake {
			continue
		}
		var resp map[string]json.RawMessage
		if json.Unmarshal(fr, &resp) != nil {
			return nil, fmt.Errorf("last contents entry part %d: function response cannot be read", i)
		}
		res := toolResultBlock{content: resp["response"]}
		if rawID, ok := resp["id"]; ok {
			var id string
			if json.Unmarshal(rawID, &id) != nil {
				return nil, fmt.Errorf("last contents entry part %d: function response id is not a string", i)
			}
			if id != "" {
				res.id = &id
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// geminiResponse is the part of a GenerateContentResponse tracing reads.
type geminiResponse struct {
	ResponseID   *string `json:"responseId"`
	ModelVersion *string `json:"modelVersion"`
	Candidates   []struct {
		Content struct {
			Parts []struct {
				FunctionCall *struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason *string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

// handleGeminiResponse reads one GenerateContentResponse. Gemini never
// splits a function call's arguments, so each one opens and closes here.
func (t *ResponseTap) handleGeminiResponse(data []byte, at time.Time) {
	defer func() {
		if p := recover(); p != nil {
			t.fail(fmt.Errorf("reading a response panicked: %v", p))
		}
	}()

	var resp geminiResponse
	if json.Unmarshal(data, &resp) != nil {
		return
	}
	if resp.ResponseID != nil {
		t.responseID = resp.ResponseID
	}
	if resp.ModelVersion != nil {
		t.responseModel = resp.ModelVersion
	}
	if u := resp.UsageMetadata; u != nil {
		t.tokens = &Tokens{Sent: u.PromptTokenCount, Received: u.CandidatesTokenCount + u.ThoughtsTokenCount}
	}
	if len(resp.Candidates) == 0 {
		return
	}
	cand := resp.Candidates[0]
	if cand.FinishReason != nil {
		t.stopReason = cand.FinishReason
	}
	for i, part := range cand.Content.Parts {
		fc := part.FunctionCall
		if fc == nil {
			continue
		}
		tc := &toolCallState{call: ToolCall{ToolName: fc.Name}, afterDisconnect: t.disconnected}
		if fc.ID != "" {
			id := fc.ID
			tc.call.ToolCallID = &id
		}
		tc.args = fc.Args
		if len(bytes.TrimSpace(tc.args)) == 0 {
			// A function with no parameters may come without args.
			tc.args = []byte("{}")
		}
		t.calls = append(t.calls, tc)
		t.closeToolCall(tc, i, at)
	}
}

// parseGeminiJSON reads a generateContent body, or the JSON array of
// responses streamGenerateContent sends without alt=sse.
func (t *ResponseTap) parseGeminiJSON(body []byte, info *ResponseInfo) {
	responses := []json.RawMessage{body}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		if json.Unmarshal(body, &responses) != nil {
			return
		}
	}
	for _, r := range responses {
		t.handleGeminiResponse(r, t.lastWrite)
	}
	info.ResponseID, info.ResponseModel, info.StopReason, info.Tokens = t.responseID, t.responseModel, t.stopReason, t.tokens
	for _, tc := range t.calls {
		info.ToolCalls = append(info.ToolCalls, tc.call)
	}
}
