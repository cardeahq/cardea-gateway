package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

const okRule = `deny contains "r" if { input.model == "m" }`

func loadErr(t *testing.T, cfg config.PoliciesConfig) error {
	t.Helper()
	cfg.EvalTimeout = 5e9
	cfg.WatchSettle = 5e7
	e, err := New(cfg, "anthropic", zaptest.NewLogger(t))
	if err == nil && e != nil {
		t.Logf("New succeeded with revision %q", e.Revision())
	}
	return err
}

func TestNew_RejectsBadPolicyFiles(t *testing.T) {
	dir := t.TempDir()
	notRego := writeFile(t, dir, "policy.txt", regoHeader+okRule)
	asDir := filepath.Join(dir, "adir.rego")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		path     string
		wantFile string // base name the error must mention
	}{
		{"missing policy file", filepath.Join(dir, "missing.rego"), "missing.rego"},
		{"not a .rego file", notRego, "policy.txt"},
		{"a directory named like a rego file", asDir, "adir.rego"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadErr(t, config.PoliciesConfig{Path: tt.path})
			if err == nil {
				t.Fatal("New = nil error, want an error naming the file")
			}
			if !strings.Contains(err.Error(), tt.wantFile) {
				t.Fatalf("error %q does not name %q", err, tt.wantFile)
			}
		})
	}
}

func TestNew_SyntaxErrorNamesFileAndRow(t *testing.T) {
	path := writeFile(t, t.TempDir(), "broken.rego", "package cardea.policy\n\ndeny contains \"x\" if { input.a == }\n")
	err := loadErr(t, config.PoliciesConfig{Path: path})
	if err == nil {
		t.Fatal("New = nil error, want a parse error")
	}
	if !strings.Contains(err.Error(), "broken.rego") || !strings.Contains(err.Error(), ":3") {
		t.Fatalf("error %q does not name the file and row 3", err)
	}
}

