package routing

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func gemReq(path string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, nil)
}

func TestExtractModel_AnthropicAndOpenAI(t *testing.T) {
	tests := []struct {
		name      string
		format    Format
		body      string
		wantModel string
		wantFound bool
		wantErr   error
	}{
		{"string model", FormatAnthropic, `{"model":"claude-sonnet-5","messages":[]}`, "claude-sonnet-5", true, nil},
		{"string model, openai format", FormatOpenAI, `{"model":"mistral-large-latest"}`, "mistral-large-latest", true, nil},
		{"missing model", FormatAnthropic, `{"messages":[]}`, "", false, nil},
		{"non-string model", FormatAnthropic, `{"model":123}`, "", false, nil},
		{"empty string model", FormatAnthropic, `{"model":""}`, "", false, nil},
		{"non-object body (array)", FormatAnthropic, `[1,2,3]`, "", false, nil},
		{"non-object body (string)", FormatAnthropic, `"just a string"`, "", false, nil},
		{"invalid JSON", FormatAnthropic, `not json at all`, "", false, nil},
		{"empty body", FormatAnthropic, ``, "", false, nil},
		{"repeated top-level model", FormatAnthropic, `{"model":"a","model":"b"}`, "", false, ErrRepeatedModel},
		{"repeated top-level model, same value", FormatAnthropic, `{"model":"a","model":"a"}`, "", false, ErrRepeatedModel},
		{"nested model key is ignored", FormatAnthropic, `{"wrapper":{"model":"nested"},"messages":[]}`, "", false, nil},
		{"nested model key does not count as a repeat of the top-level one", FormatAnthropic, `{"model":"top","wrapper":{"model":"nested"}}`, "top", true, nil},
		{"model inside an array element is not top-level", FormatAnthropic, `{"messages":[{"model":"not-top"}]}`, "", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			model, found, err := ExtractModel(tt.format, r, []byte(tt.body))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if model != tt.wantModel {
				t.Fatalf("model = %q, want %q", model, tt.wantModel)
			}
		})
	}
}

func TestExtractModel_Gemini(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantModel string
		wantFound bool
	}{
		{"model segment read from the path", "/v1beta/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro", true},
		{"streamGenerateContent action", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", "gemini-2.5-pro", true},
		{"no model segment", "/v1beta/models", "", false},
		{"no model segment, bare prefix", "/v1beta/", "", false},
		// AC26: Go has already percent-decoded the path once; ExtractModel
		// must not decode it again, so a caller-doubled escape survives as
		// a literal "%" in the recorded model.
		{"doubled percent-encoding is decoded only once", "/v1beta/models/gemini-%2570ro:generateContent", "gemini-%70ro", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, found, err := ExtractModel(FormatGemini, gemReq(tt.path), nil)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if model != tt.wantModel {
				t.Fatalf("model = %q, want %q", model, tt.wantModel)
			}
		})
	}
}

func TestExtractModel_GeminiDoesNotReadTheBody(t *testing.T) {
	// A garbage, even unreadable-looking body must not affect Gemini
	// model extraction or produce an error: the body is not read.
	r := gemReq("/v1beta/models/gemini-2.5-pro:generateContent")
	model, found, err := ExtractModel(FormatGemini, r, []byte(`{not even json`))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !found || model != "gemini-2.5-pro" {
		t.Fatalf("model=%q found=%v, want gemini-2.5-pro/true regardless of the body", model, found)
	}
}

func TestExtractModel_UnknownFormatHasNoModel(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/no/such/route", nil)
	model, found, err := ExtractModel(FormatUnknown, r, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found || model != "" {
		t.Fatalf("model=%q found=%v, want empty/false for unknown format", model, found)
	}
}

func TestTruncate(t *testing.T) {
	if MaxModelLen != 256 {
		t.Fatalf("MaxModelLen = %d, want 256", MaxModelLen)
	}

	t.Run("short string is unchanged", func(t *testing.T) {
		if got := Truncate("claude-sonnet-5"); got != "claude-sonnet-5" {
			t.Fatalf("Truncate = %q, want unchanged", got)
		}
	})

	t.Run("exactly MaxModelLen bytes is unchanged", func(t *testing.T) {
		s := strings.Repeat("a", MaxModelLen)
		if got := Truncate(s); got != s {
			t.Fatalf("len(Truncate) = %d, want %d unchanged", len(got), len(s))
		}
	})

	t.Run("a 300-byte model is cut to at most 256 bytes on a UTF-8 boundary", func(t *testing.T) {
		// "★" (U+2605) is 3 bytes in UTF-8; 100 of them is 300 bytes, and
		// byte offset 256 (not a multiple of 3) falls inside a rune.
		s := strings.Repeat("★", 100)
		if len(s) != 300 {
			t.Fatalf("test setup: len(s) = %d, want 300", len(s))
		}
		got := Truncate(s)
		if len(got) > MaxModelLen {
			t.Fatalf("len(Truncate(s)) = %d, want <= %d", len(got), MaxModelLen)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(s) = %q is not valid UTF-8", got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("Truncate(s) = %q is not a prefix of the original", got)
		}
		if len(got) != 255 {
			t.Fatalf("len(Truncate(s)) = %d, want 255 (backs off to the last full rune at or before byte 256)", len(got))
		}
	})

	t.Run("ASCII model longer than the limit is cut to exactly the limit", func(t *testing.T) {
		s := strings.Repeat("m", 300)
		got := Truncate(s)
		if len(got) != MaxModelLen {
			t.Fatalf("len(Truncate(s)) = %d, want %d", len(got), MaxModelLen)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		if got := Truncate(""); got != "" {
			t.Fatalf("Truncate(\"\") = %q, want empty", got)
		}
	})
}

func TestGeminiStreams(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1beta/models/gemini-2.5-pro:streamGenerateContent", true},
		{"/v1beta/models/gemini-2.5-pro:generateContent", false},
		{"/v1beta/models/gemini-2.5-pro:countTokens", false},
		{"/v1beta/models", false},
		{"/v1/messages", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := GeminiStreams(tt.path); got != tt.want {
				t.Fatalf("GeminiStreams(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
