package identity

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

// looksLikeJWT reports whether credential has the syntactic shape of a
// JWT: exactly three non-empty, base64url-decodable, dot-separated
// segments. It is purely a structural check, not a validity judgment.
func looksLikeJWT(credential string) bool {
	segments := strings.Split(credential, ".")
	if len(segments) != 3 {
		return false
	}
	for _, seg := range segments {
		if seg == "" || !isBase64URLSegment(seg) {
			return false
		}
	}
	return true
}

func isBase64URLSegment(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
