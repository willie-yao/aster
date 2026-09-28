package pr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

func testGatewayProvider(endpoint, model string) modelprovider.Config {
	return modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway,
		API:            modelprovider.APIChatCompletions,
		Endpoint:       endpoint,
		Model:          model,
		Auth:           modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
}

// fakeAgentRuntime is a stand-in AgentRuntime that returns canned results and
// records the spec it was called with.
type fakeAgentRuntime struct {
	res   runtime.ExecutionResult
	err   error
	spec  runtime.GenerateSpec
	calls int
}

func TestManagerGenerateMarksModelGatewayExclusion(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, operation := aiusage.Begin(t.Context(), recorder, aiusage.Metadata{
		LogicalID: "fix", Origin: aiusage.OriginServer, Feature: aiusage.FeatureFixPreview, StartedAt: now,
	})
	manager := &Manager{opts: Options{
		SourceOwner: "o", SourceName: "r", MaxFiles: 3,
		Agent: &AgentConfig{Runtime: goodAgent(), MaxFiles: 3, ModelProvider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "gateway-model")},
	}}
	if _, err := manager.generate(ctx, systemicPattern("etcd"), "ref", "", nil); err != nil {
		t.Fatal(err)
	}
	got := operation.Finish(aiusage.OutcomeSuccess)
	if !got.ModelGatewayExcluded || got.ExternalUnmetered || got.Model != "gateway-model" || got.UsageSource != aiusage.UsageSourceModelGateway {
		t.Fatalf("operation = %+v", got)
	}
}

func (f *fakeAgentRuntime) Generate(_ context.Context, spec runtime.GenerateSpec) (runtime.ExecutionResult, error) {
	f.calls++
	f.spec = spec
	return f.res, f.err
}

func agentGenParams(ar *AgentConfig) genParams {
	return genParams{owner: "o", repo: "r", ref: "ref", maxFiles: 3, agent: ar}
}

// goodAgent returns a fake agent runtime that proposes a canned single-file fix,
// used by the Reconcile tests to drive the (only) generation path.
func goodAgent() *fakeAgentRuntime {
	return &fakeAgentRuntime{res: runtime.ExecutionResult{
		Files: map[string]string{"templates/cluster.yaml": strings.Replace(sampleFile, "StandardSSD_LRS", "Premium_LRS", 1)},
		Diff:  "--- a/templates/cluster.yaml\n+++ b/templates/cluster.yaml\n@@\n-  diskType: StandardSSD_LRS\n+  diskType: Premium_LRS\n",
	}}
}

func TestGenerateWithAgent_HappyPath(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		Files: map[string]string{"templates/cluster.yaml": "diskType: Premium_LRS\n"},
		Diff:  "--- a/templates/cluster.yaml\n+++ b/templates/cluster.yaml\n",
	}}
	observer := func(context.Context, runtime.WorkRef) error { return nil }
	gp := agentGenParams(&AgentConfig{Runtime: fa, SharedModelEndpoint: true, Model: "m", Endpoint: "e", ModelToken: "t", MaxFiles: 3, OutputLimitBytes: 131072, ModelProvider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture"), AllowBash: true, NetworkDomains: []string{"registry.example.test:443"}, CommandPolicy: runtime.CommandPolicy{Commands: []runtime.ExecutionCommand{{Argv: []string{"go", "test", "./..."}}}}, ExecutionID: "request-1", WorkObserver: observer})

	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatalf("generateWithAgent: %v", err)
	}
	if fix.files["templates/cluster.yaml"] != "diskType: Premium_LRS\n" {
		t.Errorf("changed file not carried through: %v", fix.files)
	}
	if fix.diff == "" {
		t.Error("expected the agent diff to carry through")
	}
	// The instruction must convey the root cause and the source repo/ref.
	if !strings.Contains(fa.spec.Instruction, "etcd disk too slow") {
		t.Errorf("instruction missing root cause: %q", fa.spec.Instruction)
	}
	if fa.spec.Repo.Owner != "o" || fa.spec.Repo.Ref != "ref" {
		t.Errorf("repo not passed: %+v", fa.spec.Repo)
	}
	if fa.spec.Model != "m" || fa.spec.Endpoint != "e" || fa.spec.Token != "t" {
		t.Errorf("model config not passed: %+v", fa.spec)
	}
	if len(fa.spec.NetworkDomains) != 1 || fa.spec.NetworkDomains[0] != "registry.example.test:443" {
		t.Errorf("network domains not passed: %+v", fa.spec.NetworkDomains)
	}
	if fa.spec.ExecutionID != "request-1" || fa.spec.WorkObserver == nil {
		t.Errorf("runtime work identity not passed: %+v", fa.spec)
	}
	if fa.spec.ExpectedBaseSHA != "ref" || fa.spec.MaxSteps != fa.spec.MaxTurns || fa.spec.MaxFiles != 3 || fa.spec.OutputLimitBytes != 131072 || fa.spec.ModelProvider.Model != "fixture" || !fa.spec.CommandPolicy.AllowShell || len(fa.spec.CommandPolicy.Commands) != 1 {
		t.Errorf("provider-neutral execution policy not passed: %+v", fa.spec)
	}
}

