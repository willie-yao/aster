package actions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const analysisFixRevision = "0123456789abcdef0123456789abcdef01234567"

func exactJUnitDetail() models.JobDetail {
	return models.JobDetail{Name: "periodic-capz", JobID: "periodic-capz", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{
			BuildID: "123", JobName: "periodic-capz", RepoRefs: map[string]string{"kubernetes-sigs/cluster-api-provider-azure": analysisFixRevision},
		},
		TestCases: []models.TestCase{{
			Name: "TestCluster", SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit_01.xml", Status: "failed",
			FailureMessage: "cluster failed", FailureBody: "expected Ready",
			AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-08-13T01:00:00Z", Mode: ai.AgenticMode, CritiquePassed: true, CritiqueVersion: ai.CurrentCritiqueVersion(),
				RootCause: "The reconciler omitted the terminal state.", Severity: "High", SuggestedFix: "Update the reconciler branch.",
				RelevantFiles:     []string{"controllers/cluster_controller.go"},
				EvidenceCitations: []models.EvidenceCitation{{Path: "artifacts/junit_01.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
				FileLinks: map[string]string{
					"controllers/cluster_controller.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + analysisFixRevision + "/controllers/cluster_controller.go",
				},
			},
		}},
	}}}
}

func exactAnalysisConfig() *project.Config {
	return &project.Config{
		Name: "capz", Branding: project.Branding{SourceRepo: project.SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled:      true,
			Repo:         &project.SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"},
			AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"},
		}},
	}
}

func exactIdentity() AnalysisIdentity {
	return AnalysisIdentity{
		Project: "capz", JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster", SuiteName: "CAPZ", ClassName: "e2e",
		JUnitFile: "junit_01.xml", AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}
}

func TestResolveAnalysisActionSubjectEligibility(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*models.JobDetail)
		ok     bool
	}{
		{name: "eligible", ok: true},
		{name: "passing", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].Status = "passed" }},
		{name: "skipped", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].Status = "skipped" }},
		{name: "unavailable", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].AIAnalysis = nil }},
		{name: "rejected", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].AIAnalysis.CritiquePassed = false }},
		{name: "transient", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].AIAnalysis.Severity = "Transient-Ignore" }},
		{name: "build failure", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].Source = models.TestCaseSourceBuild }},
		{name: "missing junit", mutate: func(d *models.JobDetail) { d.Runs[0].TestCases[0].JUnitFile = "" }},
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
				if err != nil || !strings.HasPrefix(subject.ID, "analysis::") || subject.ContentHash == "" || subject.Identity.Project != "capz" {
					t.Fatalf("subject=%+v err=%v", subject, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ineligible subject = %+v", subject)
			}
		})
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

type mapSourceReader struct {
	files map[string]string
}

func (r *mapSourceReader) ReadFile(_ context.Context, file string) (string, bool, error) {
	content, ok := r.files[file]
	return content, ok, nil
}

func (r *mapSourceReader) ReadSourceArchive(context.Context) (actionverify.Archive, error) {
	archive := actionverify.Archive{Paths: map[string]bool{}, GoFiles: map[string]string{}, Files: map[string]string{}}
	for file, content := range r.files {
		archive.Paths[file] = true
		if strings.HasSuffix(file, ".go") {
			archive.GoFiles[file] = content
		} else {
			archive.Files[file] = content
		}
	}
	return archive, nil
}

func TestAnalysisSourceSnapshotIdentityDetectsDrift(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	reader := &mapSourceReader{files: map[string]string{"controllers/cluster_controller.go": "package controllers\nfunc reconcile() { markReady() }\n"}}
	service.sourceReaderFactory = func(sourceinvestigation.Repository) sourceSnapshotReader { return reader }
	repo := sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision}
	first, err := service.verifyAnalysisSourceSnapshot(t.Context(), repo, []string{"controllers/cluster_controller.go"})
	if err != nil {
		t.Fatal(err)
	}
	reader.files["controllers/cluster_controller.go"] = "package controllers\n// changed\n"
	second, err := service.verifyAnalysisSourceSnapshot(t.Context(), repo, []string{"controllers/cluster_controller.go"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("source verification identity did not change")
	}
	if _, err := service.verifyAnalysisSourceSnapshot(t.Context(), repo, []string{"../secret"}); err == nil {
		t.Fatal("unsafe source path was accepted")
	}
}

