package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	tree    []string
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
		if strings.HasPrefix(r.URL.Path, "/repos/example/repo/git/trees/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tree":[{"path":"controllers/reconcile.go","type":"blob"},{"path":"go.mod","type":"blob"},{"path":"migration/fix.go","type":"blob"}]}`))
			return
		}
		switch r.URL.Path {
		case "/example/repo/" + oldRevisionOne + "/controllers/reconcile.go", "/example/repo/" + oldRevisionTwo + "/controllers/reconcile.go":
			_, _ = w.Write([]byte(historical))
		case "/example/repo/" + passRevisionOne + "/controllers/reconcile.go", "/example/repo/" + passRevisionTwo + "/controllers/reconcile.go":
			_, _ = w.Write([]byte(current))
		case "/example/repo/" + oldRevisionOne + "/go.mod", "/example/repo/" + oldRevisionTwo + "/go.mod",
			"/example/repo/" + passRevisionOne + "/go.mod", "/example/repo/" + passRevisionTwo + "/go.mod":
			_, _ = w.Write([]byte("module example.com\n"))
		case "/example/repo/" + oldRevisionOne + "/migration/fix.go", "/example/repo/" + oldRevisionTwo + "/migration/fix.go",
			"/example/repo/" + passRevisionOne + "/migration/fix.go", "/example/repo/" + passRevisionTwo + "/migration/fix.go":
			_, _ = w.Write([]byte("package migration\nfunc ApplyFix() {}\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldRaw, oldAPI := rawContentBase, githubAPIBase
	rawContentBase, githubAPIBase = srv.URL, srv.URL
	t.Cleanup(func() { rawContentBase, githubAPIBase = oldRaw, oldAPI })

	repo := &verificationRepo{
		owner: "example", repo: "repo", ref: currentRevision,
		tree: []string{"controllers/reconcile.go", "go.mod", "migration/fix.go"},
		archive: actionverify.Archive{
			Paths:   map[string]bool{"controllers/reconcile.go": true, "go.mod": true, "migration/fix.go": true},
			GoFiles: map[string]string{"controllers/reconcile.go": current, "migration/fix.go": "package migration\nfunc ApplyFix() {}\n"},
			Files:   map[string]string{"go.mod": "module example.com\n"},
		},
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

func (r *verificationRepo) SourceIdentity() (string, string, string) { return r.owner, r.repo, r.ref }
func (r *verificationRepo) ListTree(context.Context) ([]string, error) {
	return append([]string(nil), r.tree...), nil
}
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

func TestVerifyPatternRemediationSamePackageLifecycle(t *testing.T) {
	const (
		currentRevision = "0123456789abcdef0123456789abcdef01234567"
		failureOne      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		failureTwo      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		passOne         = "cccccccccccccccccccccccccccccccccccccccc"
		passTwo         = "dddddddddddddddddddddddddddddddddddddddd"
	)
	currentFiles := samePackageRevisionFiles(true, true, false)
	buildConstrainedFiles := samePackageRevisionFiles(false, true, false)
	buildConstrainedFiles["controllers/helpers.go"] = "//go:build linux\n\npackage controllers\nfunc waitForReady() {}\n"
	malformedFiles := samePackageRevisionFiles(false, true, false)
	malformedFiles["controllers/helpers.go"] = "package controllers\nfunc waitForReady( {}\n"
	mixedPackageFiles := samePackageRevisionFiles(false, true, false)
	mixedPackageFiles["controllers/other.go"] = "package other\nfunc unrelated() {}\n"
	missingTargetFiles := samePackageRevisionFiles(false, true, false)
	delete(missingTargetFiles, "controllers/reconcile.go")
	for _, test := range []struct {
		name              string
		revisions         map[string]map[string]string
		passingRevisions  []string
		wantFailureState  models.PatternRemediationState
		wantFailureBuilds int
		wantPassingBuilds int
		wantLifecycle     models.PatternLifecycleState
	}{
		{
			name: "two verified passes retire the pattern",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: samePackageRevisionFiles(false, true, false),
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationUnresolved,
			wantFailureBuilds: 2, wantPassingBuilds: 2, wantLifecycle: models.PatternLifecycleVerifiedFixed,
		},
		{
			name: "one verified pass observes the pattern",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: samePackageRevisionFiles(false, true, false), passOne: currentFiles,
			},
			passingRevisions: []string{passOne}, wantFailureState: models.PatternRemediationUnresolved,
			wantFailureBuilds: 2, wantPassingBuilds: 1, wantLifecycle: models.PatternLifecycleObserving,
		},
		{
			name: "missing callee at a failure revision fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: samePackageRevisionFiles(false, false, false),
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "ambiguous package at a failure revision fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: samePackageRevisionFiles(false, true, true),
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "mixed package declarations fail closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: mixedPackageFiles,
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "build constrained callee fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: buildConstrainedFiles,
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "malformed package fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: malformedFiles,
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "missing target file fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: missingTargetFiles,
				passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantFailureBuilds: 1, wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "missing source revision fails closed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationInconclusive,
			wantFailureBuilds: 1, wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "failure revision containing the call is not fixed",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: currentFiles, passOne: currentFiles, passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationAlreadyPresent,
			wantLifecycle: models.PatternLifecycleActive,
		},
		{
			name: "passing revision without the call does not count",
			revisions: map[string]map[string]string{
				failureOne: samePackageRevisionFiles(false, true, false), failureTwo: samePackageRevisionFiles(false, true, false),
				passOne: samePackageRevisionFiles(false, true, false), passTwo: currentFiles,
			},
			passingRevisions: []string{passOne, passTwo}, wantFailureState: models.PatternRemediationUnresolved,
			wantFailureBuilds: 2, wantPassingBuilds: 1, wantLifecycle: models.PatternLifecycleObserving,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := samePackageVerificationService(t, currentRevision, currentFiles, test.revisions)
			pattern := models.PatternAnalysis{
				Systemic: true, SharedBuilds: []string{"failure-1", "failure-2"}, SuggestedFix: "call waitForReady",
				RemediationTargets: []models.RemediationTarget{{
					Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile.go",
				}},
			}
			base := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
			detail := models.JobDetail{Runs: []models.BuildResult{
				{BuildInfo: models.BuildInfo{BuildID: "failure-1", RepoVersion: failureOne, Started: base}},
				{BuildInfo: models.BuildInfo{BuildID: "failure-2", RepoVersion: failureTwo, Started: base.Add(time.Hour)}},
			}}
			for index, revision := range test.passingRevisions {
				detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
					BuildID: fmt.Sprintf("pass-%d", index+1), RepoVersion: revision, Passed: true, Started: base.Add(time.Duration(index+2) * time.Hour),
				}})
			}
			verification, err := service.VerifyPatternRemediation(t.Context(), pattern, detail)
			if err != nil || verification.State != models.PatternRemediationAlreadyPresent || verification.FailureState != test.wantFailureState || len(verification.FailureBuilds) != test.wantFailureBuilds || len(verification.PassingBuilds) != test.wantPassingBuilds {
				t.Fatalf("verification=%+v err=%v", verification, err)
			}
			pattern.RemediationVerification = &verification
			models.ApplyPatternLifecycle(detail, &pattern)
			if pattern.Lifecycle == nil || pattern.Lifecycle.State != test.wantLifecycle {
				t.Fatalf("lifecycle=%+v verification=%+v", pattern.Lifecycle, verification)
			}
		})
	}
}

func TestHistoricalPatternTargetsSupported(t *testing.T) {
	for _, target := range []models.RemediationTarget{
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/migration.Apply", Path: "controllers/reconcile.go"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "config.yaml", Value: "enabled=true"},
		{Intent: models.RemediationIntentRemoveConfiguration, Path: "config.yaml", Value: "enabled=true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "job.yaml", Job: "job", Container: "test", Name: "VALUE", Value: "true"},
	} {
		if !historicalPatternTargetsSupported([]models.RemediationTarget{target}) {
			t.Fatalf("supported target rejected: %+v", target)
		}
	}
	for _, target := range []models.RemediationTarget{
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", Path: "controllers/reconcile.go"},
		{Intent: models.RemediationIntentAddSymbol, Symbol: "waitForReady", Path: "controllers/helpers.go"},
		{Intent: models.RemediationIntentInvestigate},
	} {
		if historicalPatternTargetsSupported([]models.RemediationTarget{target}) {
			t.Fatalf("unsupported target accepted: %+v", target)
		}
	}
}

func samePackageVerificationService(t *testing.T, currentRevision string, currentFiles map[string]string, revisions map[string]map[string]string) *Service {
	t.Helper()
	paths := []string{"controllers/helpers.go", "controllers/other.go", "controllers/reconcile.go"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/example/repo/git/trees/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tree":[{"path":"controllers/helpers.go","type":"blob"},{"path":"controllers/other.go","type":"blob"},{"path":"controllers/reconcile.go","type":"blob"}]}`))
			return
		}
		prefix := "/example/repo/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		revisionAndPath := strings.TrimPrefix(r.URL.Path, prefix)
		revision, file, ok := strings.Cut(revisionAndPath, "/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		content, found := revisions[revision][file]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)
	oldRaw, oldAPI := rawContentBase, githubAPIBase
	rawContentBase, githubAPIBase = srv.URL, srv.URL
	t.Cleanup(func() { rawContentBase, githubAPIBase = oldRaw, oldAPI })
	archive := actionverify.Archive{Paths: map[string]bool{}, GoFiles: map[string]string{}, Files: map[string]string{}}
	for _, file := range paths {
		archive.Paths[file] = true
		archive.GoFiles[file] = currentFiles[file]
	}
	return &Service{
		patternRepo:     &verificationRepo{owner: "example", repo: "repo", ref: currentRevision, tree: paths, archive: archive},
		sourceRepoOwner: "example", sourceRepoName: "repo",
	}
}

func samePackageRevisionFiles(call, callee, duplicate bool) map[string]string {
	reconcile := "package controllers\nfunc reconcile() {}\n"
	if call {
		reconcile = "package controllers\nfunc reconcile() { waitForReady() }\n"
	}
	helper := "package controllers\n"
	if callee {
		helper += "func waitForReady() {}\n"
	}
	other := "package controllers\n"
	if duplicate {
		other += "func waitForReady() {}\n"
	}
	return map[string]string{
		"controllers/reconcile.go": reconcile,
		"controllers/helpers.go":   helper,
		"controllers/other.go":     other,
	}
}
