package identity

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

// --- fixtures -------------------------------------------------------------

const alphaAPIKey = "alpha-opaque-api-key-value"

// betaSecret and wrongSecret are both >= 32 bytes, HS256's RFC 7518 §3.2
// floor.
var (
	betaSecret  = bytes.Repeat([]byte("B"), 32)
	wrongSecret = bytes.Repeat([]byte("W"), 32)
)

// newAlphaBetaRegistry builds a two-agent registry: agent-alpha authenticates
// with an API key only, agent-beta with a JWT only. Used by every
// Authenticate-level test below so denial-reason tests share one fixed,
// known registry.
func newAlphaBetaRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("AK_TEST_ALPHA_API_KEY", alphaAPIKey)
	t.Setenv("AK_TEST_BETA_JWT_SECRET", string(betaSecret))

	reg, err := NewRegistry([]config.AgentConfig{
		{AgentID: "agent-alpha", APIKeyEnv: "AK_TEST_ALPHA_API_KEY"},
		{AgentID: "agent-beta", JWTSecretEnv: "AK_TEST_BETA_JWT_SECRET"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// mintJWT signs a JWT with the given method/secret/claims, failing the test
// if signing itself errors (it shouldn't, for the inputs used here).
func mintJWT(t *testing.T, method jwt.SigningMethod, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("signing test JWT: %v", err)
	}
	return s
}

func validClaims(sub string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"sub": sub,
		"iat": jwt.NewNumericDate(now),
		"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp": jwt.NewNumericDate(now.Add(time.Hour)),
	}
}

// --- registry construction -------------------------------------------------

func TestNewRegistry_RejectsUnsetAPIKeyEnv(t *testing.T) {
	agents := []config.AgentConfig{{AgentID: "agent-a", APIKeyEnv: "AK_TEST_DOES_NOT_EXIST_9f3c"}}
	if _, err := NewRegistry(agents); err == nil {
		t.Fatal("NewRegistry with an unset api_key_env = nil error, want error")
	}
}

func TestNewRegistry_RejectsEmptyAPIKeyEnvValue(t *testing.T) {
	t.Setenv("AK_TEST_EMPTY_API_KEY", "")
	agents := []config.AgentConfig{{AgentID: "agent-a", APIKeyEnv: "AK_TEST_EMPTY_API_KEY"}}
	if _, err := NewRegistry(agents); err == nil {
		t.Fatal("NewRegistry with an empty-valued api_key_env = nil error, want error")
	}
}

func TestNewRegistry_RejectsUnsetJWTSecretEnv(t *testing.T) {
	agents := []config.AgentConfig{{AgentID: "agent-a", JWTSecretEnv: "AK_TEST_DOES_NOT_EXIST_JWT_7a1b"}}
	if _, err := NewRegistry(agents); err == nil {
		t.Fatal("NewRegistry with an unset jwt_secret_env = nil error, want error")
	}
}

func TestNewRegistry_RejectsJWTSecretShorterThan32Bytes(t *testing.T) {
	t.Setenv("AK_TEST_SHORT_JWT_SECRET", strings.Repeat("x", 31))
	agents := []config.AgentConfig{{AgentID: "agent-a", JWTSecretEnv: "AK_TEST_SHORT_JWT_SECRET"}}
	if _, err := NewRegistry(agents); err == nil {
		t.Fatal("NewRegistry with a 31-byte jwt_secret_env value = nil error, want error")
	}
}

func TestNewRegistry_AcceptsJWTSecretExactly32Bytes(t *testing.T) {
	t.Setenv("AK_TEST_32_BYTE_JWT_SECRET", strings.Repeat("x", 32))
	agents := []config.AgentConfig{{AgentID: "agent-a", JWTSecretEnv: "AK_TEST_32_BYTE_JWT_SECRET"}}
	if _, err := NewRegistry(agents); err != nil {
		t.Fatalf("NewRegistry with a 32-byte jwt_secret_env value: %v, want success", err)
	}
}

func TestNewRegistry_RejectsTwoAgentsSharingIdenticalLiteralAPIKey(t *testing.T) {
	t.Setenv("AK_TEST_SHARED_KEY_1", "identical-shared-key")
	t.Setenv("AK_TEST_SHARED_KEY_2", "identical-shared-key")
	agents := []config.AgentConfig{
		{AgentID: "agent-a", APIKeyEnv: "AK_TEST_SHARED_KEY_1"},
		{AgentID: "agent-b", APIKeyEnv: "AK_TEST_SHARED_KEY_2"},
	}
	if _, err := NewRegistry(agents); err == nil {
		t.Fatal("NewRegistry with two agents sharing one literal API key = nil error, want error (ambiguous match)")
	}
}

func TestNewRegistry_AcceptsMixedRegistry(t *testing.T) {
	t.Setenv("AK_TEST_MIXED_API_ONLY", "api-only-key")
	t.Setenv("AK_TEST_MIXED_JWT_ONLY", strings.Repeat("j", 32))
	t.Setenv("AK_TEST_MIXED_BOTH_API", "both-shapes-key")
	t.Setenv("AK_TEST_MIXED_BOTH_JWT", strings.Repeat("k", 32))

	agents := []config.AgentConfig{
		{AgentID: "api-only-agent", APIKeyEnv: "AK_TEST_MIXED_API_ONLY"},
		{AgentID: "jwt-only-agent", JWTSecretEnv: "AK_TEST_MIXED_JWT_ONLY"},
		{AgentID: "both-shapes-agent", APIKeyEnv: "AK_TEST_MIXED_BOTH_API", JWTSecretEnv: "AK_TEST_MIXED_BOTH_JWT"},
	}
	reg, err := NewRegistry(agents)
	if err != nil {
		t.Fatalf("NewRegistry with a mixed registry: %v, want success", err)
	}
	if reg == nil {
		t.Fatal("NewRegistry returned a nil *Registry with a nil error")
	}
}

func TestNewRegistry_EmptyAgentListSucceeds(t *testing.T) {
	reg, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v, want success (zero agents is a valid, if maximally restrictive, registry)", err)
	}
	if reg == nil {
		t.Fatal("NewRegistry(nil) returned a nil *Registry with a nil error")
	}
}

