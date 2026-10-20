package authorization

import "strings"

// isBearerToken68 reports whether s matches RFC 6750 §2.1's b64token
// grammar: one or more characters from [A-Za-z0-9-._~+/], followed by zero
// or more trailing "=" padding characters.
func isBearerToken68(s string) bool {
	body := strings.TrimRight(s, "=")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if !isB64TokenChar(body[i]) {
			return false
		}
	}
	return true
}

func isB64TokenChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/':
		return true
	default:
		return false
	}
}
