package actions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const analysisFixRevision = "0123456789abcdef0123456789abcdef01234567"

const (
	capzFailureRevision        = "a866aca055bcaa205648e81d15c67668179fdfab"
	capzGenerationBaseRevision = "c83d69ab8c572a4c00816076222d65262ee690cc"
	capzReleaseBaseRevision    = "8caa35df8680f64693a3f76ea3d35c2349ab4828"
)

func exactJUnitDetail() models.JobDetail {
	return models.JobDetail{Name: "periodic-capz", JobID: "periodic-capz", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{
			BuildID: "123", JobName: "periodic-capz",
			RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main:" + analysisFixRevision},
		},
		TestCases: []models.TestCase{{
			Name: "TestCluster", SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit_01.xml", Status: "failed",
			FailureMessage: "cluster failed", FailureBody: "expected Ready",
			AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-08-13T01:00:00Z", Mode: ai.AgenticMode,
				CritiquePassed: true, CritiqueVersion: ai.CurrentCritiqueVersion(),
				RootCause: "The reconciler omitted the terminal state.", Severity: "High",
				SuggestedFix:  "Update the reconciler branch.",
				RelevantFiles: []string{"controllers/cluster_controller.go"},
				EvidenceCitations: []models.EvidenceCitation{{
					Path: "artifacts/junit_01.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready",
				}},
				FileLinks: map[string]string{
					"controllers/cluster_controller.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + analysisFixRevision + "/controllers/cluster_controller.go",
				},
			},
		}},
	}}}
}

func exactAnalysisConfig() *project.Config {
	return &project.Config{
		Name: "capz",
		Branding: project.Branding{
			SourceRepo: project.SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"},
		},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled: true,
			Repo: &project.SourceRepo{
				Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure",
			},
			AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"},
		}},
	}
}

func exactIdentity() AnalysisIdentity {
	return AnalysisIdentity{
		Project: "capz", JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster",
		SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit_01.xml",
		AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}
}

type fakeAnalysisSourceRevisionClient struct {
	base           ghpr.Base
	branchBases    map[string]ghpr.Base
	resolveErr     error
	contains       bool
	compareErr     error
	compareCalls   int
	branchRequests []string
}

func (f *fakeAnalysisSourceRevisionClient) ResolveBase(_ context.Context, _, _, branch string) (ghpr.Base, error) {
	f.branchRequests = append(f.branchRequests, branch)
	if f.resolveErr != nil {
		return ghpr.Base{}, f.resolveErr
	}
	if base, ok := f.branchBases[branch]; ok {
		return base, nil
	}
	if branch == "" || branch == f.base.Branch {
		return f.base, nil
	}
	return ghpr.Base{}, fmt.Errorf("branch %s not found", branch)
}

func (f *fakeAnalysisSourceRevisionClient) CompareCommits(context.Context, string, string, string, string) (bool, string, error) {
	f.compareCalls++
	if f.compareErr != nil {
		return false, "", f.compareErr
	}
	if f.contains {
		return true, "ahead", nil
	}
	return false, "diverged", nil
}

func TestResolveAnalysisActionSubjectUsesOnlyStructuralEligibility(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*models.JobDetail)
		ok     bool
	}{
		{name: "published analysis", ok: true},
		{name: "unverified model mode", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis.Mode = "legacy"
		}, ok: true},
		{name: "critique failed", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis.CritiquePassed = false
		}, ok: true},
		{name: "empty diagnosis", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis.RootCause = ""
			d.Runs[0].TestCases[0].AIAnalysis.SuggestedFix = ""
		}, ok: true},
		{name: "transient", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis.Severity = "Transient-Ignore"
		}, ok: true},
		{name: "missing source hints", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis.FileLinks = nil
		}, ok: true},
		{name: "passing", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].Status = "passed"
		}},
		{name: "missing analysis", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].AIAnalysis = nil
		}},
		{name: "build failure", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].Source = models.TestCaseSourceBuild
		}},
		{name: "missing junit", mutate: func(d *models.JobDetail) {
			d.Runs[0].TestCases[0].JUnitFile = ""
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			detail := exactJUnitDetail()
			if testCase.mutate != nil {
				testCase.mutate(&detail)
			}
			writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
			subject, err := NewService(exactAnalysisConfig(), dir, AIConfig{}).ResolveAnalysisActionSubject(exactIdentity())
			if testCase.ok {
				if err != nil || subject == nil || subject.ContentHash == "" {
					t.Fatalf("subject=%+v err=%v", subject, err)
				}
				if testCase.name == "missing source hints" && len(subject.SourceHints) != 0 {
					t.Fatalf("source hints = %v", subject.SourceHints)
				}
				return
			}
			if err == nil {
				t.Fatalf("ineligible subject = %+v", subject)
			}
		})
	}
}