func TestAgentRuntimeSpecOmitsProviderPolicyForAgentOwnedEndpoint(t *testing.T) {
	observer := func(context.Context, runtime.WorkRef) error { return nil }
	spec := agentRuntimeSpec(&AgentConfig{
		Model: "model", Endpoint: "https://model.example.test/v1", ModelToken: "secret",
		NetworkDomains: []string{"model.example.test:443"}, ExecutionID: "request-1", WorkObserver: observer,
	}, runtime.RepoRef{Owner: "o", Name: "r", Ref: "ref", Token: "git-token"}, "fix")
	if spec.Model != "" || spec.Endpoint != "" || spec.Token != "" || len(spec.NetworkDomains) != 0 {
		t.Fatalf("agent-owned provider policy leaked into runtime spec: %+v", spec)
	}
	if spec.Repo.Token != "git-token" || spec.ExecutionID != "request-1" || spec.WorkObserver == nil {
		t.Fatalf("repository and work identity were not preserved: %+v", spec)
	}
}

func TestGenerateWithAgent_NoChangeIsNotFixable(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{Files: map[string]string{}}}
	_, err := generateWithAgent(context.Background(), agentGenParams(&AgentConfig{Runtime: fa}), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "no repository change") {
		t.Errorf("expected a not-auto-fixable error, got %v", err)
	}
}

func TestGenerateWithAgent_RejectsTooManyFiles(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{Files: map[string]string{
		"a": "1", "b": "2", "c": "3", "d": "4",
	}}}
	gp := agentGenParams(&AgentConfig{Runtime: fa}) // maxFiles 3
	_, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "exceeding max_files") {
		t.Errorf("expected a max_files rejection, got %v", err)
	}
}

func TestGenerateWithAgent_ValidationFailureIsOneShotAndNotActionable(t *testing.T) {
	fa := &fakeAgentRuntime{
		res: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureReason: "validation command 1 failed"},
		err: errors.New("validation command 1 failed"),
	}
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = &fakeCompleter{critique: `{"issues":["retry"]}`}
	gp.critiqueRetries = 3
	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "validation command 1 failed") {
		t.Fatalf("fix=%v error=%v", fix, err)
	}
	if fix != nil || fa.calls != 1 {
		t.Fatalf("actionable fix=%v runtime calls=%d", fix, fa.calls)
	}
}

func TestGenerateWithAgentWarnsOnCompletedFailedCommandResults(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].ExitCode = 1
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		BaseSHA: "ref", Files: map[string]string{"a.yaml": "fixed\n"}, Diff: "diff", CommandResults: results,
	}}
	gp := agentGenParams(&AgentConfig{
		Runtime: fa, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands},
	})
	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if fa.calls != 1 || fix.executionVerification.verifyResult().Status != VerifyFailed || !slices.Contains(fix.warnings, patchVerifyWarning) {
		t.Fatalf("calls=%d verify=%+v warnings=%v", fa.calls, fix.executionVerification.verifyResult(), fix.warnings)
	}
}

