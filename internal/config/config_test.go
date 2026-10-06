package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentkeeper.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestLoad_PipelineDefaults(t *testing.T) {
	path := writeConfigFile(t, "") // empty file: every value must come from defaults()

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.MaxBodyBytes != 10485760 {
		t.Fatalf("Pipeline.MaxBodyBytes = %d, want %d", cfg.Pipeline.MaxBodyBytes, 10485760)
	}
	if !cfg.Pipeline.Stages.Tracing {
		t.Fatal("Pipeline.Stages.Tracing = false, want true: every stage boolean defaults to true")
	}
}

func TestLoad_PipelineMaxBodyBytesEnvVarOverride(t *testing.T) {
	path := writeConfigFile(t, "")
	t.Setenv("AGENTKEEPER_PIPELINE_MAX_BODY_BYTES", "123")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.MaxBodyBytes != 123 {
		t.Fatalf("Pipeline.MaxBodyBytes = %d, want 123", cfg.Pipeline.MaxBodyBytes)
	}
}

func TestLoad_RejectsUnknownPipelineStageKey(t *testing.T) {
	path := writeConfigFile(t, "pipeline:\n  stages:\n    made_up_stage: true\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown pipeline.stages key = nil error, want error")
	}
}

func TestLoad_RejectsNonPositiveMaxBodyBytes(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"zero", "0"},
		{"negative", "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "pipeline:\n  max_body_bytes: "+tt.value+"\n")
			if _, err := Load(path); err == nil {
				t.Fatalf("Load with max_body_bytes: %s = nil error, want error", tt.value)
			}
		})
	}
}

func TestDefaults_IdentificationDisabledUnlikeTracing(t *testing.T) {
	d := defaults()
	if d.Pipeline.Stages.Identification {
		t.Fatal("defaults().Pipeline.Stages.Identification = true, want false")
	}
	if len(d.Agents) != 0 {
		t.Fatalf("defaults().Agents = %+v, want empty", d.Agents)
	}
	if !d.Pipeline.Stages.Tracing {
		t.Fatal("defaults().Pipeline.Stages.Tracing = false, want true: identification's default deliberately diverges from every other stage's default of true")
	}
}

func TestLoad_IdentificationAndAgentsDefaultToDisabledAndEmpty(t *testing.T) {
	path := writeConfigFile(t, "") // empty file: every value must come from defaults()

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.Identification {
		t.Fatal("Pipeline.Stages.Identification = true, want false by default")
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("Agents = %+v, want empty by default", cfg.Agents)
	}
	if !cfg.Pipeline.Stages.Tracing {
		t.Fatal("Pipeline.Stages.Tracing = false, want true: unlike identification, this stage still defaults to on")
	}
}

func TestLoad_AgentsWellFormedListUnmarshalsCleanly(t *testing.T) {
	yaml := "agents:\n" +
		"  - agent_id: \"acme-bot\"\n" +
		"    api_key_env: \"AGENT_ACME_BOT_API_KEY\"\n" +
		"  - agent_id: \"beta-bot\"\n" +
		"    jwt_secret_env: \"AGENT_BETA_BOT_JWT_SECRET\"\n" +
		"  - agent_id: \"gamma-bot\"\n" +
		"    api_key_env: \"AGENT_GAMMA_BOT_API_KEY\"\n" +
		"    jwt_secret_env: \"AGENT_GAMMA_BOT_JWT_SECRET\"\n"
	path := writeConfigFile(t, yaml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []AgentConfig{
		{AgentID: "acme-bot", APIKeyEnv: "AGENT_ACME_BOT_API_KEY"},
		{AgentID: "beta-bot", JWTSecretEnv: "AGENT_BETA_BOT_JWT_SECRET"},
		{AgentID: "gamma-bot", APIKeyEnv: "AGENT_GAMMA_BOT_API_KEY", JWTSecretEnv: "AGENT_GAMMA_BOT_JWT_SECRET"},
	}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("Agents = %+v, want %+v", cfg.Agents, want)
	}
}

func TestLoad_RejectsDuplicateAgentID(t *testing.T) {
	yaml := "agents:\n" +
		"  - agent_id: \"dup-bot\"\n" +
		"    api_key_env: \"AGENT_DUP_BOT_API_KEY_1\"\n" +
		"  - agent_id: \"dup-bot\"\n" +
		"    api_key_env: \"AGENT_DUP_BOT_API_KEY_2\"\n"
	path := writeConfigFile(t, yaml)

	if _, err := Load(path); err == nil {
		t.Fatal("Load with two agents sharing one agent_id = nil error, want error naming the conflict")
	}
}

