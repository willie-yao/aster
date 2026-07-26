package models

import "testing"

func TestPatternHashBindsReviewedContent(t *testing.T) {
	base := PatternAnalysis{
		ID: "stable-id", ContentHash: "old-hash", Subject: "retry failure", JobID: "periodic-x",
		GeneratedAt: "2026-07-25T00:00:00Z", BuildsAnalyzed: 3, Systemic: true, Confidence: "high",
		SharedRootCause: "terminal failures retry", SharedBuilds: []string{"1", "2"},
		SuggestedFix: "bound retries", RelevantFiles: []string{"pkg/retry.go"}, Summary: "shared failure",
	}
	want := PatternHash(base)
	identityOnly := base
	identityOnly.ID = "other-id"
	identityOnly.ContentHash = "other-hash"
	if got := PatternHash(identityOnly); got != want {
		t.Fatalf("identity fields changed hash: %q != %q", got, want)
	}
	timestampOnly := base
	timestampOnly.GeneratedAt = "2026-07-25T01:00:00Z"
	if got := PatternHash(timestampOnly); got != want {
		t.Fatalf("generated timestamp changed hash: %q != %q", got, want)
	}
	emptySlices := PatternAnalysis{SharedBuilds: []string{}, RelevantFiles: []string{}}
	if PatternHash(emptySlices) != PatternHash(PatternAnalysis{}) {
		t.Fatal("empty and omitted slices produced different hashes")
	}

	mutations := map[string]func(*PatternAnalysis){
		"suggested fix":  func(pattern *PatternAnalysis) { pattern.SuggestedFix = "stop retries" },
		"confidence":     func(pattern *PatternAnalysis) { pattern.Confidence = "medium" },
		"summary":        func(pattern *PatternAnalysis) { pattern.Summary = "replacement summary" },
		"relevant files": func(pattern *PatternAnalysis) { pattern.RelevantFiles = []string{"pkg/other.go"} },
		"shared builds":  func(pattern *PatternAnalysis) { pattern.SharedBuilds = []string{"2", "3"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if got := PatternHash(changed); got == want {
				t.Fatalf("changed pattern retained hash %q", got)
			}
		})
	}
}

func TestBackfillPatternIdentityPreservesStableID(t *testing.T) {
	pattern := PatternAnalysis{
		ID: "legacy-stable-id", ContentHash: "stale", Subject: "retry failure", JobID: "periodic-x",
		BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "terminal failures retry",
		SharedBuilds: []string{"1", "2"}, SuggestedFix: "bound retries", Summary: "shared failure",
	}
	if !BackfillPatternIdentity(&pattern) {
		t.Fatal("stale identity was not updated")
	}
	if pattern.ID != "legacy-stable-id" {
		t.Fatalf("stable ID changed to %q", pattern.ID)
	}
	if pattern.ContentHash != PatternHash(pattern) {
		t.Fatalf("content hash = %q", pattern.ContentHash)
	}

	missing := pattern
	missing.ID = ""
	missing.ContentHash = ""
	if !BackfillPatternIdentity(&missing) || missing.ID != PatternID(missing) || missing.ContentHash != PatternHash(missing) {
		t.Fatalf("missing identity = %+v", missing)
	}
}

func TestBackfillPatternIdentitiesReturnsCopy(t *testing.T) {
	input := []PatternAnalysis{{JobID: "job", Summary: "summary", ContentHash: "stale"}}
	output, changed := BackfillPatternIdentities(input)
	if !changed || output[0].ID == "" || output[0].ContentHash != PatternHash(output[0]) {
		t.Fatalf("output = %+v changed=%v", output, changed)
	}
	if input[0].ID != "" || input[0].ContentHash != "stale" {
		t.Fatalf("input was mutated: %+v", input)
	}
}
