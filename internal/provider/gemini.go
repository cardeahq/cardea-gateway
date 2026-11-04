package provider

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

// geminiProvider forwards requests to the Gemini API, injecting the API
// key server-side so callers never need to know or supply it.
type geminiProvider struct {
	proxy *httputil.ReverseProxy
}

// NewGeminiProvider builds a Provider for the Gemini API. apiKeyEnv names
// the environment variable holding the API key; the key itself is read
// once at startup and is never accepted via config/YAML.
func NewGeminiProvider(baseURL, apiKeyEnv string) (Provider, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL %q for provider \"gemini\": %w", baseURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("base URL %q for provider \"gemini\" must be absolute", baseURL)
	}

	if apiKeyEnv == "" {
		return nil, fmt.Errorf("provider \"gemini\" requires provider.api_key_env to be set")
	}
	apiKey := os.Getenv(apiKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("environment variable %q (provider.api_key_env) is not set", apiKeyEnv)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			stripCallerCredentials(pr.Out)
			pr.Out.Header.Set("x-goog-api-key", apiKey)
		},
		ErrorHandler: errorHandler,
	}

	return &geminiProvider{proxy: proxy}, nil
}

func (p *geminiProvider) Name() string { return "gemini" }

func (p *geminiProvider) Handler() http.Handler { return p.proxy }