type rejectingAnalysisPreviewValidator struct{}

func (rejectingAnalysisPreviewValidator) ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error {
	return errors.New("chat response changed")
}

func TestAnalysisPreviewBindingSurvivesRestartAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	first := NewService(exactAnalysisConfig(), dir, AIConfig{})
	binding := &AnalysisPreviewBinding{
		Identity: exactIdentity(), AnalysisID: "analysis::id", AnalysisHash: "analysis-hash",
		ChatSessionID: "session", ChatRequestID: "request", ChatResponseHash: "chat-hash", VerificationVersion: analysisSourceVerificationVersion,
		SourceRepository: sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision},
		SourceFiles:      []string{"controllers/cluster_controller.go"}, SourceVerification: "source-hash",
	}
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "TestCluster", Rationale: "fix", Diff: "diff", Files: map[string]string{"controllers/cluster_controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifyPassed}, Title: "fix: test", Description: "safe description", Body: "body",
		Key: "fix-analysis::id", Base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"}, RequireBaseCurrent: true,
	})
	token, err := first.stash("alice", &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind, targetRepo: "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig: fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()), verificationVersion: sourceVerificationVersion,
		fix: fix, analysisBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := NewService(exactAnalysisConfig(), dir, AIConfig{})
	entry, err := second.take("alice", token)
	if err != nil {
		t.Fatal(err)
	}
	if entry.analysisBinding == nil || entry.analysisBinding.ChatResponseHash != "chat-hash" ||
		!slices.Equal(entry.analysisBinding.SourceFiles, []string{"controllers/cluster_controller.go"}) {
		t.Fatalf("restored binding = %+v", entry.analysisBinding)
	}

	third := NewService(exactAnalysisConfig(), dir, AIConfig{})
	third.analysisPreviewValidator = rejectingAnalysisPreviewValidator{}
	token, err = third.stash("alice", &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind, targetRepo: "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig: fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()), verificationVersion: sourceVerificationVersion,
		fix: fix, analysisBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Confirm(t.Context(), token, "alice", "write-token"); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("changed chat confirmation error = %v", err)
	}
}

func TestAnalysisPreviewIdempotencyDoesNotDuplicatePersistedPreview(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "TestCluster", Rationale: "fix", Diff: "diff", Files: map[string]string{"controllers/cluster_controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifyPassed}, Title: "fix: test", Description: "safe description", Body: "body",
		Key: "fix-analysis::id", Base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"}, RequireBaseCurrent: true,
	})
	entry := &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind, targetRepo: "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig: fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()), verificationVersion: sourceVerificationVersion,
		fix: fix, analysisBinding: &AnalysisPreviewBinding{PreviewRequestHash: "request-hash"},
	}
	first, err := service.previewStore.stashIdempotent("alice", "request-hash", entry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.previewStore.stashIdempotent("alice", "request-hash", entry)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent tokens differ: %q != %q", first, second)
	}
	state, _, err := service.previewStore.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Previews) != 1 {
		t.Fatalf("persisted previews = %d", len(state.Previews))
	}
}

type acceptingAnalysisPreviewValidator struct{}

func (acceptingAnalysisPreviewValidator) ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error {
	return nil
}

