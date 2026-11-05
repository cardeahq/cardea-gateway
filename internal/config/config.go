// Package config defines the Gateway's configuration schema and loads it
// via Viper.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// EnvPrefix is prepended to environment variable names, e.g.
// CARDEA_GATEWAY_SERVER_LISTENADDR maps to Server.ListenAddr.
const EnvPrefix = "CARDEA_GATEWAY"

type Config struct {
	Server         ServerConfig         `mapstructure:"server"`
	Logging        LoggingConfig        `mapstructure:"logging"`
	Rules          RulesConfig          `mapstructure:"rules"`
	Provider       ProviderConfig       `mapstructure:"provider"`
	Pipeline       PipelineConfig       `mapstructure:"pipeline"`
	Authorization  AuthorizationConfig  `mapstructure:"authorization"`
	Identification IdentificationConfig `mapstructure:"identification"`
	Tracing        TracingConfig        `mapstructure:"tracing"`
	Policies       PoliciesConfig       `mapstructure:"policies"`
	Filtering      FilteringConfig      `mapstructure:"filtering"`
	Routing        RoutingConfig        `mapstructure:"routing"`
}

// RoutingConfig configures multi-provider routing (specs/0009). Empty
// Providers means single mode: the Provider block above applies instead.
type RoutingConfig struct {
	Providers []ProviderEntry `mapstructure:"providers"`
	// Defaults maps a format ("anthropic", "openai", "gemini") to a
	// provider name.
	Defaults map[string]string `mapstructure:"defaults"`
}

// ProviderEntry is one listed provider.
type ProviderEntry struct {
	Name string `mapstructure:"name"`
	// Kind is one of: anthropic, mistral, gemini.
	Kind      string   `mapstructure:"kind"`
	BaseURL   string   `mapstructure:"base_url"`
	APIKeyEnv string   `mapstructure:"api_key_env"`
	Models    []string `mapstructure:"models"`
}

// ListMode reports whether a provider list is configured.
func (r RoutingConfig) ListMode() bool {
	return len(r.Providers) > 0
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
	// Name is one of: anthropic, mistral, gemini.
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
	Authorization  bool `mapstructure:"authorization"`
	Identification bool `mapstructure:"identification"`
	Tracing        bool `mapstructure:"tracing"`
	Policies       bool `mapstructure:"policies"`
	Filtering      bool `mapstructure:"filtering"`
}

// FilteringConfig configures the filtering stage (specs/0006, amended
// by specs/0007): which PII categories are detected.
type FilteringConfig struct {
	Categories CategoriesConfig `mapstructure:"categories"`
}

