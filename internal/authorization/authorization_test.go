package authorization

import (
	"strings"
	"testing"
)

// --- Gate construction -------------------------------------------------------

func TestNewGate_RejectsUnsetEnvVar(t *testing.T) {
	if _, err := NewGate([]string{"AK_AUTHZ_TEST_DOES_NOT_EXIST_9f3c"}); err == nil {
		t.Fatal("NewGate with an unset env var = nil error, want error")
	}
}

func TestNewGate_RejectsEmptyEnvVarValue(t *testing.T) {
	t.Setenv("AK_AUTHZ_TEST_EMPTY", "")
	if _, err := NewGate([]string{"AK_AUTHZ_TEST_EMPTY"}); err == nil {
		t.Fatal("NewGate with an empty-valued env var = nil error, want error")
	}
}

func TestNewGate_AcceptsZeroLengthAPIKeyEnvs(t *testing.T) {
	g, err := NewGate(nil)
	if err != nil {
		t.Fatalf("NewGate(nil): %v, want success (zero secrets is a valid, if maximally restrictive, gate)", err)
	}
	if g == nil {
		t.Fatal("NewGate(nil) returned a nil *Gate with a nil error")
	}
}

func TestNewGate_AcceptsOneConfiguredSecret(t *testing.T) {
	t.Setenv("AK_AUTHZ_TEST_ONE_SECRET", "one-secret-value")
	g, err := NewGate([]string{"AK_AUTHZ_TEST_ONE_SECRET"})
	if err != nil {
		t.Fatalf("NewGate with one configured secret: %v, want success", err)
	}
	if g == nil {
		t.Fatal("NewGate returned a nil *Gate with a nil error")
	}
}

func TestNewGate_AcceptsTwoConfiguredSecrets(t *testing.T) {
	t.Setenv("AK_AUTHZ_TEST_TWO_SECRET_1", "secret-one")
	t.Setenv("AK_AUTHZ_TEST_TWO_SECRET_2", "secret-two")
	g, err := NewGate([]string{"AK_AUTHZ_TEST_TWO_SECRET_1", "AK_AUTHZ_TEST_TWO_SECRET_2"})
	if err != nil {
		t.Fatalf("NewGate with two configured secrets: %v, want success", err)
	}
	if g == nil {
		t.Fatal("NewGate returned a nil *Gate with a nil error")
	}
}

// --- Authorize: a gate with configured secrets --------------------------------

const (
	secretOneEnv   = "AK_AUTHZ_TEST_AUTHORIZE_SECRET_1"
	secretTwoEnv   = "AK_AUTHZ_TEST_AUTHORIZE_SECRET_2"
	secretOneValue = "rotation-secret-one-opaque-value"
	secretTwoValue = "rotation-secret-two-opaque-value"
)

// newTwoSecretGate builds a real *Gate with two configured secrets, so
// denial-reason tests run against one fixed, known gate.
func newTwoSecretGate(t *testing.T) *Gate {
	t.Helper()
	t.Setenv(secretOneEnv, secretOneValue)
	t.Setenv(secretTwoEnv, secretTwoValue)
	g, err := NewGate([]string{secretOneEnv, secretTwoEnv})
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return g
}

func TestGate_Authorize_ShapeDetection(t *testing.T) {
	g := newTwoSecretGate(t)

	tests := []struct {
		name       string
		header     string
		wantReason DenialReason
	}{
		{"missing Authorization header", "", ReasonNoCredential},
		{"empty Bearer value", "Bearer ", ReasonUnrecognizedShape},
		{"non-Bearer scheme", "Basic dXNlcjpwYXNz", ReasonUnrecognizedShape},
		{"bare token with no scheme at all", secretOneValue, ReasonUnrecognizedShape},
		{"Bearer value containing a byte outside the b64token grammar", "Bearer abc!def", ReasonUnrecognizedShape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := g.Authorize(tt.header)
			if result.OK {
				t.Fatalf("Authorize(%q).OK = true, want false", tt.header)
			}
			if result.Reason != tt.wantReason {
				t.Fatalf("Authorize(%q).Reason = %q, want %q", tt.header, result.Reason, tt.wantReason)
			}
		})
	}
}

