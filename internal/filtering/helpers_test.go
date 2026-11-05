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

	exAPIKeyOpenAI        = "sk-" + strings.Repeat("A", 24)
	exAPIKeyOpenAIProject = "sk-proj-" + strings.Repeat("Ab1_-", 10) // '_' and '-' inside the body
	exAPIKeyGitHub        = "ghp_" + strings.Repeat("A", 36)
	exAPIKeyAWS           = "AKIA" + strings.Repeat("A", 16)
	exAPIKeySlack         = "xoxb-" + strings.Repeat("A", 20)
	exAPIKeyGoogle        = "AIza" + strings.Repeat("A", 35)

	// exNoPrefixHex looks like a secret (40 hex characters, the shape of a
	// SHA-1 or a GitHub token stripped of its prefix) but carries none of
	// the vendor prefixes patterns.go matches.
	exNoPrefixHex = strings.Repeat("a1b2c3d4", 5)
)

// --- Engine construction for white-box tests --------------------------------
//
// Engine's own doc comment names its unexported field (patterns), so
// tests build one directly rather than going through New. Which requests
// are targets is decided by Request.Format (specs/0009), not anything
// stored on the Engine.

func testEngine() *Engine {
	return testEngineWithCategories(CategoryEmail, CategorySSN, CategoryAPIKey)
}

func testEngineWithCategories(cats ...Category) *Engine {
	patterns := map[Category][]*regexp.Regexp{}
	for _, c := range cats {
		patterns[c] = categoryPatterns[c]
	}
	return &Engine{patterns: patterns}
}

// --- Placeholder parsing -----------------------------------------------------

var placeholderPattern = regexp.MustCompile(`\[REDACTED:(email|ssn|api_key)\]`)

// placeholders returns the category of every placeholder found in body, in
// order of appearance.
func placeholders(body []byte) []string {
	var out []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(string(body), -1) {
		out = append(out, m[1])
	}
	return out
}
