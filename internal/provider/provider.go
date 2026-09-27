// Package provider defines the Provider abstraction that lets the gateway
// route requests to different upstream LLM APIs interchangeably.
package provider

import "net/http"

type Provider interface {
	Name() string
	Handler() http.Handler
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
