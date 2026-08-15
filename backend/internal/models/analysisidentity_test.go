package models

import "testing"

func TestTestAnalysisContentHashTracksAnalysisAndSourceEvidence(t *testing.T) {
	testCase := TestCase{Name: "TestCluster", Status: "failed", JUnitFile: "junit.xml", AIAnalysis: &AIAnalysis{
		GeneratedAt: "2026-08-13T01:00:00Z", RootCause: "cause", Severity: "High", SuggestedFix: "fix",
		RelevantFiles: []string{"pkg/controller.go"}, FileLinks: map[string]string{"pkg/controller.go": "https://github.com/o/r/blob/sha/pkg/controller.go"},
		EvidenceCitations: []EvidenceCitation{{Path: "artifacts/junit.xml", LineStart: 1, LineEnd: 1, Quote: "failed"}},
	}}
	base := TestAnalysisContentHash(testCase)
	if base == "" {
		t.Fatal("analysis hash is empty")
	}
	changed := testCase
	analysis := *testCase.AIAnalysis
	changed.AIAnalysis = &analysis
	changed.AIAnalysis.FileLinks = map[string]string{"pkg/controller.go": "https://github.com/o/r/blob/other/pkg/controller.go"}
	if TestAnalysisContentHash(changed) == base {
		t.Fatal("changed source evidence retained analysis hash")
	}
	changed = testCase
	analysis = *testCase.AIAnalysis
	changed.AIAnalysis = &analysis
	changed.AIAnalysis.EvidenceCitations = []EvidenceCitation{{Path: "artifacts/junit.xml", LineStart: 2, LineEnd: 2, Quote: "different"}}
	if TestAnalysisContentHash(changed) == base {
		t.Fatal("changed artifact evidence retained analysis hash")
	}
}
