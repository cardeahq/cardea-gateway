package filtering

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// sysBody is a minimal Anthropic messages request with a plain-string
// system prompt.
func sysBody(system string) string {
	return `{"model":"m","system":"` + system + `","messages":[{"role":"user","content":"hi"}]}`
}

func defaultFilteringConfig() config.FilteringConfig {
	return config.FilteringConfig{
		Categories: config.CategoriesConfig{Email: true, SSN: true, APIKey: true},
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
			eng := testEngine()
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
		})
	}
}

func TestEngine_Redact_NegativeAPIKeyCaseIsNotDetected(t *testing.T) {
	eng := testEngine()
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
	eng := testEngineWithCategories(CategorySSN, CategoryAPIKey) // email disabled
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

func TestEngine_Redact_RepeatedValueGetsDistinctUUIDs(t *testing.T) {
	eng := testEngine()
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
	}
}

func TestEngine_Redact_OverlappingMatchesKeepTheEarlierLongerOne(t *testing.T) {
	eng := testEngine()
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
	eng := testEngine()
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
			eng := testEngine()
			body := []byte(tt.body)
			res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: tt.path, Body: body})
			if err != nil {
				t.Fatalf("Redact: %v, want nil error for a non-target body", err)
			}
			if string(res.Body) != string(body) || res.Counts != nil {
				t.Fatalf("Result = %+v, want the body unchanged and no counts", res)
			}
		})
	}
}

func TestEngine_Redact_WrongProviderNameOnTheEngineIsNotATarget(t *testing.T) {
	eng := testEngineWithCategories(CategoryEmail)
	eng.providerName = "openai" // this Engine was built for a different provider
	body := []byte(sysBody(exEmail))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})
	if err != nil || string(res.Body) != string(body) {
		t.Fatalf("Redact = %+v, %v, want the body unchanged with no error", res, err)
	}
}

func TestEngine_Redact_EveryCategoryDisabledNeverInspectsTheBody(t *testing.T) {
	eng := &Engine{patterns: map[Category][]*regexp.Regexp{}, providerName: "anthropic"}
	malformed := []byte(`{"model":`) // not even valid JSON, on a target path

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: malformed})
	if err != nil {
		t.Fatalf("Redact: %v, want nil: every category disabled must not even parse the body", err)
	}
	if string(res.Body) != string(malformed) || res.Counts != nil {
		t.Fatalf("Result = %+v, want the body unchanged", res)
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
			eng := testEngine()
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

// --- Failure: internal (panic recovery) ------------------------------------------

func TestEngine_Redact_PanicDuringRedactionIsRecoveredWithoutLeakingItsValue(t *testing.T) {
	// There is no cache left to fail on demand, so the panic is forced via
	// a deliberately broken pattern set: a nil *regexp.Regexp panics as
	// soon as redactText tries to match against it, standing in for "an
	// internal failure while scanning" (specs/0007-technical, Design).
	const secretText = "do-not-leak-this-value"
	eng := &Engine{patterns: map[Category][]*regexp.Regexp{CategoryEmail: {nil}}, providerName: "anthropic"}
	body := []byte(sysBody(secretText))

	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: body})

	var fe *FilterError
	if !errors.As(err, &fe) || fe.Kind != "internal" {
		t.Fatalf("err = %v, want *FilterError{Kind: \"internal\"}", err)
	}
	if strings.Contains(err.Error(), secretText) {
		t.Fatalf("err.Error() = %q, leaks the request content present when the panic occurred", err.Error())
	}
	if res.Body != nil || res.Counts != nil {
		t.Fatalf("Result = %+v on a panic, want the zero value", res)
	}
}

// --- No content in outputs --------------------------------------------------------

func TestEngine_Redact_NoMarkerAnywhereInTheOutput(t *testing.T) {
	// The marker is the local-part of an email address, so the entire
	// marker falls inside the detected (and therefore redacted) span —
	// unlike arbitrary surrounding text, which legitimately survives
	// redaction untouched.
	const markerEmail = "zqfiltermarker9@example.com"
	eng := testEngine()
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
}

// --- New -----------------------------------------------------------------------

func TestNew_LogsNothingAboutAFilteringCacheOrItsVolatility(t *testing.T) {
	logger, logs := observedLogger()
	if _, err := New(defaultFilteringConfig(), "anthropic", logger); err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, e := range logs.All() {
		msg := strings.ToLower(e.Message)
		if strings.Contains(msg, "cache") || strings.Contains(msg, "volatil") || strings.Contains(msg, "restart") {
			t.Fatalf("startup logged a line naming a filtering cache or its volatility, want none: %q", e.Message)
		}
	}
}

func TestNew_CategoriesFromConfigRestrictWhichPatternsRun(t *testing.T) {
	cfg := defaultFilteringConfig()
	cfg.Categories = config.CategoriesConfig{Email: false, SSN: true, APIKey: false}
	eng, err := New(cfg, "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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

func TestNew_EveryCategoryDisabledStillSucceedsButRedactIsANoOp(t *testing.T) {
	cfg := defaultFilteringConfig()
	cfg.Categories = config.CategoriesConfig{Email: false, SSN: false, APIKey: false}
	eng, err := New(cfg, "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v, want success: there is nothing left that can fail with every category off", err)
	}

	malformed := []byte(`{"model":`)
	res, err := eng.Redact(context.Background(), Request{CallID: "c1", Path: "/v1/messages", Body: malformed})
	if err != nil {
		t.Fatalf("Redact: %v, want nil: nothing to scan, so a malformed body is not even inspected", err)
	}
	if string(res.Body) != string(malformed) {
		t.Fatalf("Body = %s, want the original bytes", res.Body)
	}
}
