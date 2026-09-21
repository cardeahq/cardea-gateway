// Package config defines agentkeeper's configuration schema and loads it
// via Viper.
package config

import (
	"fmt"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// EnvPrefix is prepended to environment variable names, e.g.
// AGENTKEEPER_SERVER_LISTENADDR maps to Server.ListenAddr.
const EnvPrefix = "AGENTKEEPER"

type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Logging  LoggingConfig  `mapstructure:"logging"`
	Rules    RulesConfig    `mapstructure:"rules"`
	Provider ProviderConfig `mapstructure:"provider"`
	Pipeline PipelineConfig `mapstructure:"pipeline"`
	Agents   []AgentConfig  `mapstructure:"agents"`
}

type ServerConfig struct {
	ListenAddr string `mapstructure:"listen_addr"`
}

type LoggingConfig struct {
	// One of: debug, info, warn, error.
	Level string `mapstructure:"level"`
	// One of: console, json.
	Format string `mapstructure:"format"`
}

type RulesConfig struct {
	Path string `mapstructure:"path"`
}

type ProviderConfig struct {
	Name    string `mapstructure:"name"`
	BaseURL string `mapstructure:"base_url"`
	// APIKeyEnv is the name of the environment variable holding the
	// upstream API key. The key itself is never read from config/YAML —
	// only this env var name is.
	APIKeyEnv string `mapstructure:"api_key_env"`
}

type PipelineConfig struct {
	// MaxBodyBytes is the largest request body the pipeline will read for
	// stage inspection, and the cap on how much of a response is retained
	// for observation.
	MaxBodyBytes int64        `mapstructure:"max_body_bytes"`
	Stages       StagesConfig `mapstructure:"stages"`
}

type StagesConfig struct {
	TokenCounting  bool `mapstructure:"token_counting"`
	Identification bool `mapstructure:"identification"`
}

// AgentConfig is one operator-registered agent. At least one of
// APIKeyEnv/JWTSecretEnv must be set; both may be, e.g. while migrating an
// agent from an API key to a JWT. Credential values are never written here
// — only the name of the environment variable holding them, matching
// ProviderConfig.APIKeyEnv.
type AgentConfig struct {
	AgentID      string `mapstructure:"agent_id"`
	APIKeyEnv    string `mapstructure:"api_key_env"`
	JWTSecretEnv string `mapstructure:"jwt_secret_env"`
}

func defaults() Config {
	return Config{
		Server: ServerConfig{
			ListenAddr: ":8080",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "console",
		},
		Rules: RulesConfig{
			Path: "",
		},
		Provider: ProviderConfig{
			Name:      "anthropic",
			BaseURL:   "https://api.anthropic.com",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		},
		Pipeline: PipelineConfig{
			MaxBodyBytes: 10485760,
			Stages: StagesConfig{
				TokenCounting:  true,
				Identification: false,
			},
		},
		Agents: nil,
	}
}

// Load applies, in increasing priority: built-in defaults, the config
// file, then AGENTKEEPER_-prefixed environment variables.
func Load(cfgFile string) (Config, error) {
	v := viper.New()

	d := defaults()
	v.SetDefault("server.listen_addr", d.Server.ListenAddr)
	v.SetDefault("logging.level", d.Logging.Level)
	v.SetDefault("logging.format", d.Logging.Format)
	v.SetDefault("rules.path", d.Rules.Path)
	v.SetDefault("provider.name", d.Provider.Name)
	v.SetDefault("provider.base_url", d.Provider.BaseURL)
	v.SetDefault("provider.api_key_env", d.Provider.APIKeyEnv)
	v.SetDefault("pipeline.max_body_bytes", d.Pipeline.MaxBodyBytes)
	v.SetDefault("pipeline.stages.token_counting", d.Pipeline.Stages.TokenCounting)
	v.SetDefault("pipeline.stages.identification", d.Pipeline.Stages.Identification)

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("agentkeeper")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/agentkeeper")
		v.AddConfigPath("/etc/agentkeeper")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return Config{}, fmt.Errorf("reading config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshalling config: %w", err)
	}

	var stages StagesConfig
	err := v.UnmarshalKey("pipeline.stages", &stages, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling pipeline.stages: %w", err)
	}

	var agents []AgentConfig
	err = v.UnmarshalKey("agents", &agents, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling agents: %w", err)
	}

	if cfg.Pipeline.MaxBodyBytes <= 0 {
		return Config{}, fmt.Errorf("pipeline.max_body_bytes must be positive, got %d", cfg.Pipeline.MaxBodyBytes)
	}

	seenAgentIDs := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		if a.AgentID == "" {
			return Config{}, fmt.Errorf("agents entry has an empty agent_id")
		}
		if seenAgentIDs[a.AgentID] {
			return Config{}, fmt.Errorf("agents: duplicate agent_id %q", a.AgentID)
		}
		seenAgentIDs[a.AgentID] = true
		if a.APIKeyEnv == "" && a.JWTSecretEnv == "" {
			return Config{}, fmt.Errorf("agents: agent_id %q must set api_key_env, jwt_secret_env, or both", a.AgentID)
		}
	}

	return cfg, nil
}
