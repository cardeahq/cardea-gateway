package identification

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// agentTypeFile is one agent-type definition as it appears on disk.
type agentTypeFile struct {
	AgentID string      `yaml:"agent_id"`
	Match   []MatchRule `yaml:"match"`
}

// NewRegistry scans dir for *.yaml/*.yml files, each one agent-type
// definition, sorted by filename (Go's os.ReadDir order). dir == ""
// returns an empty, valid Registry — every call resolves to Unknown — it
// is not an error to run identification with no agent-type directory
// configured. It fails if dir is set but does not exist, if two files
// share an agent_id, if a file's agent_id is Unknown, or if any match
// rule does not set exactly one of Value/Prefix/Pattern, or sets a
// Pattern that does not compile as RE2.
func NewRegistry(dir string) (*Registry, error) {
	if dir == "" {
		return &Registry{}, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading agent_types_dir %q: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	reg := &Registry{}
	seen := make(map[string]string, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading agent-type file %s: %w", name, err)
		}
		var f agentTypeFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parsing agent-type file %s: %w", name, err)
		}
		if f.AgentID == "" {
			return nil, fmt.Errorf("agent-type file %s: agent_id is required", name)
		}
		if f.AgentID == Unknown {
			return nil, fmt.Errorf("agent-type file %s: agent_id must not be the reserved name %q", name, Unknown)
		}
		if other, ok := seen[f.AgentID]; ok {
			return nil, fmt.Errorf("agent-type files %s and %s share agent_id %q", other, name, f.AgentID)
		}
		seen[f.AgentID] = name

		rules := make([]compiledRule, 0, len(f.Match))
		for _, m := range f.Match {
			c, err := compileRule(m)
			if err != nil {
				return nil, fmt.Errorf("agent-type file %s: %w", name, err)
			}
			rules = append(rules, c)
		}
		reg.types = append(reg.types, agentType{id: f.AgentID, rules: rules})
	}

	return reg, nil
}
