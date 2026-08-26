package fetcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/analysispublisher"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type fakeShadowRunner struct {
	result            agentanalysis.WorkspaceSandboxResult
	err               error
	calls             int
	deadlineRemaining time.Duration
}

func (r *fakeShadowRunner) Analyze(ctx context.Context, _ agentanalysis.WorkspaceSandboxSpec) (agentanalysis.WorkspaceSandboxResult, error) {
	r.calls++
	if deadline, ok := ctx.Deadline(); ok {
		r.deadlineRemaining = time.Until(deadline)
	}
	return r.result, r.err
}
func (r *fakeShadowRunner) RuntimeIdentity() string { return strings.Repeat("9", 64) }

type fakeShadowPublisher struct {
	publishErr        error
	cleanupErr        error
	cleanupContextErr error
	publishDelay      time.Duration
}

func (p *fakeShadowPublisher) Publish(context.Context, agentanalysis.WorkspacePublishRequest, string) (analysispublisher.Result, error) {
	time.Sleep(p.publishDelay)
	return analysispublisher.Result{JobName: "publisher", PodName: "publisher-pod", Publication: agentanalysis.WorkspaceSourceModePreserve}, p.publishErr
}
func (p *fakeShadowPublisher) Cleanup(ctx context.Context, _ agentanalysis.WorkspaceCleanupRequest, _ string) (analysispublisher.Result, error) {
	p.cleanupContextErr = ctx.Err()
	return analysispublisher.Result{JobName: "cleanup", PodName: "cleanup-pod"}, p.cleanupErr
}

func shadowTestDetails(testNames ...string) []models.JobDetail {
	sha := strings.Repeat("a", 40)
	var cases []models.TestCase
	for _, name := range testNames {
		cases = append(cases, models.TestCase{
			Name: name, Status: "failed", FailureMessage: "failed request",
			AISummary:  &models.AISummary{Summary: "authoritative"},
			AIAnalysis: &models.AIAnalysis{Mode: ai.AgenticMode, RootCause: "cause", Severity: "High", SuggestedFix: "fix", CritiquePassed: true, Disposition: models.AnalysisDispositionGrounded},
		})
	}
	return []models.JobDetail{{
		Name: "job", JobID: "periodic::job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": sha}},
			TestCases: cases,
		}},
	}}
}

func shadowTestModelProvider() modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeDirect, API: modelprovider.APIChatCompletions,
		Endpoint: "https://models.invalid/v1/chat/completions", Model: "test-model",
		Auth: modelprovider.Auth{Type: modelprovider.AuthTypeBearer},
	})
}

func shadowTestPipeline(t *testing.T) *pipeline {
	t.Helper()
	out := filepath.Join(t.TempDir(), "public")
	set, _, err := skills.LoadForTools(t.TempDir(), []string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline{
		opts: Options{
			OutDir: out, EnableAI: true, AIMaxOutputTokens: 8192,
			ShadowAnalysis: ShadowAnalysisOptions{
				Enabled: true, LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), InputRoot: filepath.Join(t.TempDir(), "input"),
				MaxPerRun: 1, MaxSteps: 20, Timeout: time.Minute, ModelProvider: shadowTestModelProvider(), OutputLimitBytes: 64 << 10,
				ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true,
			},
		},
		cfg: &project.Config{AI: &project.AI{}, Storage: project.Storage{Provider: "gcs", Bucket: "bucket"}},
		aiProject: &analysisruntime.Project{
			Config: &project.Config{AI: &project.AI{}}, AnalysisSource: project.SourceRepo{Owner: "example", Name: "repo"},
			ConsumerPrompt: "Inspect this project.", SkillSet: set,
		},
		shadowClaim:     func(string, string, agentanalysis.ShadowRecord) (bool, error) { return true, nil },
		shadowPublisher: &fakeShadowPublisher{},
		shadowCleanup:   func(string, string, string) error { return nil },
		shadowNow:       func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
	}
	p.shadowPrepare = func(_ context.Context, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, _ agentanalysis.WorkspacePreparationOptions) (agentanalysis.WorkspacePreparedInput, error) {
		fileHash := sha256.Sum256([]byte("failure\n"))
		manifest, err := agentanalysis.NewWorkspaceManifestWithSkills(request, source, p.aiProject.ConsumerPrompt, set, []agentanalysis.WorkspaceFile{{Path: "build-log.txt", Size: 8, SHA256: hex.EncodeToString(fileHash[:])}})
		if err != nil {
			return agentanalysis.WorkspacePreparedInput{}, err
		}
		root := t.TempDir()
		return agentanalysis.WorkspacePreparedInput{Root: root, SourceRoot: filepath.Join(root, "source"), ArtifactRoot: filepath.Join(root, "artifacts"), SourceModePolicy: agentanalysis.WorkspaceSourceModePreserve, Manifest: manifest}, nil
	}
	return p
}

