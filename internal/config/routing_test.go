package config

import (
	"strings"
	"testing"
)

// --- RoutingConfig.ListMode --------------------------------------------------

func TestRoutingConfig_ListMode(t *testing.T) {
	if (RoutingConfig{}).ListMode() {
		t.Fatal("ListMode() = true for an empty RoutingConfig, want false")
	}
	cfg := RoutingConfig{Providers: []ProviderEntry{{Name: "a", Kind: "anthropic", BaseURL: "https://x", APIKeyEnv: "X", Models: []string{"*"}}}}
	if !cfg.ListMode() {
		t.Fatal("ListMode() = false with a non-empty provider list, want true")
	}
}

// --- defaults() and Load() base cases ----------------------------------------

func TestDefaults_RoutingIsEmpty(t *testing.T) {
	d := defaults()
	if len(d.Routing.Providers) != 0 {
		t.Fatalf("defaults().Routing.Providers = %+v, want empty", d.Routing.Providers)
	}
	if len(d.Routing.Defaults) != 0 {
		t.Fatalf("defaults().Routing.Defaults = %+v, want empty", d.Routing.Defaults)
	}
}

func TestLoad_RoutingEmptyByDefault(t *testing.T) {
	path := writeConfigFile(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routing.ListMode() {
		t.Fatal("ListMode() = true with no routing configured, want false: single mode is the safe default")
	}
}

// validRoutingYAML is a complete, valid list-mode configuration. It omits
// the provider: key entirely (list mode and the single provider setting
// conflict when provider: is present in the file).
const validRoutingYAML = `routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*", "codestral-*"]
      accept_unscanned: true
  defaults:
    openai: mistral
`

func TestLoad_ValidRoutingListAccepted(t *testing.T) {
	path := writeConfigFile(t, validRoutingYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Routing.ListMode() {
		t.Fatal("ListMode() = false, want true")
	}
	if len(cfg.Routing.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Routing.Providers))
	}
	if cfg.Routing.Defaults["openai"] != "mistral" {
		t.Fatalf("Defaults[openai] = %q, want mistral", cfg.Routing.Defaults["openai"])
	}
	if !cfg.Routing.Providers[1].AcceptUnscanned {
		t.Fatal("mistral's accept_unscanned = false, want true")
	}
}

// --- ErrorUnused on routing ---------------------------------------------------

func TestLoad_RejectsUnknownRoutingKey(t *testing.T) {
	path := writeConfigFile(t, "routing:\n  made_up_key: true\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown routing key = nil error, want error")
	}
}

func TestLoad_RejectsUnknownProviderEntryKey(t *testing.T) {
	yaml := `routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
      made_up_field: true
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown providers[] key = nil error, want error")
	}
}

// --- Single provider vs. list conflict (AC 13, 25) ----------------------------

func TestLoad_SingleProviderInFilePlusListIsAnError(t *testing.T) {
	// AC 13: provider: is set in the file (even to defaults' own values)
	// alongside a non-empty routing.providers.
	yaml := `provider:
  name: anthropic
  base_url: "https://api.anthropic.com"
  api_key_env: ANTHROPIC_TEST_KEY
routing:
  providers:
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*"]
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with provider: in the file plus a routing list = nil error, want a conflict error")
	}
}

func TestLoad_SingleProviderByEnvVarPlusListIsAnError(t *testing.T) {
	// AC 25: a single provider set by environment variable, not the file,
	// still conflicts with a file-based provider list.
	tests := []string{
		"CARDEA_GATEWAY_PROVIDER_NAME",
		"CARDEA_GATEWAY_PROVIDER_BASE_URL",
		"CARDEA_GATEWAY_PROVIDER_API_KEY_ENV",
	}
	for _, envVar := range tests {
		t.Run(envVar, func(t *testing.T) {
			yaml := `routing:
  providers:
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*"]
`
			path := writeConfigFile(t, yaml)
			t.Setenv(envVar, "something")
			if _, err := Load(path); err == nil {
				t.Fatalf("Load with %s set plus a routing list = nil error, want a conflict error", envVar)
			}
		})
	}
}

