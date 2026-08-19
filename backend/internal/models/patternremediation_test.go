package models

import "testing"

func causalPattern() PatternAnalysis {
	pattern := PatternAnalysis{
		JobID:      "periodic-capz",
		Recurrence: PatternRecurrenceMixedCauses,
		CausalGroups: []PatternCausalGroup{
			{Builds: []string{"2", "1"}, RootCause: "the same call is missing", Confidence: "high"},
			{Builds: []string{"3"}, RootCause: "one-off infrastructure failure", Confidence: "medium"},
		},
		Summary: "mixed causes",
	}
	AssignPatternIdentity(&pattern)
	return pattern
}

func TestWithDefaultPatternRemediationInvestigationsDefaultsRepeatedGroups(t *testing.T) {
	pattern := causalPattern()
	out := WithDefaultPatternRemediationInvestigations([]PatternAnalysis{pattern})
	if len(out[0].RemediationInvestigations) != 1 {
		t.Fatalf("summaries=%+v", out[0].RemediationInvestigations)
	}
	summary := out[0].RemediationInvestigations[0]
	group := out[0].CausalGroups[0]
	if group.ID == "" || group.ContentHash == "" {
		t.Fatalf("group identity=%+v", group)
	}
	if summary.CausalGroupID != group.ID || summary.CausalGroupHash != group.ContentHash {
		t.Fatalf("summary=%+v group=%+v", summary, group)
	}
	if summary.State != PatternRemediationNotInvestigated || summary.Reason != patternRemediationNotInvestigatedReason {
		t.Fatalf("summary=%+v", summary)
	}
	if len(pattern.RemediationInvestigations) != 0 || pattern.CausalGroups[0].Builds[0] != "2" {
		t.Fatalf("input mutated: %+v", pattern)
	}
}

func TestWithDefaultPatternRemediationInvestigationsPreservesMatchingState(t *testing.T) {
	pattern := causalPattern()
	group := pattern.CausalGroups[0]
	pattern.RemediationInvestigations = []PatternRemediationInvestigationSummary{{
		CausalGroupID:   group.ID,
		CausalGroupHash: group.ContentHash,
		State:           PatternRemediationAlreadyFixed,
		Reason:          "current source contains the fix",
	}}
	out := WithDefaultPatternRemediationInvestigations([]PatternAnalysis{pattern})
	if got := out[0].RemediationInvestigations[0].State; got != PatternRemediationAlreadyFixed {
		t.Fatalf("state=%q", got)
	}
}

func TestRemediationStateDoesNotChangeCausalHashes(t *testing.T) {
	pattern := causalPattern()
	groupHash := PatternCausalGroupHash(pattern.CausalGroups[0])
	patternHash := PatternHash(pattern)
	pattern.CausalGroups[0].ID = "different"
	pattern.CausalGroups[0].ContentHash = "different"
	pattern.RemediationInvestigations = []PatternRemediationInvestigationSummary{{
		CausalGroupID: "different", CausalGroupHash: "different", State: PatternRemediationActionable, Reason: "verified",
	}}
	if after := PatternCausalGroupHash(pattern.CausalGroups[0]); after != groupHash {
		t.Fatalf("remediation state changed group hash: before=%s after=%s", groupHash, after)
	}
	if after := PatternHash(pattern); after != patternHash {
		t.Fatalf("remediation state changed pattern hash: before=%s after=%s", patternHash, after)
	}
}

func TestPatternCausalGroupHashCanonicalizesBuildOrder(t *testing.T) {
	left := PatternCausalGroup{Builds: []string{"2", "1"}, RootCause: "cause", Confidence: "high"}
	right := PatternCausalGroup{Builds: []string{"1", "2"}, RootCause: "cause", Confidence: "high"}
	if PatternCausalGroupHash(left) != PatternCausalGroupHash(right) {
		t.Fatal("build order changed causal-group hash")
	}
}

// The durable signature must stay out of both hashes: folding it in would churn
// every causal-group ID and make patterns.retainPriorPattern reject retained
// patterns whose stored ContentHash predates the signature.
func TestSignatureDoesNotChangeCausalHashesOrIdentity(t *testing.T) {
	pattern := causalPattern()
	groupHash := PatternCausalGroupHash(pattern.CausalGroups[0])
	groupID := PatternCausalGroupID(pattern.ID, pattern.CausalGroups[0])
	patternHash := PatternHash(pattern)
	patternID := PatternID(pattern)

	for index := range pattern.CausalGroups {
		pattern.CausalGroups[index].Signature = "0123456789abcdef"
	}
	if after := PatternCausalGroupHash(pattern.CausalGroups[0]); after != groupHash {
		t.Fatalf("signature changed group hash: before=%s after=%s", groupHash, after)
	}
	if after := PatternCausalGroupID(pattern.ID, pattern.CausalGroups[0]); after != groupID {
		t.Fatalf("signature changed group id: before=%s after=%s", groupID, after)
	}
	if after := PatternHash(pattern); after != patternHash {
		t.Fatalf("signature changed pattern hash: before=%s after=%s", patternHash, after)
	}
	if after := PatternID(pattern); after != patternID {
		t.Fatalf("signature changed pattern id: before=%s after=%s", patternID, after)
	}
	if AssignPatternIdentity(&pattern); pattern.ContentHash != patternHash {
		t.Fatalf("reassigned identity diverged: want=%s got=%s", patternHash, pattern.ContentHash)
	}
	if pattern.CausalGroups[0].Signature != "0123456789abcdef" {
		t.Fatal("assigning identity dropped the signature")
	}
}

func TestValidPatternRemediationInvestigationState(t *testing.T) {
	states := []PatternRemediationInvestigationState{
		PatternRemediationNotInvestigated,
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
		PatternRemediationStale,
	}
	for _, state := range states {
		if !ValidPatternRemediationInvestigationState(state) {
			t.Fatalf("state %q is not valid", state)
		}
	}
	if ValidPatternRemediationInvestigationState("unknown") {
		t.Fatal("unknown state accepted")
	}
}