// --- shape detection (tier 1 / tier 2) -------------------------------------

func TestRegistry_Authenticate_ShapeDetection(t *testing.T) {
	reg := newAlphaBetaRegistry(t)

	tests := []struct {
		name   string
		header string
	}{
		{"missing Authorization header", ""},
		{"empty Bearer value", "Bearer "},
		{"non-Bearer scheme", "Basic dXNlcjpwYXNz"},
		{"bare token with no scheme at all", alphaAPIKey},
		{"Bearer value containing a byte outside the b64token grammar", "Bearer abc!def"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reg.Authenticate(tt.header)
			if result.OK {
				t.Fatalf("Authenticate(%q).OK = true, want false", tt.header)
			}
			wantReason := ReasonNoCredential
			if tt.header != "" {
				wantReason = ReasonUnrecognizedShape
			}
			if result.Reason != wantReason {
				t.Fatalf("Authenticate(%q).Reason = %q, want %q", tt.header, result.Reason, wantReason)
			}
		})
	}
}

func TestRegistry_Authenticate_MalformedThreeSegmentStringFallsToAPIKeyBranch(t *testing.T) {
	// "abc+.defghi.jklmno" has three dot-separated, non-empty segments (the
	// syntactic shape of a JWT) but its first segment contains "+", which is
	// legal in RFC 6750's b64token grammar (so it passes tier 1) but outside
	// the strict base64url alphabet (so it fails tier 2's JWT sub-shape
	// check). Per the spec, this must still enter the API-key branch rather
	// than the JWT branch. We prove that by registering this exact string as
	// a literal API key: only the API-key branch could ever match it, since
	// no valid HS256 signature could coincidentally verify against it.
	const malformed = "abc+.defghi.jklmno"
	t.Setenv("AK_TEST_MALFORMED_LOOKALIKE_KEY", malformed)
	reg, err := NewRegistry([]config.AgentConfig{
		{AgentID: "lookalike-agent", APIKeyEnv: "AK_TEST_MALFORMED_LOOKALIKE_KEY"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	result := reg.Authenticate("Bearer " + malformed)
	if !result.OK {
		t.Fatalf("Authenticate(malformed 3-segment lookalike) = %+v, want OK (matched via the API-key branch)", result)
	}
	if result.AgentID != "lookalike-agent" {
		t.Fatalf("AgentID = %q, want %q", result.AgentID, "lookalike-agent")
	}
}

// --- API-key matching (tier 3, API-key branch) -----------------------------

func TestRegistry_Authenticate_APIKeyMatching(t *testing.T) {
	reg := newAlphaBetaRegistry(t)

	tests := []struct {
		name        string
		credential  string
		wantOK      bool
		wantAgentID string
	}{
		{"exact match succeeds", alphaAPIKey, true, "agent-alpha"},
		{"case-sensitive: differently-cased key does not match", strings.ToUpper(alphaAPIKey), false, ""},
		{"unregistered opaque key does not match", "some-other-key-nobody-registered", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reg.Authenticate("Bearer " + tt.credential)
			if result.OK != tt.wantOK {
				t.Fatalf("Authenticate(...).OK = %v, want %v", result.OK, tt.wantOK)
			}
			if tt.wantOK {
				if result.AgentID != tt.wantAgentID {
					t.Fatalf("AgentID = %q, want %q", result.AgentID, tt.wantAgentID)
				}
				return
			}
			if result.Reason != ReasonNoMatch {
				t.Fatalf("Reason = %q, want %q", result.Reason, ReasonNoMatch)
			}
			if result.ClaimedAgentID != "" {
				t.Fatalf("ClaimedAgentID = %q, want empty on the API-key branch", result.ClaimedAgentID)
			}
		})
	}
}

func TestRegistry_Authenticate_EmptyRegistryDeniesAPIKeyShapedCredential(t *testing.T) {
	reg, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	result := reg.Authenticate("Bearer any-opaque-key")
	if result.OK {
		t.Fatal("Authenticate against an empty registry = OK true, want false")
	}
	if result.Reason != ReasonNoMatch {
		t.Fatalf("Reason = %q, want %q", result.Reason, ReasonNoMatch)
	}
}

func TestRegistry_Authenticate_EmptyRegistryDeniesJWTShapedCredential(t *testing.T) {
	reg, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	token := mintJWT(t, jwt.SigningMethodHS256, wrongSecret, validClaims("nobody"))
	result := reg.Authenticate("Bearer " + token)
	if result.OK {
		t.Fatal("Authenticate against an empty registry = OK true, want false")
	}
	if result.Reason != ReasonNoMatch {
		t.Fatalf("Reason = %q, want %q", result.Reason, ReasonNoMatch)
	}
}

// --- JWT matching (tier 3, JWT branch) --------------------------------------

func TestRegistry_Authenticate_JWTValidAndInWindowSucceeds(t *testing.T) {
	reg := newAlphaBetaRegistry(t)
	token := mintJWT(t, jwt.SigningMethodHS256, betaSecret, validClaims("agent-beta"))

	result := reg.Authenticate("Bearer " + token)
	if !result.OK {
		t.Fatalf("Authenticate(valid in-window JWT) = %+v, want OK", result)
	}
	if result.AgentID != "agent-beta" {
		t.Fatalf("AgentID = %q, want %q", result.AgentID, "agent-beta")
	}
}

func TestRegistry_Authenticate_JWTDenialCases(t *testing.T) {
	reg := newAlphaBetaRegistry(t)
	now := time.Now()

	tests := []struct {
		name       string
		token      func(t *testing.T) string
		wantReason DenialReason
		// wantClaimedID, when non-empty, asserts ClaimedAgentID equals it.
		// assertClaimedIDEmpty, when true, asserts ClaimedAgentID == "".
		// Neither set means "don't assert" (sub readability is genuinely
		// implementation-defined for that case).
		wantClaimedID        string
		assertClaimedIDEmpty bool
	}{
		{
			name: "wrong secret: signature does not verify",
			token: func(t *testing.T) string {
				return mintJWT(t, jwt.SigningMethodHS256, wrongSecret, validClaims("agent-beta"))
			},
			wantReason:    ReasonNoMatch,
			wantClaimedID: "agent-beta",
		},
		{
			name: "sub names no registered agent",
			token: func(t *testing.T) string {
				return mintJWT(t, jwt.SigningMethodHS256, wrongSecret, validClaims("no-such-agent"))
			},
			wantReason: ReasonNoMatch,
		},
		{
			name: "sub names an agent with no JWT material (API-key-only agent)",
			token: func(t *testing.T) string {
				return mintJWT(t, jwt.SigningMethodHS256, wrongSecret, validClaims("agent-alpha"))
			},
			wantReason: ReasonNoMatch,
		},
		{
			name: "malformed claims: no sub claim at all",
			token: func(t *testing.T) string {
				claims := jwt.MapClaims{"exp": jwt.NewNumericDate(now.Add(time.Hour))}
				return mintJWT(t, jwt.SigningMethodHS256, betaSecret, claims)
			},
			wantReason:           ReasonNoMatch,
			assertClaimedIDEmpty: true,
		},
		{
			name: "signed with a non-HS256 method is rejected without consulting any agent's secret",
			token: func(t *testing.T) string {
				return mintJWT(t, jwt.SigningMethodHS384, betaSecret, validClaims("agent-beta"))
			},
			wantReason: ReasonNoMatch,
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				claims := validClaims("agent-beta")
				claims["exp"] = jwt.NewNumericDate(now.Add(-time.Hour))
				return mintJWT(t, jwt.SigningMethodHS256, betaSecret, claims)
			},
			wantReason:    ReasonOutsideValidity,
			wantClaimedID: "agent-beta",
		},
		{
			name: "not yet valid",
			token: func(t *testing.T) string {
				claims := validClaims("agent-beta")
				claims["nbf"] = jwt.NewNumericDate(now.Add(time.Hour))
				return mintJWT(t, jwt.SigningMethodHS256, betaSecret, claims)
			},
			wantReason:    ReasonOutsideValidity,
			wantClaimedID: "agent-beta",
		},
		{
			name: "missing exp claim entirely",
			token: func(t *testing.T) string {
				claims := jwt.MapClaims{
					"sub": "agent-beta",
					"iat": jwt.NewNumericDate(now),
				}
				return mintJWT(t, jwt.SigningMethodHS256, betaSecret, claims)
			},
			wantReason:    ReasonOutsideValidity,
			wantClaimedID: "agent-beta",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reg.Authenticate("Bearer " + tt.token(t))
			if result.OK {
				t.Fatalf("Authenticate(...) = OK true, want denial with reason %q", tt.wantReason)
			}
			if result.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", result.Reason, tt.wantReason)
			}
			if tt.wantClaimedID != "" && result.ClaimedAgentID != tt.wantClaimedID {
				t.Fatalf("ClaimedAgentID = %q, want %q", result.ClaimedAgentID, tt.wantClaimedID)
			}
			if tt.assertClaimedIDEmpty && result.ClaimedAgentID != "" {
				t.Fatalf("ClaimedAgentID = %q, want empty: sub was never readable in this case", result.ClaimedAgentID)
			}
		})
	}
}