func successfulShadowResult() agentanalysis.WorkspaceSandboxResult {
	analysis := &agentanalysis.WorkspaceAnalysis{Summary: "shadow", RootCause: "grounded cause", Severity: "High", EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "failure"}}}
	return agentanalysis.WorkspaceSandboxResult{
		Execution: agentanalysis.WorkspaceExecutionResult{Analysis: analysis, TerminalState: agentruntime.TerminalSucceeded, Usage: agentanalysis.WorkspaceUsage{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, ModelRequests: 2}},
		Resources: agentruntime.ResourceMetadata{Namespace: "sandbox", Name: "analysis-1"},
		Telemetry: agentruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, FinalizationValid: true, CleanupCompleted: true},
	}
}

func TestRunShadowAnalysisNeverMutatesAuthoritativeDetails(t *testing.T) {
	tests := []struct {
		name          string
		result        agentanalysis.WorkspaceSandboxResult
		runErr        error
		prepareErr    error
		setupErr      error
		cleanupErr    error
		wantStatus    agentanalysis.ShadowStatus
		wantErrorCode string
	}{
		{name: "success", result: successfulShadowResult(), wantStatus: agentanalysis.ShadowStatusSucceeded},
		{name: "malformed", result: agentanalysis.WorkspaceSandboxResult{Telemetry: agentruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true}}, runErr: agentruntime.ErrMalformedResult, wantStatus: agentanalysis.ShadowStatusMalformedResult},
		{name: "timeout", runErr: context.DeadlineExceeded, wantStatus: agentanalysis.ShadowStatusTimeout},
		{name: "no result", result: agentanalysis.WorkspaceSandboxResult{Telemetry: agentruntime.GenerateTelemetry{TaskFinalized: true}}, wantStatus: agentanalysis.ShadowStatusNoResult},
		{name: "staging failure", result: agentanalysis.WorkspaceSandboxResult{Telemetry: agentruntime.GenerateTelemetry{TaskFinalized: true, FailurePhase: "staging", FailureCode: "source_untracked_files"}}, runErr: agentruntime.ErrStaging, wantStatus: agentanalysis.ShadowStatusRuntimeFailed, wantErrorCode: "source_untracked_files"},
		{name: "cleanup pending", result: func() agentanalysis.WorkspaceSandboxResult {
			value := successfulShadowResult()
			value.Telemetry.CleanupCompleted = false
			value.CleanupWork = &agentruntime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "analysis-1"}
			return value
		}(), runErr: agentruntime.ErrCleanupPending, wantStatus: agentanalysis.ShadowStatusCleanupPending},
		{name: "input cleanup pending", result: successfulShadowResult(), cleanupErr: errors.New("busy"), wantStatus: agentanalysis.ShadowStatusCleanupPending},
		{name: "prepare failed", prepareErr: errors.New("unavailable"), wantStatus: agentanalysis.ShadowStatusEvidenceFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := shadowTestPipeline(t)
			p.shadowRunner = &fakeShadowRunner{result: test.result, err: test.runErr}
			if test.prepareErr != nil {
				p.shadowPrepare = func(context.Context, ai.FailureAnalysisRequest, sourceinvestigation.Repository, agentanalysis.WorkspacePreparationOptions) (agentanalysis.WorkspacePreparedInput, error) {
					return agentanalysis.WorkspacePreparedInput{}, test.prepareErr
				}
			}
			p.shadowCleanup = func(string, string, string) error { return test.cleanupErr }
			publicSentinels := map[string][]byte{"dashboard.json": []byte("public-dashboard"), "manifest.json": []byte("public-manifest")}
			for name, data := range publicSentinels {
				if err := os.MkdirAll(p.opts.OutDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.opts.OutDir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			details := shadowTestDetails("TestFailure")
			before, _ := json.Marshal(details)
			var records []agentanalysis.ShadowRecord
			p.shadowAppend = func(_, _ string, record agentanalysis.ShadowRecord) error {
				records = append(records, record)
				return nil
			}
			p.runShadowAnalysis(t.Context(), &refreshResult{details: details})
			after, _ := json.Marshal(details)
			if string(before) != string(after) {
				t.Fatal("authoritative details changed")
			}
			if len(records) != 1 || records[0].Status != test.wantStatus {
				t.Fatalf("records = %+v", records)
			}
			if test.wantErrorCode != "" && records[0].ErrorCode != test.wantErrorCode {
				t.Fatalf("error code=%q want=%q", records[0].ErrorCode, test.wantErrorCode)
			}
			for name, want := range publicSentinels {
				got, err := os.ReadFile(filepath.Join(p.opts.OutDir, name))
				if err != nil || string(got) != string(want) {
					t.Fatalf("public sentinel %s changed", name)
				}
			}
		})
	}
}

