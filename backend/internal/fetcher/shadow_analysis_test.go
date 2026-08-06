package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeShadowRunner struct {
	result agentanalysis.Result
	err    error
	calls  int
}

func (r *fakeShadowRunner) Generate(_ context.Context, spec agentanalysis.Spec) (agentanalysis.Result, error) {
	r.calls++
	result := r.result
	if result.EvidenceHash == "" {
		result.EvidenceHash = spec.Bundle.Hash
	}
	return result, r.err
}

func shadowTestDetails(testNames ...string) []models.JobDetail {
	sha := strings.Repeat("a", 40)
	var cases []models.TestCase
	for _, name := range testNames {
		cases = append(cases, models.TestCase{
			Name: name, Status: "failed", FailureMessage: "failed request",
			AISummary: &models.AISummary{Summary: "authoritative"},
			AIAnalysis: &models.AIAnalysis{
				Mode: ai.AgenticMode, RootCause: "cause", Severity: "High", SuggestedFix: "fix", CritiquePassed: true,
			},
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

func shadowTestPipeline(t *testing.T) *pipeline {
	t.Helper()
	out := filepath.Join(t.TempDir(), "public")
	return &pipeline{
		opts: Options{
			OutDir: out, EnableAI: true, AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess},
			ShadowAnalysis: ShadowAnalysisOptions{
				Enabled: true, Namespace: "orka-system", ResultAPI: "https://orka.invalid", AgentRef: "agent", AgentVersion: "v1",
				LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), MaxPerRun: 1, MaxTurns: 12, Timeout: time.Minute,
			},
		},
		cfg: &project.Config{AI: &project.AI{}, Storage: project.Storage{Bucket: "bucket"}},
		aiProject: &analysisruntime.Project{
			Config: &project.Config{AI: &project.AI{}}, AnalysisSource: project.SourceRepo{Owner: "example", Name: "repo"},
		},
		shadowClaim: func(string, string, agentanalysis.ShadowRecord) (bool, error) { return true, nil },
		shadowNow:   func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
	}
}

func shadowTestBundle(t *testing.T, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository) agentanalysis.EvidenceBundle {
	t.Helper()
	bundle, err := agentanalysis.NewEvidenceBundle(
		request, source, agentanalysis.ArtifactScan{PathCount: 1}, nil,
		[]agentanalysis.EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "failure\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestRunShadowAnalysisNeverMutatesAuthoritativeDetails(t *testing.T) {
	tests := []struct {
		name       string
		runner     *fakeShadowRunner
		freezeErr  error
		appendErr  error
		wantStatus agentanalysis.ShadowStatus
	}{
		{name: "success", runner: &fakeShadowRunner{result: agentanalysis.Result{Analysis: agentanalysis.Analysis{Summary: "shadow"}}}, wantStatus: agentanalysis.ShadowStatusSucceeded},
		{name: "invalid", runner: &fakeShadowRunner{err: agentanalysis.ErrInvalidResult}, wantStatus: agentanalysis.ShadowStatusInvalidResult},
		{name: "runtime unavailable", runner: &fakeShadowRunner{err: agentruntime.ErrUnavailable}, wantStatus: agentanalysis.ShadowStatusRuntimeFailed},
		{name: "cancelled", runner: &fakeShadowRunner{err: context.DeadlineExceeded}, wantStatus: agentanalysis.ShadowStatusCancelled},
		{name: "cleanup pending", runner: &fakeShadowRunner{result: agentanalysis.Result{Analysis: agentanalysis.Analysis{Summary: "shadow"}, CleanupPending: true}, err: agentruntime.ErrCleanupPending}, wantStatus: agentanalysis.ShadowStatusCleanupPending},
		{name: "evidence failed", runner: &fakeShadowRunner{}, freezeErr: agentanalysis.ErrEvidenceUnavailable, wantStatus: agentanalysis.ShadowStatusEvidenceFailed},
		{name: "ledger write failed", runner: &fakeShadowRunner{result: agentanalysis.Result{Analysis: agentanalysis.Analysis{Summary: "shadow"}}}, appendErr: errors.New("disk unavailable"), wantStatus: agentanalysis.ShadowStatusSucceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := shadowTestPipeline(t)
			p.shadowRunner = test.runner
			details := shadowTestDetails("TestFailure")
			if err := os.MkdirAll(filepath.Join(p.opts.OutDir, "jobs"), 0o755); err != nil {
				t.Fatal(err)
			}
			publicSentinels := map[string][]byte{
				"dashboard.json": []byte(`{"jobs":[]}`),
				"ai_cache.json":  []byte(`{"private":true}`),
				"jobs/job.json":  []byte(`{"runs":[]}`),
			}
			for name, content := range publicSentinels {
				if err := os.WriteFile(filepath.Join(p.opts.OutDir, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(details)
			p.shadowFreeze = func(_ context.Context, _ artifacts.Browser, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, _ *skills.Set) (agentanalysis.EvidenceBundle, error) {
				if test.freezeErr != nil {
					return agentanalysis.EvidenceBundle{}, test.freezeErr
				}
				return shadowTestBundle(t, request, source), nil
			}
			var records []agentanalysis.ShadowRecord
			p.shadowAppend = func(_, _ string, record agentanalysis.ShadowRecord) error {
				records = append(records, record)
				return test.appendErr
			}
			p.runShadowAnalysis(t.Context(), &refreshResult{details: details})
			after, _ := json.Marshal(details)
			if string(before) != string(after) {
				t.Fatalf("authoritative details changed:\n%s\n%s", before, after)
			}
			if len(records) != 1 || records[0].Status != test.wantStatus {
				t.Fatalf("records = %+v", records)
			}
			for name, want := range publicSentinels {
				got, err := os.ReadFile(filepath.Join(p.opts.OutDir, name))
				if err != nil || string(got) != string(want) {
					t.Fatalf("public sentinel %s changed: %q error=%v", name, got, err)
				}
			}
		})
	}
}

func TestSelectShadowCandidatesIsDeterministicAndBounded(t *testing.T) {
	p := shadowTestPipeline(t)
	first := p.selectShadowCandidates(shadowTestDetails("TestB", "TestA"), models.FlakinessReport{})
	second := p.selectShadowCandidates(shadowTestDetails("TestA", "TestB"), models.FlakinessReport{})
	if len(first) != 2 || len(second) != 2 || first[0].subject != second[0].subject || first[1].subject != second[1].subject {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	details := shadowTestDetails("TestFailure")
	candidate := p.selectShadowCandidates(details, models.FlakinessReport{})[0]
	candidate.request.Build.RepoRefs["example/repo"] = strings.Repeat("b", 40)
	if details[0].Runs[0].RepoRefs["example/repo"] != strings.Repeat("a", 40) {
		t.Fatal("candidate request aliases authoritative RepoRefs")
	}
}

func TestRunShadowAnalysisSkipsAttemptedIdentity(t *testing.T) {
	p := shadowTestPipeline(t)
	runner := &fakeShadowRunner{result: agentanalysis.Result{Analysis: agentanalysis.Analysis{Summary: "shadow"}}}
	p.shadowRunner = runner
	p.shadowClaim = func(string, string, agentanalysis.ShadowRecord) (bool, error) { return false, nil }
	freezeCalls := 0
	p.shadowFreeze = func(context.Context, artifacts.Browser, ai.FailureAnalysisRequest, sourceinvestigation.Repository, *skills.Set) (agentanalysis.EvidenceBundle, error) {
		freezeCalls++
		return agentanalysis.EvidenceBundle{}, nil
	}
	appendCalls := 0
	p.shadowAppend = func(string, string, agentanalysis.ShadowRecord) error { appendCalls++; return nil }
	p.runShadowAnalysis(t.Context(), &refreshResult{details: shadowTestDetails("TestFailure")})
	if freezeCalls != 0 || runner.calls != 0 || appendCalls != 0 {
		t.Fatalf("freeze=%d runner=%d append=%d", freezeCalls, runner.calls, appendCalls)
	}
}

func TestRunShadowAnalysisAdvancesPastAttemptedCandidate(t *testing.T) {
	p := shadowTestPipeline(t)
	runner := &fakeShadowRunner{result: agentanalysis.Result{Analysis: agentanalysis.Analysis{Summary: "shadow"}}}
	p.shadowRunner = runner
	containsCalls := 0
	p.shadowClaim = func(string, string, agentanalysis.ShadowRecord) (bool, error) {
		containsCalls++
		return containsCalls != 1, nil
	}
	p.shadowFreeze = func(_ context.Context, _ artifacts.Browser, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, _ *skills.Set) (agentanalysis.EvidenceBundle, error) {
		return shadowTestBundle(t, request, source), nil
	}
	appendCalls := 0
	p.shadowAppend = func(string, string, agentanalysis.ShadowRecord) error { appendCalls++; return nil }
	p.runShadowAnalysis(t.Context(), &refreshResult{details: shadowTestDetails("TestA", "TestB")})
	if containsCalls != 2 || runner.calls != 1 || appendCalls != 1 {
		t.Fatalf("contains=%d runner=%d append=%d", containsCalls, runner.calls, appendCalls)
	}
}

func TestValidateShadowAnalysisOptions(t *testing.T) {
	out := filepath.Join(t.TempDir(), "public")
	valid := Options{
		EnableAI: true, OutDir: out, AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess},
		ShadowAnalysis: ShadowAnalysisOptions{
			Enabled: true, Namespace: "orka-system", ResultAPI: "https://orka.invalid", AgentRef: "agent", AgentVersion: "v1",
			LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), MaxPerRun: 1, MaxTurns: 12, Timeout: time.Minute,
		},
	}
	if err := validateAnalysisRuntimeOptions(valid); err != nil {
		t.Fatal(err)
	}
	inside := valid
	inside.ShadowAnalysis.LedgerPath = filepath.Join(out, "analysis_shadow.json")
	if err := validateAnalysisRuntimeOptions(inside); err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("inside ledger error = %v", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := t.TempDir()
	symlinkParent := filepath.Join(symlinkRoot, "private-link")
	if err := os.Symlink(out, symlinkParent); err != nil {
		t.Fatal(err)
	}
	symlinked := valid
	symlinked.ShadowAnalysis.LedgerPath = filepath.Join(symlinkParent, "analysis_shadow.json")
	if err := validateAnalysisRuntimeOptions(symlinked); err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("symlink ledger error = %v", err)
	}
	badAPI := valid
	badAPI.ShadowAnalysis.ResultAPI = "https://user:secret@orka.invalid?token=secret"
	if err := validateAnalysisRuntimeOptions(badAPI); err == nil || !strings.Contains(err.Error(), "absolute HTTP") {
		t.Fatalf("API error = %v", err)
	}
	container := valid
	container.AnalysisRuntime.Type = AnalysisRuntimeOrkaContainer
	if err := validateAnalysisRuntimeOptions(container); err == nil || !strings.Contains(err.Error(), "inprocess") {
		t.Fatalf("container error = %v", err)
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
		ProjectDir: projectDir, OutDir: out, EnableAI: true, AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess},
		ShadowAnalysis: ShadowAnalysisOptions{
			Enabled: true, Namespace: "orka-system", ResultAPI: "https://orka.invalid", AgentRef: "agent", AgentVersion: "v1",
			LedgerPath: filepath.Join(t.TempDir(), "private", "ledger.json"), MaxPerRun: 1, MaxTurns: 12, Timeout: time.Minute,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires AI_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}
