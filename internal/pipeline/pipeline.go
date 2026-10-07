// Package pipeline runs every proxied call through a fixed sequence of
// governance stages that can allow or deny it, and records the outcome for
// observation regardless of how the call ended.
package pipeline

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// Call is the state every stage shares over the life of one request.
// Stages read it; only the pipeline mutates it.
type Call struct {
	ID       string
	Received time.Time
	Request  *http.Request

	// Body holds the whole request body. An over-limit request is denied
	// outright rather than truncated, so every stage that runs sees
	// exactly what the provider receives.
	Body []byte

	// AgentID is set by the pipeline from the identification stage's
	// Decision on a successful match (AllowAs). Empty when identification
	// is disabled, hasn't run yet, or denied the call. Once set, no later
	// stage's Decision can overwrite it — see Handler.
	AgentID string

	Status   int
	Duration time.Duration
	Denial   *Denial

	// ResponseBody is capped at maxBodyBytes, with ResponseTruncated set
	// when it was cut. The cap only affects what stages retain for
	// observation — the caller already has the full response by the time
	// this is populated, so there's no "deny" option here, only "keep
	// less of it."
	ResponseBody      []byte
	ResponseTruncated bool
	ResponseErr       error

	// Trace is set by the pipeline from the tracing stage's Decision
	// (AllowTraced). Nil when tracing is disabled or did not run.
	Trace *trace.RequestInfo
}

// Denial is attached to Call when a stage — or the pipeline itself —
// refuses it. Message goes to the caller; Reason is for the audit trail
// only and must never leave the process in a response.
type Denial struct {
	Stage   string
	Status  int
	Message string
	Reason  string
}

// Decision's fields are unexported on purpose: Allow and Deny are the only
// way to produce one, so a stage that returns a bare Decision{} by mistake
// denies rather than silently allowing.
type Decision struct {
	allow   bool
	status  int
	message string
	reason  string
	agentID string
	trace   *trace.RequestInfo
	tap     *trace.ResponseTap
	detail  *trace.StageDetail
}

func Allow() Decision {
	return Decision{allow: true}
}

func Deny(status int, message, reason string) Decision {
	return Decision{status: status, message: message, reason: reason}
}

// WithDetail returns d with detail attached; it is carried to the stage's
// entry in the request record, for allows and denies alike.
func (d Decision) WithDetail(detail trace.StageDetail) Decision {
	d.detail = &detail
	return d
}

// AllowAs allows the call and attaches agentID as its identity. A later
// stage's plain Allow() never clears or overwrites an identity a prior
// AllowAs already set — see Handler.
func AllowAs(agentID string) Decision {
	return Decision{allow: true, agentID: agentID}
}

// AllowTraced allows the call and attaches what tracing read from the
// request, plus the tap that will read its response.
func AllowTraced(info trace.RequestInfo, tap *trace.ResponseTap) Decision {
	return Decision{allow: true, trace: &info, tap: tap}
}

type Stage interface {
	Name() string
	Request(ctx context.Context, c *Call) (Decision, error)
}

// ResponseObserver is the optional half of Stage for stages that need to
// see how a call ended. Response fires for every call the pipeline
// handles, including ones another stage denied, so an implementation must
// tolerate c.Denial != nil and a c.ResponseBody that's empty or absent.
type ResponseObserver interface {
	Response(ctx context.Context, c *Call)
}

type Pipeline struct {
	logger  *zap.Logger
	maxBody int64
	sink    trace.Sink
	stages  []Stage
}

// New's sink is nil when tracing is disabled; the pipeline then builds no
// records.
func New(logger *zap.Logger, maxBodyBytes int64, sink trace.Sink, stages ...Stage) *Pipeline {
	return &Pipeline{logger: logger, maxBody: maxBodyBytes, sink: sink, stages: stages}
}

// stageResult is one stage's decision on the request, kept for the
// request record. Decision is "allowed", "denied", "failed" or "not_run".
type stageResult struct {
	name     string
	started  time.Time
	duration time.Duration
	decision string
	detail   *trace.StageDetail
}

// observation is one stage's observation of the response, kept for the
// response record.
type observation struct {
	name      string
	started   time.Time
	duration  time.Duration
	completed bool
}

