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
}

func Allow() Decision {
	return Decision{allow: true}
}

func Deny(status int, message, reason string) Decision {
	return Decision{status: status, message: message, reason: reason}
}

// AllowAs allows the call and attaches agentID as its identity. A later
// stage's plain Allow() never clears or overwrites an identity a prior
// AllowAs already set — see Handler.
func AllowAs(agentID string) Decision {
	return Decision{allow: true, agentID: agentID}
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
	stages  []Stage
}

func New(logger *zap.Logger, maxBodyBytes int64, stages ...Stage) *Pipeline {
	return &Pipeline{logger: logger, maxBody: maxBodyBytes, stages: stages}
}

func (p *Pipeline) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r.Header.Del("X-Agentkeeper-Request-Id")

		c := &Call{ID: newRequestID(), Received: start, Request: r}

		body, readErr := io.ReadAll(io.LimitReader(r.Body, p.maxBody+1))
		_ = r.Body.Close()

		var denial *Denial
		var stageErr error

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
			c.Body = body
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		ctx := r.Context()
		if denial == nil {
			for _, s := range p.stages {
				decision, err := p.call(ctx, s, c)
				if err != nil {
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
					status := decision.status
					if status == 0 {
						status = http.StatusInternalServerError
					}
					denial = &Denial{Stage: s.Name(), Status: status, Message: decision.message, Reason: decision.reason}
					p.logger.Warn("call denied",
						zap.String("stage", s.Name()),
						zap.String("request_id", c.ID),
						zap.Int("status", status),
						zap.String("reason", decision.reason),
					)
					break
				}
				if decision.agentID != "" && c.AgentID == "" {
					c.AgentID = decision.agentID
				}
			}
		}

		c.Denial = denial

		var rec *bodyRecorder
		if denial == nil {
			rec = newBodyRecorder(w, p.maxBody)
			next.ServeHTTP(rec, r)
			c.Status = rec.status
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

		for i := len(p.stages) - 1; i >= 0; i-- {
			s := p.stages[i]
			obs, ok := s.(ResponseObserver)
			if !ok {
				continue
			}
			p.observe(ctx, s.Name(), obs, c)
		}
	})
}

func (p *Pipeline) call(ctx context.Context, s Stage, c *Call) (d Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("stage %s panicked: %v", s.Name(), r)
		}
	}()
	return s.Request(ctx, c)
}

func (p *Pipeline) observe(ctx context.Context, name string, obs ResponseObserver, c *Call) {
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("stage response observer panicked", zap.String("stage", name), zap.Any("panic", r))
		}
	}()
	obs.Response(ctx, c)
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ak_" + hex.EncodeToString(b)
}
