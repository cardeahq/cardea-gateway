package provider

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

// mistralProvider forwards requests to the Mistral API, injecting the API
// key server-side as a bearer token so callers never need to know or
// supply it.
type mistralProvider struct {
	proxy *httputil.ReverseProxy
}

// NewMistralProvider builds a Provider for the Mistral API. apiKeyEnv
// names the environment variable holding the API key; the key itself is
// read once at startup and is never accepted via config/YAML.
func NewMistralProvider(baseURL, apiKeyEnv string) (Provider, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL %q for provider \"mistral\": %w", baseURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("base URL %q for provider \"mistral\" must be absolute", baseURL)
	}

	if apiKeyEnv == "" {
		return nil, fmt.Errorf("provider \"mistral\" requires provider.api_key_env to be set")
	}
	apiKey := os.Getenv(apiKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("environment variable %q (provider.api_key_env) is not set", apiKeyEnv)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			stripCallerCredentials(pr.Out)
			pr.Out.Header.Set("Authorization", "Bearer "+apiKey)
		},
		ErrorHandler: errorHandler,
	}

	return &mistralProvider{proxy: proxy}, nil
}

func (p *mistralProvider) Name() string { return "mistral" }

func (p *mistralProvider) Handler() http.Handler { return p.proxy }
