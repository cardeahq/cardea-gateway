package identification

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeAgentType writes one agent-type YAML file into dir, named filename.
func writeAgentType(t *testing.T, dir, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing agent-type file %s: %v", filename, err)
	}
}

func reqWithHeader(t *testing.T, header, value string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	return req
}

// --- empty / unconfigured directory ------------------------------------------

func TestNewRegistry_EmptyDirStringSucceedsWithZeroTypes(t *testing.T) {
	reg, err := NewRegistry("")
	if err != nil {
		t.Fatalf(`NewRegistry(""): %v, want success (no agent-types directory configured)`, err)
	}
	if reg == nil {
		t.Fatal(`NewRegistry("") returned a nil *Registry with a nil error`)
	}
	if ids := reg.IDs(); len(ids) != 0 {
		t.Fatalf("IDs() = %v, want empty", ids)
	}
}

func TestRegistry_Identify_EmptyRegistryAlwaysReturnsUnknown(t *testing.T) {
	reg, err := NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tests := []struct {
		name   string
		header string
		value  string
	}{
		{"no headers at all", "", ""},
		{"a header that would match something, if anything were configured", "X-Claude-Code-Session-Id", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.Identify(reqWithHeader(t, tt.header, tt.value))
			if got != Unknown {
				t.Fatalf("Identify(...) = %q, want %q", got, Unknown)
			}
		})
	}
}

// --- a directory with one well-formed file -----------------------------------

func TestRegistry_Identify_MatchesByValuePrefixAndPattern(t *testing.T) {
	tests := []struct {
		name          string
		fileContents  string
		matchHeader   string
		matchValue    string
		nonMatchValue string
	}{
		{
			name: "exact value match",
			fileContents: "agent_id: \"value-agent\"\n" +
				"match:\n" +
				"  - header: \"X-Agent-Marker\"\n" +
				"    value: \"exact-value\"\n",
			matchHeader:   "X-Agent-Marker",
			matchValue:    "exact-value",
			nonMatchValue: "not-the-value",
		},
		{
			name: "prefix match",
			fileContents: "agent_id: \"prefix-agent\"\n" +
				"match:\n" +
				"  - header: \"User-Agent\"\n" +
				"    prefix: \"my-sdk/\"\n",
			matchHeader:   "User-Agent",
			matchValue:    "my-sdk/1.2.3",
			nonMatchValue: "other-sdk/1.2.3",
		},
		{
			name: "pattern match",
			fileContents: "agent_id: \"pattern-agent\"\n" +
				"match:\n" +
				"  - header: \"X-Session-Marker\"\n" +
				"    pattern: \"^sess-[0-9]+$\"\n",
			matchHeader:   "X-Session-Marker",
			matchValue:    "sess-42",
			nonMatchValue: "not-a-session",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAgentType(t, dir, "a.yaml", tt.fileContents)
			reg, err := NewRegistry(dir)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}

			var wantID string
			switch tt.name {
			case "exact value match":
				wantID = "value-agent"
			case "prefix match":
				wantID = "prefix-agent"
			case "pattern match":
				wantID = "pattern-agent"
			}

			if got := reg.Identify(reqWithHeader(t, tt.matchHeader, tt.matchValue)); got != wantID {
				t.Fatalf("Identify(matching request) = %q, want %q", got, wantID)
			}
			if got := reg.Identify(reqWithHeader(t, tt.matchHeader, tt.nonMatchValue)); got != Unknown {
				t.Fatalf("Identify(non-matching request) = %q, want %q", got, Unknown)
			}
			if got := reg.Identify(reqWithHeader(t, "", "")); got != Unknown {
				t.Fatalf("Identify(no headers at all) = %q, want %q", got, Unknown)
			}
		})
	}
}

// --- precedence: first-match-by-filename wins --------------------------------

func TestRegistry_Identify_PrecedenceIsAlphabeticalByFilenameNotRegistrationOrder(t *testing.T) {
	// Both files' rules match the same request (the same header/value); the
	// alphabetically-first filename must win, regardless of which file we
	// write to disk first.
	fileA := "agent_id: \"agent-a\"\n" +
		"match:\n" +
		"  - header: \"X-Overlap\"\n" +
		"    value: \"shared\"\n"
	fileB := "agent_id: \"agent-b\"\n" +
		"match:\n" +
		"  - header: \"X-Overlap\"\n" +
		"    value: \"shared\"\n"

	tests := []struct {
		name          string
		writeAThenB   bool
		filenameA     string
		filenameB     string
		wantWinningID string
	}{
		{
			name:          "a.yaml written before b.yaml, a.yaml sorts first and wins",
			writeAThenB:   true,
			filenameA:     "a.yaml",
			filenameB:     "b.yaml",
			wantWinningID: "agent-a",
		},
		{
			name:          "b.yaml written before a.yaml, a.yaml still sorts first and still wins",
			writeAThenB:   false,
			filenameA:     "a.yaml",
			filenameB:     "b.yaml",
			wantWinningID: "agent-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.writeAThenB {
				writeAgentType(t, dir, tt.filenameA, fileA)
				writeAgentType(t, dir, tt.filenameB, fileB)
			} else {
				writeAgentType(t, dir, tt.filenameB, fileB)
				writeAgentType(t, dir, tt.filenameA, fileA)
			}
			reg, err := NewRegistry(dir)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			got := reg.Identify(reqWithHeader(t, "X-Overlap", "shared"))
			if got != tt.wantWinningID {
				t.Fatalf("Identify(...) = %q, want %q (alphabetically-first filename)", got, tt.wantWinningID)
			}
		})
	}
}

