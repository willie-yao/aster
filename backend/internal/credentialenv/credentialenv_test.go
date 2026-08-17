package credentialenv

import (
	"os"
	"testing"
)

func TestSanitizeTrimsAndReportsOnlyCorrectedNames(t *testing.T) {
	t.Setenv("OAUTH_CLIENT_SECRET", "secret40\n")
	t.Setenv("BOT_TOKEN", "  ghp_tok  ")
	t.Setenv("AI_TOKEN", "clean-token")

	corrected := Sanitize()

	if got := os.Getenv("OAUTH_CLIENT_SECRET"); got != "secret40" {
		t.Errorf("OAUTH_CLIENT_SECRET = %q, want %q", got, "secret40")
	}
	if got := os.Getenv("BOT_TOKEN"); got != "ghp_tok" {
		t.Errorf("BOT_TOKEN = %q, want %q", got, "ghp_tok")
	}
	if got := os.Getenv("AI_TOKEN"); got != "clean-token" {
		t.Errorf("AI_TOKEN = %q, want it unchanged", got)
	}
	names := map[string]bool{}
	for _, n := range corrected {
		names[n] = true
	}
	if !names["OAUTH_CLIENT_SECRET"] || !names["BOT_TOKEN"] {
		t.Errorf("corrected = %v, want it to name both malformed variables", corrected)
	}
	if names["AI_TOKEN"] {
		t.Errorf("corrected = %v, want it to omit the already-clean variable", corrected)
	}
}

// An unset variable must stay unset: creating it as empty would turn "not
// configured" into "configured but blank" and skip a required-value check.
func TestSanitizeLeavesUnsetVariablesUnset(t *testing.T) {
	os.Unsetenv("ISSUE_TOKEN")
	Sanitize()
	if _, ok := os.LookupEnv("ISSUE_TOKEN"); ok {
		t.Errorf("ISSUE_TOKEN became set")
	}
}

// A whitespace-only value must collapse to empty so the existing "is it
// configured" checks treat it as missing rather than as a usable credential.
func TestSanitizeCollapsesWhitespaceOnlyValue(t *testing.T) {
	t.Setenv("FIX_TOKEN", "   \n")
	Sanitize()
	if got := os.Getenv("FIX_TOKEN"); got != "" {
		t.Errorf("FIX_TOKEN = %q, want empty", got)
	}
}
