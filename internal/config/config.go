// Package config defines agentkeeper's configuration schema and loads it
// via Viper.
package config

import (
	"fmt"
	"strings"

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

	return cfg, nil
}
