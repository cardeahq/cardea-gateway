package trace

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// canonicalJSON re-encodes raw with sorted keys, no whitespace, no HTML
// escaping and number text preserved, so equal content gives equal bytes
// whether it was streamed or not. Its errors never quote the input.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, jsonError(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("not valid JSON: trailing data at byte %d", dec.InputOffset())
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, errors.New("could not re-encode JSON")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// jsonError describes a decoding failure by position only, since
// encoding/json's syntax errors quote the offending input character.
func jsonError(err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Errorf("not valid JSON at byte %d", syn.Offset)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("not valid JSON: unexpected end of input")
	}
	return errors.New("not valid JSON")
}

// argumentNames returns the sorted top-level keys of a canonical object,
// or an empty slice for any other JSON value.
func argumentNames(canonical []byte) []string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(canonical, &obj) != nil || obj == nil {
		return []string{}
	}
	return slices.Sorted(maps.Keys(obj))
}

func fingerprint(key, canonical []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(canonical)
	return "fp_" + hex.EncodeToString(m.Sum(nil))[:16]
}
