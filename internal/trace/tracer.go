package trace

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"regexp"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

const minKeyBytes = 32

// Tracer holds what tracing needs for the life of the process: the
// fingerprint key, the compiled session sources and the read limit. It
// keeps nothing about any call.
type Tracer struct {
	key          []byte
	maxReadBytes int64
	agentSources map[string][]source
	defaultSrcs  []source
}

// NewTracer reads the fingerprint key from the env var cfg names. If the
// var is unset or empty, it generates a random 32-byte key and logs a
// warning naming the var; if it is set but shorter than 32 bytes, it
// fails. agentSources holds each agent's own session sources, keyed by
// agent ID; cfg.DefaultSessionSources serve every other agent. It compiles every session pattern, rejecting one that does not
// compile or has a capture-group count other than one. Its errors name
// the offending source by list and position.
func NewTracer(cfg config.TracingConfig, agentSources map[string][]config.SessionSource, logger *zap.Logger) (*Tracer, error) {
	t := &Tracer{maxReadBytes: cfg.MaxReadBytes, agentSources: map[string][]source{}}

	key := os.Getenv(cfg.FingerprintKeyEnv)
	switch {
	case key == "":
		t.key = make([]byte, minKeyBytes)
		if _, err := rand.Read(t.key); err != nil {
			return nil, fmt.Errorf("generating a fingerprint key: %w", err)
		}
		logger.Warn(fmt.Sprintf("fingerprint key env var %s is not set; using a random per-process key — fingerprints will not match across restarts or instances", cfg.FingerprintKeyEnv),
			zap.String("env_var", cfg.FingerprintKeyEnv))
	case len(key) < minKeyBytes:
		return nil, fmt.Errorf("fingerprint key in %s is %d bytes, want at least %d", cfg.FingerprintKeyEnv, len(key), minKeyBytes)
	default:
		t.key = []byte(key)
	}

	var err error
	if t.defaultSrcs, err = compileSources(cfg.DefaultSessionSources); err != nil {
		return nil, fmt.Errorf("tracing.default_session_sources: %w", err)
	}
	for agent, list := range agentSources {
		if t.agentSources[agent], err = compileSources(list); err != nil {
			return nil, fmt.Errorf("agent %q session_sources: %w", agent, err)
		}
	}
	return t, nil
}

// RequestInfo is what tracing contributes to a request record.
type RequestInfo struct {
	Session     Session
	Model       *string
	Stream      bool
	ToolResults []ToolResult
}

// RequestOptions tells Request how to read this call.
type RequestOptions struct {
	Format       string  // routing format: "anthropic", "openai" or "gemini"
	Operation    string  // routing operation
	Model        *string // the URL's model for Gemini; nil otherwise
	GeminiStream bool    // the call is a Gemini streaming action
}

// readers lists, per format, the operations tracing can read.
var readers = map[string]map[string]bool{
	"anthropic": {"messages": true, "count_tokens": true, "models": true},
	"openai":    {"messages": true, "completion": true, "embeddings": true, "models": true},
	"gemini":    {"messages": true, "count_tokens": true, "embeddings": true, "models": true},
}

func noReader(format, operation string) error {
	if readers[format][operation] {
		return nil
	}
	return fmt.Errorf("tracing has no reader for format %q, operation %q", format, operation)
}

// Request never returns an error for a missing or invalid session, only
// for tool results it cannot read, or a format/operation it has no reader for.
func (t *Tracer) Request(r *http.Request, body []byte, agentID string, opts RequestOptions) (RequestInfo, error) {
	if err := noReader(opts.Format, opts.Operation); err != nil {
		return RequestInfo{}, err
	}
	info := RequestInfo{
		Session:     t.session(r, body, agentID),
		ToolResults: []ToolResult{},
	}
	if opts.Operation == "models" {
		return info, nil
	}

	var results []toolResultBlock
	var err error
	if opts.Format == "gemini" {
		info.Model = opts.Model
		info.Stream = opts.GeminiStream
		if opts.Operation == "messages" {
			results, err = newGeminiToolResults(body)
		}
	} else {
		req, ok := parseMessagesRequest(body)
		if !ok {
			return info, nil
		}
		info.Model = req.Model
		info.Stream = req.Stream
		switch {
		case opts.Format == "anthropic":
			results, err = newToolResults(req.Messages)
		case opts.Operation == "messages":
			results, err = newOpenAIToolResults(req.Messages)
		}
	}
	if err != nil {
		return RequestInfo{}, err
	}

	for i, res := range results {
		tr := ToolResult{ToolCallID: res.id, IsError: res.isError}
		if len(res.content) > 0 {
			canonical, err := canonicalJSON(res.content)
			if err != nil {
				return RequestInfo{}, fmt.Errorf("tool result %d: content is %w", i, err)
			}
			fp := fingerprint(t.key, canonical)
			tr.SizeBytes = int64(len(canonical))
			tr.Fingerprint = &fp
		}
		info.ToolResults = append(info.ToolResults, tr)
	}
	return info, nil
}

// NewResponseTap returns a tap that reads responses of this format and operation.
func (t *Tracer) NewResponseTap(format, operation string) *ResponseTap {
	return &ResponseTap{key: t.key, maxReadBytes: t.maxReadBytes, format: format, operation: operation}
}

func (t *Tracer) sourcesFor(agentID string) []source {
	if srcs, ok := t.agentSources[agentID]; ok && agentID != "" {
		return srcs
	}
	return t.defaultSrcs
}

func compileSources(list []config.SessionSource) ([]source, error) {
	srcs := make([]source, len(list))
	for i, s := range list {
		src, err := compileSource(s)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		srcs[i] = src
	}
	return srcs, nil
}

func compileSource(s config.SessionSource) (source, error) {
	src := source{}
	switch {
	case s.Header != "":
		src.kind, src.name = "header", s.Header
	case s.BodyField != "":
		src.kind, src.name = "body_field", s.BodyField
	case s.Baggage != "":
		src.kind, src.name = "baggage", s.Baggage
	default:
		return source{}, fmt.Errorf("names no header, body_field or baggage key")
	}
	if s.Pattern == "" {
		return src, nil
	}
	re, err := regexp.Compile(s.Pattern)
	if err != nil {
		return source{}, fmt.Errorf("pattern does not compile: %w", err)
	}
	if n := re.NumSubexp(); n != 1 {
		return source{}, fmt.Errorf("pattern has %d capture groups, want exactly 1", n)
	}
	src.pattern = re
	return src, nil
}
