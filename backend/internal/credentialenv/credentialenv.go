// Package credentialenv normalizes secret-bearing environment variables once at
// process startup, so a Secret written with `echo` instead of `echo -n` fails
// visibly here instead of as an opaque rejection from a remote service.
package credentialenv

import (
	"log"
	"os"

	"github.com/willie-yao/aster/backend/internal/textutil"
)

// Trimmed lists credentials whose format forbids surrounding whitespace: OAuth
// client credentials and bearer tokens are fixed-charset, so a leading or
// trailing byte is always corruption and is safe to remove.
var Trimmed = []string{
	"AI_TOKEN",
	"BOT_TOKEN",
	"FIX_TOKEN",
	"GITHUB_READ_TOKEN",
	"GITHUB_TOKEN",
	"ISSUE_TOKEN",
	"OAUTH_CLIENT_ID",
	"OAUTH_CLIENT_SECRET",
	"SOURCE_INVESTIGATION_GITHUB_TOKEN",
}

// Reported lists secrets that are only warned about. Their values are free-form,
// so trimming could break a working deployment or, worse, weaken it: an empty
// AUTH_PROXY_SECRET disables the shared-secret check, and a changed SESSION_KEY
// invalidates every live session. Surrounding whitespace here is still worth
// naming, because it means the Secret was written with `echo` and the
// fixed-format credentials beside it are probably corrupt too.
var Reported = []string{
	"AUTH_PROXY_SECRET",
	"EMAIL_SMTP_PASSWORD",
	"SESSION_KEY",
}

// Sanitize trims every Trimmed credential that carries surrounding whitespace
// and returns the names it corrected, then the names it only flagged.
//
// Call this from main before any goroutine starts, so the process environment
// is not mutated concurrently with a read.
func Sanitize() (corrected, flagged []string) {
	for _, name := range Trimmed {
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
	for _, name := range Reported {
		raw, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		if _, trimmed := textutil.TrimCredential(raw); trimmed {
			flagged = append(flagged, name)
		}
	}
	return corrected, flagged
}

// SanitizeAndReport applies Sanitize and names every affected variable, so an
// operator can repair the Secret instead of chasing a downstream rejection.
func SanitizeAndReport() {
	corrected, flagged := Sanitize()
	for _, name := range corrected {
		log.Printf("⚠️  %s had leading or trailing whitespace; using the trimmed value. Rewrite the Secret with `printf %%s` instead of `echo` to drop the stray newline.", name)
	}
	for _, name := range flagged {
		log.Printf("⚠️  %s has leading or trailing whitespace and is used as written, because trimming it could change or weaken its meaning. Rewrite the Secret with `printf %%s` if the whitespace is not intentional.", name)
	}
}
