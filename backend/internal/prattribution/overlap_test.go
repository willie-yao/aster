package prattribution

import (
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

var capzRepo = Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}

const (
	e2eSitePath = "test/e2e/azure_test.go"
	e2eSiteURL  = "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/test/e2e/azure_test.go#L412"
)

// locatedFailure is a failing test whose JUnit body named a source location.
func locatedFailure(name, location, url string) models.PullRequestFailure {
	return models.PullRequestFailure{TestCase: models.TestCase{
		Name: name, Status: "failed", FailureLocation: location, FailureLocURL: url,
	}}
}

func annotateWithChanges(t *testing.T, failure models.PullRequestFailure, changes PullChanges) *models.FailureAttribution {
	t.Helper()
	details := []models.PullRequestDetail{detail(1, e2eJob, failure)}
	// A baseline that observed the base branch but never ran this test keeps the
	// verdict unexplained, which is the residual set overlap applies to.
	Annotate(details, observedBaseline("some-other-test"), capzRepo, map[int]PullChanges{1: changes})
	got := details[0].Checks[0].Failures[0].Attribution
	if got == nil {
		t.Fatal("expected an attribution")
	}
	return got
}

func TestFailureInAChangedFileReportsOverlap(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		NewPullChanges([]string{e2eSitePath, "azure/scope/cluster.go"}, false))

	if got.Verdict != models.AttributionTouchesChangedCode {
		t.Fatalf("verdict = %q, want touches_changed_code", got.Verdict)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceChangedCode {
		t.Fatalf("evidence = %+v", got.Evidence)
	}
	if len(got.Evidence[0].Paths) != 1 || got.Evidence[0].Paths[0] != e2eSitePath {
		t.Errorf("paths = %v, want the overlapping file", got.Evidence[0].Paths)
	}
	// Overlap is an observation. The summary must hedge explicitly rather than
	// assert that the change is responsible.
	if !strings.Contains(got.Summary, "not proof") {
		t.Errorf("summary must qualify the overlap: %q", got.Summary)
	}
	if containsAny(got.Summary, "pull request caused", "change caused", "caused this", "is responsible for") {
		t.Errorf("summary asserts causation: %q", got.Summary)
	}
}

func TestFailureOutsideChangedFilesStaysUnexplained(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		NewPullChanges([]string{"docs/README.md"}, false))

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q, want unexplained", got.Verdict)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != models.AttributionEvidenceUnchangedCode {
		t.Fatalf("evidence = %+v, want an unchanged-code citation", got.Evidence)
	}
}

// A truncated changed-file list cannot rule the change out, because the
// unobserved files may include the failure site.
func TestTruncatedChangesNeverClaimAbsenceOfOverlap(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		NewPullChanges([]string{"docs/README.md"}, true))

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q", got.Verdict)
	}
	for _, evidence := range got.Evidence {
		if evidence.Kind == models.AttributionEvidenceUnchangedCode {
			t.Fatal("a truncated changed-file list must not claim the file was untouched")
		}
	}
}

// Overlap is still reported from a truncated list, because an observed match is
// evidence regardless of what was not observed.
func TestTruncatedChangesStillReportOverlap(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		NewPullChanges([]string{e2eSitePath}, true))

	if got.Verdict != models.AttributionTouchesChangedCode {
		t.Fatalf("verdict = %q, want touches_changed_code", got.Verdict)
	}
}

// A failure inside a dependency must never match the pull request's repository.
func TestFailureInAnotherRepositoryIsNotOverlap(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115",
			"https://github.com/kubernetes-sigs/cluster-api/blob/v1.12.3/test/framework/controlplane_helpers.go#L115"),
		// The same relative path exists in the pull request, but in another repo.
		NewPullChanges([]string{"test/framework/controlplane_helpers.go"}, false))

	if got.Verdict == models.AttributionTouchesChangedCode {
		t.Fatal("a cluster-api failure site must not match a cluster-api-provider-azure change")
	}
}

func TestFailureWithoutALocationIsUnchanged(t *testing.T) {
	got := annotateWithChanges(t, failure(testName), NewPullChanges([]string{e2eSitePath}, false))

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q", got.Verdict)
	}
	for _, evidence := range got.Evidence {
		if evidence.Kind == models.AttributionEvidenceChangedCode || evidence.Kind == models.AttributionEvidenceUnchangedCode {
			t.Fatalf("no location means no overlap claim, got %+v", evidence)
		}
	}
}

func TestMissingChangedFilesLeaveTheBaselineVerdict(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		PullChanges{})

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q", got.Verdict)
	}
	for _, evidence := range got.Evidence {
		if evidence.Kind == models.AttributionEvidenceUnchangedCode {
			t.Fatal("an unfetched changed-file list must not claim the file was untouched")
		}
	}
}

// Overlap only refines the residual set. A failure the base branch already
// explains is not made more explicable by touching changed code.
func TestOverlapDoesNotOverrideAnExplainedVerdict(t *testing.T) {
	baseline := observedBaseline(testName)
	baseline.FailingOnBase[testName] = []string{"periodic-project-e2e"}
	details := []models.PullRequestDetail{detail(1, e2eJob,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL))}

	Annotate(details, baseline, capzRepo, map[int]PullChanges{1: NewPullChanges([]string{e2eSitePath}, false)})

	if got := details[0].Checks[0].Failures[0].Attribution; got.Verdict != models.AttributionPreExisting {
		t.Fatalf("verdict = %q, want pre_existing to win", got.Verdict)
	}
}

func TestNewPullChanges(t *testing.T) {
	changes := NewPullChanges([]string{"a.go", "  b.go  ", "", "   "}, false)

	if !changes.Known() || len(changes.Paths) != 2 {
		t.Fatalf("paths = %v", changes.Paths)
	}
	if !changes.Paths["b.go"] {
		t.Error("surrounding whitespace should be trimmed")
	}
	var empty PullChanges
	if empty.Known() {
		t.Error("an empty set is not known")
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
