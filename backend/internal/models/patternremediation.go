package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

const patternRemediationNotInvestigatedReason = "No source-grounded implementation target has been verified for this recurring cause."

// ValidPatternRemediationInvestigationState reports whether state is public-safe.
func ValidPatternRemediationInvestigationState(state PatternRemediationInvestigationState) bool {
	switch state {
	case PatternRemediationNotInvestigated,
		PatternRemediationQueued,
		PatternRemediationInvestigating,
		PatternRemediationVerifying,
		PatternRemediationActionable,
		PatternRemediationAlreadyFixed,
		PatternRemediationExternalDependency,
		PatternRemediationEnvironmentOrInfrastructure,
		PatternRemediationMitigationOnly,
		PatternRemediationInsufficientEvidence,
		PatternRemediationInvestigationFailed,
		PatternRemediationStale:
		return true
	default:
		return false
	}
}

// PatternCausalGroupHash identifies the causal-group content shown to the maintainer.
func PatternCausalGroupHash(group PatternCausalGroup) string {
	builds := append([]string(nil), group.Builds...)
	slices.Sort(builds)
	snapshot := struct {
		Builds        []string               `json:"builds"`
		RootCause     string                 `json:"root_cause"`
		Confidence    string                 `json:"confidence"`
		CauseLocation *AnalysisCauseLocation `json:"cause_location,omitempty"`
	}{
		Builds:        builds,
		RootCause:     strings.TrimSpace(group.RootCause),
		Confidence:    strings.TrimSpace(group.Confidence),
		CauseLocation: group.CauseLocation,
	}
	encoded, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// PatternCausalGroupID returns a stable short identity within one pattern.
func PatternCausalGroupID(patternID string, group PatternCausalGroup) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(patternID) + "\x00" + PatternCausalGroupHash(group)))
	return hex.EncodeToString(sum[:8])
}

func assignPatternCausalGroupIdentities(pattern *PatternAnalysis) bool {
	if pattern == nil {
		return false
	}
	changed := false
	for index := range pattern.CausalGroups {
		group := &pattern.CausalGroups[index]
		hash := PatternCausalGroupHash(*group)
		id := PatternCausalGroupID(pattern.ID, *group)
		if group.ContentHash != hash {
			group.ContentHash = hash
			changed = true
		}
		if group.ID != id {
			group.ID = id
			changed = true
		}
	}
	return changed
}

// WithDefaultPatternRemediationInvestigations returns a public projection with
// explicit defaults for repeated causal groups. The input remains unchanged.
func WithDefaultPatternRemediationInvestigations(patterns []PatternAnalysis) []PatternAnalysis {
	if patterns == nil {
		return nil
	}
	out := clonePatternAnalyses(patterns)
	for patternIndex := range out {
		pattern := &out[patternIndex]
		if PatternAllowsActions(*pattern) {
			continue
		}
		existing := make(map[string]PatternRemediationInvestigationSummary, len(pattern.RemediationInvestigations))
		for _, summary := range pattern.RemediationInvestigations {
			if summary.CausalGroupHash != "" && ValidPatternRemediationInvestigationState(summary.State) {
				existing[summary.CausalGroupHash] = summary
			}
		}
		summaries := make([]PatternRemediationInvestigationSummary, 0, len(pattern.CausalGroups))
		for _, group := range pattern.CausalGroups {
			if len(group.Builds) < 2 {
				continue
			}
			summary, ok := existing[group.ContentHash]
			if !ok {
				summary = PatternRemediationInvestigationSummary{
					State:  PatternRemediationNotInvestigated,
					Reason: patternRemediationNotInvestigatedReason,
				}
			}
			summary.CausalGroupID = group.ID
			summary.CausalGroupHash = group.ContentHash
			summaries = append(summaries, summary)
		}
		pattern.RemediationInvestigations = summaries
	}
	return out
}

// ClonePatternAnalyses deep copies the slices a caller may mutate, so writing to
// one copy's causal groups never reaches another's backing array.
func ClonePatternAnalyses(patterns []PatternAnalysis) []PatternAnalysis {
	return clonePatternAnalyses(patterns)
}

func clonePatternAnalyses(patterns []PatternAnalysis) []PatternAnalysis {
	out := append([]PatternAnalysis(nil), patterns...)
	for index := range out {
		out[index].CausalGroups = append([]PatternCausalGroup(nil), patterns[index].CausalGroups...)
		for groupIndex := range out[index].CausalGroups {
			out[index].CausalGroups[groupIndex].Builds = append([]string(nil), patterns[index].CausalGroups[groupIndex].Builds...)
			out[index].CausalGroups[groupIndex].CauseLocation = patterns[index].CausalGroups[groupIndex].CauseLocation.Clone()
		}
		out[index].RemediationInvestigations = append([]PatternRemediationInvestigationSummary(nil), patterns[index].RemediationInvestigations...)
	}
	return out
}
