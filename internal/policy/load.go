package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"
	"github.com/open-policy-agent/opa/v1/util"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

const (
	denyQuery     = "data.cardea.policy.deny"
	reservedRoot  = "cardea"
	policyPackage = "data.cardea.policy"
)

// removedBuiltins reach outside the process.
var removedBuiltins = map[string]bool{
	"http.send":          true,
	"net.lookup_ip_addr": true,
	"opa.runtime":        true,
}

type snapshot struct {
	query    rego.PreparedEvalQuery
	revision string
	files    int // data files plus the policy file
}

func isDataExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".yaml", ".yml":
		return true
	}
	return false
}

func loadSnapshot(ctx context.Context, cfg config.PoliciesConfig) (*snapshot, error) {
	if filepath.Ext(cfg.Path) != ".rego" {
		return nil, fmt.Errorf("policy %s: not a .rego file", cfg.Path)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", cfg.Path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("policy %s: not a regular file", cfg.Path)
	}
	src, err := os.ReadFile(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", cfg.Path, err)
	}

	paths, err := dataPaths(cfg.Data)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(cfg.Path))
	h.Write([]byte{0})
	h.Write(src)
	data := map[string]any{}
	owner := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("data file %s: %w", p, err)
		}
		h.Write([]byte{0})
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)

		var v any
		if err := util.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("data file %s: not valid JSON or YAML", p)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("data file %s: top-level value is not an object", p)
		}
		for k, val := range obj {
			if k == reservedRoot {
				return nil, fmt.Errorf("data file %s: top-level key %q is reserved for the policy package", p, k)
			}
			if prev, dup := owner[k]; dup {
				return nil, fmt.Errorf("data files %s and %s both define top-level key %q", prev, p, k)
			}
			owner[k] = p
			data[k] = val
		}
	}
	revision := hex.EncodeToString(h.Sum(nil))[:12]

	module, err := ast.ParseModule(cfg.Path, string(src))
	if err != nil {
		return nil, err
	}
	if module == nil {
		return nil, fmt.Errorf("policy %s: file is empty", cfg.Path)
	}
	if module.Package.Path.String() != policyPackage || !definesDeny(module) {
		return nil, fmt.Errorf("policy %s: policy defines no `deny` in package cardea.policy; it could never deny", cfg.Path)
	}

	caps := ast.CapabilitiesForThisVersion()
	caps.Builtins = slices.DeleteFunc(caps.Builtins, func(b *ast.Builtin) bool { return removedBuiltins[b.Name] })

	prepared, err := rego.New(
		rego.Query(denyQuery),
		rego.ParsedModule(module),
		rego.Store(inmem.NewFromObject(data)),
		rego.Capabilities(caps),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, err
	}

	rs, err := prepared.Eval(ctx, rego.EvalInput(map[string]any{}))
	if err != nil {
		return nil, fmt.Errorf("policy %s: evaluating with an empty input: %w", cfg.Path, err)
	}
	if _, err := decisionRules(rs); err != nil {
		return nil, fmt.Errorf("policy %s: `deny` must be a set of strings", cfg.Path)
	}

	return &snapshot{query: prepared, revision: revision, files: len(paths) + 1}, nil
}

func definesDeny(m *ast.Module) bool {
	for _, r := range m.Rules {
		if ref := r.Head.Ref(); len(ref) > 0 && ref[0].Value.Compare(ast.Var("deny")) == 0 {
			return true
		}
	}
	return false
}

// dataPaths expands files and directories into the data files to load,
// sorted by path.
func dataPaths(configured []string) ([]string, error) {
	var out []string
	for _, p := range configured {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("data path %s: %w", p, err)
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && isDataExt(path) {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("data path %s: %w", p, err)
		}
	}
	slices.Sort(out)
	return out, nil
}
