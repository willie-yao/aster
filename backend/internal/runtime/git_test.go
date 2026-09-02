package runtime

import (
	"strings"
	"testing"
)

func TestTail(t *testing.T) {
	if got := tail("short", 100); got != "short" {
		t.Errorf("tail short = %q", got)
	}
	got := tail("0123456789", 4)
	if !strings.HasSuffix(got, "6789") || !strings.Contains(got, "truncated") {
		t.Errorf("tail truncation = %q", got)
	}
}

func TestRedactToken(t *testing.T) {
	got := redactToken("url https://x-access-token:secret@github.com token=secret", "secret")
	if strings.Contains(got, "secret") {
		t.Errorf("token not redacted: %q", got)
	}
	if strings.Count(got, "REDACTED") != 2 {
		t.Errorf("redacted output = %q, want both token occurrences replaced", got)
	}
	if got := redactToken("no token here", ""); got != "no token here" {
		t.Errorf("empty token changed output: %q", got)
	}
}