func TestResolveAnalysisActionSubjectCapsSortedSourceHints(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	detail.Runs[0].TestCases[0].AIAnalysis.FileLinks = map[string]string{}
	for i := 19; i >= 0; i-- {
		file := fmt.Sprintf("controllers/file-%02d.go", i)
		detail.Runs[0].TestCases[0].AIAnalysis.FileLinks[file] =
			"https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + analysisFixRevision + "/" + file
	}
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)

	subject, err := NewService(exactAnalysisConfig(), dir, AIConfig{}).ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if len(subject.SourceHints) != maxAnalysisSourceFiles || !slices.IsSorted(subject.SourceHints) {
		t.Fatalf("source hints = %v", subject.SourceHints)
	}
}

func TestResolveAnalysisActionSubjectRejectsStaleAndAmbiguousIdentity(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	stale := exactIdentity()
	stale.AnalysisGeneratedAt = "2026-08-13T00:00:00Z"
	if _, err := service.ResolveAnalysisActionSubject(stale); err == nil {
		t.Fatal("stale analysis remained eligible")
	}
	detail.Runs[0].TestCases = append(detail.Runs[0].TestCases, detail.Runs[0].TestCases[0])
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	if _, err := service.ResolveAnalysisActionSubject(exactIdentity()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ambiguous analysis error = %v", err)
	}
}

func TestAnalysisSourceCompatibilityRequiresExplicitBranch(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	client := &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: capzFailureRevision, TreeSHA: "tree"},
	}
	service.sourceRevisionClient = client
	repo := sourceinvestigation.Repository{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: capzFailureRevision,
	}

	_, err := service.verifyAnalysisSourceCompatibility(t.Context(), repo, "")
	if code, ok := ReasonCodeFrom(err); !ok || code != ReasonSourceBranchUnknown {
		t.Fatalf("err=%v code=%q ok=%t", err, code, ok)
	}
	if len(client.branchRequests) != 0 || client.compareCalls != 0 {
		t.Fatalf("unexpected revision calls: branches=%v compares=%d", client.branchRequests, client.compareCalls)
	}
}

