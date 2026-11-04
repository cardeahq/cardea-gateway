package routing

import (
	"testing"
)

func mustTable(t *testing.T, routes []Route, defaults map[Format]string) *Table {
	t.Helper()
	tbl, err := NewTable(routes, defaults)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tbl
}

func anthropicRoute(name string, patterns ...string) Route {
	return Route{Provider: name, Kind: "anthropic", Formats: []Format{FormatAnthropic}, Patterns: patterns}
}

func mistralRoute(name string, patterns ...string) Route {
	return Route{Provider: name, Kind: "mistral", Formats: []Format{FormatOpenAI}, Patterns: patterns}
}

func geminiRoute(name string, patterns ...string) Route {
	return Route{Provider: name, Kind: "gemini", Formats: []Format{FormatGemini}, Patterns: patterns}
}

// --- Resolve: pattern matching ----------------------------------------------

func TestResolve_ExactAndPrefixMatch(t *testing.T) {
	tbl := mustTable(t, []Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-large-latest", "codestral-*"),
	}, nil)

	tests := []struct {
		name   string
		format Format
		model  string
		want   string
	}{
		{"exact match", FormatOpenAI, "mistral-large-latest", "mistral"},
		{"prefix match", FormatOpenAI, "codestral-25.01", "mistral"},
		{"prefix pattern on the other route", FormatAnthropic, "claude-sonnet-5", "anthropic"},
		{"exact pattern does not match a longer name", FormatOpenAI, "mistral-large-latest-2", "mistral"}, // falls to default-less -> no_provider; see below
	}
	for _, tt := range tests[:3] {
		t.Run(tt.name, func(t *testing.T) {
			r, rej := tbl.Resolve(tt.format, tt.model, true)
			if rej != RejectNone {
				t.Fatalf("rejection = %q, want none", rej)
			}
			if r.Provider != tt.want {
				t.Fatalf("provider = %q, want %q", r.Provider, tt.want)
			}
		})
	}

	// "mistral-large-latest" is an exact pattern: a longer name must not match it.
	_, rej := tbl.Resolve(FormatOpenAI, "mistral-large-latest-2", true)
	if rej != RejectNoProvider {
		t.Fatalf("rejection = %q, want %q: an exact pattern must not match as a prefix", rej, RejectNoProvider)
	}
}

func TestResolve_ListOrderBeatsSpecificity(t *testing.T) {
	// AC 11: a provider matching "mistral-*" placed before one matching
	// "mistral-large-latest" wins, even though the second is a more
	// specific match.
	tbl := mustTable(t, []Route{
		mistralRoute("broad", "mistral-*"),
		mistralRoute("specific", "mistral-large-latest"),
	}, nil)

	r, rej := tbl.Resolve(FormatOpenAI, "mistral-large-latest", true)
	if rej != RejectNone {
		t.Fatalf("rejection = %q, want none", rej)
	}
	if r.Provider != "broad" {
		t.Fatalf("provider = %q, want %q: list order must win over specificity", r.Provider, "broad")
	}
}

func TestResolve_TwoProvidersOfTheSameFormat_FirstListedWins(t *testing.T) {
	// AC 10: two Mistral providers that both list "mistral-*".
	tbl := mustTable(t, []Route{
		mistralRoute("mistral-1", "mistral-*"),
		mistralRoute("mistral-2", "mistral-*"),
	}, nil)

	r, rej := tbl.Resolve(FormatOpenAI, "mistral-large-latest", true)
	if rej != RejectNone || r.Provider != "mistral-1" {
		t.Fatalf("provider = %q (rej %q), want mistral-1", r.Provider, rej)
	}
}

func TestResolve_StarAloneMatchesEveryModel(t *testing.T) {
	tbl := mustTable(t, []Route{mistralRoute("catchall", "*")}, nil)
	r, rej := tbl.Resolve(FormatOpenAI, "anything-at-all", true)
	if rej != RejectNone || r.Provider != "catchall" {
		t.Fatalf("provider = %q (rej %q), want catchall", r.Provider, rej)
	}
}

// --- Resolve: format guard --------------------------------------------------

