package identity

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// authenticateJWT parses and verifies token against the registry's
// per-agent JWT secrets, returning a Result the same way the API-key
// branch does. The token's own (not yet verified) "sub" claim, when
// readable, selects which agent's secret to check the signature against.
func (r *Registry) authenticateJWT(token string) Result {
	var claimedAgentID string

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		claims, ok := t.Claims.(jwt.MapClaims)
		if !ok {
			return nil, fmt.Errorf("unreadable claims")
		}
		sub, err := claims.GetSubject()
		if err != nil || sub == "" {
			return nil, fmt.Errorf("missing or unreadable sub claim")
		}
		claimedAgentID = sub

		entry, ok := r.byAgent[sub]
		if !ok || entry.jwtSecret == nil {
			return nil, fmt.Errorf("agent %q has no registered JWT material", sub)
		}
		return entry.jwtSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())

	if err == nil && parsed.Valid {
		return Result{OK: true, AgentID: claimedAgentID}
	}

	switch {
	case errors.Is(err, jwt.ErrTokenExpired),
		errors.Is(err, jwt.ErrTokenNotValidYet),
		errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return Result{Reason: ReasonOutsideValidity, ClaimedAgentID: claimedAgentID}
	default:
		return Result{Reason: ReasonNoMatch, ClaimedAgentID: claimedAgentID}
	}
}
