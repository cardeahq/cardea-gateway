package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cardea-gateway.yaml")
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
	t.Setenv("CARDEA_GATEWAY_PIPELINE_MAX_BODY_BYTES", "123")

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
	t.Setenv("CARDEA_GATEWAY_PIPELINE_STAGES_IDENTIFICATION", "true")

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
		Sink:              TraceSinkConfig{Type: "logging", Path: "", Cardea: wantCardeaDefaults()},
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
		{"CARDEA_GATEWAY_PIPELINE_STAGES_TRACING", "false", func(c Config) bool { return !c.Pipeline.Stages.Tracing }},
		{"CARDEA_GATEWAY_TRACING_FINGERPRINT_KEY_ENV", "MY_TRACE_KEY", func(c Config) bool { return c.Tracing.FingerprintKeyEnv == "MY_TRACE_KEY" }},
		{"CARDEA_GATEWAY_TRACING_MAX_READ_BYTES", "4096", func(c Config) bool { return c.Tracing.MaxReadBytes == 4096 }},
		{"CARDEA_GATEWAY_TRACING_SINK_TYPE", "logging", func(c Config) bool { return c.Tracing.Sink.Type == "logging" }},
		{"CARDEA_GATEWAY_TRACING_SINK_PATH", "/var/log/cardea-gateway/records.jsonl", func(c Config) bool {
			return c.Tracing.Sink.Path == "/var/log/cardea-gateway/records.jsonl"
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
	t.Setenv("CARDEA_GATEWAY_PIPELINE_STAGES_POLICIES", "false")
	t.Setenv("CARDEA_GATEWAY_POLICIES_PATH", "/env/policy.rego")
	t.Setenv("CARDEA_GATEWAY_POLICIES_DATA", "/env/a.json,/env/b")
	t.Setenv("CARDEA_GATEWAY_POLICIES_WATCH", "true")
	t.Setenv("CARDEA_GATEWAY_POLICIES_WATCH_SETTLE", "1s")
	t.Setenv("CARDEA_GATEWAY_POLICIES_EVAL_TIMEOUT", "30ms")

	cfg, err := Load(writeConfigFile(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.Policies {
		t.Fatal("CARDEA_GATEWAY_PIPELINE_STAGES_POLICIES=false was ignored")
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
	t.Setenv("CARDEA_GATEWAY_POLICIES_PATH", "/env/policy.rego")
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

// --- cardea sink (specs/0005) --------------------------------------------------

func wantCardeaDefaults() CardeaSinkConfig {
	return CardeaSinkConfig{
		Endpoint:        "",
		APIKeyEnv:       "CARDEA_API_KEY",
		QueueSize:       10000,
		QueueMaxBytes:   67108864,
		BatchMaxRecords: 500,
		BatchMaxBytes:   1048576,
		BatchMaxDelay:   time.Second,
		RequestTimeout:  10 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	}
}

// cardeaYAML selects the Cardea sink with the two mandatory settings, plus
// extra lines indented under tracing.sink.cardea.
func cardeaYAML(sinkType string, extra ...string) string {
	y := "tracing:\n  sink:\n    type: " + sinkType + "\n    cardea:\n" +
		"      endpoint: https://platform.example.com/ingest\n" +
		"      api_key_env: CARDEA_API_KEY\n"
	for _, e := range extra {
		y += "      " + e + "\n"
	}
	return y
}

func TestLoad_CardeaSinkDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tracing.Sink.Type != "logging" {
		t.Fatalf("Tracing.Sink.Type = %q, want logging by default: sending data off the machine is a deliberate act", cfg.Tracing.Sink.Type)
	}
	if want := wantCardeaDefaults(); cfg.Tracing.Sink.Cardea != want {
		t.Fatalf("Tracing.Sink.Cardea = %+v, want %+v", cfg.Tracing.Sink.Cardea, want)
	}
	if d := defaults().Tracing.Sink.Cardea; d != wantCardeaDefaults() {
		t.Fatalf("defaults().Tracing.Sink.Cardea = %+v, want %+v", d, wantCardeaDefaults())
	}
	if cfg.Tracing.Sink.UsesCardea() || !cfg.Tracing.Sink.UsesLogging() {
		t.Fatalf("default sink: UsesCardea=%v UsesLogging=%v, want false and true", cfg.Tracing.Sink.UsesCardea(), cfg.Tracing.Sink.UsesLogging())
	}
}

func TestLoad_CardeaSinkFromFileKeepsTheOtherDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, cardeaYAML("logging+cardea")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := wantCardeaDefaults()
	want.Endpoint = "https://platform.example.com/ingest"
	want.APIKeyEnv = "CARDEA_API_KEY"
	if cfg.Tracing.Sink.Cardea != want {
		t.Fatalf("Cardea = %+v, want %+v", cfg.Tracing.Sink.Cardea, want)
	}
}

func TestTraceSinkConfig_UsesCardeaAndUsesLogging(t *testing.T) {
	tests := []struct {
		sinkType    string
		wantCardea  bool
		wantLogging bool
	}{
		{"logging", false, true},
		{"cardea", true, false},
		{"logging+cardea", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.sinkType, func(t *testing.T) {
			s := TraceSinkConfig{Type: tt.sinkType}
			if s.UsesCardea() != tt.wantCardea || s.UsesLogging() != tt.wantLogging {
				t.Fatalf("type %q: UsesCardea=%v UsesLogging=%v, want %v and %v", tt.sinkType, s.UsesCardea(), s.UsesLogging(), tt.wantCardea, tt.wantLogging)
			}
		})
	}
}

func TestLoad_CardeaSinkEnvVarOverrides(t *testing.T) {
	// The mandatory settings come from the environment too, so every case
	// is a valid configuration with one field changed.
	base := map[string]string{
		"CARDEA_GATEWAY_TRACING_SINK_TYPE":               "cardea",
		"CARDEA_GATEWAY_TRACING_SINK_CARDEA_ENDPOINT":    "https://platform.example.com/ingest",
		"CARDEA_GATEWAY_TRACING_SINK_CARDEA_API_KEY_ENV": "CARDEA_API_KEY",
	}
	tests := []struct {
		env   string
		value string
		check func(TraceSinkConfig) bool
	}{
		{"CARDEA_GATEWAY_TRACING_SINK_TYPE", "logging+cardea", func(s TraceSinkConfig) bool { return s.Type == "logging+cardea" }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_ENDPOINT", "http://localhost:9000/in", func(s TraceSinkConfig) bool { return s.Cardea.Endpoint == "http://localhost:9000/in" }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_API_KEY_ENV", "OTHER_KEY", func(s TraceSinkConfig) bool { return s.Cardea.APIKeyEnv == "OTHER_KEY" }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_QUEUE_SIZE", "20000", func(s TraceSinkConfig) bool { return s.Cardea.QueueSize == 20000 }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_QUEUE_MAX_BYTES", "134217728", func(s TraceSinkConfig) bool { return s.Cardea.QueueMaxBytes == 134217728 }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_RECORDS", "100", func(s TraceSinkConfig) bool { return s.Cardea.BatchMaxRecords == 100 }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_BYTES", "2097152", func(s TraceSinkConfig) bool { return s.Cardea.BatchMaxBytes == 2097152 }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_DELAY", "2s", func(s TraceSinkConfig) bool { return s.Cardea.BatchMaxDelay == 2*time.Second }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_REQUEST_TIMEOUT", "5s", func(s TraceSinkConfig) bool { return s.Cardea.RequestTimeout == 5*time.Second }},
		{"CARDEA_GATEWAY_TRACING_SINK_CARDEA_SHUTDOWN_TIMEOUT", "3s", func(s TraceSinkConfig) bool { return s.Cardea.ShutdownTimeout == 3*time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			t.Setenv(tt.env, tt.value)
			cfg, err := Load(writeConfigFile(t, ""))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !tt.check(cfg.Tracing.Sink) {
				t.Fatalf("%s=%s was not honored: Sink = %+v", tt.env, tt.value, cfg.Tracing.Sink)
			}
		})
	}
}

func TestLoad_CardeaSinkAcceptsValidConfigurations(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"cardea", cardeaYAML("cardea")},
		{"logging+cardea", cardeaYAML("logging+cardea")},
		{"https endpoint", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "https://platform.example.com", 1)},
		{"http on localhost", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "http://localhost", 1)},
		{"http on localhost with port and path", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "http://localhost:9000/in?x=1", 1)},
		{"http on 127.0.0.1", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "http://127.0.0.1:9", 1)},
		{"http elsewhere in 127.0.0.0/8", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "http://127.5.5.5:9000", 1)},
		{"http on ::1", strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", "http://[::1]:9000", 1)},
		{"batch_max_bytes at 8 MiB", cardeaYAML("cardea", "batch_max_bytes: 8388608", "queue_max_bytes: 8388608")},
		{"shutdown_timeout 0 skips draining", cardeaYAML("cardea", "shutdown_timeout: 0s")},
		{"queue exactly one batch", cardeaYAML("cardea", "batch_max_records: 10", "queue_size: 10", "batch_max_bytes: 4096", "queue_max_bytes: 4096")},
		{"batch of one record", cardeaYAML("cardea", "batch_max_records: 1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, tt.yaml)); err != nil {
				t.Fatalf("Load: %v, want the configuration accepted", err)
			}
		})
	}
}

