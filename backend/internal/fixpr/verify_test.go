package fixpr

import (
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func TestPRBody_RendersVerdict(t *testing.T) {
	p := models.PatternAnalysis{Subject: "flaky job", BuildsAnalyzed: 4, Confidence: "high"}
	fix := &proposedFix{diff: "- a\n+ b", rationale: "do x"}

	passed := prBody(p, fix, VerifyResult{Status: VerifyPassed, Summary: "go build ./... passed"}, "k", "", "desc")
	if !strings.Contains(passed, "verification passed") {
		t.Errorf("passed body missing verdict:\n%s", passed)
	}

	failed := prBody(p, fix, VerifyResult{Status: VerifyFailed, Summary: "go build ./... failed", Output: "undefined: Foo"}, "k", "", "desc")
	if !strings.Contains(failed, "verification failed") {
		t.Errorf("failed body missing verdict:\n%s", failed)
	}
	if !strings.Contains(failed, "undefined: Foo") {
		t.Errorf("failed body missing verification output:\n%s", failed)
	}

	skipped := prBody(p, fix, VerifyResult{Status: VerifySkipped}, "k", "", "desc")
	if strings.Contains(skipped, "verification passed") || strings.Contains(skipped, "verification failed") {
		t.Errorf("skipped body should carry no verdict banner:\n%s", skipped)
	}
}
