// Package credentialenv normalizes secret-bearing environment variables once at
// process startup, so every later os.Getenv sees a usable value.
package credentialenv

import (
	"log"
	"os"

	"github.com/willie-yao/aster/backend/internal/textutil"
)

// Names lists every environment variable that carries a secret. A variable
// belongs here when its value is sent to a remote service as a credential.
var Names = []string{
	"AI_TOKEN",
	"AUTH_SHARED_SECRET",
	"BOT_TOKEN",
	"EMAIL_SMTP_PASSWORD",
	"FIX_TOKEN",
	"GITHUB_READ_TOKEN",
	"GITHUB_TOKEN",
	"ISSUE_TOKEN",
	"OAUTH_CLIENT_ID",
	"OAUTH_CLIENT_SECRET",
	"SESSION_KEY",
}

// Sanitize trims surrounding whitespace from every credential variable that is
// set, and returns the names it had to correct.
//
// A Secret written with `echo` rather than `echo -n` keeps a trailing newline.
// Surrounding whitespace is never meaningful in a token, key, or password, but
// the remote side sees a different credential and rejects it: GitHub reports
// incorrect_client_credentials, and a model endpoint reports 401. Neither names
// the variable, so the misconfiguration surfaces far from its cause.
//
// Call this from main before any goroutine starts, so the process environment
// is not mutated concurrently with a read.
func Sanitize() []string {
	var corrected []string
	for _, name := range Names {
		raw, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		value, trimmed := textutil.TrimCredential(raw)
		if !trimmed {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			continue
		}
		corrected = append(corrected, name)
	}
	return corrected
}

// SanitizeAndReport applies Sanitize and names each corrected variable, so an
// operator can fix the Secret instead of chasing a downstream rejection.
func SanitizeAndReport() {
	for _, name := range Sanitize() {
		log.Printf("⚠️  %s had leading or trailing whitespace; using the trimmed value. Rewrite the Secret with `printf %%s` instead of `echo` to drop the stray newline.", name)
	}
}
