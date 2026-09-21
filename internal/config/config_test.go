package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	if !cfg.Pipeline.Stages.TokenCounting {
		t.Fatal("Pipeline.Stages.TokenCounting = false, want true: every stage boolean defaults to true")
	}
}

func TestLoad_PipelineStageExplicitFalseInYAMLIsHonored(t *testing.T) {
	path := writeConfigFile(t, "pipeline:\n  stages:\n    token_counting: false\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.TokenCounting {
		t.Fatal("Pipeline.Stages.TokenCounting = true, want false: an explicit false must be honored")
	}
}

func TestLoad_PipelineStageEnvVarOverride(t *testing.T) {
	path := writeConfigFile(t, "")
	t.Setenv("AGENTKEEPER_PIPELINE_STAGES_TOKEN_COUNTING", "false")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Stages.TokenCounting {
		t.Fatal("Pipeline.Stages.TokenCounting = true, want false: an env var override must be honored")
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

func TestDefaults_IdentificationDisabledUnlikeTokenCounting(t *testing.T) {
	d := defaults()
	if d.Pipeline.Stages.Identification {
		t.Fatal("defaults().Pipeline.Stages.Identification = true, want false")
	}
	if len(d.Agents) != 0 {
		t.Fatalf("defaults().Agents = %+v, want empty", d.Agents)
	}
	if !d.Pipeline.Stages.TokenCounting {
		t.Fatal("defaults().Pipeline.Stages.TokenCounting = false, want true: identification's default deliberately diverges from every other stage's default of true")
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
	if !cfg.Pipeline.Stages.TokenCounting {
		t.Fatal("Pipeline.Stages.TokenCounting = false, want true: unlike identification, this stage still defaults to on")
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