// --- the actual shipped presets (functional spec acceptance criterion 8) ----

// shippedPresetsDir locates configs/identification.example relative to this
// package, the same files an operator would copy into their own
// agent_types_dir.
const shippedPresetsDir = "../../configs/identification.example"

func TestRegistry_Identify_ShippedClaudeCodeAndOpenCodePresets(t *testing.T) {
	if _, err := os.Stat(shippedPresetsDir); err != nil {
		t.Fatalf("shipped presets directory %s is missing: %v", shippedPresetsDir, err)
	}
	reg, err := NewRegistry(shippedPresetsDir)
	if err != nil {
		t.Fatalf("NewRegistry(%s): %v", shippedPresetsDir, err)
	}

	tests := []struct {
		name   string
		header string
		value  string
		wantID string
	}{
		{
			name:   "Claude Code's session-id header",
			header: "X-Claude-Code-Session-Id",
			value:  "any-non-empty-session-id",
			wantID: "claude-code",
		},
		{
			name:   "Claude Code's user-agent prefix",
			header: "User-Agent",
			value:  "claude-cli/1.0.0",
			wantID: "claude-code",
		},
		{
			name:   "Open Code's session header",
			header: "x-opencode-session",
			value:  "any-non-empty-session-id",
			wantID: "opencode",
		},
		{
			name:   "neither agent's headers present",
			header: "",
			value:  "",
			wantID: Unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.Identify(reqWithHeader(t, tt.header, tt.value))
			if got != tt.wantID {
				t.Fatalf("Identify(%s) = %q, want %q", tt.name, got, tt.wantID)
			}
		})
	}
}

// --- rejected at NewRegistry ---------------------------------------------------

func TestNewRegistry_RejectsNonexistentConfiguredDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with a nonexistent configured directory = nil error, want error")
	}
}

func TestNewRegistry_RejectsTwoFilesSharingAgentID(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"dup-agent\"\nmatch:\n  - header: \"X-A\"\n    value: \"a\"\n")
	writeAgentType(t, dir, "b.yaml", "agent_id: \"dup-agent\"\nmatch:\n  - header: \"X-B\"\n    value: \"b\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with two files sharing an agent_id = nil error, want error naming the conflict")
	}
}

func TestNewRegistry_RejectsAgentIDEqualToUnknown(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"unknown\"\nmatch:\n  - header: \"X-A\"\n    value: \"a\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal(`NewRegistry with agent_id: "unknown" = nil error, want error`)
	}
}

func TestNewRegistry_RejectsMatchRuleSettingZeroOfValuePrefixPattern(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"no-rule-kind\"\nmatch:\n  - header: \"X-A\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with a match rule setting none of value/prefix/pattern = nil error, want error")
	}
}

func TestNewRegistry_RejectsMatchRuleSettingMoreThanOneOfValuePrefixPattern(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"two-rule-kinds\"\nmatch:\n  - header: \"X-A\"\n    value: \"a\"\n    prefix: \"a\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with a match rule setting two of value/prefix/pattern = nil error, want error")
	}
}

func TestNewRegistry_RejectsInvalidRE2Pattern(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"bad-pattern\"\nmatch:\n  - header: \"X-A\"\n    pattern: \"(unclosed\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with a pattern that does not compile as RE2 = nil error, want error")
	}
}

func TestNewRegistry_RejectsFileWithNoAgentID(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "match:\n  - header: \"X-A\"\n    value: \"a\"\n")

	if _, err := NewRegistry(dir); err == nil {
		t.Fatal("NewRegistry with a file lacking agent_id = nil error, want error")
	}
}

// --- IDs() ----------------------------------------------------------------------

func TestRegistry_IDs_ReturnsEveryLoadedTypeAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	writeAgentType(t, dir, "a.yaml", "agent_id: \"agent-a\"\nmatch:\n  - header: \"X-A\"\n    value: \"a\"\n")
	writeAgentType(t, dir, "b.yaml", "agent_id: \"agent-b\"\nmatch:\n  - header: \"X-B\"\n    value: \"b\"\n")

	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ids := reg.IDs()
	if len(ids) != 2 {
		t.Fatalf("IDs() = %v, want exactly 2 entries", ids)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got["agent-a"] || !got["agent-b"] {
		t.Fatalf("IDs() = %v, want {agent-a, agent-b}", ids)
	}
}

func TestRegistry_IDs_EmptyRegistryReturnsEmpty(t *testing.T) {
	reg, err := NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if ids := reg.IDs(); len(ids) != 0 {
		t.Fatalf("IDs() = %v, want empty", ids)
	}
}
