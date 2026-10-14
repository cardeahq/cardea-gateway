package filtering

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

func logText(logs *observer.ObservedLogs) string {
	var b strings.Builder
	for _, e := range logs.All() {
		fmt.Fprintf(&b, "%s %v\n", e.Message, e.ContextMap())
	}
	return b.String()
}

// sysBody is a minimal Anthropic messages request with a plain-string
// system prompt.
func sysBody(system string) string {
	return `{"model":"m","system":"` + system + `","messages":[{"role":"user","content":"hi"}]}`
}

func defaultFilteringConfig() config.FilteringConfig {
	return config.FilteringConfig{
		Categories: config.CategoriesConfig{Email: true, SSN: true, APIKey: true},
		Cache:      config.FilterCacheConfig{Backend: "memory", Capacity: 1000, TTL: time.Minute},
	}
}

// --- Detection and replacement -----------------------------------------------

func TestEngine_Redact_DetectionAndReplacement(t *testing.T) {
	tests := []struct {
		name string
		cat  Category
		val  string
	}{
		{"email", CategoryEmail, exEmail},
		{"ssn us", CategorySSN, exSSNUS},
		{"ssn uk", CategorySSN, exSSNUK},
		{"ssn fr", CategorySSN, exSSNFR},
		{"api key openai", CategoryAPIKey, exAPIKeyOpenAI},
		{"api key github", CategoryAPIKey, exAPIKeyGitHub},
		{"api key aws", CategoryAPIKey, exAPIKeyAWS},
		{"api key slack", CategoryAPIKey, exAPIKeySlack},
		{"api key google", CategoryAPIKey, exAPIKeyGoogle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeCache{}
			eng := testEngine(fc)
			body := []byte(sysBody("see " + tt.val + " please"))

			res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
			if err != nil {
				t.Fatalf("Redact: %v", err)
			}
			if res.Counts[tt.cat] != 1 {
				t.Fatalf("Counts = %v, want %s:1", res.Counts, tt.cat)
			}
			ph := placeholders(res.Body)
			if len(ph) != 1 || ph[0][0] != string(tt.cat) {
				t.Fatalf("placeholders = %v, want exactly one of category %s", ph, tt.cat)
			}
			if strings.Contains(string(res.Body), tt.val) {
				t.Fatalf("redacted body still contains the real value: %s", res.Body)
			}
			entry, ok, err := fc.Get(context.Background(), ph[0][1])
			if err != nil || !ok {
				t.Fatalf("cache.Get(%s) = ok=%v err=%v, want a hit", ph[0][1], ok, err)
			}
			if entry.Category != tt.cat || entry.Value != tt.val {
				t.Fatalf("cache entry = %+v, want {%s %s}", entry, tt.cat, tt.val)
			}
		})
	}
}

func TestEngine_Redact_NegativeAPIKeyCaseIsNotDetected(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	body := []byte(sysBody("token " + exNoPrefixHex + " end"))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts != nil {
		t.Fatalf("Counts = %v, want nil: a non-vendor-prefixed token must not be detected", res.Counts)
	}
	if string(res.Body) != string(body) {
		t.Fatalf("Body = %s, want the original bytes unchanged", res.Body)
	}
}

func TestEngine_Redact_DisabledCategoryLetsItsPatternThroughOthersStillRedacted(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngineWithCategories(fc, CategorySSN, CategoryAPIKey) // email disabled
	body := []byte(sysBody(exEmail + " and " + exSSNUS))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts[CategoryEmail] != 0 {
		t.Fatalf("Counts[email] = %d, want 0: email is disabled", res.Counts[CategoryEmail])
	}
	if res.Counts[CategorySSN] != 1 {
		t.Fatalf("Counts[ssn] = %d, want 1", res.Counts[CategorySSN])
	}
	if !strings.Contains(string(res.Body), exEmail) {
		t.Fatalf("redacted body lost the disabled category's plaintext: %s", res.Body)
	}
	if strings.Contains(string(res.Body), exSSNUS) {
		t.Fatalf("redacted body still contains the enabled category's plaintext: %s", res.Body)
	}
}

