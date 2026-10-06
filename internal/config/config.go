// Package config defines agentkeeper's configuration schema and loads it
// via Viper.
package config

import (
	"fmt"
	"strings"
	"time"

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
	Tracing  TracingConfig  `mapstructure:"tracing"`
	Policies PoliciesConfig `mapstructure:"policies"`
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
	Identification bool `mapstructure:"identification"`
	Tracing        bool `mapstructure:"tracing"`
	Policies       bool `mapstructure:"policies"`
}

type PoliciesConfig struct {
	// Path is the .rego policy file. Empty: no policy.
	Path string `mapstructure:"path"`
	// Data are JSON or YAML files, or directories of them, exposed to
	// rules as `data`.
	Data []string `mapstructure:"data"`
	// Watch turns on reload on file change.
	Watch bool `mapstructure:"watch"`
	// WatchSettle is how long to wait after the last change before
	// reloading.
	WatchSettle time.Duration `mapstructure:"watch_settle"`
	// EvalTimeout is the longest one evaluation may take.
	EvalTimeout time.Duration `mapstructure:"eval_timeout"`
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

type TracingConfig struct {
	// FingerprintKeyEnv names the env var holding the HMAC key for
	// fingerprints (at least 32 bytes). The key itself is never in config.
	// Unset var: a random per-process key is generated, with a warning.
	FingerprintKeyEnv string `mapstructure:"fingerprint_key_env"`
	// MaxReadBytes caps what the response tap buffers: one streamed tool
	// call's arguments, one SSE event, or a whole non-streamed body.
	MaxReadBytes int64               `mapstructure:"max_read_bytes"`
	Sink         TraceSinkConfig     `mapstructure:"sink"`
	Sessions     []SessionSourceList `mapstructure:"sessions"`
}

type TraceSinkConfig struct {
	// Type is "logging", the only sink in this version.
	Type string `mapstructure:"type"`
	// Path is where the logging sink appends records; "" means stdout.
	Path string `mapstructure:"path"`
}

// SessionSourceList applies to the named Agents, or to every agent
// without a list of its own when Default is true. Exactly one of the two
// is set.
type SessionSourceList struct {
	Agents  []string        `mapstructure:"agents"`
	Default bool            `mapstructure:"default"`
	Sources []SessionSource `mapstructure:"sources"`
}

// SessionSource sets exactly one of Header, BodyField, Baggage. Pattern is
// an optional RE2 regexp with exactly one capture group.
type SessionSource struct {
	Header    string `mapstructure:"header"`
	BodyField string `mapstructure:"body_field"`
	Baggage   string `mapstructure:"baggage"`
	Pattern   string `mapstructure:"pattern"`
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
				Identification: false,
				Tracing:        true,
				Policies:       true,
			},
		},
		Agents: nil,
		Tracing: TracingConfig{
			FingerprintKeyEnv: "TRACE_FINGERPRINT_KEY",
			MaxReadBytes:      16777216,
			Sink:              TraceSinkConfig{Type: "logging", Path: ""},
		},
		Policies: PoliciesConfig{
			Data:        []string{},
			WatchSettle: 500 * time.Millisecond,
			EvalTimeout: 100 * time.Millisecond,
		},
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
	v.SetDefault("pipeline.stages.identification", d.Pipeline.Stages.Identification)
	v.SetDefault("pipeline.stages.tracing", d.Pipeline.Stages.Tracing)
	v.SetDefault("pipeline.stages.policies", d.Pipeline.Stages.Policies)
	v.SetDefault("policies.path", d.Policies.Path)
	v.SetDefault("policies.data", d.Policies.Data)
	v.SetDefault("policies.watch", d.Policies.Watch)
	v.SetDefault("policies.watch_settle", d.Policies.WatchSettle)
	v.SetDefault("policies.eval_timeout", d.Policies.EvalTimeout)
	v.SetDefault("tracing.fingerprint_key_env", d.Tracing.FingerprintKeyEnv)
	v.SetDefault("tracing.max_read_bytes", d.Tracing.MaxReadBytes)
	v.SetDefault("tracing.sink.type", d.Tracing.Sink.Type)
	v.SetDefault("tracing.sink.path", d.Tracing.Sink.Path)

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

	var policies PoliciesConfig
	err = v.UnmarshalKey("policies", &policies, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling policies: %w", err)
	}

	var agents []AgentConfig
	err = v.UnmarshalKey("agents", &agents, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling agents: %w", err)
	}

	var sessions []SessionSourceList
	err = v.UnmarshalKey("tracing.sessions", &sessions, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling tracing.sessions: %w", err)
	}

	if cfg.Pipeline.MaxBodyBytes <= 0 {
		return Config{}, fmt.Errorf("pipeline.max_body_bytes must be positive, got %d", cfg.Pipeline.MaxBodyBytes)
	}

	if cfg.Policies.EvalTimeout <= 0 {
		return Config{}, fmt.Errorf("policies.eval_timeout must be positive, got %s", cfg.Policies.EvalTimeout)
	}
	if cfg.Policies.WatchSettle <= 0 {
		return Config{}, fmt.Errorf("policies.watch_settle must be positive, got %s", cfg.Policies.WatchSettle)
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

	if err := validateTracing(cfg.Tracing, seenAgentIDs); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func validateTracing(t TracingConfig, agents map[string]bool) error {
	if t.Sink.Type != "logging" {
		return fmt.Errorf("tracing.sink.type must be \"logging\", got %q", t.Sink.Type)
	}
	if t.MaxReadBytes <= 0 {
		return fmt.Errorf("tracing.max_read_bytes must be positive, got %d", t.MaxReadBytes)
	}

	hasDefault := false
	listed := make(map[string]bool)
	for i, l := range t.Sessions {
		where := fmt.Sprintf("tracing.sessions[%d]", i)
		switch {
		case l.Default && len(l.Agents) > 0:
			return fmt.Errorf("%s: set either agents or default, not both", where)
		case !l.Default && len(l.Agents) == 0:
			return fmt.Errorf("%s: set a non-empty agents list or default: true", where)
		case l.Default:
			if hasDefault {
				return fmt.Errorf("%s: the default session source list is given twice", where)
			}
			hasDefault = true
		}
		for _, a := range l.Agents {
			if !agents[a] {
				return fmt.Errorf("%s: agent %q is not registered in agents", where, a)
			}
			if listed[a] {
				return fmt.Errorf("%s: agent %q appears in more than one session source list", where, a)
			}
			listed[a] = true
		}
		if len(l.Sources) == 0 {
			return fmt.Errorf("%s: at least one source is required", where)
		}
		for j, s := range l.Sources {
			kinds := 0
			for _, v := range []string{s.Header, s.BodyField, s.Baggage} {
				if v != "" {
					kinds++
				}
			}
			if kinds != 1 {
				return fmt.Errorf("%s.sources[%d]: set exactly one of header, body_field, baggage", where, j)
			}
		}
	}
	return nil
}