func TestLoad_RejectsEmptyAgentID(t *testing.T) {
	yaml := "agents:\n" +
		"  - agent_id: \"\"\n" +
		"    api_key_env: \"AGENT_EMPTY_ID_API_KEY\"\n"
	path := writeConfigFile(t, yaml)

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an empty agent_id = nil error, want error")
	}
}

func TestLoad_RejectsAgentWithNeitherAPIKeyEnvNorJWTSecretEnvSet(t *testing.T) {
	yaml := "agents:\n" +
		"  - agent_id: \"no-credential-bot\"\n"
	path := writeConfigFile(t, yaml)

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an agent entry setting neither api_key_env nor jwt_secret_env = nil error, want error")
	}
}

func TestLoad_IdentificationEnvVarOverride(t *testing.T) {
	path := writeConfigFile(t, "")
	t.Setenv("AGENTKEEPER_PIPELINE_STAGES_IDENTIFICATION", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pipeline.Stages.Identification {
		t.Fatal("Pipeline.Stages.Identification = false, want true: an env var override must be honored")
	}
}

func TestLoad_RejectsUnknownKeyInAgentsEntry(t *testing.T) {
	yaml := "agents:\n" +
		"  - agent_id: \"typo-bot\"\n" +
		"    jwt_secert_env: \"AGENT_TYPO_BOT_JWT_SECRET\"\n" // misspelled key
	path := writeConfigFile(t, yaml)

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown key inside an agents entry = nil error, want error")
	}
}

func TestLoad_TracingDefaults(t *testing.T) {
	path := writeConfigFile(t, "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pipeline.Stages.Tracing {
		t.Fatal("Pipeline.Stages.Tracing = false, want true by default")
	}
	want := TracingConfig{
		FingerprintKeyEnv: "TRACE_FINGERPRINT_KEY",
		MaxReadBytes:      16777216,
		Sink:              TraceSinkConfig{Type: "logging", Path: ""},
	}
	if cfg.Tracing.FingerprintKeyEnv != want.FingerprintKeyEnv || cfg.Tracing.MaxReadBytes != want.MaxReadBytes || cfg.Tracing.Sink != want.Sink {
		t.Fatalf("Tracing = %+v, want %+v", cfg.Tracing, want)
	}
	if len(cfg.Tracing.Sessions) != 0 {
		t.Fatalf("Tracing.Sessions = %+v, want empty by default", cfg.Tracing.Sessions)
	}

	d := defaults()
	if !d.Pipeline.Stages.Tracing || d.Tracing.MaxReadBytes != want.MaxReadBytes || d.Tracing.Sink != want.Sink || d.Tracing.FingerprintKeyEnv != want.FingerprintKeyEnv {
		t.Fatalf("defaults() tracing = %+v / stage %v, want %+v / true", d.Tracing, d.Pipeline.Stages.Tracing, want)
	}
}

func TestLoad_TracingStageExplicitFalseIsHonored(t *testing.T) {
	path := writeConfigFile(t, "pipeline:\n  stages:\n    tracing: false\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.Tracing {
		t.Fatal("Pipeline.Stages.Tracing = true, want false: an explicit false must be honored")
	}
}

func TestLoad_TracingEnvVarOverrides(t *testing.T) {
	tests := []struct {
		env   string
		value string
		check func(Config) bool
	}{
		{"AGENTKEEPER_PIPELINE_STAGES_TRACING", "false", func(c Config) bool { return !c.Pipeline.Stages.Tracing }},
		{"AGENTKEEPER_TRACING_FINGERPRINT_KEY_ENV", "MY_TRACE_KEY", func(c Config) bool { return c.Tracing.FingerprintKeyEnv == "MY_TRACE_KEY" }},
		{"AGENTKEEPER_TRACING_MAX_READ_BYTES", "4096", func(c Config) bool { return c.Tracing.MaxReadBytes == 4096 }},
		{"AGENTKEEPER_TRACING_SINK_TYPE", "logging", func(c Config) bool { return c.Tracing.Sink.Type == "logging" }},
		{"AGENTKEEPER_TRACING_SINK_PATH", "/var/log/agentkeeper/records.jsonl", func(c Config) bool {
			return c.Tracing.Sink.Path == "/var/log/agentkeeper/records.jsonl"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			path := writeConfigFile(t, "")
			t.Setenv(tt.env, tt.value)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !tt.check(cfg) {
				t.Fatalf("%s=%s was not honored: Tracing = %+v, stages = %+v", tt.env, tt.value, cfg.Tracing, cfg.Pipeline.Stages)
			}
		})
	}
}