func TestResolve_FormatMismatchIsRejectedEvenWithADefault(t *testing.T) {
	// AC 4: a model matches a provider of the wrong format; even though a
	// default exists for the request's actual format, the format guard
	// rejects before ever consulting the default.
	tbl := mustTable(t, []Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-large-latest"),
	}, map[Format]string{FormatAnthropic: "anthropic"})

	_, rej := tbl.Resolve(FormatAnthropic, "mistral-large-latest", true)
	if rej != RejectFormatMismatch {
		t.Fatalf("rejection = %q, want %q", rej, RejectFormatMismatch)
	}
}

func TestResolve_FormatMismatch_FirstMatchingRouteDecides(t *testing.T) {
	// Rule 6/7: among routes whose pattern matches the model, the first
	// one that speaks the request's format serves it; the format guard
	// only rejects when *none* of the matching routes speak it.
	tbl := mustTable(t, []Route{
		anthropicRoute("wrong-format", "shared-*"),
		mistralRoute("right-format", "shared-*"),
	}, nil)

	r, rej := tbl.Resolve(FormatOpenAI, "shared-model", true)
	if rej != RejectNone || r.Provider != "right-format" {
		t.Fatalf("provider = %q (rej %q), want right-format: the first matching route that speaks the format", r.Provider, rej)
	}
}

// --- Resolve: URL fallback to the default -----------------------------------

func TestResolve_NoModelGoesToTheDefault(t *testing.T) {
	// AC 5, 7, 8
	tbl := mustTable(t, []Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-*"),
	}, map[Format]string{FormatOpenAI: "mistral"})

	tests := []struct {
		name  string
		model string
		found bool
	}{
		{"no model found at all", "", false},
		{"model found but matches no route", "unknown-model", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, rej := tbl.Resolve(FormatOpenAI, tt.model, tt.found)
			if rej != RejectNone || r.Provider != "mistral" {
				t.Fatalf("provider = %q (rej %q), want mistral (the OpenAI-style default)", r.Provider, rej)
			}
		})
	}
}

func TestResolve_NoDefaultIsRejected(t *testing.T) {
	// AC 6
	tbl := mustTable(t, []Route{anthropicRoute("anthropic", "claude-*")}, nil)
	_, rej := tbl.Resolve(FormatAnthropic, "unknown-model", true)
	if rej != RejectNoProvider {
		t.Fatalf("rejection = %q, want %q", rej, RejectNoProvider)
	}
}

// --- Resolve: unknown format -------------------------------------------------

func TestResolve_UnknownFormatIsRejectedWhateverTheModel(t *testing.T) {
	// AC 9
	tbl := mustTable(t, []Route{anthropicRoute("anthropic", "*")}, map[Format]string{FormatAnthropic: "anthropic"})
	_, rej := tbl.Resolve(FormatUnknown, "claude-sonnet-5", true)
	if rej != RejectUnknownFormat {
		t.Fatalf("rejection = %q, want %q", rej, RejectUnknownFormat)
	}
}

// --- Resolve: matching uses the full, untruncated name ----------------------

func TestResolve_MatchesTheFullUntruncatedName(t *testing.T) {
	// Build a model name > MaxModelLen whose full form matches a pattern
	// that only the untruncated suffix would satisfy.
	suffix := "zzz-final-marker"
	pattern := "claude-" + repeat("x", 300) + suffix
	tbl := mustTable(t, []Route{anthropicRoute("anthropic", pattern)}, nil)

	r, rej := tbl.Resolve(FormatAnthropic, pattern, true)
	if rej != RejectNone || r.Provider != "anthropic" {
		t.Fatalf("provider = %q (rej %q), want anthropic: Resolve must match on the full, untruncated model name", r.Provider, rej)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

// --- Single mode -------------------------------------------------------------

func TestResolve_SingleModeResolvesEverything(t *testing.T) {
	tbl := NewSingleTable(Route{Provider: "anthropic", Kind: "anthropic", Formats: []Format{FormatAnthropic}, Single: true})

	tests := []struct {
		name   string
		format Format
		model  string
		found  bool
	}{
		{"known format, model found", FormatAnthropic, "claude-sonnet-5", true},
		{"unknown format", FormatUnknown, "", false},
		{"no model", FormatOpenAI, "", false},
		{"any format, any model", FormatGemini, "gemini-2.5-pro", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, rej := tbl.Resolve(tt.format, tt.model, tt.found)
			if rej != RejectNone {
				t.Fatalf("rejection = %q, want none: single mode never denies", rej)
			}
			if r.Provider != "anthropic" || !r.Single {
				t.Fatalf("route = %+v, want the single anthropic route", r)
			}
		})
	}
	if !tbl.Single() {
		t.Fatal("Single() = false, want true")
	}
}

func TestNewSingleTable_RouteIsReturnedAsGiven(t *testing.T) {
	route := Route{Provider: "custom", Kind: "", Formats: nil, Single: true}
	tbl := NewSingleTable(route)
	r, rej := tbl.Resolve(FormatUnknown, "", false)
	if rej != RejectNone || r.Provider != "custom" {
		t.Fatalf("route = %+v (rej %q), want the passthrough route custom", r, rej)
	}
}

// --- NewTable validation ------------------------------------------------------

func TestNewTable_DefaultNamingAnUnknownProviderIsAnError(t *testing.T) {
	_, err := NewTable([]Route{anthropicRoute("anthropic", "claude-*")}, map[Format]string{FormatAnthropic: "does-not-exist"})
	if err == nil {
		t.Fatal("NewTable = nil error, want an error for a default naming an unlisted provider")
	}
}

func TestNewTable_DefaultOfTheWrongFormatIsAnError(t *testing.T) {
	// AC 19: a default names a provider of a different format.
	_, err := NewTable([]Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-*"),
	}, map[Format]string{FormatAnthropic: "mistral"})
	if err == nil {
		t.Fatal("NewTable = nil error, want an error: mistral does not speak the anthropic format")
	}
}