func TestValidateAnalysisPreviewRejectsWrongOrAmbiguousSourceIdentity(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	service.analysisPreviewValidator = acceptingAnalysisPreviewValidator{}
	reader := &mapSourceReader{files: map[string]string{
		"controllers/cluster_controller.go": "package controllers\nfunc markReady() {}\nfunc reconcile() {}\n",
	}}
	service.sourceReaderFactory = func(sourceinvestigation.Repository) sourceSnapshotReader { return reader }
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	repo := sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision}
	verification, err := service.verifyAnalysisSourceSnapshot(t.Context(), repo, []string{"controllers/cluster_controller.go"})
	if err != nil {
		t.Fatal(err)
	}
	findingText := "Update `markReady` in the terminal branch."
	findingVerification, err := service.verifyAnalysisFinding(t.Context(), repo, []string{"controllers/cluster_controller.go"}, findingText)
	if err != nil {
		t.Fatal(err)
	}
	binding := AnalysisPreviewBinding{
		Identity: exactIdentity(), AnalysisID: subject.ID, AnalysisHash: subject.ContentHash, AnalysisContentHash: subject.AnalysisContentHash,
		ChatSessionID: "session", ChatRequestID: "request", ChatResponseHash: "chat", PreviewRequestHash: "preview",
		SourceRepository: repo, SourceFiles: []string{"controllers/cluster_controller.go"}, SourceVerification: verification,
		FindingText: findingText, FindingVerification: findingVerification, VerificationVersion: analysisSourceVerificationVersion,
	}
	if err := service.validateAnalysisPreview(t.Context(), "alice", binding); err != nil {
		t.Fatalf("valid binding error = %v", err)
	}
	changedFinding := binding
	changedFinding.FindingVerification = "changed"
	if err := service.validateAnalysisPreview(t.Context(), "alice", changedFinding); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("changed finding verification error = %v", err)
	}
	reader.files["controllers/cluster_controller.go"] += "// drift\n"
	if err := service.validateAnalysisPreview(t.Context(), "alice", binding); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("source drift error = %v", err)
	}
	reader.files["controllers/cluster_controller.go"] = "package controllers\nfunc markReady() {}\nfunc reconcile() {}\n"
	wrongRepo := binding
	wrongRepo.SourceRepository.Name = "wrong-repo"
	if err := service.validateAnalysisPreview(t.Context(), "alice", wrongRepo); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("wrong repository error = %v", err)
	}
	wrongRevision := binding
	wrongRevision.SourceRepository.Revision = strings.Repeat("f", 40)
	if err := service.validateAnalysisPreview(t.Context(), "alice", wrongRevision); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("wrong revision error = %v", err)
	}
	detail.Runs[0].RepoRefs["kubernetes-sigs/cluster-api-provider-azure"] = "main:" + analysisFixRevision + ",pull:" + strings.Repeat("e", 40)
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	if err := service.validateAnalysisPreview(t.Context(), "alice", binding); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("ambiguous source error = %v", err)
	}
}

func TestVerifyAnalysisFindingRequiresDeterministicSourceProof(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	repo := sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision}
	reader := &mapSourceReader{files: map[string]string{
		"controllers/cluster_controller.go": "package controllers\nfunc markReady() {}\nfunc reconcile() {}\n",
	}}
	service.sourceReaderFactory = func(sourceinvestigation.Repository) sourceSnapshotReader { return reader }
	if _, err := service.verifyAnalysisFinding(t.Context(), repo, []string{"controllers/cluster_controller.go"}, "Change the terminal branch."); err == nil {
		t.Fatal("finding without an explicit source symbol was accepted")
	}
	first, err := service.verifyAnalysisFinding(t.Context(), repo, []string{"controllers/cluster_controller.go"}, "Update `markReady` in the terminal branch.")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.verifyAnalysisFinding(t.Context(), repo, []string{"controllers/cluster_controller.go"}, "Update `markReady` before returning.")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changed source finding retained the same verification identity")
	}
}

