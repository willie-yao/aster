package fixpr

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// fakeRuntime is a runtime.Runtime test double returning a scripted result.
type fakeRuntime struct {
	res runtime.Result
	err error
}

func (f fakeRuntime) Run(context.Context, runtime.Spec) (runtime.Result, error) {
	return f.res, f.err
}

func TestVerify_Verdicts(t *testing.T) {
	base := ghpr.Base{HeadSHA: "deadbeef"}
	files := map[string]string{"a.go": "package a"}

	cases := []struct {
		name string
		rt   runtime.Runtime
		want VerifyStatus
	}{
		{name: "passed", rt: fakeRuntime{res: runtime.Result{ExitCode: 0}}, want: VerifyPassed},
		{name: "failed", rt: fakeRuntime{res: runtime.Result{ExitCode: 1, Output: "boom"}}, want: VerifyFailed},
		{name: "timeout", rt: fakeRuntime{res: runtime.Result{TimedOut: true}}, want: VerifyFailed},
		{name: "unavailable-skips", rt: fakeRuntime{err: runtime.ErrUnavailable}, want: VerifySkipped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{opts: Options{Verify: &VerifyConfig{Runtime: tc.rt}}}
			got := m.verify(context.Background(), base, files)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (summary=%q)", got.Status, tc.want, got.Summary)
			}
		})
	}
}

func TestVerify_NotConfiguredSkips(t *testing.T) {
	m := &Manager{opts: Options{}}
	if got := m.verify(context.Background(), ghpr.Base{HeadSHA: "x"}, nil); got.Status != VerifySkipped {
		t.Errorf("status = %q, want skipped", got.Status)
	}
}

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
