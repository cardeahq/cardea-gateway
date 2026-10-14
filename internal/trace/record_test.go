package trace

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"
)

// assertKeys fails unless obj has exactly the keys in want: every field of
// a v1 record is always emitted, and nothing else is.
func assertKeys(t *testing.T, where string, obj map[string]any, want ...string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(obj))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s keys = %v, want exactly %v", where, got, want)
	}
}

func asObject(t *testing.T, where string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON object", where, v)
	}
	return m
}

func asArray(t *testing.T, where string, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON array (never null)", where, v)
	}
	return a
}

func TestTimestamp_MarshalsRFC3339UTCWithSixFractionalDigits(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "microseconds kept, zone converted to UTC",
			in:   time.Date(2026, 9, 27, 16, 3, 11, 402113000, cest),
			want: `"2026-09-27T14:03:11.402113Z"`,
		},
		{
			name: "whole second still has six fractional digits",
			in:   time.Date(2026, 9, 27, 14, 3, 11, 0, time.UTC),
			want: `"2026-09-27T14:03:11.000000Z"`,
		},
		{
			name: "trailing zeros in the fraction are not trimmed",
			in:   time.Date(2026, 9, 27, 14, 3, 11, 400000000, time.UTC),
			want: `"2026-09-27T14:03:11.400000Z"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(Timestamp(tt.in))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("Marshal(Timestamp) = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSchema_IsV1(t *testing.T) {
	if Schema != "cardea-gateway.trace/v1" {
		t.Fatalf("Schema = %q, want %q", Schema, "cardea-gateway.trace/v1")
	}
}

// --- StageDetail (specs/0006 adds Redactions) ------------------------------------

func TestStageDetail_MarshalsRedactionsAlongsideThePolicyFields(t *testing.T) {
	tests := []struct {
		name string
		in   StageDetail
		want map[string]any
	}{
		{
			name: "zero value: every field is its zero/null, not omitted",
			in:   StageDetail{},
			want: map[string]any{"revision": "", "rules": nil, "redactions": nil},
		},
		{
			name: "a policy detail leaves redactions null",
			in:   StageDetail{Revision: "r7", Rules: []string{"no-rm-rf"}},
			want: map[string]any{"revision": "r7", "rules": []any{"no-rm-rf"}, "redactions": nil},
		},
		{
			name: "a filtering detail leaves revision empty and rules null",
			in:   StageDetail{Redactions: map[string]int{"email": 2, "ssn": 1}},
			want: map[string]any{"revision": "", "rules": nil, "redactions": map[string]any{"email": float64(2), "ssn": float64(1)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Marshal(%+v) = %s, want fields %v", tt.in, raw, tt.want)
			}
		})
	}
}
