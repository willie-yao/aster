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

func TestCausalIdentityFieldsDoNotChangeHashes(t *testing.T) {
	pattern := causalPattern()
	groupHash := PatternCausalGroupHash(pattern.CausalGroups[0])
	patternHash := PatternHash(pattern)
	pattern.CausalGroups[0].ID = "different"
	pattern.CausalGroups[0].ContentHash = "different"
	if after := PatternCausalGroupHash(pattern.CausalGroups[0]); after != groupHash {
		t.Fatalf("identity fields changed group hash: before=%s after=%s", groupHash, after)
	}
	if after := PatternHash(pattern); after != patternHash {
		t.Fatalf("identity fields changed pattern hash: before=%s after=%s", patternHash, after)
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

// TestClonePatternAnalysesDeepCopiesReportedRemediation stops a public
// projection from sharing the reported fix with the pattern it came from, so
// sanitizing one copy cannot reach through to another.
func TestClonePatternAnalysesDeepCopiesReportedRemediation(t *testing.T) {
	original := []PatternAnalysis{{CausalGroups: []PatternCausalGroup{{
		Builds:      []string{"1"},
		Remediation: &PatternCausalGroupRemediation{SuggestedFix: "Raise the join budget.", BuildID: "1"},
	}}}}

	cloned := clonePatternAnalyses(original)
	cloned[0].CausalGroups[0].Remediation.SuggestedFix = "mutated"
	cloned[0].CausalGroups[0].Remediation.BuildID = "9"

	source := original[0].CausalGroups[0].Remediation
	if source.SuggestedFix != "Raise the join budget." || source.BuildID != "1" {
		t.Fatalf("clone aliased the original reported remediation: %+v", source)
	}
}