func TestAnalysisSourceCompatibilityBindsCurrentBaseWithoutReadingHints(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	client := &fakeAnalysisSourceRevisionClient{
		base:     ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision, TreeSHA: "tree"},
		contains: true,
	}
	service.sourceRevisionClient = client
	repo := sourceinvestigation.Repository{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: capzFailureRevision,
	}

	compatibility, err := service.verifyAnalysisSourceCompatibility(t.Context(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility.GenerationBaseRevision != capzGenerationBaseRevision || client.compareCalls != 1 {
		t.Fatalf("compatibility=%+v compares=%d", compatibility, client.compareCalls)
	}
}

func TestAnalysisSourceCompatibilityPreservesRepositoryBranchAndAncestryChecks(t *testing.T) {
	repo := sourceinvestigation.Repository{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: capzFailureRevision,
	}
	for _, testCase := range []struct {
		name       string
		repository sourceinvestigation.Repository
		base       ghpr.Base
		contains   bool
		wantReason ReasonCode
		wantText   string
	}{
		{
			name: "diverged", repository: repo,
			base:       ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision},
			wantReason: ReasonSourceRevisionDiverged,
		},
		{
			name: "wrong branch", repository: repo,
			base:     ghpr.Base{Branch: "release", HeadSHA: capzFailureRevision},
			wantText: "branch does not match",
		},
		{
			name: "wrong repository",
			repository: sourceinvestigation.Repository{
				Owner: "kubernetes", Name: "kubernetes", Revision: capzFailureRevision,
			},
			base:     ghpr.Base{Branch: "main", HeadSHA: capzFailureRevision},
			wantText: "repositories do not match",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
			service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{
				base: testCase.base, branchBases: map[string]ghpr.Base{"main": testCase.base},
				contains: testCase.contains,
			}
			_, err := service.verifyAnalysisSourceCompatibility(t.Context(), testCase.repository, "main")
			if err == nil {
				t.Fatal("incompatible source was accepted")
			}
			if testCase.wantReason != "" && ReasonCodeOf(err) != testCase.wantReason {
				t.Fatalf("err=%v code=%q", err, ReasonCodeOf(err))
			}
			if testCase.wantText != "" && !strings.Contains(err.Error(), testCase.wantText) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAnalysisSourceCompatibilityResolvesFailureBranchBase(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	client := &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision},
		branchBases: map[string]ghpr.Base{
			"release-1.25": {Branch: "release-1.25", HeadSHA: capzReleaseBaseRevision},
		},
		contains: true,
	}
	service.sourceRevisionClient = client
	repo := sourceinvestigation.Repository{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: capzFailureRevision,
	}

	got, err := service.verifyAnalysisSourceCompatibility(t.Context(), repo, "release-1.25")
	if err != nil {
		t.Fatal(err)
	}
	if got.GenerationBaseRevision != capzReleaseBaseRevision ||
		!slices.Equal(client.branchRequests, []string{"release-1.25"}) {
		t.Fatalf("compatibility=%+v branches=%v", got, client.branchRequests)
	}
}

func TestPreflightAnalysisFixSourcePropagatesAccessAndAncestryErrors(t *testing.T) {
	repo := sourceinvestigation.Repository{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: capzFailureRevision,
	}
	t.Run("access", func(t *testing.T) {
		accessErr := errors.New("repository access denied")
		service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
		service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{resolveErr: accessErr}
		_, err := service.PreflightAnalysisFixSource(t.Context(), repo, "main")
		if !errors.Is(err, ErrPreviewRejected) || !errors.Is(err, accessErr) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("ancestry", func(t *testing.T) {
		service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
		service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{
			base: ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision},
		}
		_, err := service.PreflightAnalysisFixSource(t.Context(), repo, "main")
		if !errors.Is(err, ErrPreviewRejected) || ReasonCodeOf(err) != ReasonSourceRevisionDiverged {
			t.Fatalf("error = %v code=%q", err, ReasonCodeOf(err))
		}
	})
}

type acceptingAnalysisPreviewValidator struct{}

func (acceptingAnalysisPreviewValidator) ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error {
	return nil
}

type rejectingAnalysisPreviewValidator struct{}

func (rejectingAnalysisPreviewValidator) ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error {
	return errors.New("chat response changed")
}

func TestUnverifiedUncitedTransientAnalysisFixReachesGeneratorAndRestoresPreview(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	failure := &detail.Runs[0].TestCases[0]
	failure.FailureMessage = strings.Repeat("message-", maxAnalysisFailureTextBytes)
	failure.FailureBody = strings.Repeat("body-", maxAnalysisFailureTextBytes)
	failure.AIAnalysis.FileLinks = nil
	failure.AIAnalysis.Severity = "Transient-Ignore"
	failure.AIAnalysis.CritiquePassed = false
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)

	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	service.ConfigureAsyncRequests(time.Minute, nil)
	service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
	}
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	generated := make(chan fixpr.AnalysisFailure, 1)
	service.analysisRequestGenerator = func(
		ctx context.Context, input AnalysisFixInput, owner, _, _ string,
	) (PreviewResult, error) {
		current, err := service.ResolveAnalysisActionSubject(input.Identity)
		if err != nil {
			return PreviewResult{}, err
		}
		generation := analysisFailureForGeneration(current, input, "main", analysisFixRevision)
		generated <- generation
		if err := service.setRequestWarning(ctx, analysisQualityWarnings(current.Failure.AIAnalysis, input, current.SourceHints)...); err != nil {
			return PreviewResult{}, err
		}
		fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
			Subject: current.Identity.TestName, Rationale: input.AssistantAnswer,
			Diff:   "--- a/controllers/cluster_controller.go\n+++ b/controllers/cluster_controller.go\n+func retryConflict() {}\n",
			Files:  map[string]string{"controllers/cluster_controller.go": "package controllers\nfunc retryConflict() {}\n"},
			Verify: fixpr.VerifyResult{Status: fixpr.VerifyPassed},
			Title:  "fix: investigate transient failure", Description: "safe description", Body: "safe body",
			Key:                "fix-analysis::unverified",
			Base:               ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
			RequireBaseCurrent: true,
		})
		binding := &AnalysisPreviewBinding{
			Identity: current.Identity, AnalysisID: current.ID, AnalysisHash: current.ContentHash,
			AnalysisContentHash: current.AnalysisContentHash,
			ChatSessionID:       input.ChatSessionID, ChatRequestID: input.ChatRequestID,
			ChatResponseHash: input.ChatResponseHash, PreviewRequestHash: input.PreviewRequestHash,
			SourceRepository: current.SourceRepository, SourceBranch: "main",
			FailureRevision: current.SourceRepository.Revision, GenerationBaseRevision: analysisFixRevision,
			VerificationVersion: analysisSourceVerificationVersion,
		}
		entry := &previewEntry{
			failureID: current.ID, patternHash: current.ContentHash, kind: gfKind,
			targetRepo:          current.SourceRepository.Owner + "/" + current.SourceRepository.Name,
			targetConfig:        fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()),
			verificationVersion: sourceVerificationVersion, fix: fix, analysisBinding: binding,
		}
		preview, err := validatedPreviewEntry(entry)
		if err != nil {
			return PreviewResult{}, err
		}
		token, err := service.stash(owner, entry)
		if err != nil {
			return PreviewResult{}, err
		}
		preview.Token = token
		return preview, nil
	}

	input := AnalysisFixInput{
		Identity: exactIdentity(), ChatSessionID: "session", ChatRequestID: "request",
		ChatResponseHash: "chat-hash", PreviewRequestHash: "preview-hash",
		AnalysisContentHash: subject.AnalysisContentHash, SourceRepository: subject.SourceRepository,
		FailureRevision: subject.SourceRepository.Revision, GenerationBaseRevision: analysisFixRevision,
		SourceBranch: "main", AssistantAnswer: "Investigate whether reconciliation skips the terminal update.",
		AssistantUnverified: true, AssistantUnverifiedReason: "artifact access ended before verification",
	}
	created, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Token == "" {
		t.Fatalf("ready request = %+v", ready)
	}
	for _, warning := range []string{
		analysisWarningCritique, analysisWarningTransient, analysisWarningAssistantUnverified,
		analysisWarningNoCitations, analysisWarningNoSourceHints,
	} {
		if !strings.Contains(ready.Warning, warning) {
			t.Fatalf("warning %q missing from %q", warning, ready.Warning)
		}
	}
	select {
	case got := <-generated:
		if len(got.ArtifactCitations) != 0 || len(got.SourceHints) != 0 || !got.AssistantUnverified ||
			got.AssistantUnverifiedReason != input.AssistantUnverifiedReason {
			t.Fatalf("generation context = %+v", got)
		}
		if len(got.FailureMessage) > maxAnalysisFailureTextBytes || len(got.FailureBody) > maxAnalysisFailureTextBytes ||
			!strings.HasSuffix(got.FailureMessage, "…") || !strings.HasSuffix(got.FailureBody, "…") {
			t.Fatalf("raw failure bounds: message=%d body=%d", len(got.FailureMessage), len(got.FailureBody))
		}
	case <-time.After(time.Second):
		t.Fatal("fake exact-fix generator was not invoked")
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(exactAnalysisConfig(), dir, AIConfig{})
	restored, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || restored.Status != RequestReady || restored.Preview == nil || restored.Preview.Token != ready.Preview.Token {
		t.Fatalf("restored request=%+v err=%v", restored, err)
	}
	entry, err := reloaded.previewStore.take("alice", restored.Preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validatedPreviewEntry(entry); err != nil {
		t.Fatalf("restored preview contract error = %v", err)
	}
	reloaded.analysisPreviewValidator = acceptingAnalysisPreviewValidator{}
	reloaded.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
	}
	if entry.analysisBinding == nil {
		t.Fatal("restored preview lost exact analysis binding")
	}
	if err := reloaded.validateAnalysisPreview(t.Context(), "alice", *entry.analysisBinding); err != nil {
		t.Fatalf("restored preview is not confirmable: %v", err)
	}
}