type CategoriesConfig struct {
	Email  bool `mapstructure:"email"`
	SSN    bool `mapstructure:"ssn"`
	APIKey bool `mapstructure:"api_key"`
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

// AuthorizationConfig configures the shared-secret gate. Secret values
// are never written here — only the names of the environment variables
// holding them, matching ProviderConfig.APIKeyEnv.
type AuthorizationConfig struct {
	APIKeyEnvs []string `mapstructure:"api_key_envs"`
}

// IdentificationConfig configures agent-type recognition.
type IdentificationConfig struct {
	// AgentTypesDir holds one YAML file per recognized agent type. ""
	// means none configured — every call resolves to
	// identification.Unknown.
	AgentTypesDir string `mapstructure:"agent_types_dir"`
}

type TracingConfig struct {
	// FingerprintKeyEnv names the env var holding the HMAC key for
	// fingerprints (at least 32 bytes). The key itself is never in config.
	// Unset var: a random per-process key is generated, with a warning.
	FingerprintKeyEnv string `mapstructure:"fingerprint_key_env"`
	// MaxReadBytes caps what the response tap buffers: one streamed tool
	// call's arguments, one SSE event, or a whole non-streamed body.
	MaxReadBytes int64           `mapstructure:"max_read_bytes"`
	Sink         TraceSinkConfig `mapstructure:"sink"`
	// DefaultSessionSources are tried, in order, for every agent whose
	// agent-type file lists no session_sources of its own.
	DefaultSessionSources []SessionSource `mapstructure:"default_session_sources"`
}

type TraceSinkConfig struct {
	// Type is "logging", "cardea" or "logging+cardea".
	Type string `mapstructure:"type"`
	// Path is where the logging sink appends records; "" means stdout.
	Path   string           `mapstructure:"path"`
	Cardea CardeaSinkConfig `mapstructure:"cardea"`
}

// CardeaSinkConfig is read only when Type includes "cardea".
type CardeaSinkConfig struct {
	// Endpoint is the full URL batches are POSTed to. Required, no default.
	Endpoint string `mapstructure:"endpoint"`
	// APIKeyEnv names the env var holding the platform API key. Defaults to
	// CARDEA_API_KEY. The key itself is never in config.
	APIKeyEnv       string        `mapstructure:"api_key_env"`
	QueueSize       int           `mapstructure:"queue_size"`
	QueueMaxBytes   int64         `mapstructure:"queue_max_bytes"`
	BatchMaxRecords int           `mapstructure:"batch_max_records"`
	BatchMaxBytes   int           `mapstructure:"batch_max_bytes"`
	BatchMaxDelay   time.Duration `mapstructure:"batch_max_delay"`
	RequestTimeout  time.Duration `mapstructure:"request_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// UsesCardea reports whether t.Type selects the Cardea sink.
func (t TraceSinkConfig) UsesCardea() bool {
	return t.Type == "cardea" || t.Type == "logging+cardea"
}

// UsesLogging reports whether t.Type selects the local sink.
func (t TraceSinkConfig) UsesLogging() bool {
	return t.Type == "logging" || t.Type == "logging+cardea"
}

// SessionSource sets exactly one of Header, BodyField, Baggage. Pattern is
// an optional RE2 regexp with exactly one capture group.
type SessionSource struct {
	Header    string `mapstructure:"header" yaml:"header"`
	BodyField string `mapstructure:"body_field" yaml:"body_field"`
	Baggage   string `mapstructure:"baggage" yaml:"baggage"`
	Pattern   string `mapstructure:"pattern" yaml:"pattern"`
}

// Validate reports whether s names exactly one of Header, BodyField,
// Baggage.
func (s SessionSource) Validate() error {
	kinds := 0
	for _, v := range []string{s.Header, s.BodyField, s.Baggage} {
		if v != "" {
			kinds++
		}
	}
	if kinds != 1 {
		return fmt.Errorf("set exactly one of header, body_field, baggage")
	}
	return nil
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
				Authorization:  false,
				Identification: true,
				Tracing:        true,
				Policies:       true,
				Filtering:      true,
			},
		},
		Authorization:  AuthorizationConfig{APIKeyEnvs: nil},
		Identification: IdentificationConfig{AgentTypesDir: ""},
		Tracing: TracingConfig{
			FingerprintKeyEnv: "TRACE_FINGERPRINT_KEY",
			MaxReadBytes:      16777216,
			Sink: TraceSinkConfig{
				Type: "logging",
				Path: "",
				Cardea: CardeaSinkConfig{
					APIKeyEnv:       "CARDEA_API_KEY",
					QueueSize:       10000,
					QueueMaxBytes:   67108864,
					BatchMaxRecords: 500,
					BatchMaxBytes:   1048576,
					BatchMaxDelay:   time.Second,
					RequestTimeout:  10 * time.Second,
					ShutdownTimeout: 10 * time.Second,
				},
			},
		},
		Policies: PoliciesConfig{
			Data:        []string{},
			WatchSettle: 500 * time.Millisecond,
			EvalTimeout: 100 * time.Millisecond,
		},
		Filtering: FilteringConfig{
			Categories: CategoriesConfig{Email: true, SSN: true, APIKey: true},
		},
		Routing: RoutingConfig{Providers: nil, Defaults: nil},
	}
}

// Load applies, in increasing priority: built-in defaults, the config
// file, then CARDEA_GATEWAY_-prefixed environment variables.
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
	v.SetDefault("pipeline.stages.authorization", d.Pipeline.Stages.Authorization)
	v.SetDefault("pipeline.stages.identification", d.Pipeline.Stages.Identification)
	v.SetDefault("pipeline.stages.tracing", d.Pipeline.Stages.Tracing)
	v.SetDefault("pipeline.stages.policies", d.Pipeline.Stages.Policies)
	v.SetDefault("pipeline.stages.filtering", d.Pipeline.Stages.Filtering)
	v.SetDefault("authorization.api_key_envs", d.Authorization.APIKeyEnvs)
	v.SetDefault("identification.agent_types_dir", d.Identification.AgentTypesDir)
	v.SetDefault("filtering.categories.email", d.Filtering.Categories.Email)
	v.SetDefault("filtering.categories.ssn", d.Filtering.Categories.SSN)
	v.SetDefault("filtering.categories.api_key", d.Filtering.Categories.APIKey)
	v.SetDefault("policies.path", d.Policies.Path)
	v.SetDefault("policies.data", d.Policies.Data)
	v.SetDefault("policies.watch", d.Policies.Watch)
	v.SetDefault("policies.watch_settle", d.Policies.WatchSettle)
	v.SetDefault("policies.eval_timeout", d.Policies.EvalTimeout)
	v.SetDefault("tracing.fingerprint_key_env", d.Tracing.FingerprintKeyEnv)
	v.SetDefault("tracing.max_read_bytes", d.Tracing.MaxReadBytes)
	v.SetDefault("tracing.sink.type", d.Tracing.Sink.Type)
	v.SetDefault("tracing.sink.path", d.Tracing.Sink.Path)
	c := d.Tracing.Sink.Cardea
	v.SetDefault("tracing.sink.cardea.endpoint", c.Endpoint)
	v.SetDefault("tracing.sink.cardea.api_key_env", c.APIKeyEnv)
	v.SetDefault("tracing.sink.cardea.queue_size", c.QueueSize)
	v.SetDefault("tracing.sink.cardea.queue_max_bytes", c.QueueMaxBytes)
	v.SetDefault("tracing.sink.cardea.batch_max_records", c.BatchMaxRecords)
	v.SetDefault("tracing.sink.cardea.batch_max_bytes", c.BatchMaxBytes)
	v.SetDefault("tracing.sink.cardea.batch_max_delay", c.BatchMaxDelay)
	v.SetDefault("tracing.sink.cardea.request_timeout", c.RequestTimeout)
	v.SetDefault("tracing.sink.cardea.shutdown_timeout", c.ShutdownTimeout)
	v.SetDefault("routing.providers", d.Routing.Providers)
	v.SetDefault("routing.defaults", d.Routing.Defaults)

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("cardea-gateway")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/cardea-gateway")
		v.AddConfigPath("/etc/cardea-gateway")
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

	var authz AuthorizationConfig
	err = v.UnmarshalKey("authorization", &authz, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling authorization: %w", err)
	}

	var ident IdentificationConfig
	err = v.UnmarshalKey("identification", &ident, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling identification: %w", err)
	}

	var defaultSessions []SessionSource
	err = v.UnmarshalKey("tracing.default_session_sources", &defaultSessions, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling tracing.default_session_sources: %w", err)
	}

	// discardFiltering exists only to run an ErrorUnused decode of
	// "filtering" and catch a typo'd key; cfg.Filtering (set by the
	// top-level v.Unmarshal above) is authoritative, since UnmarshalKey
	// does not apply AutomaticEnv overrides below the first nesting level.
	var discardFiltering FilteringConfig
	err = v.UnmarshalKey("filtering", &discardFiltering, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling filtering: %w", err)
	}

	var discardRouting RoutingConfig
	err = v.UnmarshalKey("routing", &discardRouting, func(dc *mapstructure.DecoderConfig) {
		dc.ErrorUnused = true
	})
	if err != nil {
		return Config{}, fmt.Errorf("unmarshalling routing: %w", err)
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

	if err := validateTracing(cfg.Tracing); err != nil {
		return Config{}, err
	}

	if err := validateRouting(v, cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validateRouting checks the single-provider/list conflict and, in list
// mode, every provider entry and the defaults map.
func validateRouting(v *viper.Viper, cfg Config) error {
	singleSet := v.InConfig("provider")
	for _, key := range []string{"PROVIDER_NAME", "PROVIDER_BASE_URL", "PROVIDER_API_KEY_ENV"} {
		if _, ok := os.LookupEnv(EnvPrefix + "_" + key); ok {
			singleSet = true
		}
	}
	if singleSet && cfg.Routing.ListMode() {
		return fmt.Errorf("provider and routing.providers must not both be set")
	}

	if err := validateRoutingDefaults(cfg.Routing); err != nil {
		return err
	}
	if !cfg.Routing.ListMode() {
		switch cfg.Provider.Name {
		case "anthropic", "mistral", "gemini":
			return nil
		default:
			return fmt.Errorf("provider.name %q must be one of anthropic, mistral, gemini", cfg.Provider.Name)
		}
	}

	seen := map[string]bool{}
	for i, e := range cfg.Routing.Providers {
		if err := validateProviderEntry(i, e); err != nil {
			return err
		}
		if seen[e.Name] {
			return fmt.Errorf("routing.providers has duplicate name %q", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

func validateRoutingDefaults(cfg RoutingConfig) error {
	if len(cfg.Defaults) > 0 && !cfg.ListMode() {
		return fmt.Errorf("routing.defaults requires routing.providers to be set")
	}
	for k := range cfg.Defaults {
		switch k {
		case "anthropic", "openai", "gemini":
		default:
			return fmt.Errorf("routing.defaults key %q must be one of anthropic, openai, gemini", k)
		}
	}
	return nil
}

func validateProviderEntry(i int, e ProviderEntry) error {
	if e.Name == "" {
		return fmt.Errorf("routing.providers[%d].name must not be empty", i)
	}
	switch e.Kind {
	case "anthropic", "mistral", "gemini":
	default:
		return fmt.Errorf("routing.providers[%d].kind %q must be one of anthropic, mistral, gemini", i, e.Kind)
	}
	if err := validateProviderBaseURL(e.BaseURL); err != nil {
		return fmt.Errorf("routing.providers[%d].base_url %w", i, err)
	}
	if e.APIKeyEnv == "" {
		return fmt.Errorf("routing.providers[%d].api_key_env must not be empty", i)
	}
	if len(e.Models) == 0 {
		return fmt.Errorf("routing.providers[%d].models must not be empty", i)
	}
	for j, m := range e.Models {
		if m == "" {
			return fmt.Errorf("routing.providers[%d].models[%d] must not be empty", i, j)
		}
		if strings.Contains(m[:len(m)-1], "*") {
			return fmt.Errorf("routing.providers[%d].models[%d] may only use * as its last character", i, j)
		}
	}
	return nil
}

// validateProviderBaseURL never puts the URL in its errors, matching
// validateEndpoint, even though a provider base_url carries no secret:
// kept consistent with how every other URL setting here is validated.
func validateProviderBaseURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("is not a valid URL")
	case u.Hostname() == "":
		return fmt.Errorf("must be absolute with a host")
	case u.User != nil:
		return fmt.Errorf("must not contain userinfo")
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("must use https (http is allowed only for localhost)")
}

func validateTracing(t TracingConfig) error {
	switch t.Sink.Type {
	case "logging", "cardea", "logging+cardea":
	default:
		return fmt.Errorf("tracing.sink.type must be \"logging\", \"cardea\" or \"logging+cardea\", got %q", t.Sink.Type)
	}
	if t.Sink.UsesCardea() {
		if err := validateCardea(t.Sink.Cardea); err != nil {
			return err
		}
	}
	if t.MaxReadBytes <= 0 {
		return fmt.Errorf("tracing.max_read_bytes must be positive, got %d", t.MaxReadBytes)
	}

	for i, src := range t.DefaultSessionSources {
		if err := src.Validate(); err != nil {
			return fmt.Errorf("tracing.default_session_sources[%d]: %w", i, err)
		}
	}
	return nil
}

const maxBatchBytes = 8 << 20

func validateCardea(c CardeaSinkConfig) error {
	if err := validateEndpoint(c.Endpoint); err != nil {
		return err
	}
	switch {
	case c.APIKeyEnv == "":
		return fmt.Errorf("tracing.sink.cardea.api_key_env is required")
	case c.BatchMaxRecords < 1:
		return fmt.Errorf("tracing.sink.cardea.batch_max_records must be at least 1, got %d", c.BatchMaxRecords)
	case c.BatchMaxBytes < 1 || c.BatchMaxBytes > maxBatchBytes:
		return fmt.Errorf("tracing.sink.cardea.batch_max_bytes must be between 1 and %d, got %d", maxBatchBytes, c.BatchMaxBytes)
	case c.QueueSize < c.BatchMaxRecords:
		return fmt.Errorf("tracing.sink.cardea.queue_size (%d) must be at least batch_max_records (%d)", c.QueueSize, c.BatchMaxRecords)
	case c.QueueMaxBytes < int64(c.BatchMaxBytes):
		return fmt.Errorf("tracing.sink.cardea.queue_max_bytes (%d) must be at least batch_max_bytes (%d)", c.QueueMaxBytes, c.BatchMaxBytes)
	case c.BatchMaxDelay <= 0:
		return fmt.Errorf("tracing.sink.cardea.batch_max_delay must be positive, got %s", c.BatchMaxDelay)
	case c.RequestTimeout <= 0:
		return fmt.Errorf("tracing.sink.cardea.request_timeout must be positive, got %s", c.RequestTimeout)
	case c.ShutdownTimeout < 0:
		return fmt.Errorf("tracing.sink.cardea.shutdown_timeout must not be negative, got %s", c.ShutdownTimeout)
	}
	return nil
}

// validateEndpoint never puts the endpoint in its errors: it may carry
// credentials in its userinfo.
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("tracing.sink.cardea.endpoint is required")
	}
	u, err := url.Parse(endpoint)
	switch {
	case err != nil:
		return fmt.Errorf("tracing.sink.cardea.endpoint is not a valid URL")
	case u.Hostname() == "":
		return fmt.Errorf("tracing.sink.cardea.endpoint must have a host")
	case u.User != nil:
		return fmt.Errorf("tracing.sink.cardea.endpoint must not contain userinfo")
	case u.Fragment != "" || strings.Contains(endpoint, "#"):
		return fmt.Errorf("tracing.sink.cardea.endpoint must not contain a fragment")
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("tracing.sink.cardea.endpoint must use https (http is allowed only for localhost)")
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