func TestEngine_Redact_RepeatedValueGetsDistinctUUIDsAndCacheEntries(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	body := []byte(sysBody(exEmail + " then " + exEmail + " then " + exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts[CategoryEmail] != 3 {
		t.Fatalf("Counts[email] = %d, want 3", res.Counts[CategoryEmail])
	}
	ph := placeholders(res.Body)
	if len(ph) != 3 {
		t.Fatalf("placeholders = %v, want 3", ph)
	}
	seen := map[string]bool{}
	for _, p := range ph {
		if seen[p[1]] {
			t.Fatalf("UUID %s reused across occurrences of the same value", p[1])
		}
		seen[p[1]] = true
		entry, ok, _ := fc.Get(context.Background(), p[1])
		if !ok || entry.Value != exEmail {
			t.Fatalf("cache entry for %s = %+v (ok=%v), want Value %q", p[1], entry, ok, exEmail)
		}
	}
	if fc.setCount() != 3 {
		t.Fatalf("cache.Set called %d times, want 3 (one per occurrence)", fc.setCount())
	}
}

func TestEngine_Redact_OverlappingMatchesKeepTheEarlierLongerOne(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	// A Slack token whose suffix, once inside the token, is shaped exactly
	// like a US SSN. The Slack match starts first and is longer, so the
	// embedded SSN-shaped substring must not get its own placeholder.
	overlapping := "xoxb-" + exSSNUS + "-abcdefghij"
	body := []byte(sysBody("secret " + overlapping + " end"))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	ph := placeholders(res.Body)
	if len(ph) != 1 || ph[0][0] != string(CategoryAPIKey) {
		t.Fatalf("placeholders = %v, want exactly one api_key placeholder covering the whole token", ph)
	}
	if res.Counts[CategorySSN] != 0 {
		t.Fatalf("Counts[ssn] = %d, want 0: the embedded SSN-shaped text must not get its own match", res.Counts[CategorySSN])
	}
}

func TestEngine_Redact_NothingMatchedReturnsTheOriginalBytes(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	body := []byte(`{"system":"nothing to see here","model":"m","messages":[{"role":"user","content":"hi"}]}`)

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts != nil {
		t.Fatalf("Counts = %v, want nil", res.Counts)
	}
	if string(res.Body) != string(body) {
		t.Fatalf("Body = %s, want the exact original bytes (not a re-marshal, even a byte-identical one)", res.Body)
	}
}

// --- Non-target bodies --------------------------------------------------------

func TestEngine_Redact_NonTargetBodiesAreUnchangedWithNoError(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"not an anthropic conversation path", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"` + exEmail + `"}]}`},
		{"non-JSON body on an unrelated path", "/v1/chat/completions", "not json at all " + exEmail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeCache{}
			eng := testEngine(fc)
			body := []byte(tt.body)
			res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: tt.path, Body: body})
			if err != nil {
				t.Fatalf("Redact: %v, want nil error for a non-target body", err)
			}
			if string(res.Body) != string(body) || res.Counts != nil {
				t.Fatalf("Result = %+v, want the body unchanged and no counts", res)
			}
			if fc.setCount() != 0 {
				t.Fatal("a non-target body wrote to the cache")
			}
		})
	}
}

func TestEngine_Redact_WrongProviderNameOnTheEngineIsNotATarget(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngineWithCategories(fc, CategoryEmail)
	eng.providerName = "openai" // this Engine was built for a different provider
	body := []byte(sysBody(exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil || string(res.Body) != string(body) {
		t.Fatalf("Redact = %+v, %v, want the body unchanged with no error", res, err)
	}
}

func TestEngine_Redact_EveryCategoryDisabledNeverInspectsTheBody(t *testing.T) {
	fc := &fakeCache{}
	eng := &Engine{patterns: map[Category][]*regexp.Regexp{}, cache: fc, ttl: time.Minute, providerName: "anthropic"}
	malformed := []byte(`{"model":`) // not even valid JSON, on a target path

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: malformed})
	if err != nil {
		t.Fatalf("Redact: %v, want nil: every category disabled must not even parse the body", err)
	}
	if string(res.Body) != string(malformed) || res.Counts != nil {
		t.Fatalf("Result = %+v, want the body unchanged", res)
	}
	if fc.setCount() != 0 {
		t.Fatal("every category disabled but the cache was still written to")
	}
}

// --- Failure: input ------------------------------------------------------------

func TestEngine_Redact_TargetBodyParseFailuresGiveInputError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not valid JSON", `{"model":"m","messages":[`},
		{"duplicate top-level key", `{"model":"a","model":"b","messages":[]}`},
		{"duplicate key nested inside a tool_use input", `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{"a":1,"a":2}}]}]}`},
		{"messages is not an array", `{"model":"m","messages":"nope"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeCache{}
			eng := testEngine(fc)
			res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: []byte(tt.body)})

			var fe *FilterError
			if !errors.As(err, &fe) || fe.Kind != "input" {
				t.Fatalf("err = %v, want *FilterError{Kind: \"input\"}", err)
			}
			if res.Body != nil || res.Counts != nil {
				t.Fatalf("Result = %+v on error, want the zero value: never a partially redacted body", res)
			}
		})
	}
}

// --- Failure: cache_write --------------------------------------------------------

func TestEngine_Redact_CacheWriteFailureFailsClosed(t *testing.T) {
	fc := &fakeCache{setErr: errors.New("backend unreachable")}
	eng := testEngine(fc)
	body := []byte(sysBody(exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})

	var fe *FilterError
	if !errors.As(err, &fe) || fe.Kind != "cache_write" {
		t.Fatalf("err = %v, want *FilterError{Kind: \"cache_write\"}", err)
	}
	if res.Body != nil || res.Counts != nil {
		t.Fatalf("Result = %+v on a cache write failure, want the zero value: not even a partially redacted body", res)
	}
}

// --- Failure: internal (panic recovery) ------------------------------------------

