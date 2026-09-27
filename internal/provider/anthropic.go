package provider

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

// anthropicProvider forwards requests to the Anthropic API, injecting the
// API key server-side so callers never need to know or supply it.
type anthropicProvider struct {
	proxy *httputil.ReverseProxy
}

// NewAnthropicProvider builds a Provider for the Anthropic API. apiKeyEnv
// names the environment variable holding the API key; the key itself is
// read once at startup and is never accepted via config/YAML.
func NewAnthropicProvider(baseURL, apiKeyEnv string) (Provider, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL %q for provider \"anthropic\": %w", baseURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("base URL %q for provider \"anthropic\" must be absolute", baseURL)
	}

	if apiKeyEnv == "" {
		return nil, fmt.Errorf("provider \"anthropic\" requires provider.api_key_env to be set")
	}
	apiKey := os.Getenv(apiKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("environment variable %q (provider.api_key_env) is not set", apiKeyEnv)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("x-api-key", apiKey)
		},
		ErrorHandler: errorHandler,
	}

	return &anthropicProvider{proxy: proxy}, nil
}

func (p *anthropicProvider) Name() string { return "anthropic" }

func (p *anthropicProvider) Handler() http.Handler { return p.proxy }
