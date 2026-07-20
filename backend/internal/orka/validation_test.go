package orka

import (
	"strings"
	"testing"
)

func TestAnalysisValidationTokenTracksKeyAndFinalFields(t *testing.T) {
	base := AnalysisValidation{Summary: "summary", RootCause: "cause", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"b", "a"}}
	first := AnalysisValidationToken("key-a", "task-a", base, 10)
	reordered := base
	reordered.RelevantFiles = []string{"a", "b"}
	if AnalysisValidationToken("key-a", "task-a", reordered, 10) != first {
		t.Fatal("reordering relevant files changed token")
	}
	changed := base
	changed.RootCause = "different"
	if AnalysisValidationToken("key-a", "task-a", changed, 10) == first {
		t.Fatal("changed final field did not change token")
	}
	if AnalysisValidationToken("key-b", "task-a", base, 10) == first {
		t.Fatal("different validation key produced the same token")
	}
	if AnalysisValidationToken("key-a", "task-b", base, 10) == first {
		t.Fatal("different Task identity produced the same token")
	}
}

func TestAnalysisValidationEvidenceTextMatchesInProcessFields(t *testing.T) {
	analysis := AnalysisValidation{
		Summary: "summary", RootCause: "cause", Severity: "High",
		SuggestedFix: "fix", RelevantFiles: []string{"build-log.txt"},
	}
	text := analysis.EvidenceText()
	if strings.Contains(text, "High") {
		t.Fatalf("EvidenceText included severity: %q", text)
	}
	for _, want := range []string{"cause", "summary", "fix", "build-log.txt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("EvidenceText = %q, missing %q", text, want)
		}
	}
}
