package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type patternCausalGroupContent struct {
	Builds        []string               `json:"builds"`
	RootCause     string                 `json:"root_cause"`
	Confidence    string                 `json:"confidence"`
	CauseLocation *AnalysisCauseLocation `json:"cause_location,omitempty"`
}

func patternCausalGroupContents(groups []PatternCausalGroup) []patternCausalGroupContent {
	if groups == nil {
		return nil
	}
	out := make([]patternCausalGroupContent, len(groups))
	for index, group := range groups {
		out[index] = patternCausalGroupContent{
			Builds:        group.Builds,
			RootCause:     group.RootCause,
			Confidence:    group.Confidence,
			CauseLocation: group.CauseLocation,
		}
	}
	return out
}

// PatternHash identifies the complete published pattern context.
func PatternHash(p PatternAnalysis) string {
	snapshot := struct {
		Subject                 string                          `json:"subject"`
		JobID                   string                          `json:"job_id"`
		BuildsAnalyzed          int                             `json:"builds_analyzed"`
		Recurrence              PatternRecurrence               `json:"recurrence_classification,omitempty"`
		CausalGroups            []patternCausalGroupContent     `json:"causal_groups,omitempty"`
		UnclassifiedBuilds      []string                        `json:"unclassified_builds,omitempty"`
		Systemic                bool                            `json:"systemic"`
		Confidence              string                          `json:"confidence"`
		SharedRootCause         string                          `json:"shared_root_cause"`
		SharedBuilds            []string                        `json:"shared_builds,omitempty"`
		SuggestedFix            string                          `json:"suggested_fix"`
		RemediationTargets      []RemediationTarget             `json:"remediation_targets,omitempty"`
		RelevantFiles           []string                        `json:"relevant_files,omitempty"`
		FileLinks               map[string]string               `json:"file_links,omitempty"`
		SourceRef               string                          `json:"source_ref,omitempty"`
		RemediationVerification *PatternRemediationVerification `json:"remediation_verification,omitempty"`
		Lifecycle               *PatternLifecycle               `json:"lifecycle,omitempty"`
		Summary                 string                          `json:"summary"`
	}{
		Subject:                 p.Subject,
		JobID:                   p.JobID,
		BuildsAnalyzed:          p.BuildsAnalyzed,
		Recurrence:              p.Recurrence,
		CausalGroups:            patternCausalGroupContents(p.CausalGroups),
		UnclassifiedBuilds:      p.UnclassifiedBuilds,
		Systemic:                p.Systemic,
		Confidence:              p.Confidence,
		SharedRootCause:         p.SharedRootCause,
		SharedBuilds:            p.SharedBuilds,
		SuggestedFix:            p.SuggestedFix,
		RemediationTargets:      p.RemediationTargets,
		RelevantFiles:           p.RelevantFiles,
		FileLinks:               p.FileLinks,
		SourceRef:               p.SourceRef,
		RemediationVerification: p.RemediationVerification,
		Lifecycle:               p.Lifecycle,
		Summary:                 p.Summary,
	}
	encoded, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// AssignPatternIdentity sets the canonical ID and content hash for a newly
// generated pattern.
func AssignPatternIdentity(pattern *PatternAnalysis) {
	if pattern == nil {
		return
	}
	pattern.ID = PatternID(*pattern)
	assignPatternCausalGroupIdentities(pattern)
	pattern.ContentHash = PatternHash(*pattern)
}

// BackfillPatternIdentity preserves an existing stable ID and refreshes the
// canonical content hash for published compatibility.
func BackfillPatternIdentity(pattern *PatternAnalysis) bool {
	if pattern == nil {
		return false
	}
	beforeID, beforeHash := pattern.ID, pattern.ContentHash
	changed := false
	if pattern.ID == "" {
		pattern.ID = PatternID(*pattern)
	}
	changed = assignPatternCausalGroupIdentities(pattern) || changed
	pattern.ContentHash = PatternHash(*pattern)
	return changed || pattern.ID != beforeID || pattern.ContentHash != beforeHash
}

// BackfillPatternIdentities returns a normalized copy of the pattern list.
func BackfillPatternIdentities(patterns []PatternAnalysis) ([]PatternAnalysis, bool) {
	if patterns == nil {
		return nil, false
	}
	out := clonePatternAnalyses(patterns)
	changed := false
	for index := range out {
		changed = BackfillPatternIdentity(&out[index]) || changed
	}
	return out, changed
}
