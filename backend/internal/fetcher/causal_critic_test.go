package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalcritic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeCausalCriticReviewer struct{ calls int }

func (f *fakeCausalCriticReviewer) RuntimeIdentity() string { return strings.Repeat("d", 64) }

func (f *fakeCausalCriticReviewer) Review(_ context.Context, input causalcritic.Input, _ string, _ engineruntime.WorkObserver) (causalcritic.Result, error) {
	f.calls++
	review := causalcritic.Review{
		SchemaVersion: causalcritic.ReviewSchemaVersion, ContractVersion: causalcritic.ContractVersion,
		PairHash: input.PairHash, Verdict: "pass", Findings: []causalcritic.Finding{}, Confidence: "medium",
	}
	return causalcritic.Result{
		Execution: causalcritic.ExecutionResult{Review: &review, Usage: causalcritic.GatewayUsage{Status: "reported", Source: "gateway_response", Model: "critic", InputTokens: 10, OutputTokens: 2}},
		Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, FinalizationChecked: true, FinalizationValid: true, CleanupCompleted: true},
	}, nil
}

func TestRunCausalCriticPersistsPrivateNonAuthoritativeReview(t *testing.T) {
	p := shadowTestPipeline(t)
	root := t.TempDir()
	p.opts.OutDir = filepath.Join(root, "public")
	p.opts.CausalCritic = CausalCriticOptions{
		Enabled: true, LedgerPath: filepath.Join(root, "private", "critic.json"), MaxPerRun: 1,
		Timeout: time.Minute, OutputLimitBytes: causalcritic.DefaultOutputLimit,
		ModelGateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic", ProtocolVersion: "openai-chat-completions-v1"},
	}
	reviewer := &fakeCausalCriticReviewer{}
	p.criticReviewer = reviewer
	p.criticNow = func() time.Time { return time.Unix(100, 0) }
	freezeCalls := 0
	p.criticFreeze = func(_ context.Context, _ artifacts.Browser, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, _ *skills.Set) (agentanalysis.EvidenceBundle, error) {
		freezeCalls++
		return shadowTestBundle(t, request, source), nil
	}
	details := shadowTestDetails("TestFailure")
	before, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.opts.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(p.opts.OutDir, "dashboard.json")
	if err := os.WriteFile(publicPath, []byte("public-sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.runCausalCritic(t.Context(), &refreshResult{details: details})
	p.runCausalCritic(t.Context(), &refreshResult{details: details})
	after, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || reviewer.calls != 1 || freezeCalls != 1 {
		t.Fatalf("authoritative details changed or duplicate reran: before=%s after=%s reviewer=%d freeze=%d", before, after, reviewer.calls, freezeCalls)
	}
	if got, err := os.ReadFile(publicPath); err != nil || string(got) != "public-sentinel" {
		t.Fatalf("public output changed: %q err=%v", got, err)
	}
	ledger, err := os.ReadFile(p.opts.CausalCritic.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "succeeded"`, `"finalized": true`, `"pair_hash"`} {
		if !strings.Contains(string(ledger), want) {
			t.Fatalf("ledger missing %s: %s", want, ledger)
		}
	}
}

func TestValidateCausalCriticOptions(t *testing.T) {
	root := t.TempDir()
	valid := Options{
		EnableAI: true, OutDir: filepath.Join(root, "public"), AnalysisRuntime: AnalysisRuntimeOptions{Type: AnalysisRuntimeInProcess},
		CausalCritic: CausalCriticOptions{
			Enabled: true, LedgerPath: filepath.Join(root, "private", "critic.json"), MaxPerRun: 1,
			Timeout: time.Minute, OutputLimitBytes: causalcritic.DefaultOutputLimit,
			ModelGateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic", ProtocolVersion: "openai-chat-completions-v1"},
		},
	}
	if err := validateAnalysisRuntimeOptions(valid); err != nil {
		t.Fatal(err)
	}
	inside := valid
	inside.CausalCritic.LedgerPath = filepath.Join(valid.OutDir, "critic.json")
	if err := validateAnalysisRuntimeOptions(inside); err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("inside error = %v", err)
	}
	direct := valid
	direct.CausalCritic.ModelGateway.Endpoint = "https://api.openai.com/v1"
	if err := validateAnalysisRuntimeOptions(direct); err == nil || !strings.Contains(err.Error(), "internal service DNS") {
		t.Fatalf("direct provider error = %v", err)
	}
	orka := valid
	orka.ShadowAnalysis.Enabled = true
	if err := validateCausalCriticOptions(orka); err == nil || !strings.Contains(err.Error(), "cannot run") {
		t.Fatalf("dual shadow error = %v", err)
	}
}

func TestRunCausalCriticCandidateDoesNotConsumeQuotaBeforeClaim(t *testing.T) {
	p := shadowTestPipeline(t)
	root := t.TempDir()
	p.opts.OutDir = filepath.Join(root, "public")
	p.opts.CausalCritic = CausalCriticOptions{
		Enabled: true, LedgerPath: filepath.Join(root, "private", "critic.json"), MaxPerRun: 1,
		Timeout: time.Minute, OutputLimitBytes: causalcritic.DefaultOutputLimit,
	}
	p.criticReviewer = &fakeCausalCriticReviewer{}
	p.criticNow = func() time.Time { return time.Unix(100, 0) }
	freezeCalls := 0
	p.criticFreeze = func(context.Context, artifacts.Browser, ai.FailureAnalysisRequest, sourceinvestigation.Repository, *skills.Set) (agentanalysis.EvidenceBundle, error) {
		freezeCalls++
		return agentanalysis.EvidenceBundle{}, errors.New("fixture evidence failure")
	}
	candidate := p.selectShadowCandidates(shadowTestDetails("TestFailure"), models.FlakinessReport{})[0]
	if p.runCausalCriticCandidate(t.Context(), candidate) {
		t.Fatal("evidence failure consumed critic quota")
	}
	if p.runCausalCriticCandidate(t.Context(), candidate) {
		t.Fatal("duplicate evidence failure consumed critic quota")
	}
	if freezeCalls != 1 {
		t.Fatalf("evidence freezes=%d, want 1", freezeCalls)
	}
}

func TestCausalCriticCaseIdentityBoundsLongTestNames(t *testing.T) {
	subject := agentanalysis.Subject{JobID: "periodic::job", BuildID: "1", TestName: strings.Repeat("long-test-name", 20)}
	caseID, stableID := causalCriticCaseIdentity(subject)
	if caseID != "critic-"+stableID || len(caseID) > 160 || len(stableID) != 20 {
		t.Fatalf("caseID=%q stableID=%q", caseID, stableID)
	}
}
