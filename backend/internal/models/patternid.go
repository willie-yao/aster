package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PatternID is a stable, URL-safe identifier for a pattern analysis, derived
// from the job and its shared root cause. It lets the frontend and the API
// address one specific failure. It is independent of the fixpr dedup key so
// changing one never disturbs the other.
func PatternID(p PatternAnalysis) string {
	job := strings.TrimSpace(p.JobID)
	if job == "" {
		job = strings.TrimSpace(p.Subject)
	}
	cause := strings.ToLower(strings.TrimSpace(p.SharedRootCause))
	if cause == "" {
		cause = strings.ToLower(strings.TrimSpace(p.Summary))
	}
	sum := sha256.Sum256([]byte(job + "\x00" + cause))
	return hex.EncodeToString(sum[:8])
}
