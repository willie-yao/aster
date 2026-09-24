package models

import "testing"

func TestTestFailureContentHashTracksOnlyJUnitEvidence(t *testing.T) {
	base := TestCase{
		Name: "TestCluster", Source: "junit", SuiteName: "e2e", ClassName: "cluster", JUnitFile: "junit.xml",
		Status: "failed", FailureMessage: "cluster failed", FailureBody: "expected Ready", FailureLocation: "test.go:42",
		AIAnalysis: &AIAnalysis{GeneratedAt: "2026-08-13T01:00:00Z", RootCause: "cause"},
	}
	hash := TestFailureContentHash(base)
	if hash == "" {
		t.Fatal("failure hash is empty")
	}
	for _, field := range []struct {
		name   string
		mutate func(*TestCase)
	}{
		{"name", func(tc *TestCase) { tc.Name = "Different" }},
		{"source", func(tc *TestCase) { tc.Source = "other" }},
		{"suite", func(tc *TestCase) { tc.SuiteName = "different" }},
		{"class", func(tc *TestCase) { tc.ClassName = "different" }},
		{"JUnit file", func(tc *TestCase) { tc.JUnitFile = "other.xml" }},
		{"status", func(tc *TestCase) { tc.Status = "passed" }},
		{"failure message", func(tc *TestCase) { tc.FailureMessage = "different" }},
		{"failure body", func(tc *TestCase) { tc.FailureBody = "different" }},
		{"failure location", func(tc *TestCase) { tc.FailureLocation = "other.go:42" }},
	} {
		t.Run(field.name, func(t *testing.T) {
			changed := base
			field.mutate(&changed)
			if TestFailureContentHash(changed) == hash {
				t.Fatal("changed JUnit evidence retained failure hash")
			}
		})
	}
	changed := base
	changed.AIAnalysis = &AIAnalysis{GeneratedAt: "2026-08-13T02:00:00Z", RootCause: "new cause"}
	if TestFailureContentHash(changed) != hash {
		t.Fatal("replacement analysis changed JUnit hash")
	}
	changed.AIAnalysis = nil
	changed.DurationSeconds = 42
	changed.FailureLocURL = "https://example.invalid/failure"
	if TestFailureContentHash(changed) != hash {
		t.Fatal("analysis removal, duration, or URL changed JUnit hash")
	}
}

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