func TestSelectShadowCandidatesIsDeterministicAndBounded(t *testing.T) {
	p := shadowTestPipeline(t)
	first := p.selectShadowCandidates(shadowTestDetails("TestB", "TestA"))
	second := p.selectShadowCandidates(shadowTestDetails("TestA", "TestB"))
	if len(first) != 2 || len(second) != 2 || first[0].subject != second[0].subject || first[1].subject != second[1].subject {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	details := shadowTestDetails("TestFailure")
	candidate := p.selectShadowCandidates(details)[0]
	candidate.request.Build.RepoRefs["example/repo"] = strings.Repeat("b", 40)
	if details[0].Runs[0].RepoRefs["example/repo"] != strings.Repeat("a", 40) {
		t.Fatal("candidate request aliases authoritative RepoRefs")
	}
}

func TestRunShadowAnalysisSkipsAttemptedIdentity(t *testing.T) {
	p := shadowTestPipeline(t)
	runner := &fakeShadowRunner{result: successfulShadowResult()}
	p.shadowRunner = runner
	p.shadowClaim = func(string, string, agentanalysis.ShadowRecord) (bool, error) { return false, nil }
	prepareCalls := 0
	original := p.shadowPrepare
	p.shadowPrepare = func(ctx context.Context, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, opts agentanalysis.WorkspacePreparationOptions) (agentanalysis.WorkspacePreparedInput, error) {
		prepareCalls++
		return original(ctx, request, source, opts)
	}
	appendCalls := 0
	p.shadowAppend = func(string, string, agentanalysis.ShadowRecord) error { appendCalls++; return nil }
	p.runShadowAnalysis(t.Context(), &refreshResult{details: shadowTestDetails("TestFailure")})
	if prepareCalls != 0 || runner.calls != 0 || appendCalls != 0 {
		t.Fatalf("prepare=%d runner=%d append=%d", prepareCalls, runner.calls, appendCalls)
	}
}

func TestRunShadowAnalysisAdvancesPastAttemptedCandidate(t *testing.T) {
	p := shadowTestPipeline(t)
	runner := &fakeShadowRunner{result: successfulShadowResult()}
	p.shadowRunner = runner
	claimCalls := 0
	p.shadowClaim = func(string, string, agentanalysis.ShadowRecord) (bool, error) {
		claimCalls++
		return claimCalls != 1, nil
	}
	appendCalls := 0
	p.shadowAppend = func(string, string, agentanalysis.ShadowRecord) error { appendCalls++; return nil }
	p.runShadowAnalysis(t.Context(), &refreshResult{details: shadowTestDetails("TestA", "TestB")})
	if claimCalls != 2 || runner.calls != 1 || appendCalls != 1 {
		t.Fatalf("claims=%d runner=%d append=%d", claimCalls, runner.calls, appendCalls)
	}
}

func TestValidateShadowAnalysisOptions(t *testing.T) {
	out := filepath.Join(t.TempDir(), "public")
	valid := Options{
		EnableAI: true, OutDir: out, AIMaxOutputTokens: 8192,
		ShadowAnalysis: ShadowAnalysisOptions{
			Enabled: true, LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), InputRoot: filepath.Join(t.TempDir(), "input"),
			MaxPerRun: 1, MaxSteps: 20, Timeout: time.Minute, ModelProvider: shadowTestModelProvider(), OutputLimitBytes: 64 << 10,
			ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true,
		},
	}
	if err := validateShadowAnalysisOptions(valid); err != nil {
		t.Fatal(err)
	}
	outputMismatch := valid
	outputMismatch.AIMaxOutputTokens = 4096
	if err := validateShadowAnalysisOptions(outputMismatch); err == nil || !strings.Contains(err.Error(), "explicit authoritative AI output cap") {
		t.Fatalf("output parity error = %v", err)
	}
	inside := valid
	inside.ShadowAnalysis.LedgerPath = filepath.Join(out, "analysis_shadow.json")
	if err := validateShadowAnalysisOptions(inside); err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("inside ledger error = %v", err)
	}
	insideInput := valid
	insideInput.ShadowAnalysis.InputRoot = filepath.Join(out, "analysis-input")
	if err := validateShadowAnalysisOptions(insideInput); err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("inside input error = %v", err)
	}
	badProvider := valid
	badProvider.ShadowAnalysis.ModelProvider.Endpoint = "http://models.invalid/v1/chat/completions"
	if err := validateShadowAnalysisOptions(badProvider); err == nil {
		t.Fatal("plaintext provider endpoint was accepted")
	}
}

