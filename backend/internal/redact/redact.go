// Package redact scrubs sensitive values from strings before they are logged or
// published, so error text cannot disclose a hidden endpoint or a secret-bearing
// URL whose path or query contains a secret.
package redact

import "regexp"

// urlPattern matches an http or https URL up to the first whitespace, quote, or
// angle bracket. Go's *url.Error embeds the full request URL in its message and
// only redacts a userinfo password, so a host, path, or query (which may itself
// be the secret) survives; this strips the whole URL.
var urlPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)
var bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
var credentialPattern = regexp.MustCompile(`(?i)\b(api[-_ ]?key|token|secret|password)\b["']?\s*[:=]\s*["']?[^\s"',;}]+`)

// URLs replaces every http/https URL in s with a fixed placeholder. Use it on
// any error text that may reach a log line or a published field.
func URLs(s string) string {
	return urlPattern.ReplaceAllString(s, "[redacted-url]")
}

// Credentials replaces common inline credential forms in diagnostic text.
func Credentials(s string) string {
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	return credentialPattern.ReplaceAllString(s, "$1=[redacted]")
}
