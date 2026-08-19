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

// TestPatternCausalGroupHashTracksCauseOwnership keeps a remediation binding
// tied to the ownership shown with the cause.
func TestPatternCausalGroupHashTracksCauseOwnership(t *testing.T) {
	group := PatternCausalGroup{Builds: []string{"2", "1"}, RootCause: "cause", Confidence: "high"}
	unattributed := PatternCausalGroupHash(group)

	external := group
	external.CauseLocation = &AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true}
	project := group
	project.CauseLocation = &AnalysisCauseLocation{Repository: "kubernetes-sigs/cluster-api-provider-azure"}

	if PatternCausalGroupHash(external) == unattributed || PatternCausalGroupHash(project) == unattributed ||
		PatternCausalGroupHash(external) == PatternCausalGroupHash(project) {
		t.Fatal("causal group ownership did not change the content hash")
	}
}

// TestClonePatternAnalysesDeepCopiesCauseOwnership stops a public projection
// from sharing file hints with the pattern it was derived from.
func TestClonePatternAnalysesDeepCopiesCauseOwnership(t *testing.T) {
	original := []PatternAnalysis{{CausalGroups: []PatternCausalGroup{{
		Builds:        []string{"1"},
		CauseLocation: &AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true, Files: []string{"pkg/one.go"}},
	}}}}

	cloned := clonePatternAnalyses(original)
	cloned[0].CausalGroups[0].CauseLocation.Files[0] = "mutated"
	cloned[0].CausalGroups[0].CauseLocation.Repository = "other/repo"

	source := original[0].CausalGroups[0].CauseLocation
	if source.Files[0] != "pkg/one.go" || source.Repository != "kubernetes/kubernetes" {
		t.Fatalf("clone aliased the original cause location: %+v", source)
	}
}