func TestGenerateWithAgent_UnavailableSurfaces(t *testing.T) {
	// Wrap the sentinel so errors.Is matches through the fix/pr error.
	fa := &fakeAgentRuntime{err: errWrap{runtime.ErrUnavailable}}
	_, err := generateWithAgent(context.Background(), agentGenParams(&AgentConfig{Runtime: fa}), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("expected an unavailable error, got %v", err)
	}
}

func TestGenerateWithAgent_SandboxUnavailableSurfaces(t *testing.T) {
	fa := &fakeAgentRuntime{err: errWrap{runtime.ErrSandboxUnavailable}}
	_, err := generateWithAgent(context.Background(), agentGenParams(&AgentConfig{Runtime: fa}), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("expected a sandbox-unavailable error, got %v", err)
	}
}

type errWrap struct{ err error }

func (e errWrap) Error() string { return "agent: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

func TestGenerateWithAgent_CritiqueApproves(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		Files: map[string]string{"a.yaml": "fixed\n"}, Diff: "diff",
	}}
	rev := &fakeCompleter{} // empty critique -> approved
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = rev
	gp.critiqueRetries = 1

	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatalf("generateWithAgent: %v", err)
	}
	if fix.files["a.yaml"] != "fixed\n" {
		t.Errorf("fix not returned: %v", fix.files)
	}
}

func TestGenerateWithAgent_CritiqueConcernsWarnAfterRetries(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		Files: map[string]string{"a.yaml": "still wrong\n"}, Diff: "diff",
	}}
	rev := &fakeCompleter{critique: `{"issues": ["wrong value"]}`}
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = rev
	gp.critiqueRetries = 1

	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if fa.calls != 2 || fix.files["a.yaml"] != "still wrong\n" || !slices.Contains(fix.warnings, patchCritiqueWarning("wrong value")) {
		t.Fatalf("calls=%d fix=%+v", fa.calls, fix)
	}
	// The reviewer's objection must be fed back into the retry instruction.
	if !strings.Contains(fa.spec.Instruction, "wrong value") {
		t.Errorf("retry instruction missing reviewer feedback: %q", fa.spec.Instruction)
	}
}

func TestGenerateWithAgent_CritiqueErrorWarns(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		Files: map[string]string{"a.yaml": "fixed\n"}, Diff: "diff",
	}}
	rev := &fakeCompleter{critiqueErr: errors.New("review endpoint down")}
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = rev
	gp.critiqueRetries = 1

	fix, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if fa.calls != 1 || !slices.Contains(fix.warnings, patchCritiqueUnavailableWarning) {
		t.Fatalf("calls=%d warnings=%v", fa.calls, fix.warnings)
	}
}