// --- cross-agent attribution (functional acceptance criterion 8) -----------

func TestRegistry_Authenticate_CrossAgentAttributionNeverSwapped(t *testing.T) {
	reg := newAlphaBetaRegistry(t)
	betaToken := mintJWT(t, jwt.SigningMethodHS256, betaSecret, validClaims("agent-beta"))

	alphaResult := reg.Authenticate("Bearer " + alphaAPIKey)
	betaResult := reg.Authenticate("Bearer " + betaToken)

	if !alphaResult.OK || alphaResult.AgentID != "agent-alpha" {
		t.Fatalf("alpha's own credential resolved to %+v, want OK agent-alpha", alphaResult)
	}
	if !betaResult.OK || betaResult.AgentID != "agent-beta" {
		t.Fatalf("beta's own credential resolved to %+v, want OK agent-beta", betaResult)
	}
	if alphaResult.AgentID == betaResult.AgentID {
		t.Fatal("two different agents' own credentials resolved to the same agent_id")
	}
	if alphaResult.AgentID == "agent-beta" || betaResult.AgentID == "agent-alpha" {
		t.Fatal("a credential resolved to the other agent's agent_id: cross-attribution")
	}
}

// --- ClaimedAgentID population contract -------------------------------------

func TestRegistry_Authenticate_ClaimedAgentIDAlwaysEmptyOnAPIKeyBranch(t *testing.T) {
	reg := newAlphaBetaRegistry(t)

	tests := []struct {
		name       string
		credential string
	}{
		{"successful API-key match", alphaAPIKey},
		{"unmatched API-key-shaped credential", "no-such-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reg.Authenticate("Bearer " + tt.credential)
			if result.ClaimedAgentID != "" {
				t.Fatalf("ClaimedAgentID = %q, want empty on the API-key branch", result.ClaimedAgentID)
			}
		})
	}
}

// --- no raw credential leak --------------------------------------------------

func TestRegistry_Authenticate_ResultNeverContainsRawCredential(t *testing.T) {
	reg := newAlphaBetaRegistry(t)
	validToken := mintJWT(t, jwt.SigningMethodHS256, betaSecret, validClaims("agent-beta"))
	badSigToken := mintJWT(t, jwt.SigningMethodHS256, wrongSecret, validClaims("agent-beta"))

	credentials := []string{
		alphaAPIKey,
		"some-unmatched-opaque-key",
		validToken,
		badSigToken,
	}

	for _, cred := range credentials {
		result := reg.Authenticate("Bearer " + cred)
		if strings.Contains(result.AgentID, cred) && cred != "" {
			t.Fatalf("Result.AgentID contains the raw credential for input %q", cred)
		}
		if strings.Contains(string(result.Reason), cred) {
			t.Fatalf("Result.Reason contains the raw credential for input %q", cred)
		}
		if strings.Contains(result.ClaimedAgentID, cred) {
			t.Fatalf("Result.ClaimedAgentID contains the raw credential for input %q", cred)
		}
	}
}
