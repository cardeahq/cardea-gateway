package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// Helpers for reading a provider's responses through a real trace.Tracer's
// tap, the way the gateway does.

const testKeyEnv = "AK_PROVIDER_TEST_FINGERPRINT_KEY"

var testKey = strings.Repeat("k", 32)

// newTestTracer builds a Tracer keyed with testKey. mutate, if non-nil,
// adjusts the config before construction.
func newTestTracer(t *testing.T, mutate func(*config.TracingConfig)) *trace.Tracer {
	t.Helper()
	t.Setenv(testKeyEnv, testKey)
	cfg := config.TracingConfig{
		FingerprintKeyEnv: testKeyEnv,
		MaxReadBytes:      1 << 20,
		Sink:              config.TraceSinkConfig{Type: "logging"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	tr, err := trace.NewTracer(cfg, nil, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	return tr
}

func expectedFingerprint(key, canonical string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(canonical))
	return "fp_" + hex.EncodeToString(m.Sum(nil))[:16]
}

func ptr[T any](v T) *T { return &v }

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
