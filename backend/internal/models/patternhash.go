package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PatternHash identifies the complete published pattern context.
func PatternHash(p PatternAnalysis) string {
	snapshot := struct {
		Subject         string   `json:"subject"`
		JobID           string   `json:"job_id"`
		BuildsAnalyzed  int      `json:"builds_analyzed"`
		Systemic        bool     `json:"systemic"`
		Confidence      string   `json:"confidence"`
		SharedRootCause string   `json:"shared_root_cause"`
		SharedBuilds    []string `json:"shared_builds,omitempty"`
		SuggestedFix    string   `json:"suggested_fix"`
		RelevantFiles   []string `json:"relevant_files,omitempty"`
		Summary         string   `json:"summary"`
	}{
		Subject:         p.Subject,
		JobID:           p.JobID,
		BuildsAnalyzed:  p.BuildsAnalyzed,
		Systemic:        p.Systemic,
		Confidence:      p.Confidence,
		SharedRootCause: p.SharedRootCause,
		SharedBuilds:    p.SharedBuilds,
		SuggestedFix:    p.SuggestedFix,
		RelevantFiles:   p.RelevantFiles,
		Summary:         p.Summary,
	}
	encoded, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
