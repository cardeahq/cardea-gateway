package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"
)

// MaxModelLen bounds a model name in logs, denial reasons and trace records.
const MaxModelLen = 256

// ErrRepeatedModel is returned by ExtractModel when a request body names
// its top-level model more than once.
var ErrRepeatedModel = errors.New("request names its model more than once")

// ExtractModel returns the model for format f (specs/0009-technical,
// Design -> Model extraction). found is false when there is no usable
// model. err is ErrRepeatedModel or nil.
func ExtractModel(f Format, r *http.Request, body []byte) (model string, found bool, err error) {
	switch f {
	case FormatAnthropic, FormatOpenAI:
		return extractTopLevelModel(body)
	case FormatGemini:
		model, _, ok := geminiModelAndAction(r.URL.Path)
		return model, ok, nil
	default:
		return "", false, nil
	}
}

// extractTopLevelModel scans body token by token, reading only the
// top-level "model" member. A nested "model" key (inside an object or
// array value) is never read. A second top-level "model" member returns
// ErrRepeatedModel.
func extractTopLevelModel(body []byte) (model string, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, derr := dec.Token()
	if derr != nil {
		return "", false, nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", false, nil
	}

	seenModel := false
	for dec.More() {
		keyTok, derr := dec.Token()
		if derr != nil {
			return "", false, nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", false, nil
		}
		valTok, derr := dec.Token()
		if derr != nil {
			return "", false, nil
		}
		isModelKey := key == "model"
		if isModelKey && seenModel {
			return "", false, ErrRepeatedModel
		}
		if d, isDelim := valTok.(json.Delim); isDelim {
			if err := skipValue(dec, d); err != nil {
				return "", false, nil
			}
			if isModelKey {
				seenModel = true
			}
			continue
		}
		if isModelKey {
			seenModel = true
			if s, ok := valTok.(string); ok && s != "" {
				model, found = s, true
			} else {
				found = false
			}
		}
	}
	return model, found, nil
}

// skipValue consumes tokens from dec until the value opened by open is
// fully consumed, keeping nested objects/arrays balanced.
func skipValue(dec *json.Decoder, open json.Delim) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// Truncate shortens s to at most MaxModelLen bytes on a UTF-8 boundary.
func Truncate(s string) string {
	if len(s) <= MaxModelLen {
		return s
	}
	n := MaxModelLen
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
