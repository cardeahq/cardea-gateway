package provider

import (
	"fmt"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

// Set holds every built provider by configured name.
type Set struct {
	byName map[string]Provider
}

// NewSet builds one provider per entry by kind. It fails on an unknown
// kind or any constructor error, including an unset key variable.
func NewSet(entries []config.ProviderEntry) (*Set, error) {
	byName := make(map[string]Provider, len(entries))
	for _, e := range entries {
		var p Provider
		var err error
		switch e.Kind {
		case "anthropic":
			p, err = NewAnthropicProvider(e.BaseURL, e.APIKeyEnv)
		case "mistral":
			p, err = NewMistralProvider(e.BaseURL, e.APIKeyEnv)
		case "gemini":
			p, err = NewGeminiProvider(e.BaseURL, e.APIKeyEnv)
		default:
			err = fmt.Errorf("unknown kind %q", e.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", e.Name, err)
		}
		byName[e.Name] = p
	}
	return &Set{byName: byName}, nil
}

// NewSingleSet wraps the single provider under its configured name.
func NewSingleSet(name string, p Provider) *Set {
	return &Set{byName: map[string]Provider{name: p}}
}

// Get returns the provider configured under name.
func (s *Set) Get(name string) (Provider, bool) {
	p, ok := s.byName[name]
	return p, ok
}