func TestGate_Authorize_SyntacticallyFineValueMatchingNoSecret(t *testing.T) {
	g := newTwoSecretGate(t)

	result := g.Authorize("Bearer some-opaque-value-nobody-configured")
	if result.OK {
		t.Fatal("Authorize(unmatched bearer value).OK = true, want false")
	}
	if result.Reason != ReasonNoMatch {
		t.Fatalf("Reason = %q, want %q", result.Reason, ReasonNoMatch)
	}
}

func TestGate_Authorize_MatchesEitherConfiguredSecret(t *testing.T) {
	g := newTwoSecretGate(t)

	tests := []struct {
		name   string
		secret string
	}{
		{"matches the first secret", secretOneValue},
		{"matches the second secret", secretTwoValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := g.Authorize("Bearer " + tt.secret)
			if !result.OK {
				t.Fatalf("Authorize(%q) = %+v, want OK", tt.name, result)
			}
			if result.Reason != "" {
				t.Fatalf("Reason = %q, want empty on success", result.Reason)
			}
		})
	}
}

func TestGate_Authorize_CaseSensitiveMatch(t *testing.T) {
	g := newTwoSecretGate(t)
	result := g.Authorize("Bearer " + strings.ToUpper(secretOneValue))
	if result.OK {
		t.Fatal("Authorize with a differently-cased secret = OK true, want false")
	}
	if result.Reason != ReasonNoMatch {
		t.Fatalf("Reason = %q, want %q", result.Reason, ReasonNoMatch)
	}
}

// --- Authorize: a gate built from zero api_key_envs ---------------------------
//
// functional spec acceptance criterion 5: zero shared secrets configured
// denies every call, with or without a credential.

func TestGate_Authorize_ZeroSecretsDeniesEveryCase(t *testing.T) {
	g, err := NewGate(nil)
	if err != nil {
		t.Fatalf("NewGate(nil): %v", err)
	}

	tests := []struct {
		name       string
		header     string
		wantReason DenialReason
	}{
		{"missing Authorization header", "", ReasonNoCredential},
		{"empty string header value", "", ReasonNoCredential},
		{"non-Bearer scheme", "Basic dXNlcjpwYXNz", ReasonUnrecognizedShape},
		{"empty Bearer value", "Bearer ", ReasonUnrecognizedShape},
		{"a syntactically valid bearer value", "Bearer any-plausible-opaque-credential", ReasonNoMatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := g.Authorize(tt.header)
			if result.OK {
				t.Fatalf("Authorize(%q) against a zero-secret gate = OK true, want false", tt.header)
			}
			if result.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", result.Reason, tt.wantReason)
			}
		})
	}
}

// --- no raw credential leak ----------------------------------------------------

func TestGate_Authorize_ResultNeverContainsRawCredential(t *testing.T) {
	g := newTwoSecretGate(t)

	credentials := []string{
		secretOneValue,
		secretTwoValue,
		"some-unmatched-opaque-credential",
	}
	for _, cred := range credentials {
		result := g.Authorize("Bearer " + cred)
		if strings.Contains(string(result.Reason), cred) {
			t.Fatalf("Result.Reason contains the raw credential for input %q", cred)
		}
	}

	// Also true of a zero-secret gate, whose every outcome is a denial.
	zero, err := NewGate(nil)
	if err != nil {
		t.Fatalf("NewGate(nil): %v", err)
	}
	for _, cred := range credentials {
		result := zero.Authorize("Bearer " + cred)
		if strings.Contains(string(result.Reason), cred) {
			t.Fatalf("zero-secret gate: Result.Reason contains the raw credential for input %q", cred)
		}
	}
}
