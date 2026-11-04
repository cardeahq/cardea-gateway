package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

func TestNewSet_BuildsOneProviderPerEntryByKind(t *testing.T) {
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer anthropicUpstream.Close()
	mistralUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer mistralUpstream.Close()
	geminiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer geminiUpstream.Close()

	t.Setenv("TEST_SET_ANTHROPIC_KEY", "sk-a")
	t.Setenv("TEST_SET_MISTRAL_KEY", "sk-m")
	t.Setenv("TEST_SET_GEMINI_KEY", "sk-g")

	entries := []config.ProviderEntry{
		{Name: "anthropic", Kind: "anthropic", BaseURL: anthropicUpstream.URL, APIKeyEnv: "TEST_SET_ANTHROPIC_KEY", Models: []string{"claude-*"}},
		{Name: "mistral", Kind: "mistral", BaseURL: mistralUpstream.URL, APIKeyEnv: "TEST_SET_MISTRAL_KEY", Models: []string{"mistral-*"}},
		{Name: "gemini", Kind: "gemini", BaseURL: geminiUpstream.URL, APIKeyEnv: "TEST_SET_GEMINI_KEY", Models: []string{"gemini-*"}},
	}
	set, err := NewSet(entries)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	for _, name := range []string{"anthropic", "mistral", "gemini"} {
		p, ok := set.Get(name)
		if !ok {
			t.Fatalf("Get(%q) = false, want a provider built for it", name)
		}
		if p.Name() != name {
			t.Fatalf("provider %q has Name() = %q, want %q", name, p.Name(), name)
		}
	}
}

func TestNewSet_GetUnknownNameIsFalse(t *testing.T) {
	set, err := NewSet(nil)
	if err != nil {
		t.Fatalf("NewSet(nil): %v", err)
	}
	if _, ok := set.Get("nope"); ok {
		t.Fatal("Get(\"nope\") = true, want false for an empty set")
	}
}

func TestNewSet_RejectsAnUnknownKind(t *testing.T) {
	t.Setenv("TEST_SET_UNKNOWN_KEY", "sk-x")
	_, err := NewSet([]config.ProviderEntry{
		{Name: "weird", Kind: "openai-generic", BaseURL: "https://example.com", APIKeyEnv: "TEST_SET_UNKNOWN_KEY", Models: []string{"*"}},
	})
	if err == nil {
		t.Fatal("NewSet = nil error, want an error for an unknown kind")
	}
}

func TestNewSet_RejectsAnUnsetKeyVariable(t *testing.T) {
	_, err := NewSet([]config.ProviderEntry{
		{Name: "anthropic", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "TEST_SET_DEFINITELY_UNSET_VAR", Models: []string{"claude-*"}},
	})
	if err == nil {
		t.Fatal("NewSet = nil error, want an error: the key variable is unset")
	}
}

func TestNewSet_ErrorNamesTheProviderNeverAKeyValue(t *testing.T) {
	const secretLikeValue = "sk-this-must-never-appear-in-an-error"
	t.Setenv("TEST_SET_NAMED_ERROR_KEY", secretLikeValue)
	_, err := NewSet([]config.ProviderEntry{
		{Name: "my-anthropic", Kind: "anthropic", BaseURL: "://bad-url", APIKeyEnv: "TEST_SET_NAMED_ERROR_KEY", Models: []string{"claude-*"}},
	})
	if err == nil {
		t.Fatal("NewSet = nil error, want an error for a bad base_url")
	}
	if got := err.Error(); !strings.Contains(got, "my-anthropic") {
		t.Fatalf("error %q does not name the provider", got)
	}
	if strings.Contains(err.Error(), secretLikeValue) {
		t.Fatalf("error %q leaks a key-shaped value", err.Error())
	}
}

func TestNewSingleSet_WrapsTheProviderUnderItsName(t *testing.T) {
	p, err := NewHTTPProxy("custom", "https://example.com")
	if err != nil {
		t.Fatalf("NewHTTPProxy: %v", err)
	}
	set := NewSingleSet("custom", p)

	got, ok := set.Get("custom")
	if !ok || got != p {
		t.Fatalf("Get(%q) = %v, %v, want the wrapped provider, true", "custom", got, ok)
	}
	if _, ok := set.Get("anything-else"); ok {
		t.Fatal("Get on an unrelated name = true, want false: a single set holds exactly one provider")
	}
}
