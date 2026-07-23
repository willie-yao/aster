// Package orka contains the dashboard adapters for Orka lifecycle execution.
package orka

import "strings"

// ManagedByLabel identifies the component that owns an Orka Task.
const ManagedByLabel = "app.kubernetes.io/managed-by"

var nameUnsafe = strings.NewReplacer("_", "-", ".", "-", "/", "-", " ", "-", ":", "-")

// Sanitize lowercases and reduces s to a valid, bounded RFC1123 name fragment.
func Sanitize(s string) string {
	s = strings.ToLower(nameUnsafe.Replace(s))
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 200 {
		s = strings.Trim(s[:200], "-")
	}
	return s
}