func TestNewTable_ValidDefaultsSucceed(t *testing.T) {
	_, err := NewTable([]Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-*"),
		geminiRoute("gemini", "gemini-*"),
	}, map[Format]string{FormatOpenAI: "mistral", FormatGemini: "gemini"})
	if err != nil {
		t.Fatalf("NewTable: %v, want success", err)
	}
}

func TestNewTable_NoRoutesNoDefaultsSucceeds(t *testing.T) {
	tbl, err := NewTable(nil, nil)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	_, rej := tbl.Resolve(FormatAnthropic, "anything", true)
	if rej != RejectNoProvider {
		t.Fatalf("rejection = %q, want %q", rej, RejectNoProvider)
	}
}

// --- Routes(), DefaultFormats(), Single() ------------------------------------

func TestTable_RoutesPreservesOrder(t *testing.T) {
	tbl := mustTable(t, []Route{
		anthropicRoute("a", "claude-*"),
		mistralRoute("b", "mistral-*"),
		geminiRoute("c", "gemini-*"),
	}, nil)
	got := tbl.Routes()
	if len(got) != 3 || got[0].Provider != "a" || got[1].Provider != "b" || got[2].Provider != "c" {
		t.Fatalf("Routes() = %+v, want a, b, c in order", got)
	}
}

func TestTable_DefaultFormats(t *testing.T) {
	tbl := mustTable(t, []Route{
		anthropicRoute("anthropic", "claude-*"),
		mistralRoute("mistral", "mistral-*"),
	}, map[Format]string{FormatOpenAI: "mistral"})

	if got := tbl.DefaultFormats("mistral"); len(got) != 1 || got[0] != FormatOpenAI {
		t.Fatalf("DefaultFormats(mistral) = %v, want [openai]", got)
	}
	if got := tbl.DefaultFormats("anthropic"); len(got) != 0 {
		t.Fatalf("DefaultFormats(anthropic) = %v, want none", got)
	}
	if got := tbl.DefaultFormats("no-such-provider"); len(got) != 0 {
		t.Fatalf("DefaultFormats(no-such-provider) = %v, want none", got)
	}
}

func TestTable_SingleFalseInListMode(t *testing.T) {
	tbl := mustTable(t, []Route{anthropicRoute("anthropic", "claude-*")}, nil)
	if tbl.Single() {
		t.Fatal("Single() = true for a list-mode table, want false")
	}
}

func TestRoute_SingleFlagOnlyTrueForNewSingleTablesRoute(t *testing.T) {
	tbl := mustTable(t, []Route{anthropicRoute("anthropic", "claude-*")}, nil)
	r, rej := tbl.Resolve(FormatAnthropic, "claude-sonnet-5", true)
	if rej != RejectNone {
		t.Fatalf("rejection = %q, want none", rej)
	}
	if r.Single {
		t.Fatal("a list-mode route has Single = true, want false")
	}
}
