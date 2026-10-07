package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/cardeahq/cardea-gateway/internal/identity"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

const identityDenialMessage = "could not identify caller"

// maxClaimedAgentIDLen bounds how much of an unverified, attacker-influenced
// JWT "sub" claim reaches a log line.
const maxClaimedAgentIDLen = 128

type identityStage struct {
	registry *identity.Registry
}

func (s *identityStage) Name() string { return "identification" }

func (s *identityStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	result := s.registry.Authenticate(c.Request.Header.Get("Authorization"))
	if !result.OK {
		reason := string(result.Reason)
		if result.ClaimedAgentID != "" {
			reason += ": claimed agent_id=" + sanitizeClaimed(result.ClaimedAgentID)
		}
		return pipeline.Deny(http.StatusUnauthorized, identityDenialMessage, reason), nil
	}
	return pipeline.AllowAs(result.AgentID), nil
}

// sanitizeClaimed truncates and strips control characters from an
// unverified JWT "sub" claim before it reaches a log line, since it's
// attacker-influenced input even though it isn't the raw credential.
func sanitizeClaimed(claimed string) string {
	var b strings.Builder
	for _, r := range claimed {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) > maxClaimedAgentIDLen {
		s = s[:maxClaimedAgentIDLen]
	}
	return s
}