func TestAnalysisPreviewBindingSurvivesRestartAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	binding := &AnalysisPreviewBinding{
		Identity: exactIdentity(), AnalysisID: "analysis::id", AnalysisHash: "analysis-hash",
		AnalysisContentHash: "content-hash",
		ChatSessionID:       "session", ChatRequestID: "request", ChatResponseHash: "chat",
		PreviewRequestHash: "preview",
		SourceRepository: sourceinvestigation.Repository{
			Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision,
		},
		SourceBranch: "main", FailureRevision: analysisFixRevision,
		GenerationBaseRevision: analysisFixRevision,
		VerificationVersion:    analysisSourceVerificationVersion,
	}
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "TestCluster", Rationale: "fix", Diff: "diff",
		Files:  map[string]string{"controllers/cluster_controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifyPassed},
		Title:  "fix: test", Description: "safe description", Body: "body",
		Key:                "fix-analysis::id",
		Base:               ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
		RequireBaseCurrent: true,
	})
	first := NewService(exactAnalysisConfig(), dir, AIConfig{})
	token, err := first.stash("alice", &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind,
		targetRepo:          "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig:        fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()),
		verificationVersion: sourceVerificationVersion, fix: fix, analysisBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}

	second := NewService(exactAnalysisConfig(), dir, AIConfig{})
	entry, err := second.previewStore.take("alice", token)
	if err != nil {
		t.Fatal(err)
	}
	if entry.analysisBinding == nil || entry.analysisBinding.SourceBranch != "main" ||
		entry.analysisBinding.VerificationVersion != analysisSourceVerificationVersion {
		t.Fatalf("restored binding = %+v", entry.analysisBinding)
	}

	third := NewService(exactAnalysisConfig(), dir, AIConfig{})
	third.analysisPreviewValidator = rejectingAnalysisPreviewValidator{}
	token, err = third.stash("alice", &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind,
		targetRepo:          "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig:        fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()),
		verificationVersion: sourceVerificationVersion, fix: fix, analysisBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Confirm(t.Context(), token, "alice", "write-token"); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("changed chat confirmation error = %v", err)
	}
}

