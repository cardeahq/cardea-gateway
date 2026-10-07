// Package trace builds the Gateway's call-trace records from the requests
// and responses it relays, and hands them to a sink.
package trace

import "time"

const Schema = "cardea-gateway.trace/v1"

// Record is a RequestRecord or a ResponseRecord.
type Record interface{ recordType() string }

// Timestamp marshals as RFC 3339 UTC with exactly six fractional digits.
type Timestamp time.Time

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format("2006-01-02T15:04:05.000000Z") + `"`), nil
}

type RequestRecord struct {
	Schema      string         `json:"schema"`
	Record      string         `json:"record"` // "request"
	CallID      string         `json:"call_id"`
	AgentID     *string        `json:"agent_id"`
	Session     Session        `json:"session"`
	ReceivedAt  Timestamp      `json:"received_at"`
	DurationUS  int64          `json:"duration_us"`
	Model       *string        `json:"model"`
	Stream      bool           `json:"stream"`
	SizeBytes   *int64         `json:"size_bytes"` // nil when the body was not read in full (400, 413)
	Outcome     RequestOutcome `json:"outcome"`
	Stages      []RequestStage `json:"stages"`
	ToolResults []ToolResult   `json:"tool_results"`
	Findings    []Finding      `json:"findings"`
}

func (RequestRecord) recordType() string { return "request" }

type ResponseRecord struct {
	Schema        string          `json:"schema"`
	Record        string          `json:"record"` // "response"
	CallID        string          `json:"call_id"`
	AgentID       *string         `json:"agent_id"`
	Session       Session         `json:"session"`
	StartedAt     Timestamp       `json:"started_at"`
	FirstByteUS   *int64          `json:"first_byte_us"`
	DurationUS    int64           `json:"duration_us"`
	Outcome       ResponseOutcome `json:"outcome"`
	Stages        []ResponseStage `json:"stages"`
	Streamed      bool            `json:"streamed"`
	ResponseID    *string         `json:"response_id"`
	ResponseModel *string         `json:"response_model"`
	StopReason    *string         `json:"stop_reason"`
	Tokens        *Tokens         `json:"tokens"`
	ToolCalls     []ToolCall      `json:"tool_calls"`
	Findings      []Finding       `json:"findings"`
}

func (ResponseRecord) recordType() string { return "response" }

// Session.Status is one of "claimed", "invalid", "absent"; Source one of
// "header", "body_field", "baggage", or nil when absent.
type Session struct {
	ID         *string `json:"id"`
	Status     string  `json:"status"`
	Source     *string `json:"source"`
	SourceName *string `json:"source_name"`
}

// Result is one of "forwarded", "denied", "failed".
type RequestOutcome struct {
	Result string  `json:"result"`
	Status *int    `json:"status"`
	Stage  *string `json:"stage"`
	Reason *string `json:"reason"`
}

// Result is one of "delivered", "unanswered".
type ResponseOutcome struct {
	Result         string `json:"result"`
	UpstreamStatus *int   `json:"upstream_status"`
	Status         int    `json:"status"`
}

// Decision is one of "allowed", "denied", "failed", "not_run"; timings
// are nil for "not_run".
type RequestStage struct {
	Name       string       `json:"name"`
	StartedAt  *Timestamp   `json:"started_at"`
	DurationUS *int64       `json:"duration_us"`
	Decision   string       `json:"decision"`
	Detail     *StageDetail `json:"detail"` // nil encodes null: the stage reported none
}

// StageDetail is what a stage reports about its own decision. Rules is
// never nil when set, so it encodes as [].
type StageDetail struct {
	Revision string   `json:"revision"`
	Rules    []string `json:"rules"`
}

// Observation is one of "completed", "failed".
type ResponseStage struct {
	Name        string    `json:"name"`
	StartedAt   Timestamp `json:"started_at"`
	DurationUS  int64     `json:"duration_us"`
	Observation string    `json:"observation"`
}

type ToolResult struct {
	ToolCallID  string  `json:"tool_call_id"`
	IsError     bool    `json:"is_error"`
	SizeBytes   int64   `json:"size_bytes"`
	Fingerprint *string `json:"fingerprint"`
}

// Outcome is one of "delivered", "not_delivered".
type ToolCall struct {
	ToolCallID    string     `json:"tool_call_id"`
	ToolName      string     `json:"tool_name"`
	ServerSide    bool       `json:"server_side"`
	ArgumentNames []string   `json:"argument_names"` // nil encodes null: not recorded
	SizeBytes     *int64     `json:"size_bytes"`
	Fingerprint   *string    `json:"fingerprint"`
	RelayedAt     *Timestamp `json:"relayed_at"`
	Outcome       string     `json:"outcome"`
	Recorded      bool       `json:"recorded"`
}

type Tokens struct {
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
}

// Kind is "response_truncated" or "tool_calls_unread" in v1.
type Finding struct {
	Kind string `json:"kind"`
}
