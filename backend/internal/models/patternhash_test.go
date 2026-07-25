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
