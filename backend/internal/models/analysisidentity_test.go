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

// TestTestAnalysisContentHashTracksCauseOwnership keeps actions bound to the
// ownership the maintainer was shown: an upstream verdict and a project verdict
// are different content even when every other field matches.
func TestTestAnalysisContentHashTracksCauseOwnership(t *testing.T) {
	base := TestCase{Name: "TestCluster", Status: "failed", JUnitFile: "junit.xml", AIAnalysis: &AIAnalysis{
		GeneratedAt: "2026-08-13T01:00:00Z", RootCause: "cause", Severity: "High", SuggestedFix: "fix",
	}}
	unattributed := TestAnalysisContentHash(base)

	withOwner := func(location *AnalysisCauseLocation) string {
		changed := base
		analysis := *base.AIAnalysis
		analysis.CauseLocation = location
		changed.AIAnalysis = &analysis
		return TestAnalysisContentHash(changed)
	}

	external := withOwner(&AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true})
	project := withOwner(&AnalysisCauseLocation{Repository: "kubernetes-sigs/cluster-api-provider-azure"})
	withFile := withOwner(&AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes", External: true,
		Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"},
	})

	for name, hash := range map[string]string{"external": external, "project": project, "with file": withFile} {
		if hash == unattributed {
			t.Errorf("%s ownership retained the unattributed hash", name)
		}
	}
	if external == project || external == withFile {
		t.Fatalf("distinct ownership collided: external=%s project=%s withFile=%s", external, project, withFile)
	}
}
