// Package provider defines the Provider abstraction that lets the gateway
// route requests to different upstream LLM APIs interchangeably.
package provider

import "net/http"

type Provider interface {
	Name() string
	Handler() http.Handler
}