const tracingAgentsYAML = "agents:\n" +
	"  - agent_id: \"agent-alpha\"\n" +
	"    api_key_env: \"AGENT_ALPHA_API_KEY\"\n" +
	"  - agent_id: \"agent-beta\"\n" +
	"    api_key_env: \"AGENT_BETA_API_KEY\"\n"

func TestLoad_TracingSessionsWellFormedListUnmarshalsCleanly(t *testing.T) {
	yaml := tracingAgentsYAML +
		"tracing:\n" +
		"  sessions:\n" +
		"    - agents: [\"agent-alpha\"]\n" +
		"      sources:\n" +
		"        - body_field: metadata.user_id\n" +
		"          pattern: '_session_([0-9a-f-]{36})$'\n" +
		"    - default: true\n" +
		"      sources:\n" +
		"        - header: X-Session-Id\n" +
		"        - baggage: session.id\n"
	path := writeConfigFile(t, yaml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []SessionSourceList{
		{Agents: []string{"agent-alpha"}, Sources: []SessionSource{{BodyField: "metadata.user_id", Pattern: `_session_([0-9a-f-]{36})$`}}},
		{Default: true, Sources: []SessionSource{{Header: "X-Session-Id"}, {Baggage: "session.id"}}},
	}
	if !reflect.DeepEqual(cfg.Tracing.Sessions, want) {
		t.Fatalf("Tracing.Sessions = %+v, want %+v", cfg.Tracing.Sessions, want)
	}
}

func TestLoad_TracingRejections(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown sink type", "tracing:\n  sink:\n    type: kafka\n"},
		{"zero max_read_bytes", "tracing:\n  max_read_bytes: 0\n"},
		{"negative max_read_bytes", "tracing:\n  max_read_bytes: -1\n"},
		{
			"sessions entry with neither agents nor default",
			"tracing:\n  sessions:\n    - sources:\n        - header: X-Session-Id\n",
		},
		{
			"sessions entry with empty agents and no default",
			"tracing:\n  sessions:\n    - agents: []\n      sources:\n        - header: X-Session-Id\n",
		},
		{
			"sessions entry with both agents and default",
			tracingAgentsYAML + "tracing:\n  sessions:\n    - agents: [\"agent-alpha\"]\n      default: true\n      sources:\n        - header: X-Session-Id\n",
		},
		{
			"default given twice",
			"tracing:\n  sessions:\n    - default: true\n      sources:\n        - header: A\n    - default: true\n      sources:\n        - header: B\n",
		},
		{
			"agent in two entries",
			tracingAgentsYAML + "tracing:\n  sessions:\n" +
				"    - agents: [\"agent-alpha\"]\n      sources:\n        - header: A\n" +
				"    - agents: [\"agent-beta\", \"agent-alpha\"]\n      sources:\n        - header: B\n",
		},
		{
			"agent not registered in agents[]",
			tracingAgentsYAML + "tracing:\n  sessions:\n    - agents: [\"agent-gamma\"]\n      sources:\n        - header: X-Session-Id\n",
		},
		{
			"entry with no sources",
			"tracing:\n  sessions:\n    - default: true\n      sources: []\n",
		},
		{
			"source setting no kind",
			"tracing:\n  sessions:\n    - default: true\n      sources:\n        - pattern: '(.+)'\n",
		},
		{
			"source setting two kinds",
			"tracing:\n  sessions:\n    - default: true\n      sources:\n        - header: X-Session-Id\n          baggage: session.id\n",
		},
		{
			"unknown key in a sessions entry",
			"tracing:\n  sessions:\n    - defualt: true\n      sources:\n        - header: X-Session-Id\n",
		},
		{
			"unknown key in a source",
			"tracing:\n  sessions:\n    - default: true\n      sources:\n        - headr: X-Session-Id\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, tt.yaml)
			if cfg, err := Load(path); err == nil {
				t.Fatalf("Load = nil error, want an error; Tracing = %+v", cfg.Tracing)
			}
		})
	}
}

// --- policies (specs/0004) -----------------------------------------------------

