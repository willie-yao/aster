// Package orka holds naming helpers shared by the Orka pipeline commands
// (orka-producer and orka-ingestor) so the content-addressed Task name derived
// from a failure is identical on both sides.
package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// FailureHash is a deterministic short hash of a failing test's identity, used
// as the content-address for its analysis Task.
func FailureHash(testName, failureMessage string) string {
	sum := sha256.Sum256([]byte(testName + "\x00" + strings.Join(strings.Fields(failureMessage), " ")))
	return hex.EncodeToString(sum[:6])
}

// TaskName is the RFC1123 content-addressed Task name for a failure. Prow build
// IDs are globally unique, so build+hash+version is unique per failure+version.
func TaskName(buildID, hash, version string) string {
	return Sanitize("az-" + buildID + "-" + hash + "-" + version)
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
