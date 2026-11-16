package gateway

import (
	"bytes"
	"unicode/utf8"
)

func FilterInvisibleChars(body []byte) (cleaned []byte, hadSumggling bool) {
	var buf bytes.Buffer
	hasSmuggling := false
	for len(body) > 0 {
		r, size := utf8.DecodeRune(body)
		if r == utf8.RuneError {
			buf.WriteByte(body[0])
			body = body[1:]
			continue
		}
		if !isSmugglingRune(r) {
			buf.WriteRune(r)
		} else {
			buf.WriteByte(' ')
			hasSmuggling = true
		}
		body = body[size:]
		if len(body) == 0 {
			break
		}
	}
	return buf.Bytes(), hasSmuggling
}

func isSmugglingRune(r rune) bool {
	// Control chars
	if (r >= 0x0000 && r <= 0x1F) || (r >= 0x7F && r <= 0x9F) {
		return true
	}

	// Bidi override
	if r >= 0x202A && r <= 0x202E {
		return true
	}

	// Zero width
	if (r >= 0x200B && r <= 0x200F) || r == 0xFEFF {
		return true
	}

	return false
}
