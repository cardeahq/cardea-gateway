package gateway

import (
	"testing"
)

func TestFilterInvisibleChars(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		expected        string
		expectedSmuggle bool
	}{
		// Normal text - unchanged
		{
			name:            "plain ASCII",
			input:           "Hello, World!",
			expected:        "Hello, World!",
			expectedSmuggle: false,
		},
		{
			name:            "valid UTF-8",
			input:           "Café Résumé 你好",
			expected:        "Café Résumé 你好",
			expectedSmuggle: false,
		},
		{
			name:            "empty input",
			input:           "",
			expected:        "",
			expectedSmuggle: false,
		},

		// Control characters replaced with spaces
		{
			name:            "null byte",
			input:           "hello\x00world",
			expected:        "hello world",
			expectedSmuggle: true,
		},
		{
			name:            "tab and newline",
			input:           "hello\tworld\n",
			expected:        "hello world ",
			expectedSmuggle: true,
		},
		{
			name:            "all C0 controls",
			input:           "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0B\x0C\r\x0E\x0F\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1A\x1B\x1C\x1D\x1E\x1F",
			expected:        "                                ", // 32 spaces
			expectedSmuggle: true,
		},
		// C1 control (DEL)
		{
			name:            "DEL character",
			input:           "hello\x7Fworld",
			expected:        "hello world",
			expectedSmuggle: true,
		},

		// Bidi override replaced with spaces
		{
			name:            "RLO - Right-to-Left Override",
			input:           "hello\u202Eworld",
			expected:        "hello world",
			expectedSmuggle: true,
		},
		{
			name:            "LRO - Left-to-Right Override",
			input:           "hello\u202Dworld",
			expected:        "hello world",
			expectedSmuggle: true,
		},
		{
			name:            "all bidi overrides",
			input:           "\u202A\u202B\u202C\u202D\u202E",
			expected:        "     ",
			expectedSmuggle: true,
		},

		// Zero-width characters replaced with spaces
		{
			name:            "zero-width space",
			input:           "hello\u200Bworld",
			expected:        "hello world",
			expectedSmuggle: true,
		},
		{
			name:            "byte order mark",
			input:           "hello\uFEFFworld",
			expected:        "hello world",
			expectedSmuggle: true,
		},

		// Mixed content
		{
			name:            "normal text with bidi override",
			input:           "user\u202Eexploit.exe",
			expected:        "user exploit.exe",
			expectedSmuggle: true,
		},
		{
			name:            "JSON with control chars",
			input:           "{\"name\":\"test\x00value\"}",
			expected:        "{\"name\":\"test value\"}",
			expectedSmuggle: true,
		},

		// Edge case: invalid UTF-8
		{
			name:            "invalid UTF-8 byte",
			input:           "hello\xFFworld",
			expected:        "hello\xFFworld",
			expectedSmuggle: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, hadSmuggling := FilterInvisibleChars([]byte(tt.input))
			result := string(cleaned)

			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
			if hadSmuggling != tt.expectedSmuggle {
				t.Errorf("expected hasSmuggling=%v, got %v", tt.expectedSmuggle, hadSmuggling)
			}
		})
	}
}

func TestFilterInvisibleCharsPerformance(t *testing.T) {
	largeInput := make([]byte, 100000)
	for i := range largeInput {
		largeInput[i] = byte(i % 256)
	}

	cleaned, hadSmuggling := FilterInvisibleChars(largeInput)
	if !hadSmuggling {
		t.Error("expected smuggling to be detected in large input with control chars")
	}
	// With space replacement, output should be >= input (control chars replaced with 1-byte space)
	if len(cleaned) < len(largeInput) {
		t.Error("expected cleaned to be >= input length with space replacement")
	}
}
