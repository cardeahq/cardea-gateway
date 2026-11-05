// Package filtering detects email addresses, US/UK/France SSNs and
// vendor-prefixed API keys in the content of an Anthropic, OpenAI-style
// or Gemini request body, and replaces each occurrence with a
// "[REDACTED:category]" placeholder before the request is forwarded.
package filtering

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

// Request is what the pipeline hands the engine for one call.
type Request struct {
	CallID    string
	Path      string
	Body      []byte
	Format    string // routing format: "anthropic", "openai" or "gemini"
	Operation string // routing operation: "messages", "count_tokens", "completion", "embeddings" or "models"
}

// Result is one Redact call's outcome. Counts is nil when nothing was
// redacted (a models operation, or nothing matched); Body equals the
// input body in that case.
type Result struct {
	Body   []byte
	Counts map[Category]int
}

// Engine detects and redacts PII in a request body. Safe for concurrent
// use: it holds no per-call state (see scan).
type Engine struct {
	patterns map[Category][]*regexp.Regexp
}

// New builds an Engine from configuration. With every category
// disabled, the returned Engine's Redact is a no-op.
func New(cfg config.FilteringConfig, logger *zap.Logger) (*Engine, error) {
	patterns := map[Category][]*regexp.Regexp{}
	for cat, enabled := range map[Category]bool{
		CategoryEmail:  cfg.Categories.Email,
		CategorySSN:    cfg.Categories.SSN,
		CategoryAPIKey: cfg.Categories.APIKey,
	} {
		if enabled {
			patterns[cat] = categoryPatterns[cat]
		}
	}

	return &Engine{patterns: patterns}, nil
}

// rewriters maps each (format, operation) pair carrying content to the
// rewriter that redacts its scanned members in place.
var rewriters = map[string]map[string]func(*scan, map[string]json.RawMessage) error{
	"anthropic": {"messages": (*scan).redactAnthropic, "count_tokens": (*scan).redactAnthropic},
	"openai":    {"messages": (*scan).redactChat, "completion": (*scan).redactFIM, "embeddings": (*scan).redactEmbeddings},
	"gemini":    {"messages": (*scan).redactGenerate, "count_tokens": (*scan).redactGenerate, "embeddings": (*scan).redactEmbed},
}

// Redact scans r.Body for every enabled category and returns the
// rewritten body plus how many occurrences of each category were found.
// Redact recovers its own panics into a *FilterError{Kind: "internal"}
// so a panic value that might carry request content never reaches the
// pipeline's generic stage-panic recovery (which logs the panic's %v).
func (e *Engine) Redact(ctx context.Context, r Request) (res Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			res = Result{}
			err = &FilterError{Kind: "internal"}
		}
	}()

	if len(e.patterns) == 0 {
		return Result{Body: r.Body}, nil
	}

	byOp, known := rewriters[r.Format]
	if known && r.Operation == "models" {
		return Result{Body: r.Body}, nil
	}
	rewrite, ok := byOp[r.Operation]
	if !ok {
		return Result{}, &FilterError{Kind: "internal"}
	}

	top, err := decodeTop(r.Body)
	if err != nil {
		return Result{}, err
	}
	sc := &scan{ctx: ctx, engine: e, counts: map[Category]int{}}
	if err := rewrite(sc, top); err != nil {
		return Result{}, asInputErr(err)
	}

	body, _ := json.Marshal(top)
	if len(sc.counts) == 0 {
		return Result{Body: r.Body}, nil
	}
	return Result{Body: body, Counts: sc.counts}, nil
}

// asInputErr classifies err as a *FilterError{"input"} unless it already
// is a *FilterError.
func asInputErr(err error) error {
	var fe *FilterError
	if errors.As(err, &fe) {
		return err
	}
	return &FilterError{Kind: "input"}
}

// redactText is the leaf-level primitive every string field goes
// through: every enabled category's patterns are tried, non-overlapping
// matches (sorted by start, ties broken by the longer match) are each
// replaced by its category's placeholder.
func (s *scan) redactText(str string) string {
	type match struct {
		start, end int
		category   Category
	}
	var matches []match
	for cat, patterns := range s.engine.patterns {
		for _, re := range patterns {
			for _, loc := range re.FindAllStringIndex(str, -1) {
				matches = append(matches, match{loc[0], loc[1], cat})
			}
		}
	}
	if len(matches) == 0 {
		return str
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].end > matches[j].end // longer wins at the same start
	})

	var b strings.Builder
	last := 0
	for _, m := range matches {
		if m.start < last {
			continue // overlaps an already-consumed match
		}
		b.WriteString(str[last:m.start])
		b.WriteString("[REDACTED:" + string(m.category) + "]")
		s.counts[m.category]++
		last = m.end
	}
	b.WriteString(str[last:])
	return b.String()
}
