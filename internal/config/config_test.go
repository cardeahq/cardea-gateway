package config

import (
	"os"
	"path/filepath"
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