func TestLoad_CardeaSinkRejections(t *testing.T) {
	withEndpoint := func(endpoint string) string {
		return strings.Replace(cardeaYAML("cardea"), "https://platform.example.com/ingest", endpoint, 1)
	}
	tests := []struct {
		name      string
		yaml      string
		wantInErr string // the field the error must name
	}{
		{"plain http to a remote host", withEndpoint("http://platform.example.com"), "endpoint"},
		{"plain http to example.com", withEndpoint("http://example.com"), "endpoint"},
		{"plain http to a lookalike of localhost", withEndpoint("http://localhost.example.com"), "endpoint"},
		{"endpoint with userinfo", withEndpoint("https://user:s3cr3tpass@platform.example.com/in"), "endpoint"},
		{"endpoint with a username only", withEndpoint("https://user@platform.example.com/in"), "endpoint"},
		{"endpoint with a fragment", withEndpoint("https://platform.example.com/in#frag"), "endpoint"},
		{"endpoint without a host", withEndpoint("https:///in"), "endpoint"},
		{"endpoint that is not a URL", withEndpoint("not a url"), "endpoint"},
		{"endpoint with another scheme", withEndpoint("ftp://platform.example.com/in"), "endpoint"},
		{"endpoint that does not parse", withEndpoint("https://platform.example.com:notaport/in"), "endpoint"},
		{
			"empty endpoint",
			"tracing:\n  sink:\n    type: cardea\n    cardea:\n      api_key_env: CARDEA_API_KEY\n",
			"endpoint",
		},
		{
			"empty api_key_env",
			"tracing:\n  sink:\n    type: cardea\n    cardea:\n      endpoint: https://platform.example.com/in\n      api_key_env: \"\"\n",
			"api_key_env",
		},
		{"no cardea settings at all with logging+cardea", "tracing:\n  sink:\n    type: logging+cardea\n", "endpoint"},
		{"queue_size zero", cardeaYAML("cardea", "queue_size: 0"), "queue_size"},
		{"queue_size below batch_max_records", cardeaYAML("cardea", "batch_max_records: 500", "queue_size: 499"), "queue_size"},
		{"queue_max_bytes below batch_max_bytes", cardeaYAML("cardea", "batch_max_bytes: 1048576", "queue_max_bytes: 1048575"), "queue_max_bytes"},
		{"batch_max_records zero", cardeaYAML("cardea", "batch_max_records: 0"), "batch_max_records"},
		{"batch_max_records negative", cardeaYAML("cardea", "batch_max_records: -1"), "batch_max_records"},
		{"batch_max_bytes zero", cardeaYAML("cardea", "batch_max_bytes: 0"), "batch_max_bytes"},
		{"batch_max_bytes above 8 MiB", cardeaYAML("cardea", "batch_max_bytes: 8388609", "queue_max_bytes: 67108864"), "batch_max_bytes"},
		{"batch_max_delay zero", cardeaYAML("cardea", "batch_max_delay: 0s"), "batch_max_delay"},
		{"batch_max_delay negative", cardeaYAML("cardea", "batch_max_delay: -1s"), "batch_max_delay"},
		{"request_timeout zero", cardeaYAML("cardea", "request_timeout: 0s"), "request_timeout"},
		{"request_timeout negative", cardeaYAML("cardea", "request_timeout: -1s"), "request_timeout"},
		{"shutdown_timeout negative", cardeaYAML("cardea", "shutdown_timeout: -1s"), "shutdown_timeout"},
		{"unknown sink type", "tracing:\n  sink:\n    type: kafka\n", "type"},
		{"sink types are not combinable in other orders", "tracing:\n  sink:\n    type: cardea+logging\n", "type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, tt.yaml))
			if err == nil {
				t.Fatalf("Load = nil error, want a rejection naming %q", tt.wantInErr)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("error %q does not name %q", err, tt.wantInErr)
			}
			if strings.Contains(err.Error(), "s3cr3tpass") {
				t.Fatalf("error %q echoes the endpoint's userinfo", err)
			}
		})
	}
}

func TestLoad_CardeaSettingsAreIgnoredAndUnvalidatedWhenTheTypeIsLogging(t *testing.T) {
	yaml := "tracing:\n  sink:\n    type: logging\n    cardea:\n" +
		"      endpoint: http://platform.example.com\n" +
		"      batch_max_records: 0\n" +
		"      queue_size: -5\n" +
		"      request_timeout: 0s\n"
	cfg, err := Load(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v, want the cardea section ignored when the type does not include cardea", err)
	}
	if cfg.Tracing.Sink.UsesCardea() {
		t.Fatal("UsesCardea() = true for type logging")
	}
}

func TestLoad_NoTenantSettingAnywhereInTheTraceSinkConfig(t *testing.T) {
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag := f.Tag.Get("mapstructure")
			if strings.Contains(strings.ToLower(f.Name), "tenant") || strings.Contains(strings.ToLower(tag), "tenant") {
				t.Errorf("%s.%s (tag %q) names a tenant: the credential alone identifies it", path, f.Name, tag)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	walk("TraceSinkConfig", reflect.TypeOf(TraceSinkConfig{}))
	walk("CardeaSinkConfig", reflect.TypeOf(CardeaSinkConfig{}))
}