func (p *Pipeline) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r.Header.Del("X-Gateway-Request-Id")

		c := &Call{ID: newRequestID(), Received: start, Request: r}

		body, readErr := io.ReadAll(io.LimitReader(r.Body, p.maxBody+1))
		_ = r.Body.Close()

		var denial *Denial
		var stageErr error
		bodyRead := false

		switch {
		case readErr != nil:
			denial = &Denial{
				Stage:   "pipeline",
				Status:  http.StatusBadRequest,
				Message: "could not read request body",
				Reason:  readErr.Error(),
			}
		case int64(len(body)) > p.maxBody:
			denial = &Denial{
				Stage:   "pipeline",
				Status:  http.StatusRequestEntityTooLarge,
				Message: "request body too large",
				Reason:  fmt.Sprintf("body exceeds %d bytes", p.maxBody),
			}
		default:
			bodyRead = true
			c.Body = body
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		ctx := r.Context()
		var results []stageResult
		var tap *trace.ResponseTap
		var tapStage string
		if denial == nil {
			for _, s := range p.stages {
				started := time.Now()
				decision, err := p.call(ctx, s, c)
				res := stageResult{name: s.Name(), started: started, duration: time.Since(started)}
				if err != nil {
					res.decision = "failed"
					results = append(results, res)
					stageErr = err
					denial = &Denial{
						Stage:   s.Name(),
						Status:  http.StatusServiceUnavailable,
						Message: "governance unavailable",
						Reason:  err.Error(),
					}
					p.logger.Error("stage failed", zap.String("stage", s.Name()), zap.String("request_id", c.ID), zap.Error(err))
					break
				}
				if !decision.allow {
					res.decision = "denied"
					res.detail = decision.detail
					results = append(results, res)
					status := decision.status
					if status == 0 {
						status = http.StatusInternalServerError
					}
					denial = &Denial{Stage: s.Name(), Status: status, Message: decision.message, Reason: decision.reason}
					fields := []zap.Field{
						zap.String("stage", s.Name()),
						zap.String("request_id", c.ID),
						zap.Int("status", status),
						zap.String("reason", decision.reason),
					}
					if c.AgentID != "" {
						fields = append(fields, zap.String("agent_id", c.AgentID))
					}
					p.logger.Warn("call denied", fields...)
					break
				}
				res.decision = "allowed"
				res.detail = decision.detail
				results = append(results, res)
				if decision.agentID != "" && c.AgentID == "" {
					c.AgentID = decision.agentID
				}
				if decision.trace != nil {
					c.Trace = decision.trace
				}
				if decision.tap != nil {
					tap, tapStage = decision.tap, s.Name()
				}
			}
		}
		for _, s := range p.stages[len(results):] {
			results = append(results, stageResult{name: s.Name(), decision: "not_run"})
		}

		if p.sink != nil {
			rec := buildRequestRecord(c, results, denial, stageErr != nil, bodyRead)
			if err := p.sink.Write(rec); err != nil {
				p.logger.Error("trace sink did not accept the request record; the record is lost",
					zap.String("call_id", c.ID), zap.String("stage", "tracing"), zap.String("record", "request"), zap.Error(err))
			}
		}

		c.Denial = denial

		var rec *bodyRecorder
		var forwardedAt, ended time.Time
		var abort any
		if denial == nil {
			forwardedAt = time.Now()
			rec = newBodyRecorder(w, p.maxBody, tap)
			abort = serveNext(next, rec, r)
			ended = time.Now()
			c.Status = rec.statusSent()
		} else {
			c.Status = denial.Status
			writeDenial(w, c.ID, denial, stageErr != nil)
		}

		c.Duration = time.Since(start)
		if rec != nil {
			respBody, decErr := rec.decodedBody(w.Header().Get("Content-Encoding"))
			c.ResponseBody = respBody
			c.ResponseTruncated = rec.truncated
			c.ResponseErr = decErr
			if decErr != nil {
				p.logger.Warn("could not decode response body for observation", zap.Error(decErr))
			}
		}

		var observations []observation
		for i := len(p.stages) - 1; i >= 0; i-- {
			s := p.stages[i]
			obs, ok := s.(ResponseObserver)
			if !ok {
				continue
			}
			started := time.Now()
			completed := p.observe(ctx, s.Name(), obs, c)
			observations = append(observations, observation{name: s.Name(), started: started, duration: time.Since(started), completed: completed})
		}

		if rec != nil && p.sink != nil {
			info, tapObs := rec.tapResult(tapStage, forwardedAt)
			if tapObs != nil && !tapObs.completed {
				p.logger.Error("tracing could not record the whole response",
					zap.String("call_id", c.ID), zap.String("stage", "tracing"), zap.String("record", "response"), zap.Error(rec.tapError(info)))
			}
			if tapObs != nil {
				observations = append([]observation{*tapObs}, observations...)
			}
			resp := buildResponseRecord(c, rec, info, observations, forwardedAt, ended)
			if err := p.sink.Write(resp); err != nil {
				p.logger.Error("trace sink did not accept the response record; the record is lost",
					zap.String("call_id", c.ID), zap.String("stage", "tracing"), zap.String("record", "response"), zap.Error(err))
			}
		} else if rec != nil {
			_, _ = rec.tapResult(tapStage, forwardedAt)
		}

		if abort != nil {
			panic(abort)
		}
	})
}

// serveNext runs next, catching http.ErrAbortHandler, which ReverseProxy
// panics with when the agent or the provider drops mid-response, so the
// call can still be observed and recorded before the abort resumes.
func serveNext(next http.Handler, w http.ResponseWriter, r *http.Request) (abort any) {
	defer func() {
		if p := recover(); p != nil {
			if p != http.ErrAbortHandler {
				panic(p)
			}
			abort = p
		}
	}()
	next.ServeHTTP(w, r)
	return nil
}

func (p *Pipeline) call(ctx context.Context, s Stage, c *Call) (d Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("stage %s panicked: %v", s.Name(), r)
		}
	}()
	return s.Request(ctx, c)
}

func (p *Pipeline) observe(ctx context.Context, name string, obs ResponseObserver, c *Call) (completed bool) {
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("stage response observer panicked", zap.String("stage", name), zap.Any("panic", r))
			completed = false
		}
	}()
	obs.Response(ctx, c)
	return true
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ak_" + hex.EncodeToString(b)
}