func TestValidateAnalysisPreviewBindsIdentityRepositoryBranchAndCurrentBase(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	service.analysisPreviewValidator = acceptingAnalysisPreviewValidator{}
	client := &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
	}
	service.sourceRevisionClient = client
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	binding := AnalysisPreviewBinding{
		Identity: exactIdentity(), AnalysisID: subject.ID, AnalysisHash: subject.ContentHash,
		AnalysisContentHash: subject.AnalysisContentHash,
		ChatSessionID:       "session", ChatRequestID: "request", ChatResponseHash: "chat",
		PreviewRequestHash: "preview",
		SourceRepository:   subject.SourceRepository, SourceBranch: "main",
		FailureRevision: subject.SourceRepository.Revision, GenerationBaseRevision: analysisFixRevision,
		VerificationVersion: analysisSourceVerificationVersion,
	}
	if err := service.validateAnalysisPreview(t.Context(), "alice", binding); err != nil {
		t.Fatalf("valid binding error = %v", err)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*AnalysisPreviewBinding)
	}{
		{name: "stale contract", mutate: func(b *AnalysisPreviewBinding) { b.VerificationVersion = 2 }},
		{name: "wrong source branch", mutate: func(b *AnalysisPreviewBinding) { b.SourceBranch = "release" }},
		{name: "wrong failure revision", mutate: func(b *AnalysisPreviewBinding) { b.FailureRevision = strings.Repeat("f", 40) }},
		{name: "wrong repository", mutate: func(b *AnalysisPreviewBinding) { b.SourceRepository.Name = "other" }},
		{name: "changed analysis", mutate: func(b *AnalysisPreviewBinding) { b.AnalysisContentHash = "changed" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := binding
			testCase.mutate(&changed)
			if err := service.validateAnalysisPreview(t.Context(), "alice", changed); !errors.Is(err, ErrPreviewTargetChanged) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	client.base.HeadSHA = capzGenerationBaseRevision
	if err := service.validateAnalysisPreview(t.Context(), "alice", binding); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("advanced base error = %v", err)
	}
}

func TestValidateAnalysisFixInputAllowsInvestigativeHypotheses(t *testing.T) {
	input := AnalysisFixInput{
		Identity: exactIdentity(), ChatSessionID: "session", ChatRequestID: "request",
		ChatResponseHash: "response", PreviewRequestHash: "preview", AnalysisContentHash: "analysis",
		SourceRepository: sourceinvestigation.Repository{
			Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision,
		},
		AssistantAnswer:     "Investigate whether reconciliation skips the terminal update.",
		AssistantUnverified: true, AssistantUnverifiedReason: "no artifact access",
	}
	if err := validateAnalysisFixInput(input); err != nil {
		t.Fatal(err)
	}
	input.FailureRevision = analysisFixRevision
	if err := validateAnalysisFixInput(input); err == nil {
		t.Fatal("partial source binding was accepted")
	}
	input.GenerationBaseRevision = analysisFixRevision
	input.SourceBranch = "main"
	if err := validateAnalysisFixInput(input); err != nil {
		t.Fatalf("complete source binding error = %v", err)
	}
}

func TestAnalysisQualityWarningsPreserveWeakEvidenceSignals(t *testing.T) {
	analysis := &models.AIAnalysis{Severity: "Transient-Ignore"}
	warnings := analysisQualityWarnings(analysis, AnalysisFixInput{
		AssistantAnswer:     "The selected hypothesis is nonempty.",
		AssistantUnverified: true, AssistantUnverifiedReason: "tool budget ended",
		ProposedRevision: &fixpr.RevisionContext{},
		EvidenceWarnings: []string{"citation line range was unavailable"},
	}, nil)
	for _, warning := range []string{
		analysisWarningCritique, analysisWarningSuggestedFix, analysisWarningRootCause,
		analysisWarningTransient, analysisWarningProse, analysisWarningEvidenceQualified,
		analysisWarningAssistantUnverified, analysisWarningNoCitations, analysisWarningNoSourceHints,
	} {
		if !slices.Contains(warnings, warning) {
			t.Fatalf("warnings=%v missing=%q", warnings, warning)
		}
	}
}

func TestBoundedAnalysisFailureText(t *testing.T) {
	value := strings.Repeat("界", maxAnalysisFailureTextBytes)
	got := boundedAnalysisFailureText(value)
	if len(got) > maxAnalysisFailureTextBytes || !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded failure text length=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

func TestPreviewAnalysisFixRejectsUnsafeMaintainerInstruction(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{
		base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
	}
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewAnalysisFix(t.Context(), AnalysisFixInput{
		Identity: exactIdentity(), ChatSessionID: "session", ChatRequestID: "request",
		ChatResponseHash: "chat-hash", PreviewRequestHash: "preview-hash",
		AnalysisContentHash: subject.AnalysisContentHash, SourceRepository: subject.SourceRepository,
		AssistantAnswer: "Investigate the reconciliation path.",
	}, "alice", "github-write-token", "Remove the conversion webhook before upgrade.")
	if !errors.Is(err, ErrPreviewRejected) || ReasonCodeOf(err) != ReasonUnsafeRemediation {
		t.Fatalf("unsafe instruction error = %v", err)
	}
}

func TestValidatedAnalysisPreviewRejectsDestructiveGeneratedPatch(t *testing.T) {
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "TestCluster", Rationale: "fix",
		Diff:   "--- a/controller.go\n+++ b/controller.go\n+Remove the conversion webhook before upgrade.\n",
		Files:  map[string]string{"controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifySkipped},
		Title:  "fix: test", Description: "safe description", Body: "safe body",
		Key:                "fix-analysis::id",
		Base:               ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"},
		RequireBaseCurrent: true,
	})
	_, err := validatedPreviewEntry(&previewEntry{
		kind: gfKind, fix: fix,
		analysisBinding: &AnalysisPreviewBinding{PreviewRequestHash: "request"},
	})
	if !errors.Is(err, ErrPreviewRejected) || ReasonCodeOf(err) != ReasonUnsafeRemediation {
		t.Fatalf("destructive patch error = %v", err)
	}
}