func TestNew_RejectsInvalidPolicies(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantSub string // must appear in the error, besides the file name
	}{
		{
			name:    "http.send is removed",
			src:     regoHeader + "deny contains \"x\" if {\n\thttp.send({\"method\": \"get\", \"url\": \"http://127.0.0.1:1\"})\n}\n",
			wantSub: "http.send",
		},
		{
			name:    "opa.runtime is removed",
			src:     regoHeader + "deny contains \"x\" if {\n\topa.runtime().env\n}\n",
			wantSub: "opa.runtime",
		},
		{
			name:    "net.lookup_ip_addr is removed",
			src:     regoHeader + "deny contains \"x\" if {\n\tnet.lookup_ip_addr(\"localhost\")\n}\n",
			wantSub: "net.lookup_ip_addr",
		},
		{
			name: "deny defined as an object",
			src:  regoHeader + "deny := {\"a\": 1}\n",
		},
		{
			name: "misspelled package: no deny in cardea.policy",
			src:  "package cardea.polcy\n\ndeny contains \"x\" if { true }\n",
		},
		{
			name: "no package cardea at all",
			src:  "package other\n\ndeny contains \"x\" if { true }\n",
		},
		{
			name: "only helpers, no deny",
			src:  regoHeader + "helper := 1\n",
		},
		{
			name: "empty policy file",
			src:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "policy.rego", tt.src)
			err := loadErr(t, config.PoliciesConfig{Path: path})
			if err == nil {
				t.Fatal("New = nil error, want it to fail so serve exits before listening")
			}
			if !strings.Contains(err.Error(), "policy.rego") {
				t.Fatalf("error %q does not name the file", err)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestNew_AcceptsHelperPackagesAlongsideDeny(t *testing.T) {
	src := regoHeader + "bad_model(m) if { m == \"x\" }\n\ndeny contains \"r\" if { bad_model(input.model) }\n"
	path := writeFile(t, t.TempDir(), "policy.rego", src)
	if err := loadErr(t, config.PoliciesConfig{Path: path}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNew_DataFileProblems(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+okRule)

	t.Run("missing data path", func(t *testing.T) {
		err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{filepath.Join(dir, "nope.json")}})
		if err == nil || !strings.Contains(err.Error(), "nope.json") {
			t.Fatalf("error = %v, want one naming nope.json", err)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		bad := writeFile(t, dir, "bad.json", `{"a":`)
		err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{bad}})
		if err == nil || !strings.Contains(err.Error(), "bad.json") {
			t.Fatalf("error = %v, want one naming bad.json", err)
		}
	})
	t.Run("top-level value is not an object", func(t *testing.T) {
		arr := writeFile(t, dir, "arr.json", `["a","b"]`)
		err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{arr}})
		if err == nil || !strings.Contains(err.Error(), "arr.json") {
			t.Fatalf("error = %v, want one naming arr.json", err)
		}
	})
	t.Run("two files define the same top-level key", func(t *testing.T) {
		a := writeFile(t, dir, "one.json", `{"admins":["a"]}`)
		b := writeFile(t, dir, "two.yaml", "admins:\n  - b\n")
		err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{a, b}})
		if err == nil {
			t.Fatal("New = nil error, want a collision error")
		}
		if !strings.Contains(err.Error(), "one.json") || !strings.Contains(err.Error(), "two.yaml") {
			t.Fatalf("error %q does not name both files", err)
		}
	})
	t.Run("the key cardea is reserved", func(t *testing.T) {
		c := writeFile(t, dir, "shadow.json", `{"cardea":{"policy":{"deny":[]}}}`)
		err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{c}})
		if err == nil || !strings.Contains(err.Error(), "shadow.json") {
			t.Fatalf("error = %v, want one naming shadow.json", err)
		}
	})
	t.Run("distinct top-level keys across files are merged", func(t *testing.T) {
		d := t.TempDir()
		a := writeFile(t, d, "a.json", `{"alpha":1}`)
		b := writeFile(t, d, "b.yaml", "beta: 2\n")
		if err := loadErr(t, config.PoliciesConfig{Path: path, Data: []string{a, b}}); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
}

func TestNew_DataDirectoryKeepsOnlyJSONAndYAML(t *testing.T) {
	policyDir := t.TempDir()
	path := writeFile(t, policyDir, "policy.rego", regoHeader+`deny contains "hit" if { input.model in data.allowlist_models }`)
	dataDir := t.TempDir()
	writeFile(t, dataDir, "models.json", `{"allowlist_models":["m"]}`)
	writeFile(t, dataDir, "sub/more.yml", "extra: 1\n")
	writeFile(t, dataDir, "policy_test.rego", "package cardea.policy\n\nthis is not valid rego\n")
	writeFile(t, dataDir, "notes.txt", "not data")
	writeFile(t, dataDir, "README.md", "# not data {")
	writeFile(t, dataDir, "broken.toml", "= = =")

	e, err := New(cfgFor(path, dataDir), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: unrelated files in a data directory must be ignored: %v", err)
	}
	res, err := e.Evaluate(t.Context(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || len(res.Rules) != 1 || res.Rules[0] != "hit" {
		t.Fatalf("rules=%v err=%v, want the data in the directory (including subdirectories) loaded", res.Rules, err)
	}
}

func TestRevision(t *testing.T) {
	policyDir := t.TempDir()
	path := writeFile(t, policyDir, "policy.rego", regoHeader+okRule)
	dataDir := t.TempDir()
	writeFile(t, dataDir, "a.json", `{"a":1}`)
	writeFile(t, dataDir, "b.json", `{"b":2}`)
	writeFile(t, dataDir, "c.yaml", "c: 3\n")

	rev := func(t *testing.T, cfg config.PoliciesConfig) string {
		t.Helper()
		e, err := New(cfg, "anthropic", zaptest.NewLogger(t))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return e.Revision()
	}

	base := rev(t, cfgFor(path, dataDir))
	if !revisionPattern.MatchString(base) {
		t.Fatalf("revision %q, want 12 hex characters", base)
	}

	t.Run("stable across loads", func(t *testing.T) {
		if got := rev(t, cfgFor(path, dataDir)); got != base {
			t.Fatalf("revision %q != %q for identical files", got, base)
		}
	})
	t.Run("independent of modification time", func(t *testing.T) {
		future := time.Now().Add(48 * time.Hour)
		for _, f := range []string{path, filepath.Join(dataDir, "a.json")} {
			if err := os.Chtimes(f, future, future); err != nil {
				t.Fatal(err)
			}
		}
		if got := rev(t, cfgFor(path, dataDir)); got != base {
			t.Fatalf("revision changed with mtime: %q != %q", got, base)
		}
	})
	t.Run("a data directory and the same files listed one by one agree", func(t *testing.T) {
		got := rev(t, cfgFor(path,
			filepath.Join(dataDir, "a.json"), filepath.Join(dataDir, "b.json"), filepath.Join(dataDir, "c.yaml")))
		if got != base {
			t.Fatalf("revision %q != %q: files are hashed in sorted path order however they were named", got, base)
		}
	})
	t.Run("changes with a one-byte edit of the policy", func(t *testing.T) {
		writeFile(t, policyDir, "policy.rego", regoHeader+okRule+" ")
		got := rev(t, cfgFor(path, dataDir))
		if got == base {
			t.Fatal("revision unchanged after editing the policy by one byte")
		}
		writeFile(t, policyDir, "policy.rego", regoHeader+okRule)
	})
	t.Run("changes with a one-byte edit of a data file", func(t *testing.T) {
		writeFile(t, dataDir, "b.json", `{"b":3}`)
		got := rev(t, cfgFor(path, dataDir))
		if got == base {
			t.Fatal("revision unchanged after editing a data file by one byte")
		}
	})
}
