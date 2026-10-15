package filtering

import (
	"regexp"
	"strings"
)

// --- Example PII values, one per pattern in patterns.go -------------------

var (
	exEmail = "person@example.com"

	exSSNUS = "123-45-6789"     // area 123, group 45, serial 6789: none reserved
	exSSNUK = "AB123456C"       // valid NINO: letters/digits/suffix all in range
	exSSNFR = "185067500312345" // sex 1, YY 85, MM 06, dept 75, commune 003, order 123, key 45

	exAPIKeyOpenAI = "sk-" + strings.Repeat("A", 24)
	exAPIKeyGitHub = "ghp_" + strings.Repeat("A", 36)
	exAPIKeyAWS    = "AKIA" + strings.Repeat("A", 16)
	exAPIKeySlack  = "xoxb-" + strings.Repeat("A", 20)
	exAPIKeyGoogle = "AIza" + strings.Repeat("A", 35)

	// exNoPrefixHex looks like a secret (40 hex characters, the shape of a
	// SHA-1 or a GitHub token stripped of its prefix) but carries none of
	// the vendor prefixes patterns.go matches.
	exNoPrefixHex = strings.Repeat("a1b2c3d4", 5)
)

// --- Engine construction for white-box tests --------------------------------
//
// Engine's own doc comment names its unexported fields (patterns,
// providerName), so tests build one directly rather than going through New.

func testEngine() *Engine {
	return testEngineWithCategories(CategoryEmail, CategorySSN, CategoryAPIKey)
}

func testEngineWithCategories(cats ...Category) *Engine {
	patterns := map[Category][]*regexp.Regexp{}
	for _, c := range cats {
		patterns[c] = categoryPatterns[c]
	}
	return &Engine{patterns: patterns, providerName: "anthropic"}
}

// --- Placeholder parsing -----------------------------------------------------

var placeholderPattern = regexp.MustCompile(`\[(email|ssn|api_key):([0-9a-f-]{36})\]`)

// placeholders returns every (category, uuid) pair found in body, in order
// of appearance.
func placeholders(body []byte) [][2]string {
	var out [][2]string
	for _, m := range placeholderPattern.FindAllStringSubmatch(string(body), -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}
