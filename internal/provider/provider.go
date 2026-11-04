// Package provider defines the Provider abstraction that lets the gateway
// route requests to different upstream LLM APIs interchangeably.
package provider

import (
	"net/http"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type Provider interface {
	Name() string
	Handler() http.Handler
	// ResponseFormat reads this provider's responses for tracing; nil when
	// the Gateway does not know the upstream's format.
	ResponseFormat() trace.ResponseFormat
}

// stripCallerCredentials deletes every caller-supplied credential from
// the outbound request: the Authorization, Proxy-Authorization,
// x-api-key and x-goog-api-key headers and the key query parameter.
// Every provider that injects its own credential calls this first, in
// Rewrite, so callers can never forward their own key or borrow another
// caller's.
func stripCallerCredentials(out *http.Request) {
	out.Header.Del("Authorization")
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("x-api-key")
	out.Header.Del("x-goog-api-key")

	q := out.URL.Query()
	q.Del("key")
	out.URL.RawQuery = q.Encode()
}

// errorHandler replaces ReverseProxy's default: it still writes a bare
// 502, but first tells a writer that cares that the provider never
// answered, which a 502 the provider sent itself cannot say.
func errorHandler(w http.ResponseWriter, _ *http.Request, _ error) {
	if f, ok := w.(interface{ UpstreamFailed() }); ok {
		f.UpstreamFailed()
	}
	w.WriteHeader(http.StatusBadGateway)
}
