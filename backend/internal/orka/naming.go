// Package orka holds naming helpers shared by the Orka pipeline commands
// (orka-producer and orka-ingestor) so the content-addressed Task name derived
// from a failure is identical on both sides.
package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PatternTaskName is the content-addressed Task name for one job-level correlation.
func PatternTaskName(jobID, prompt, version string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + prompt))
	return Sanitize("az-pattern-" + hex.EncodeToString(sum[:8]) + "-" + version)
}

// Labels the producer stamps on every Task and per-build Tool it creates, so the
// ingestor can group them by build for status checks and garbage collection.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "orka-producer"
	BuildLabel     = "orka.dashboard/build"
)

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
