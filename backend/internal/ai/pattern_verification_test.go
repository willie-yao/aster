package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type verificationRepo struct {
	archive actionverify.Archive
	owner   string
	repo    string
	ref     string
	reads   int
}

func TestVerifyPatternRemediationProvesAbsentAtFailureRevisions(t *testing.T) {
	const (
		currentRevision = "0123456789abcdef0123456789abcdef01234567"
		oldRevisionOne  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		oldRevisionTwo  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		passRevisionOne = "cccccccccccccccccccccccccccccccccccccccc"
		passRevisionTwo = "dddddddddddddddddddddddddddddddddddddddd"
	)
	current := `package controllers
import "example.com/migration"
func reconcile() { migration.ApplyFix() }
`
	historical := `package controllers
import "example.com/migration"
func reconcile() {}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example/repo/" + oldRevisionOne + "/controllers/reconcile.go", "/example/repo/" + oldRevisionTwo + "/controllers/reconcile.go":
			_, _ = w.Write([]byte(historical))
		case "/example/repo/" + passRevisionOne + "/controllers/reconcile.go", "/example/repo/" + passRevisionTwo + "/controllers/reconcile.go":
			_, _ = w.Write([]byte(current))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldRaw := rawContentBase
	rawContentBase = srv.URL
	t.Cleanup(func() { rawContentBase = oldRaw })

	repo := &verificationRepo{
		owner: "example", repo: "repo", ref: currentRevision,
		archive: actionverify.Archive{Paths: map[string]bool{"controllers/reconcile.go": true}, GoFiles: map[string]string{"controllers/reconcile.go": current}},
	}
	service := &Service{patternRepo: repo, sourceRepoOwner: "example", sourceRepoName: "repo"}
	pattern := models.PatternAnalysis{
		Systemic: true, SharedBuilds: []string{"failure-1", "failure-2"}, SuggestedFix: "call migration.ApplyFix",
		RemediationTargets: []models.RemediationTarget{{
			Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/migration.ApplyFix", Path: "controllers/reconcile.go",
		}},
	}
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	detail := models.JobDetail{Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "failure-1", RepoRefs: map[string]string{"example/repo": "main:" + oldRevisionOne}, Revision: passRevisionOne, Started: base}},
		{BuildInfo: models.BuildInfo{BuildID: "failure-2", RepoVersion: oldRevisionTwo, Started: base.Add(time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "pass-1", RepoVersion: passRevisionOne, Passed: true, Started: base.Add(2 * time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "pass-2", RepoRefs: map[string]string{"example/repo": "main:" + passRevisionTwo}, Passed: true, Started: base.Add(3 * time.Hour)}},
	}}
	verification, err := service.VerifyPatternRemediation(t.Context(), pattern, detail)
	if err != nil || verification.State != models.PatternRemediationAlreadyPresent || verification.FailureState != models.PatternRemediationUnresolved || len(verification.FailureBuilds) != 2 || len(verification.PassingBuilds) != 2 {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
}

func (r *verificationRepo) SourceIdentity() (string, string, string)   { return r.owner, r.repo, r.ref }
func (r *verificationRepo) ListTree(context.Context) ([]string, error) { return nil, nil }
func (r *verificationRepo) ReadFile(_ context.Context, path string) (string, bool, error) {
	if value, ok := r.archive.GoFiles[path]; ok {
		return value, true, nil
	}
	if value, ok := r.archive.Files[path]; ok {
		return value, true, nil
	}
	return "", false, nil
}
func (r *verificationRepo) ReadSourceArchive(context.Context) (actionverify.Archive, error) {
	r.reads++
	return r.archive, nil
}

func TestVerifyPatternRemediationAlreadyPresent(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	repo := &verificationRepo{
		owner: "example", repo: "repo", ref: revision,
		archive: actionverify.Archive{Paths: map[string]bool{"pkg/reconcile.go": true}, GoFiles: map[string]string{
			"pkg/reconcile.go": "package pkg\nfunc ApplyFix() {}\nfunc reconcile() { ApplyFix() }\n",
		}},
	}
	service := &Service{patternRepo: repo, sourceRepoOwner: "example", sourceRepoName: "repo"}
	verification, err := service.VerifyPatternRemediation(t.Context(), models.PatternAnalysis{
		Systemic: true, SuggestedFix: "call ApplyFix", RemediationTargets: []models.RemediationTarget{{
			Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "pkg/reconcile.go",
		}},
	}, models.JobDetail{})
	if err != nil || verification.State != models.PatternRemediationAlreadyPresent || verification.Repository != "example/repo" || verification.Revision != revision || repo.reads != 1 {
		t.Fatalf("verification=%+v reads=%d err=%v", verification, repo.reads, err)
	}
}

func TestVerifyPatternRemediationIncompleteTargetIsInconclusive(t *testing.T) {
	service := &Service{}
	verification, err := service.VerifyPatternRemediation(t.Context(), models.PatternAnalysis{
		Systemic: true, RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", Path: "pkg/reconcile.go"}},
	}, models.JobDetail{})
	if err != nil || verification.State != models.PatternRemediationInconclusive {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
}

func TestPatternRemediationReaderRejectsMixedRepositories(t *testing.T) {
	service := &Service{patternRepo: &verificationRepo{}}
	_, _, _, err := service.patternRemediationReader(models.PatternAnalysis{RemediationTargets: []models.RemediationTarget{
		{Intent: models.RemediationIntentAddSymbol, Symbol: "Fix", Path: "fix.go"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "0123456789abcdef0123456789abcdef01234567"},
	}})
	if err == nil {
		t.Fatal("mixed repositories were accepted")
	}
}

func TestResolvePatternBuildSourceRequiresRepoRefsForExternalRepository(t *testing.T) {
	service := &Service{sourceRepoOwner: "example", sourceRepoName: "source"}
	const revision = "0123456789abcdef0123456789abcdef01234567"
	if _, ok := service.resolvePatternBuildSource(models.BuildInfo{RepoVersion: revision, Commit: revision}, "kubernetes", "test-infra"); ok {
		t.Fatal("external repository used checkout fallback metadata")
	}
	source, ok := service.resolvePatternBuildSource(models.BuildInfo{
		RepoRefs: map[string]string{"kubernetes/test-infra": "main:" + revision}, RepoVersion: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, "kubernetes", "test-infra")
	if !ok || source.Revision != revision {
		t.Fatalf("source=%+v ok=%t", source, ok)
	}
}
