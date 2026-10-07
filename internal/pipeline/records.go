package pipeline

import (
	"time"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

func buildRequestRecord(c *Call, results []stageResult, denial *Denial, failed, bodyRead bool) trace.RequestRecord {
	rec := trace.RequestRecord{
		Schema:      trace.Schema,
		Record:      "request",
		CallID:      c.ID,
		AgentID:     optional(c.AgentID),
		Session:     trace.Session{Status: "absent"},
		ReceivedAt:  trace.Timestamp(c.Received),
		DurationUS:  time.Since(c.Received).Microseconds(),
		Outcome:     trace.RequestOutcome{Result: "forwarded"},
		Stages:      make([]trace.RequestStage, 0, len(results)),
		ToolResults: []trace.ToolResult{},
		Findings:    []trace.Finding{},
	}
	if bodyRead {
		size := int64(len(c.Body))
		rec.SizeBytes = &size
	}
	if c.Trace != nil {
		rec.Session = c.Trace.Session
		rec.Model = c.Trace.Model
		rec.Stream = c.Trace.Stream
		if c.Trace.ToolResults != nil {
			rec.ToolResults = c.Trace.ToolResults
		}
	}
	if denial != nil {
		rec.Outcome = trace.RequestOutcome{
			Result: "denied",
			Status: &denial.Status,
			Stage:  &denial.Stage,
			Reason: &denial.Reason,
		}
		if failed {
			rec.Outcome.Result = "failed"
		}
	}
	for _, r := range results {
		st := trace.RequestStage{Name: r.name, Decision: r.decision, Detail: r.detail}
		if r.decision != "not_run" {
			ts := trace.Timestamp(r.started)
			us := r.duration.Microseconds()
			st.StartedAt, st.DurationUS = &ts, &us
		}
		rec.Stages = append(rec.Stages, st)
	}
	return rec
}

func buildResponseRecord(c *Call, rec *bodyRecorder, info trace.ResponseInfo, observations []observation, forwardedAt, ended time.Time) trace.ResponseRecord {
	resp := trace.ResponseRecord{
		Schema:        trace.Schema,
		Record:        "response",
		CallID:        c.ID,
		AgentID:       optional(c.AgentID),
		Session:       trace.Session{Status: "absent"},
		StartedAt:     trace.Timestamp(forwardedAt),
		DurationUS:    ended.Sub(forwardedAt).Microseconds(),
		Stages:        make([]trace.ResponseStage, 0, len(observations)),
		Streamed:      info.Streamed,
		ResponseID:    info.ResponseID,
		ResponseModel: info.ResponseModel,
		StopReason:    info.StopReason,
		Tokens:        info.Tokens,
		ToolCalls:     info.ToolCalls,
		Findings:      []trace.Finding{},
	}
	if c.Trace != nil {
		resp.Session = c.Trace.Session
	}
	if resp.ToolCalls == nil {
		resp.ToolCalls = []trace.ToolCall{}
	}

	status := rec.statusSent()
	if rec.upstreamFailed {
		resp.Outcome = trace.ResponseOutcome{Result: "unanswered", Status: status}
	} else {
		resp.Outcome = trace.ResponseOutcome{Result: "delivered", UpstreamStatus: &status, Status: status}
		if !rec.firstByte.IsZero() {
			us := rec.firstByte.Sub(forwardedAt).Microseconds()
			resp.FirstByteUS = &us
		}
	}

	for _, o := range observations {
		st := trace.ResponseStage{
			Name:        o.name,
			StartedAt:   trace.Timestamp(o.started),
			DurationUS:  o.duration.Microseconds(),
			Observation: "completed",
		}
		if !o.completed {
			st.Observation = "failed"
		}
		resp.Stages = append(resp.Stages, st)
	}

	if c.ResponseTruncated {
		resp.Findings = append(resp.Findings, trace.Finding{Kind: "response_truncated"})
	}
	if info.Unread {
		resp.Findings = append(resp.Findings, trace.Finding{Kind: "tool_calls_unread"})
	}
	return resp
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
