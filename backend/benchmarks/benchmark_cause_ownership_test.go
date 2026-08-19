package benchmarks

import (
	"slices"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

// The independently confirmed case from issue #63: the DRA conformance failure
// is caused by kubelet device-plugin code in kubernetes/kubernetes, which a
// maintainer fixed upstream in kubernetes/kubernetes#141426. Ownership is
// scored from the structured location rather than prose, because the
// publication sanitizer removes an upstream path that no pinned revision can
// verify.
var draOwnershipCase = benchCase{
	name:            "capz-dra-upstream-device-plugin",
	causeRepository: "kubernetes/kubernetes",
	causeExternal:   true,
	causeFiles:      []string{"pkg/kubelet/cm/devicemanager/manager.go"},
}

func ownershipResult(t *testing.T, bc benchCase, location *models.AnalysisCauseLocation) benchmarkAssessment {
	t.Helper()
	return assessBenchmarkCase(bc, &models.TestCase{
		AISummary:  &models.AISummary{Summary: "summary"},
		AIAnalysis: &models.AIAnalysis{RootCause: "cause", SuggestedFix: "fix", CauseLocation: location},
	})
}

func TestBenchmarkScoresUpstreamCauseOwnership(t *testing.T) {
	got := ownershipResult(t, draOwnershipCase, &models.AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes", External: true,
		Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"},
	})
	if len(got.missingMust) != 0 || got.hits != 2 || got.total != 2 || got.diagnosisHits != 2 {
		t.Fatalf("correct upstream ownership scored %+v", got)
	}
}

func TestBenchmarkFailsUnattributedOrMisattributedCause(t *testing.T) {
	for name, location := range map[string]*models.AnalysisCauseLocation{
		"unattributed": nil,
		"wrong repository": {
			Repository: "kubernetes-sigs/cloud-provider-azure", External: true,
			Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"},
		},
		// The negative control: an upstream cause reported as the project's own
		// is exactly the misclassification the UI must never act on.
		"reported as own repository": {
			Repository: "kubernetes/kubernetes",
			Files:      []string{"pkg/kubelet/cm/devicemanager/manager.go"},
		},
	} {
		got := ownershipResult(t, draOwnershipCase, location)
		if len(got.missingMust) != 2 || got.hits != 0 {
			t.Errorf("%s ownership scored %+v", name, got)
		}
	}
}

func TestBenchmarkFailsOwnershipWithoutTheReportedFile(t *testing.T) {
	got := ownershipResult(t, draOwnershipCase, &models.AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes", External: true,
	})
	if got.hits != 1 || !slices.Equal(got.missingMust, []string{"cause location names pkg/kubelet/cm/devicemanager/manager.go"}) {
		t.Fatalf("ownership without the reported file scored %+v", got)
	}
}

func TestBenchmarkSkipsOwnershipScoringWhenUndeclared(t *testing.T) {
	if got := benchmarkOwnershipExpectations(benchCase{causeFiles: []string{"a.go"}}); got != nil {
		t.Fatalf("expectations without a repository = %+v", got)
	}
	got := ownershipResult(t, benchCase{name: "no ownership expectation"}, nil)
	if got.total != 0 || len(got.missingMust) != 0 {
		t.Fatalf("undeclared ownership scored %+v", got)
	}
}
