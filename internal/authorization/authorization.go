// Package authorization gates proxied calls behind a shared secret,
// independent of any per-agent identity.
package authorization

import (
	"fmt"
	"os"
	"strings"
)

// DenialReason is the audit-only category for a failed authorization
// attempt. Never sent to the caller.
type DenialReason string

const (
	ReasonNoCredential      DenialReason = "no_credential"
	ReasonUnrecognizedShape DenialReason = "unrecognized_shape"
	ReasonNoMatch           DenialReason = "no_match"
)

// Result is Authorize's outcome. Reason is audit-only and must never
// reach the caller.
type Result struct {
	OK     bool
	Reason DenialReason // set on any denial
}

// Gate holds the set of valid shared secrets for the life of the
// process.
type Gate struct {
	secrets map[string]bool
}

// NewGate reads each named env var and registers its value as a valid
// shared secret. It fails if any named var is unset or empty. A Gate
// built from a zero-length apiKeyEnvs is valid — Authorize then denies
// every call, matching specs/0002's "zero agents" precedent.
func NewGate(apiKeyEnvs []string) (*Gate, error) {
	secrets := make(map[string]bool, len(apiKeyEnvs))
	for _, env := range apiKeyEnvs {
		value := os.Getenv(env)
		if value == "" {
			return nil, fmt.Errorf("environment variable %q (api_key_envs) is not set", env)
		}
		secrets[value] = true
	}
	return &Gate{secrets: secrets}, nil
}

// Authorize validates one raw Authorization header value (as returned by
// http.Header.Get; "" if the header is absent) against the Gate's
// secrets.
func (g *Gate) Authorize(authorizationHeader string) Result {
	if authorizationHeader == "" {
		return Result{Reason: ReasonNoCredential}
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(authorizationHeader, prefix) {
		return Result{Reason: ReasonUnrecognizedShape}
	}
	credential := authorizationHeader[len(prefix):]
	if !isBearerToken68(credential) {
		return Result{Reason: ReasonUnrecognizedShape}
	}

	if !g.secrets[credential] {
		return Result{Reason: ReasonNoMatch}
	}
	return Result{OK: true}
}
