// Package identity authenticates a proxied call's bearer credential
// against an operator-configured registry of agents, resolving it to
// exactly one agent_id or a denial reason.
package identity

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

// DenialReason is the audit-only category for a failed identification
// attempt. Never sent to the caller.
type DenialReason string

const (
	ReasonNoCredential      DenialReason = "no_credential"
	ReasonUnrecognizedShape DenialReason = "unrecognized_shape"
	ReasonNoMatch           DenialReason = "no_match"
	ReasonOutsideValidity   DenialReason = "outside_validity_window"
)

// minJWTSecretBytes is HS256's RFC 7518 §3.2 floor.
const minJWTSecretBytes = 32

// Result is Authenticate's outcome. Reason and ClaimedAgentID are
// audit-only and must never reach the caller.
type Result struct {
	OK      bool
	AgentID string // set on success

	Reason         DenialReason // set on any denial
	ClaimedAgentID string       // unverified "sub" a JWT claimed, when
	// readable, even on denial; always "" for the API-key branch
}

// agentEntry is one registered agent's verification material. Either
// field may be nil/"" — NewRegistry requires at least one to be set, but
// not both.
type agentEntry struct {
	apiKey    string // "" if this agent has no API-key credential
	jwtSecret []byte // nil if this agent has no JWT credential
}

type Registry struct {
	byAPIKey map[string]string      // api key -> agent_id
	byAgent  map[string]*agentEntry // agent_id -> entry
}

// NewRegistry builds a Registry from config, reading each entry's
// api_key_env / jwt_secret_env from the process environment — never from
// YAML, matching config.ProviderConfig.APIKeyEnv's existing pattern. It
// fails if an entry names an env var that is unset or empty, a
// jwt_secret_env resolves to fewer than 32 bytes (HS256's RFC 7518 §3.2
// floor), or two entries register the identical literal API key.
// config.Load has already rejected a duplicate agent_id or an entry with
// neither field set by the time this runs.
func NewRegistry(agents []config.AgentConfig) (*Registry, error) {
	r := &Registry{
		byAPIKey: make(map[string]string),
		byAgent:  make(map[string]*agentEntry),
	}

	for _, a := range agents {
		entry := &agentEntry{}

		if a.APIKeyEnv != "" {
			key := os.Getenv(a.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("agent %q: environment variable %q (api_key_env) is not set", a.AgentID, a.APIKeyEnv)
			}
			if existing, ok := r.byAPIKey[key]; ok {
				return nil, fmt.Errorf("agents %q and %q register the identical API key", existing, a.AgentID)
			}
			entry.apiKey = key
			r.byAPIKey[key] = a.AgentID
		}

		if a.JWTSecretEnv != "" {
			secret := os.Getenv(a.JWTSecretEnv)
			if secret == "" {
				return nil, fmt.Errorf("agent %q: environment variable %q (jwt_secret_env) is not set", a.AgentID, a.JWTSecretEnv)
			}
			if len(secret) < minJWTSecretBytes {
				return nil, fmt.Errorf("agent %q: jwt_secret_env %q resolves to %d bytes, want at least %d", a.AgentID, a.JWTSecretEnv, len(secret), minJWTSecretBytes)
			}
			entry.jwtSecret = []byte(secret)
		}

		r.byAgent[a.AgentID] = entry
	}

	return r, nil
}

// Authenticate validates one raw Authorization header value (as returned
// by http.Header.Get; "" if the header is absent) and returns exactly one
// Result. All identification logic lives here — internal/gateway's stage
// wrapper contains none of its own.
func (r *Registry) Authenticate(authorizationHeader string) Result {
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

	if looksLikeJWT(credential) {
		return r.authenticateJWT(credential)
	}

	agentID, ok := r.byAPIKey[credential]
	if !ok {
		return Result{Reason: ReasonNoMatch}
	}
	return Result{OK: true, AgentID: agentID}
}
