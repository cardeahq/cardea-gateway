package provider

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// httpProxyProvider covers any upstream reachable by forwarding the
// request as-is. Providers needing custom auth or rewriting implement
// Provider directly instead.
type httpProxyProvider struct {
	name  string
	proxy *httputil.ReverseProxy
}

func NewHTTPProxy(name, baseURL string) (Provider, error) {
	if name == "" {
		return nil, fmt.Errorf("provider name must not be empty")
	}

	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL %q for provider %q: %w", baseURL, name, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("base URL %q for provider %q must be absolute", baseURL, name)
	}

	return &httpProxyProvider{
		name:  name,
		proxy: httputil.NewSingleHostReverseProxy(target),
	}, nil
}

func (p *httpProxyProvider) Name() string { return p.name }

func (p *httpProxyProvider) Handler() http.Handler { return p.proxy }
