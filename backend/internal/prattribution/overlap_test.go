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
	// The base branch runs this test and it passes there, which is the
	// high-confidence unexplained case overlap refines.
	return annotateCase(t, failure, changes, observedBaseline(testName), false)
}

func annotateCase(t *testing.T, failure models.PullRequestFailure, changes PullChanges, baseline Baseline, stale bool) *models.FailureAttribution {
	t.Helper()
	details := []models.PullRequestDetail{detail(1, e2eJob, failure)}
	details[0].Checks[0].Stale = stale
	Annotate(details, baseline, capzRepo, map[int]PullChanges{1: changes})
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
	if !hasEvidence(got, models.AttributionEvidenceChangedCode) {
		t.Fatalf("evidence = %+v", got.Evidence)
	}
	last := got.Evidence[len(got.Evidence)-1]
	if len(last.Paths) != 1 || last.Paths[0] != e2eSitePath {
		t.Errorf("paths = %v, want the overlapping file", last.Paths)
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
	if !hasEvidence(got, models.AttributionEvidenceUnchangedCode) {
		t.Fatalf("evidence = %+v, want an unchanged-code citation", got.Evidence)
	}
}

// Overlap says nothing about baseline coverage, so it must not upgrade the
// confidence the baseline assigned, nor discard the baseline's own reason.
func TestOverlapKeepsBaselineConfidenceAndEvidence(t *testing.T) {
	// A test the base branch never ran is unexplained at LOW confidence.
	got := annotateCase(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		NewPullChanges([]string{"docs/README.md"}, false),
		observedBaseline("some-other-test"), false)

	if got.Confidence != models.AttributionConfidenceLow {
		t.Errorf("confidence = %q, want the baseline low confidence preserved", got.Confidence)
	}
	if !hasEvidence(got, models.AttributionEvidenceNoBaseline) {
		t.Errorf("the baseline reason must survive: %+v", got.Evidence)
	}
	if !hasEvidence(got, models.AttributionEvidenceUnchangedCode) {
		t.Errorf("the overlap finding must be appended: %+v", got.Evidence)
	}
}

// A build that tested a different head than the diff describes cannot be
// compared against it in either direction.
func TestStaleChecksNeverClaimOverlapOrItsAbsence(t *testing.T) {
	located := locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL)

	overlapping := annotateCase(t, located, NewPullChanges([]string{e2eSitePath}, false), observedBaseline(testName), true)
	if overlapping.Verdict == models.AttributionTouchesChangedCode {
		t.Error("a stale build must not claim overlap with the current diff")
	}
	absent := annotateCase(t, located, NewPullChanges([]string{"docs/README.md"}, false), observedBaseline(testName), true)
	if hasEvidence(absent, models.AttributionEvidenceUnchangedCode) {
		t.Error("a stale build must not claim the file was untouched")
	}
}

// A Ginkgo stack commonly enters another repository's framework before reaching
// the repository under test, so every frame must be considered.
func TestOverlapUsesEveryFrameNotJustTheFirst(t *testing.T) {
	failure := locatedFailure(testName,
		// The first location is a cluster-api helper, as junit extraction records.
		"sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115",
		"https://github.com/kubernetes-sigs/cluster-api/blob/v1.12.3/test/framework/controlplane_helpers.go#L115")
	failure.FailureBody = "sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115\n" +
		"sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412"

	got := annotateCase(t, failure, NewPullChanges([]string{e2eSitePath}, false), observedBaseline(testName), false)

	if got.Verdict != models.AttributionTouchesChangedCode {
		t.Fatalf("verdict = %q, want the later in-repo frame to match", got.Verdict)
	}
}

// A version-qualified location names a tagged dependency copy, not the tree.
func TestVersionQualifiedSameRepoLocationIsNotASite(t *testing.T) {
	failure := locatedFailure(testName,
		"sigs.k8s.io/cluster-api-provider-azure@v1.12.3/test/e2e/azure_test.go:412",
		"https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/v1.12.3/test/e2e/azure_test.go#L412")

	got := annotateCase(t, failure, NewPullChanges([]string{e2eSitePath}, false), observedBaseline(testName), false)

	if got.Verdict == models.AttributionTouchesChangedCode {
		t.Fatal("a tagged dependency copy must not count as the pull request's code")
	}
}

func hasEvidence(attribution *models.FailureAttribution, kind string) bool {
	for _, evidence := range attribution.Evidence {
		if evidence.Kind == kind {
			return true
		}
	}
	return false
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
	if hasEvidence(got, models.AttributionEvidenceUnchangedCode) {
		t.Fatal("a truncated changed-file list must not claim the file was untouched")
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
	if hasEvidence(got, models.AttributionEvidenceChangedCode) || hasEvidence(got, models.AttributionEvidenceUnchangedCode) {
		t.Fatalf("no location means no overlap claim, got %+v", got.Evidence)
	}
}

func TestMissingChangedFilesLeaveTheBaselineVerdict(t *testing.T) {
	got := annotateWithChanges(t,
		locatedFailure(testName, "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:412", e2eSiteURL),
		PullChanges{})

	if got.Verdict != models.AttributionUnexplained {
		t.Fatalf("verdict = %q", got.Verdict)
	}
	if hasEvidence(got, models.AttributionEvidenceUnchangedCode) {
		t.Fatal("an unfetched changed-file list must not claim the file was untouched")
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

// Git permits leading and trailing spaces in a path, so changed paths are
// stored exactly as GitHub reported them. Normalizing could invent a match.
func TestNewPullChangesPreservesPathsExactly(t *testing.T) {
	changes := NewPullChanges([]string{"a.go", " spaced.go ", ""}, false)

	if !changes.Known() || len(changes.Paths) != 2 {
		t.Fatalf("paths = %v", changes.Paths)
	}
	if !changes.Paths[" spaced.go "] {
		t.Error("the path should be stored verbatim")
	}
	if changes.Paths["spaced.go"] {
		t.Error("a trimmed variant must not match")
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