func TestLoad_PoliciesDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pipeline.Stages.Policies {
		t.Fatal("Pipeline.Stages.Policies = false, want true: every stage boolean defaults to true (an unregistered key would silently pull the stage out of the path)")
	}
	p := cfg.Policies
	if p.Path != "" {
		t.Fatalf("Policies.Path = %q, want empty (no policy)", p.Path)
	}
	if len(p.Data) != 0 {
		t.Fatalf("Policies.Data = %v, want empty", p.Data)
	}
	if p.Watch {
		t.Fatal("Policies.Watch = true, want false")
	}
	if p.WatchSettle != 500*time.Millisecond {
		t.Fatalf("Policies.WatchSettle = %v, want 500ms", p.WatchSettle)
	}
	if p.EvalTimeout != 100*time.Millisecond {
		t.Fatalf("Policies.EvalTimeout = %v, want 100ms", p.EvalTimeout)
	}
}

func TestDefaults_Policies(t *testing.T) {
	d := defaults()
	if !d.Pipeline.Stages.Policies {
		t.Fatal("defaults().Pipeline.Stages.Policies = false, want true")
	}
	if d.Policies.WatchSettle != 500*time.Millisecond || d.Policies.EvalTimeout != 100*time.Millisecond {
		t.Fatalf("defaults().Policies = %+v, want 500ms settle and 100ms eval timeout", d.Policies)
	}
}

func TestLoad_PoliciesFromFile(t *testing.T) {
	path := writeConfigFile(t, `
pipeline:
  stages:
    policies: false
policies:
  path: /etc/cardea/policy.rego
  data: [/etc/cardea/data, /etc/cardea/extra.json]
  watch: true
  watch_settle: 2s
  eval_timeout: 250ms
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.Policies {
		t.Fatal("Pipeline.Stages.Policies = true, want the explicit false honored")
	}
	want := PoliciesConfig{
		Path:        "/etc/cardea/policy.rego",
		Data:        []string{"/etc/cardea/data", "/etc/cardea/extra.json"},
		Watch:       true,
		WatchSettle: 2 * time.Second,
		EvalTimeout: 250 * time.Millisecond,
	}
	if !reflect.DeepEqual(cfg.Policies, want) {
		t.Fatalf("Policies = %+v, want %+v", cfg.Policies, want)
	}
}

func TestLoad_PoliciesEnvVarOverrides(t *testing.T) {
	t.Setenv("AGENTKEEPER_PIPELINE_STAGES_POLICIES", "false")
	t.Setenv("AGENTKEEPER_POLICIES_PATH", "/env/policy.rego")
	t.Setenv("AGENTKEEPER_POLICIES_DATA", "/env/a.json,/env/b")
	t.Setenv("AGENTKEEPER_POLICIES_WATCH", "true")
	t.Setenv("AGENTKEEPER_POLICIES_WATCH_SETTLE", "1s")
	t.Setenv("AGENTKEEPER_POLICIES_EVAL_TIMEOUT", "30ms")

	cfg, err := Load(writeConfigFile(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.Policies {
		t.Fatal("AGENTKEEPER_PIPELINE_STAGES_POLICIES=false was ignored")
	}
	want := PoliciesConfig{
		Path:        "/env/policy.rego",
		Data:        []string{"/env/a.json", "/env/b"},
		Watch:       true,
		WatchSettle: time.Second,
		EvalTimeout: 30 * time.Millisecond,
	}
	if !reflect.DeepEqual(cfg.Policies, want) {
		t.Fatalf("Policies = %+v, want %+v", cfg.Policies, want)
	}
}

func TestLoad_PoliciesEnvVarOverridesFile(t *testing.T) {
	path := writeConfigFile(t, "policies:\n  path: /file/policy.rego\n")
	t.Setenv("AGENTKEEPER_POLICIES_PATH", "/env/policy.rego")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Policies.Path != "/env/policy.rego" {
		t.Fatalf("Policies.Path = %q, want the env var to win", cfg.Policies.Path)
	}
}

func TestLoad_PoliciesRejections(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown key under policies", "policies:\n  pth: /x.rego\n"},
		{"eval_timeout zero", "policies:\n  eval_timeout: 0s\n"},
		{"eval_timeout negative", "policies:\n  eval_timeout: -1s\n"},
		{"watch_settle zero", "policies:\n  watch_settle: 0s\n"},
		{"watch_settle negative", "policies:\n  watch_settle: -5ms\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, tt.yaml)); err == nil {
				t.Fatalf("Load(%q) = nil error, want error", tt.yaml)
			}
		})
	}
}

func TestLoad_NonexistentPolicyPathIsNotRejectedByLoad(t *testing.T) {
	// policy.New owns that check, since loading needs the filesystem anyway.
	if _, err := Load(writeConfigFile(t, "policies:\n  path: /no/such/policy.rego\n")); err != nil {
		t.Fatalf("Load: %v, want nil: a missing policy file is policy.New's error", err)
	}
}
