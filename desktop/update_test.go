package main

import "testing"

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"0.2.3", "v0.2.3", false},
		{"0.2.3", "0.2.3", false},
		{"0.2.2", "v0.2.3", true},
		{"0.2.3", "v0.2.4", true},
		{"0.2.3", "v0.3.0", true},
		{"0.2.3", "v1.0.0", true},
		{"0.2.3", "v0.2.2", false},
		{"0.2.3", "v0.2.30", true},
		{"0.2.3", "v0.2", false},   // shorter can never be newer
		{"0.2", "v0.2.1", true},    // longer wins only past a shared prefix
		{"0.2.3", "latest", false}, // unparsable is never newer
		{"", "v0.2.3", false},
		{"0.2.3", "", false},
		{"oops", "v0.2.3", false},
	}
	for _, c := range cases {
		if got := isNewerVersion(c.current, c.tag); got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.current, c.tag, got, c.want)
		}
	}
}
