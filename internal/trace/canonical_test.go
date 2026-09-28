package trace

import "testing"

func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"keys are sorted", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"nested keys are sorted", `{"z":{"y":1,"x":2},"a":[]}`, `{"a":[],"z":{"x":2,"y":1}}`},
		{"whitespace is removed", " {\n  \"a\" : [ 1 , 2 ] ,\t\"b\" : \"c d\"\n} ", `{"a":[1,2],"b":"c d"}`},
		{"array order is preserved", `[3,1,2]`, `[3,1,2]`},
		{"HTML characters are not escaped", `{"cmd":"a < b && c > d"}`, `{"cmd":"a < b && c > d"}`},
		{"an escaped < equals a literal <", `{"cmd":"<"}`, `{"cmd":"<"}`},
		{"a decimal keeps its text", `{"n":1.0}`, `{"n":1.0}`},
		{"an integer keeps its text", `{"n":1}`, `{"n":1}`},
		{"an exponent keeps its text", `{"n":1e3}`, `{"n":1e3}`},
		{"a string is a valid canonical value", ` "hello" `, `"hello"`},
		{"null is a valid canonical value", `null`, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalJSON([]byte(tt.in))
			if err != nil {
				t.Fatalf("canonicalJSON(%q): %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Fatalf("canonicalJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalJSON_EquivalentInputsGiveIdenticalBytes(t *testing.T) {
	a, err := canonicalJSON([]byte(`{"command": "ls -la", "cwd": "/tmp"}`))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	b, err := canonicalJSON([]byte("{\"cwd\":\"/tmp\",\n\"command\":\"ls -la\"}"))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical forms differ: %q vs %q", a, b)
	}
}

func TestCanonicalJSON_DistinctNumberTextStaysDistinct(t *testing.T) {
	a, _ := canonicalJSON([]byte(`{"n":1.0}`))
	b, _ := canonicalJSON([]byte(`{"n":1}`))
	if string(a) == string(b) {
		t.Fatalf("canonicalJSON gave %q for both 1.0 and 1: number text must be preserved", a)
	}
}

func TestCanonicalJSON_RejectsInvalidJSON(t *testing.T) {
	for _, in := range []string{``, `{"a":`, `{"a":1}}`, `not json`} {
		if _, err := canonicalJSON([]byte(in)); err == nil {
			t.Fatalf("canonicalJSON(%q) = nil error, want error", in)
		}
	}
}