func TestGenerateBuildWithAgentPassesRuntimeIdentity(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{Files: map[string]string{"a": "b"}, Diff: "diff"}}
	observer := func(context.Context, runtime.WorkRef) error { return nil }
	gp := agentGenParams(&AgentConfig{Runtime: fa, ExecutionID: "build-request", WorkObserver: observer})
	_, err := generateBuildWithAgent(context.Background(), gp, BuildFailure{RootCause: "failed", SuggestedFix: "fix it", SourceFiles: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if fa.spec.ExecutionID != "build-request" || fa.spec.WorkObserver == nil {
		t.Fatalf("runtime work identity not passed: %+v", fa.spec)
	}
}

func TestGenerateBuildWithAgentWarnsOnTimedOutCommandResults(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].TimedOut = true
	results[0].ExitCode = -1
	fa := &fakeAgentRuntime{res: runtime.ExecutionResult{
		BaseSHA: "ref", Files: map[string]string{"a": "b"}, Diff: "diff", CommandResults: results,
		AgentSummary: "Guard the retry in `a` so it fails fast.",
	}}
	reviewer := &fakeCompleter{critique: `{"issues": ["the guard message is vague"]}`}
	gp := agentGenParams(&AgentConfig{
		Runtime: fa, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands},
	})
	gp.critique = reviewer
	gp.critiqueRetries = 0
	fix, err := generateBuildWithAgent(context.Background(), gp, BuildFailure{
		RootCause: "failed", SuggestedFix: "fix it", SourceFiles: []string{"a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fa.calls != 1 || reviewer.lastUser != "" || !slices.Contains(fix.warnings, patchVerifyWarning) {
		t.Fatalf("calls=%d critique=%q warnings=%v", fa.calls, reviewer.lastUser, fix.warnings)
	}
	if fix.rationale != "Guard the retry in `a` so it fails fast." {
		t.Fatalf("rationale = %q", fix.rationale)
	}
}

func TestFixInstructionsShareRepositoryChangeGuidance(t *testing.T) {
	generationContext := validGenerationContext()
	instructions := map[string]string{
		"pattern":  agentInstruction(systemicPattern("etcd"), &generationContext, "", "", 3, false),
		"build":    buildFailureInstruction(BuildFailure{JobID: "job", BuildID: "1", RootCause: "cause"}, "", "", 3, false),
		"analysis": analysisFailureInstruction(validAnalysisFailure(), "", "", 3, false),
	}
	for name, instruction := range instructions {
		for _, want := range []string{
			repositoryChangeGuidance,
			changeSummaryInstruction,
			"made, or failed to guard, the request or operation that hit the observed failure condition",
			"fails fast with a clear message, is a valid fix. Cite that code path by file and line.",
			"The published suggested fix is one candidate, not the default. An operational remedy does not rule out a repository guard.",
			"prefer a minimal patch with its caveats stated in your summary over making no change",
			"Make no change when no code in this repository is causally involved",
			"do not skip or weaken the failing test, just to produce a patch",
			"Do not delete or rename files.",
		} {
			if !strings.Contains(instruction, want) {
				t.Errorf("%s instruction missing %q:\n%s", name, want, instruction)
			}
		}
	}
}

func TestFixPreviewsDescribeGeneratedChange(t *testing.T) {
	const summary = "Fail fast when the selected disk type is unavailable (`templates/cluster.yaml:3`)."
	pattern := systemicPattern("etcd")
	pattern.SuggestedFix = "Ask the platform team to enable Premium_LRS."
	agent := goodAgent()
	agent.res.AgentSummary = summary
	manager := newManager(t, &fakePR{}, agent, Options{})

	patternFix, err := manager.GeneratePreview(t.Context(), pattern, "")
	if err != nil {
		t.Fatal(err)
	}
	buildFix, err := manager.GenerateBuildPreview(t.Context(), BuildFailure{
		ID: "build-id", JobID: "job", JobName: "job", BuildID: "1", RootCause: "cause", SuggestedFix: pattern.SuggestedFix,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, fix := range map[string]*GeneratedFix{"pattern": patternFix, "build": buildFix} {
		if fix.Preview.Rationale != summary || !strings.Contains(fix.Description, "**Proposed change:** "+summary) {
			t.Errorf("%s rationale=%q description=%q", name, fix.Preview.Rationale, fix.Description)
		}
		if strings.Contains(fix.Description, pattern.SuggestedFix) {
			t.Errorf("%s description used the published suggested fix: %s", name, fix.Description)
		}
	}
}

func TestPatternPreviewCarriesCritiqueConcernsToPreviewAndBody(t *testing.T) {
	agent := goodAgent()
	manager := newManager(t, &fakePR{}, agent, Options{Critique: &fakeCompleter{critique: `{"issues":["the guard ignores zonal SKUs"]}`}, CritiqueRetries: 1})
	fix, err := manager.GeneratePreview(t.Context(), systemicPattern("etcd"), "")
	if err != nil {
		t.Fatal(err)
	}
	warning := patchCritiqueWarning("the guard ignores zonal SKUs")
	if agent.calls != 2 || !slices.Contains(fix.Warnings, warning) || !strings.Contains(fix.Body, warning) {
		t.Fatalf("calls=%d warnings=%v body=%q", agent.calls, fix.Warnings, fix.Body)
	}
}
