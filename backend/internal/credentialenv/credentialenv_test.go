package credentialenv

import (
	"os"
	"testing"
)

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// isolateEnv snapshots every classified variable and restores it exactly,
// including its unset state. Sanitize mutates the process environment, so a
// test that touches an inherited variable would otherwise leak into the next.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range append(append([]string{}, Trimmed...), Reported...) {
		name := name
		previous, existed := os.LookupEnv(name)
		t.Cleanup(func() {
			if existed {
				os.Setenv(name, previous)
				return
			}
			os.Unsetenv(name)
		})
	}
}

func TestSanitizeTrimsFixedFormatCredentials(t *testing.T) {
	isolateEnv(t)
	t.Setenv("OAUTH_CLIENT_SECRET", "secret40\n")
	t.Setenv("BOT_TOKEN", "  ghp_tok  ")
	t.Setenv("AI_TOKEN", "clean-token")

	corrected, _ := Sanitize()

	if got := os.Getenv("OAUTH_CLIENT_SECRET"); got != "secret40" {
		t.Errorf("OAUTH_CLIENT_SECRET = %q, want %q", got, "secret40")
	}
	if got := os.Getenv("BOT_TOKEN"); got != "ghp_tok" {
		t.Errorf("BOT_TOKEN = %q, want %q", got, "ghp_tok")
	}
	if got := os.Getenv("AI_TOKEN"); got != "clean-token" {
		t.Errorf("AI_TOKEN = %q, want it unchanged", got)
	}
	if !contains(corrected, "OAUTH_CLIENT_SECRET") || !contains(corrected, "BOT_TOKEN") {
		t.Errorf("corrected = %v, want both malformed variables named", corrected)
	}
	if contains(corrected, "AI_TOKEN") {
		t.Errorf("corrected = %v, want the already-clean variable omitted", corrected)
	}
}

// Trimming a free-form secret can change or weaken its meaning, so these are
// reported and left exactly as configured.
func TestSanitizeReportsFreeFormSecretsWithoutChangingThem(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SESSION_KEY", "seed\n")
	t.Setenv("EMAIL_SMTP_PASSWORD", " pw ")
	t.Setenv("AUTH_PROXY_SECRET", "   ")

	_, flagged := Sanitize()

	if got := os.Getenv("SESSION_KEY"); got != "seed\n" {
		t.Errorf("SESSION_KEY = %q, want it unmodified", got)
	}
	if got := os.Getenv("EMAIL_SMTP_PASSWORD"); got != " pw " {
		t.Errorf("EMAIL_SMTP_PASSWORD = %q, want it unmodified", got)
	}
	for _, name := range []string{"SESSION_KEY", "EMAIL_SMTP_PASSWORD", "AUTH_PROXY_SECRET"} {
		if !contains(flagged, name) {
			t.Errorf("flagged = %v, want it to name %s", flagged, name)
		}
	}
}

// Emptying AUTH_PROXY_SECRET would disable the shared-secret check entirely, so
// a whitespace-only value must survive rather than silently weaken auth.
func TestSanitizeNeverEmptiesProxySecret(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AUTH_PROXY_SECRET", "  \n")
	Sanitize()
	if got := os.Getenv("AUTH_PROXY_SECRET"); got == "" {
		t.Fatal("AUTH_PROXY_SECRET was emptied, which disables the shared-secret check")
	}
}

// An unset variable must stay unset: creating it as empty would turn "not
// configured" into "configured but blank".
func TestSanitizeLeavesUnsetVariablesUnset(t *testing.T) {
	isolateEnv(t)
	os.Unsetenv("ISSUE_TOKEN")
	Sanitize()
	if _, ok := os.LookupEnv("ISSUE_TOKEN"); ok {
		t.Errorf("ISSUE_TOKEN became set")
	}
}

// A whitespace-only token collapses to empty so existing "is it configured"
// checks treat it as missing rather than as a usable credential.
func TestSanitizeCollapsesWhitespaceOnlyToken(t *testing.T) {
	isolateEnv(t)
	t.Setenv("FIX_TOKEN", "   \n")
	Sanitize()
	if got := os.Getenv("FIX_TOKEN"); got != "" {
		t.Errorf("FIX_TOKEN = %q, want empty", got)
	}
}

// Every name must be classified exactly once, so a new credential cannot be
// silently both trimmed and reported.
func TestCredentialListsAreDisjoint(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range append(append([]string{}, Trimmed...), Reported...) {
		if seen[n] {
			t.Errorf("%s appears in both Trimmed and Reported", n)
		}
		seen[n] = true
	}
}