func TestCleanupShadowInputsUsesFreshContextAfterAnalysisTimeout(t *testing.T) {
	p := shadowTestPipeline(t)
	publisher := &fakeShadowPublisher{}
	p.shadowPublisher = publisher
	p.shadowCleanup = func(string, string, string) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record := agentanalysis.ShadowRecord{ID: "shadow-timeout-cleanup"}
	prepared := agentanalysis.WorkspacePreparedInput{
		Root: t.TempDir(), Manifest: agentanalysis.WorkspaceManifest{Hash: strings.Repeat("a", 64)},
	}
	p.cleanupShadowInputs(ctx, &record, prepared, true)
	if publisher.cleanupContextErr != nil || !record.Provenance.InputCleanupCompleted || record.InputCleanupPending {
		t.Fatalf("context_err=%v provenance=%+v pending=%v", publisher.cleanupContextErr, record.Provenance, record.InputCleanupPending)
	}
}

func TestShadowAnalysisTimeoutStartsAfterPreparationAndPublication(t *testing.T) {
	p := shadowTestPipeline(t)
	p.opts.ShadowAnalysis.Timeout = 2 * time.Second
	runner := &fakeShadowRunner{result: successfulShadowResult()}
	p.shadowRunner = runner
	p.shadowPublisher = &fakeShadowPublisher{publishDelay: 100 * time.Millisecond}
	p.shadowAppend = func(string, string, agentanalysis.ShadowRecord) error { return nil }
	p.runShadowAnalysis(t.Context(), &refreshResult{details: shadowTestDetails("TestFailure")})
	if runner.deadlineRemaining < 66*time.Second {
		t.Fatalf("analysis deadline remaining = %v, want model timeout plus finalization grace", runner.deadlineRemaining)
	}
}

func TestValidateShadowProviderParity(t *testing.T) {
	shadow := shadowTestModelProvider()
	authoritative := project.AIProvider{API: shadow.API, Endpoint: shadow.Endpoint, Model: shadow.Model, ReasoningEffort: shadow.ReasoningEffort}
	if err := validateShadowProviderParity(authoritative, shadow); err != nil {
		t.Fatal(err)
	}
	authoritative.Model = "other"
	if err := validateShadowProviderParity(authoritative, shadow); err == nil {
		t.Fatal("provider mismatch was accepted")
	}
}

func TestSetupPipelineShadowRequiresAIToken(t *testing.T) {
	projectDir := t.TempDir()
	storageDir := t.TempDir()
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: repo
ai:
  endpoint: https://model.invalid/v1/chat/completions
  model: model
  tools: [filesystem]
`, storageDir)
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_TOKEN", "")
	out := filepath.Join(t.TempDir(), "public")
	_, err := setupPipeline(Options{
		ProjectDir: projectDir, OutDir: out, EnableAI: true, AIMaxOutputTokens: 8192,
		ShadowAnalysis: ShadowAnalysisOptions{
			Enabled: true, LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), InputRoot: filepath.Join(t.TempDir(), "input"),
			MaxPerRun: 1, MaxSteps: 20, Timeout: time.Minute, ModelProvider: shadowTestModelProvider(), OutputLimitBytes: 64 << 10,
			ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires AI_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}
