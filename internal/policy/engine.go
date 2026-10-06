// Package policy evaluates operator-written Rego policies against each
// request, using an embedded OPA, and owns the policy files' lifecycle.
package policy

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/open-policy-agent/opa/v1/rego"
	"go.uber.org/zap"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

// maxAbandoned bounds how many timed-out evaluations may still be running
// before new ones are refused.
const maxAbandoned = 16

var ruleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Request is what the pipeline hands the engine for one call. It holds
// only what the policies stage can see; the engine builds the input
// document from it.
type Request struct {
	CallID    string
	Method    string
	Path      string
	Headers   http.Header // credentials are stripped by the engine, not the caller
	Body      []byte
	AgentID   string // "" when not identified
	SessionID string // "" when tracing found none (status "claimed" only)
}

// Result is one evaluation's decision. Rules is sorted and unique; empty
// means allow. Revision names the policy the evaluation ran under.
type Result struct {
	Revision string
	Rules    []string
}

// Engine evaluates requests and owns the policy lifecycle.
type Engine struct {
	cfg       config.PoliciesConfig
	provider  string
	logger    *zap.Logger
	current   atomic.Pointer[snapshot] // nil: no policy configured
	reloadMu  sync.Mutex
	abandoned atomic.Int64
	eval      func(ctx context.Context, q rego.PreparedEvalQuery, input map[string]any) (rego.ResultSet, error)
}

var errEvalPanic = errors.New("policy evaluation panicked")

// New loads and checks the configured policy file and data files. With no
// policy path configured it returns an Engine that allows everything.
func New(cfg config.PoliciesConfig, providerName string, logger *zap.Logger) (*Engine, error) {
	e := &Engine{cfg: cfg, provider: providerName, logger: logger, eval: defaultEval}
	if cfg.Path == "" {
		return e, nil
	}
	snap, err := loadSnapshot(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	e.current.Store(snap)
	logger.Info("policy loaded", zap.String("revision", snap.revision), zap.Int("files", snap.files))
	return e, nil
}

// Revision is the revision in force, "" when no policy is configured.
func (e *Engine) Revision() string {
	if snap := e.current.Load(); snap != nil {
		return snap.revision
	}
	return ""
}

// Evaluate decides one request under the current snapshot. The returned
// error is always an *EvalError.
func (e *Engine) Evaluate(ctx context.Context, r Request) (Result, error) {
	snap := e.current.Load()
	if snap == nil {
		return Result{}, nil
	}
	input, err := buildInput(e.provider, r)
	if err != nil {
		return Result{}, &EvalError{Kind: "input"}
	}
	if e.abandoned.Load() >= maxAbandoned {
		return Result{}, &EvalError{Kind: "overloaded"}
	}

	evalCtx, cancel := context.WithTimeout(ctx, e.cfg.EvalTimeout)
	defer cancel()
	type outcome struct {
		rs  rego.ResultSet
		err error
	}
	done := make(chan outcome, 1) // buffered: an abandoned goroutine can still send and exit
	go func() {
		defer func() {
			if recover() != nil {
				done <- outcome{nil, errEvalPanic}
			}
		}()
		rs, err := e.eval(evalCtx, snap.query, input)
		done <- outcome{rs, err}
	}()

	var o outcome
	select {
	case o = <-done:
	case <-evalCtx.Done():
		e.abandoned.Add(1)
		go func() { <-done; e.abandoned.Add(-1) }()
		return Result{}, &EvalError{Kind: cancelKind(ctx, evalCtx)}
	}
	if o.err != nil {
		kind := "internal"
		if evalCtx.Err() != nil {
			kind = cancelKind(ctx, evalCtx)
		}
		return Result{}, &EvalError{Kind: kind}
	}

	ids, err := decisionRules(o.rs)
	if err != nil {
		return Result{}, &EvalError{Kind: "malformed_decision"}
	}
	for i, id := range ids {
		if !ruleIDPattern.MatchString(id) {
			ids[i] = "invalid-rule-id"
		}
	}
	slices.Sort(ids)
	return Result{Revision: snap.revision, Rules: slices.Compact(ids)}, nil
}

func defaultEval(ctx context.Context, q rego.PreparedEvalQuery, input map[string]any) (rego.ResultSet, error) {
	return q.Eval(ctx, rego.EvalInput(input))
}

// cancelKind tells the policy's own deadline apart from the caller giving up.
func cancelKind(parent, evalCtx context.Context) string {
	if parent.Err() == nil && errors.Is(evalCtx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "internal"
}

// decisionRules reads the result of the deny query: no result is allow,
// otherwise a set of strings.
func decisionRules(rs rego.ResultSet) ([]string, error) {
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return nil, nil
	}
	list, ok := rs[0].Expressions[0].Value.([]any)
	if !ok {
		return nil, errors.New("not a set")
	}
	ids := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("not a string")
		}
		ids = append(ids, s)
	}
	return ids, nil
}

// Reload rebuilds the policy from disk. trigger is "signal" or "watch"
// and only appears in the log. On error the previous snapshot stays.
func (e *Engine) Reload(trigger string) error {
	if e.cfg.Path == "" {
		return nil
	}
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()

	old := e.current.Load()
	snap, err := loadSnapshot(context.Background(), e.cfg)
	if err != nil {
		e.logger.Error("policy reload failed; keeping the current policy",
			zap.String("trigger", trigger), zap.String("revision", old.revision), zap.Error(err))
		return err
	}
	if snap.revision == old.revision {
		e.logger.Debug("policy reload: no change", zap.String("trigger", trigger), zap.String("revision", old.revision))
		return nil
	}
	e.current.Store(snap)
	e.logger.Info("policy reloaded",
		zap.String("trigger", trigger),
		zap.String("old_revision", old.revision),
		zap.String("new_revision", snap.revision),
		zap.Int("files", snap.files))
	return nil
}
