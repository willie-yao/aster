package textutil

import (
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"under limit", "hello", 10, "hello"},
		{"at limit", "hello", 5, "hello"},
		{"over limit ascii", "hello world", 5, "hello…"},
		{"zero max", "hello", 0, "…"},
		{"negative max", "hello", -3, "…"},
		{"empty", "", 5, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.s, tc.max); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
		})
	}
}

// TestTruncate_RuneSafe verifies a multi-byte rune is never split: cutting a
// 3-byte rune (a CJK char here) at a mid-rune byte backs up to the boundary.
func TestTruncate_RuneSafe(t *testing.T) {
	s := "a\u4e16\u754c" // "a" + two 3-byte runes, 7 bytes total
	// max=2 lands inside the first multi-byte rune (bytes 1..3); expect only "a".
	got := Truncate(s, 2)
	if got != "a…" {
		t.Fatalf("Truncate(%q, 2) = %q, want %q", s, got, "a…")
	}
	// The prefix before the ellipsis must be valid UTF-8 with no partial rune.
	prefix := strings.TrimSuffix(got, "…")
	if !isValidUTF8(prefix) {
		t.Errorf("prefix %q is not valid UTF-8 (rune split)", prefix)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestTrimCredential(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		trimmed bool
	}{
		{"clean", "ghp_abc123", "ghp_abc123", false},
		{"trailing newline", "ghp_abc123\n", "ghp_abc123", true},
		{"trailing crlf", "ghp_abc123\r\n", "ghp_abc123", true},
		{"leading space", " ghp_abc123", "ghp_abc123", true},
		{"empty", "", "", false},
		{"whitespace only", "\n", "", true},
		{"inner space preserved", "a b", "a b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, trimmed := TrimCredential(tc.in)
			if got != tc.want || trimmed != tc.trimmed {
				t.Errorf("TrimCredential(%q) = %q, %v; want %q, %v", tc.in, got, trimmed, tc.want, tc.trimmed)
			}
		})
	}
}