func TestLoad_DefaultsOnlyProviderPlusListIsAccepted(t *testing.T) {
	// Viper's defaults() always populates provider.name etc., but that
	// alone must not count as the operator "setting" the single provider":
	// InConfig/os.LookupEnv, not the decoded struct, decide the conflict.
	// This file omits the provider: key entirely, the true "never set" case.
	yaml := `routing:
  providers:
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*"]
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v, want it accepted: provider was never set by the operator", err)
	}
}

// --- Provider entry field validation ------------------------------------------

func routingYAMLWithProvider(provider string) string {
	return "routing:\n  providers:\n    - " + strings.ReplaceAll(strings.TrimSpace(provider), "\n", "\n      ") + "\n"
}

func TestLoad_ProviderEntryValidation(t *testing.T) {
	valid := `name: anthropic
kind: anthropic
base_url: "https://api.anthropic.com"
api_key_env: ANTHROPIC_TEST_KEY
models: ["claude-*"]
`
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr bool
	}{
		{"valid entry is accepted", func(s string) string { return s }, false},
		{"empty name", func(s string) string { return strings.Replace(s, "name: anthropic", "name: \"\"", 1) }, true},
		{"unknown kind", func(s string) string { return strings.Replace(s, "kind: anthropic", "kind: openai-generic", 1) }, true},
		{"relative base_url", func(s string) string {
			return strings.Replace(s, `base_url: "https://api.anthropic.com"`, `base_url: "/no-host"`, 1)
		}, true},
		{"http on a non-loopback host", func(s string) string {
			return strings.Replace(s, `base_url: "https://api.anthropic.com"`, `base_url: "http://api.anthropic.com"`, 1)
		}, true},
		{"http on loopback is accepted", func(s string) string {
			return strings.Replace(s, `base_url: "https://api.anthropic.com"`, `base_url: "http://127.0.0.1:9999"`, 1)
		}, false},
		{"base_url with userinfo", func(s string) string {
			return strings.Replace(s, `base_url: "https://api.anthropic.com"`, `base_url: "https://user:pass@api.anthropic.com"`, 1)
		}, true},
		{"empty api_key_env", func(s string) string {
			return strings.Replace(s, "api_key_env: ANTHROPIC_TEST_KEY", "api_key_env: \"\"", 1)
		}, true},
		{"no models", func(s string) string { return strings.Replace(s, `models: ["claude-*"]`, "models: []", 1) }, true},
		{"empty model entry", func(s string) string { return strings.Replace(s, `models: ["claude-*"]`, `models: [""]`, 1) }, true},
		{"star not at the end", func(s string) string { return strings.Replace(s, `models: ["claude-*"]`, `models: ["cla*ude"]`, 1) }, true},
		{"star alone is accepted", func(s string) string { return strings.Replace(s, `models: ["claude-*"]`, `models: ["*"]`, 1) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, routingYAMLWithProvider(tt.mutate(valid)))
			_, err := Load(path)
			if tt.wantErr && err == nil {
				t.Fatal("Load = nil error, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Load: %v, want success", err)
			}
		})
	}
}

func TestLoad_DuplicateProviderNamesIsAnError(t *testing.T) {
	yaml := `routing:
  providers:
    - name: dup
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
    - name: dup
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*"]
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with two providers named \"dup\" = nil error, want error")
	}
}

// --- defaults validation -------------------------------------------------------

func TestLoad_DefaultsKeysMustBeKnownFormats(t *testing.T) {
	yaml := `routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
  defaults:
    not-a-format: anthropic
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with a defaults key outside {anthropic, openai, gemini} = nil error, want error")
	}
}

func TestLoad_DefaultsWithNoProviderListIsAnError(t *testing.T) {
	yaml := `routing:
  defaults:
    anthropic: whoever
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with routing.defaults set and routing.providers empty = nil error, want error")
	}
}

func TestLoad_AllThreeDefaultFormatsAccepted(t *testing.T) {
	yaml := `routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_TEST_KEY
      models: ["mistral-*"]
    - name: gemini
      kind: gemini
      base_url: "https://generativelanguage.googleapis.com"
      api_key_env: GEMINI_TEST_KEY
      models: ["gemini-*"]
  defaults:
    anthropic: anthropic
    openai: mistral
    gemini: gemini
`
	path := writeConfigFile(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routing.Defaults) != 3 {
		t.Fatalf("Defaults = %+v, want all three formats", cfg.Routing.Defaults)
	}
}

// Checking that a default's provider speaks its format (AC 19) is
// routing.NewTable's job, not config.Load's (see specs/0009-technical,
// Design -> Configuration): config.Load accepts any default naming a
// known format, whatever provider it points at.
func TestLoad_DefaultNamingAnyProviderIsAcceptedRegardlessOfItsFormat(t *testing.T) {
	yaml := `routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_TEST_KEY
      models: ["claude-*"]
  defaults:
    openai: anthropic
`
	path := writeConfigFile(t, yaml)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v, want config.Load to accept it (routing.NewTable owns the kind-to-format check)", err)
	}
}
