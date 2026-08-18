package sharedfailure

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/modules/pullrequest"
	"github.com/willie-yao/aster/backend/internal/ai/modules/universal"
	"github.com/willie-yao/aster/backend/internal/models"
)

func testSubject() Subject {
	return Subject{
		BaseRef: "main", JobName: "pull-project-e2e", TestName: "[It] creates a cluster",
		PullNumbers: []int{6209, 6210, 6215}, EvidencePull: 6215,
	}
}

func promptFor(t *testing.T, subject Subject) string {
	t.Helper()
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "100", WebURL: "https://example.test/100"}}
	tc := &models.TestCase{Name: "[It] creates a cluster", Status: "failed", FailureMessage: "boom"}
	return New(subject).AnalysisPrompt(context.Background(), nil, run, tc, 1)
}

func TestNameIsolatesTheCacheFromEveryOtherAnalysis(t *testing.T) {
	if got := New(testSubject()).Name(); got != ModuleName {
		t.Fatalf("Name = %q, want %q", got, ModuleName)
	}
	// The agentic cache key is built from the module name. Colliding with
	// either other module would let one subject's analysis be served as
	// another's, which is exactly what the separate module exists to prevent.
	if ModuleName == universal.New().Name() {
		t.Error("the shared failure module must not share the universal module's name")
	}
	if ModuleName == pullrequest.ModuleName {
		t.Error("the shared failure module must not share the pull request module's name")
	}
}

// The universal seed prompt carries the investigation instructions every other
// analysis is gated on, so it must survive intact.
func TestPromptKeepsTheUniversalInvestigationSeed(t *testing.T) {
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "100"}}
	tc := &models.TestCase{Name: "[It] creates a cluster", Status: "failed"}
	base := universal.New().AnalysisPrompt(context.Background(), nil, run, tc, 1)

	got := New(testSubject()).AnalysisPrompt(context.Background(), nil, run, tc, 1)
	if !strings.HasPrefix(got, base) {
		t.Fatal("the shared failure prompt must extend the universal seed, not replace it")
	}
}

func TestPromptNamesTheCorrelationAndItsPullRequests(t *testing.T) {
	got := promptFor(t, testSubject())

	for _, want := range []string{
		"This test is failing the same way on 3 open pull requests targeting main",
		"Affected pull requests: #6209, #6210, #6215",
		"pull request #6215, which was chosen only because it is the most recent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// These anchors are the reason the module exists: the model must look for a
// common cause and must not pin a shared failure on any one pull request.
func TestPromptDirectsTheInvestigationAtTheSharedCause(t *testing.T) {
	got := promptFor(t, testSubject())

	for _, want := range []string{
		"usually has a cause they share",
		"Diagnose that shared cause.",
		"Do not attribute the failure to any pull request, including the one whose",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing the guard %q", want)
		}
	}
}

// Correlating on base branch, job, and test cannot establish that the affected
// pull requests are independent: one may be stacked on another, or two may
// carry the same change. Asserting independence would bias the model away from
// a change that really is responsible.
func TestPromptDoesNotClaimTheChangesAreUnrelated(t *testing.T) {
	got := promptFor(t, testSubject())

	if strings.Contains(got, "unrelated changes") {
		t.Error("the prompt must not assert that the affected changes are unrelated")
	}
	for _, want := range []string{
		"not established to be independent",
		"one may be stacked on another",
		"an observation, not as proof",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing the correlation caveat %q", want)
		}
	}
}

// A diff is what makes a model invent a link to one change, so the shared
// failure prompt must never carry one.
func TestPromptCarriesNoChangedFiles(t *testing.T) {
	got := promptFor(t, testSubject())

	for _, absent := range []string{"Change hunks:", "Files this pull request changes:", "@@"} {
		if strings.Contains(got, absent) {
			t.Errorf("prompt must not carry change context, found %q", absent)
		}
	}
}

func TestBuildLevelFailureNamesTheJob(t *testing.T) {
	subject := testSubject()
	subject.BuildLevel = true

	got := promptFor(t, subject)
	if !strings.Contains(got, "pull-project-e2e is failing the same way") {
		t.Fatal("a build-level failure has no useful test name, so the job should be the subject")
	}
	if strings.Contains(got, "This test is failing the same way") {
		t.Error("a build-level failure must not be described as a test")
	}
}

func TestPullRequestsAreOrderedAndBounded(t *testing.T) {
	subject := Subject{BaseRef: "main", JobName: "pull-project-e2e", TestName: "t"}
	for i := maxListedPulls * 2; i > 0; i-- {
		subject.PullNumbers = append(subject.PullNumbers, i)
	}
	got := promptFor(t, subject)

	if !strings.Contains(got, "Affected pull requests: #1, #2, #3") {
		t.Error("affected pull requests should be listed in ascending order")
	}
	if !strings.Contains(got, "and 20 more") {
		t.Errorf("a long list should be truncated:\n%s", got)
	}
}

func TestZeroPullNumbersAreDropped(t *testing.T) {
	subject := Subject{BaseRef: "main", JobName: "j", TestName: "t", PullNumbers: []int{0, 7, 9}}

	got := promptFor(t, subject)
	if !strings.Contains(got, "failing the same way on 2 open pull requests") {
		t.Fatalf("an unusable pull request number must not be counted:\n%s", got)
	}
	if strings.Contains(got, "#0") {
		t.Error("an unusable pull request number must not be listed")
	}
}
