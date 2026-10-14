package filtering

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
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

// --- fakeCache --------------------------------------------------------------

// fakeCache is a Cache double for tests that need to control or observe
// Set/Get beyond what the real MemoryCache allows: forcing a write
// failure, forcing a panic (standing in for "a redaction helper made to
// panic"), or recording call order. Tests that only need a working cache
// use the real MemoryCache instead (see memory_cache_test.go).
type fakeCache struct {
	mu       sync.Mutex
	entries  map[string]Entry
	sets     []Entry
	setErr   error
	setPanic string // non-empty: Set panics with this value instead of writing
	closed   bool
}

func (c *fakeCache) Set(_ context.Context, key string, entry Entry, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setPanic != "" {
		panic(c.setPanic)
	}
	if c.setErr != nil {
		return c.setErr
	}
	if c.entries == nil {
		c.entries = map[string]Entry{}
	}
	c.entries[key] = entry
	c.sets = append(c.sets, entry)
	return nil
}

func (c *fakeCache) Get(_ context.Context, key string) (Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok, nil
}

func (c *fakeCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeCache) setCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sets)
}

// --- Engine construction for white-box tests --------------------------------
//
// Engine's own doc comment names its unexported fields (patterns, cache,
// ttl, providerName), so tests build one directly rather than going
// through New, which hides the cache a test needs to inspect directly
// (see "No content in outputs" and "write before forward" in
// engine_test.go).

func testEngine(cache Cache) *Engine {
	return testEngineWithCategories(cache, CategoryEmail, CategorySSN, CategoryAPIKey)
}

func testEngineWithCategories(cache Cache, cats ...Category) *Engine {
	patterns := map[Category][]*regexp.Regexp{}
	for _, c := range cats {
		patterns[c] = categoryPatterns[c]
	}
	return &Engine{patterns: patterns, cache: cache, ttl: time.Minute, providerName: "anthropic"}
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
