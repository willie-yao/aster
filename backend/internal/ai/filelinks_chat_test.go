package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type staticChatRunner struct{}

func (staticChatRunner) Reply(context.Context, analysischat.Turn) (analysischat.Reply, error) {
	return analysischat.Reply{Answer: "The artifact supports the analysis.", Assessment: "supports"}, nil
}

// TestPreflightAnalysisFixSurvivesFlakyLinkVerification covers the reported
// symptom: a chat session bound to an unchanged cache-hit analysis must stay
// valid across a publication pass where GitHub link verification fails, rather
// than being rejected with "analysis changed".
func TestPreflightAnalysisFixSurvivesFlakyLinkVerification(t *testing.T) {
	revision := strings.Repeat("d", 40)
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withRawBase(t, srv.URL)

	dir := t.TempDir()
	store := NewCache(dir)
	// publish runs one publication pass over an unchanged cache-hit analysis:
	// a fresh resolver recomputes the links and the detail is rewritten.
	publish := func() {
		testCase := models.TestCase{
			Name: "TestCluster", JUnitFile: "junit.xml", Status: "failed", FailureMessage: "timed out",
			AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-08-16T07:11:06Z", RootCause: "the controller stopped", Severity: "High",
				SuggestedFix: "restart the controller", RelevantFiles: []string{"pkg/controller.go"},
				Mode: AgenticMode, CritiquePassed: true,
			},
		}
		resolver := NewFileLinkResolver("example", "repo")
		resolver.SetVerificationStore(store)
		testCase.AIAnalysis.FileLinks = resolver.ResolveAtRef(t.Context(), srv.Client(), &testCase, revision)
		detail := models.JobDetail{
			Name: "periodic-demo", JobID: "periodic-demo", JobType: models.JobTypePeriodic,
			Runs: []models.BuildResult{{
				BuildInfo: models.BuildInfo{
					BuildID: "123", JobName: "periodic-demo", WebURL: "https://example.test/build/123",
					RepoRefs: map[string]string{"example/repo": revision},
				},
				TestCases: []models.TestCase{testCase},
			}},
		}
		if err := output.WriteJobDetail(dir, detail); err != nil {
			t.Fatal(err)
		}
	}

	publish()
	service, err := analysischat.NewService(t.Context(), dir, staticChatRunner{},
		analysischat.Options{StateDir: filepath.Join(dir, ".chat"), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureTestFixPreflight(func(_ context.Context, _ sourceinvestigation.Repository, _ string, _ []string) (string, map[string]string, error) {
		return revision, map[string]string{"pkg/controller.go": strings.Repeat("a", 64)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(analysischat.AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
		AnalysisGeneratedAt: "2026-08-16T07:11:06Z",
	}, "Alice", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PreflightAnalysisFix(t.Context(), session.ID, "Alice", "request-1"); err != nil {
		t.Fatalf("initial Fix preflight error = %v", err)
	}

	healthy = false
	publish()
	if err := service.PreflightAnalysisFix(t.Context(), session.ID, "Alice", "request-1"); err != nil {
		t.Fatalf("Fix preflight after flaky link verification error = %v (analysis changed = %v)",
			err, errors.Is(err, analysischat.ErrAnalysisChanged))
	}
}