func TestPreviewAnalysisFixRejectsUnverifiedGenericFindingBeforeGeneration(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	reader := &mapSourceReader{files: map[string]string{
		"controllers/cluster_controller.go": "package controllers\nfunc reconcileDelete() {}\n",
	}}
	service.sourceReaderFactory = func(sourceinvestigation.Repository) sourceSnapshotReader { return reader }
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewAnalysisFix(t.Context(), AnalysisFixInput{
		Identity: exactIdentity(), ChatSessionID: "session", ChatRequestID: "request", ChatResponseHash: "chat-hash",
		PreviewRequestHash: "preview-hash", AnalysisContentHash: subject.AnalysisContentHash,
		SourceRepository:  sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision},
		AssistantAnswer:   "The terminal branch should record readiness.",
		ArtifactCitations: []fixpr.Evidence{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, "alice", "github-write-token", "")
	if !errors.Is(err, ErrPreviewRejected) || !strings.Contains(err.Error(), "selected source finding") {
		t.Fatalf("generic finding error = %v", err)
	}
}

func TestAnalysisPreviewReservationPreventsDuplicateGeneration(t *testing.T) {
	service := NewService(exactAnalysisConfig(), t.TempDir(), AIConfig{})
	firstToken, existing, acquired, err := service.previewStore.reserveIdempotent("alice", "request-hash", "generation-hash", time.Minute)
	if err != nil || !acquired || existing != nil {
		t.Fatalf("first reservation token=%q existing=%+v acquired=%t err=%v", firstToken, existing, acquired, err)
	}
	if _, _, _, err := service.previewStore.reserveIdempotent("alice", "request-hash", "generation-hash", time.Minute); !errors.Is(err, ErrPreviewPending) {
		t.Fatalf("concurrent reservation error = %v", err)
	}
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: "TestCluster", Rationale: "fix", Diff: "diff", Files: map[string]string{"controllers/cluster_controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifyPassed}, Title: "fix: test", Description: "safe description", Body: "body",
		Key: "fix-analysis::id", Base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"}, RequireBaseCurrent: true,
	})
	entry := &previewEntry{
		failureID: "analysis::id", patternHash: "analysis-hash", kind: gfKind, targetRepo: "kubernetes-sigs/cluster-api-provider-azure",
		targetConfig: fixTargetFingerprint(exactAnalysisConfig().EffectiveFixPRs()), verificationVersion: sourceVerificationVersion,
		fix: fix, analysisBinding: &AnalysisPreviewBinding{PreviewRequestHash: "request-hash"},
	}
	if err := service.previewStore.completeIdempotent("alice", firstToken, "request-hash", "generation-hash", entry); err != nil {
		t.Fatal(err)
	}
	secondToken, existing, acquired, err := service.previewStore.reserveIdempotent("alice", "request-hash", "generation-hash", time.Minute)
	if err != nil || acquired || existing == nil || secondToken != firstToken {
		t.Fatalf("completed reservation token=%q existing=%+v acquired=%t err=%v", secondToken, existing, acquired, err)
	}
	if _, _, _, err := service.previewStore.reserveIdempotent("alice", "request-hash", "changed-generation", time.Minute); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("changed generation identity error = %v", err)
	}
}

func TestPreviewAnalysisFixRequiresEnabledFixPRFeature(t *testing.T) {
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	cfg := exactAnalysisConfig()
	cfg.AI.FixPRs.Enabled = false
	service := NewService(cfg, dir, AIConfig{})
	subject, err := service.ResolveAnalysisActionSubject(exactIdentity())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewAnalysisFix(t.Context(), AnalysisFixInput{
		Identity: exactIdentity(), ChatSessionID: "session", ChatRequestID: "request", ChatResponseHash: "chat-hash",
		PreviewRequestHash: "preview-hash", AnalysisContentHash: subject.AnalysisContentHash,
		SourceRepository:  sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: analysisFixRevision},
		AssistantAnswer:   "Update `reconcileDelete`.",
		ArtifactCitations: []fixpr.Evidence{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, "alice", "github-write-token", "")
	if !errors.Is(err, ErrPreviewRejected) || !strings.Contains(err.Error(), "Agent Sandbox") {
		t.Fatalf("disabled fix feature error = %v", err)
	}
}