func TestEngine_Redact_PanicDuringRedactionIsRecoveredWithoutLeakingItsValue(t *testing.T) {
	const secretPanic = "panic carrying " + "do-not-leak-this-value"
	fc := &fakeCache{setPanic: secretPanic}
	eng := testEngine(fc)
	body := []byte(sysBody(exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})

	var fe *FilterError
	if !errors.As(err, &fe) || fe.Kind != "internal" {
		t.Fatalf("err = %v, want *FilterError{Kind: \"internal\"}", err)
	}
	if strings.Contains(err.Error(), "do-not-leak-this-value") {
		t.Fatalf("err.Error() = %q, leaks the recovered panic value", err.Error())
	}
	if res.Body != nil || res.Counts != nil {
		t.Fatalf("Result = %+v on a panic, want the zero value", res)
	}
}

// --- No content in outputs --------------------------------------------------------

func TestEngine_Redact_NoMarkerAnywhereExceptTheCacheValue(t *testing.T) {
	// The marker is the local-part of an email address, so the entire
	// marker falls inside the detected (and therefore redacted) span —
	// unlike arbitrary surrounding text, which legitimately survives
	// redaction untouched.
	const markerEmail = "zqfiltermarker9@example.com"
	fc := &fakeCache{}
	eng := testEngine(fc)
	body := []byte(`{"model":"m","system":"sys ` + markerEmail + `","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"a":{"b":{"c":"` + markerEmail + `"}}}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"result ` + markerEmail + `"}]}]}`)

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if strings.Contains(string(res.Body), markerEmail) {
		t.Fatalf("redacted body carries the marker: %s", res.Body)
	}
	if res.Counts[CategoryEmail] != 3 {
		t.Fatalf("Counts[email] = %d, want 3 (system, tool_use input, tool_result)", res.Counts[CategoryEmail])
	}
	found := 0
	for _, p := range placeholders(res.Body) {
		entry, ok, _ := fc.Get(context.Background(), p[1])
		if !ok {
			continue
		}
		if entry.Value == markerEmail {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("marker found as a cache Value in %d entries, want 3 (the only place it may appear)", found)
	}
}

// --- Write before forward (functional criterion 8) -------------------------------

func TestEngine_Redact_CacheIsWrittenBeforeRedactReturns(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	body := []byte(sysBody(exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	ph := placeholders(res.Body)
	if len(ph) != 1 {
		t.Fatalf("placeholders = %v, want 1", ph)
	}
	// No delay, no goroutine hand-off: the entry must already be there by
	// the time Redact has returned, since nothing has forwarded the call yet.
	entry, ok, err := fc.Get(context.Background(), ph[0][1])
	if err != nil || !ok || entry.Value != exEmail {
		t.Fatalf("cache.Get right after Redact returned = %+v ok=%v err=%v, want an immediate hit", entry, ok, err)
	}
}

// --- Engine.Close ------------------------------------------------------------------

func TestEngine_Close_ClosesTheCache(t *testing.T) {
	fc := &fakeCache{}
	eng := testEngine(fc)
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fc.closed {
		t.Fatal("Engine.Close did not close the underlying cache")
	}
}

// --- New -----------------------------------------------------------------------

func TestNew_MemoryBackendLogsTheVolatilityWarning(t *testing.T) {
	logger, logs := observedLogger()
	eng, err := New(defaultFilteringConfig(), "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = eng.Close() }()

	var warned bool
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		msg := strings.ToLower(e.Message)
		if strings.Contains(msg, "in-memory") && strings.Contains(msg, "restart") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names the in-memory cache as volatile across a restart; logs:\n%s", logText(logs))
	}
}

func TestNew_CategoriesFromConfigRestrictWhichPatternsRun(t *testing.T) {
	cfg := defaultFilteringConfig()
	cfg.Categories = config.CategoriesConfig{Email: false, SSN: true, APIKey: false}
	eng, err := New(cfg, "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = eng.Close() }()

	body := []byte(sysBody(exEmail + " " + exSSNUS + " " + exAPIKeyOpenAI))
	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if res.Counts[CategorySSN] != 1 || res.Counts[CategoryEmail] != 0 || res.Counts[CategoryAPIKey] != 0 {
		t.Fatalf("Counts = %v, want only ssn:1", res.Counts)
	}
	if !strings.Contains(string(res.Body), exEmail) || !strings.Contains(string(res.Body), exAPIKeyOpenAI) {
		t.Fatalf("Body = %s, want the disabled categories' plaintext still present", res.Body)
	}
}

func TestNew_EveryCategoryDisabledStillInitializesTheCacheButRedactIsANoOp(t *testing.T) {
	cfg := defaultFilteringConfig()
	cfg.Categories = config.CategoriesConfig{Email: false, SSN: false, APIKey: false}
	eng, err := New(cfg, "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v, want success: the cache backend still initializes with every category off", err)
	}
	defer func() { _ = eng.Close() }()

	malformed := []byte(`{"model":`)
	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: malformed})
	if err != nil {
		t.Fatalf("Redact: %v, want nil: nothing to scan, so a malformed body is not even inspected", err)
	}
	if string(res.Body) != string(malformed) {
		t.Fatalf("Body = %s, want the original bytes", res.Body)
	}
}
