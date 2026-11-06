package pr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/ghpr"
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
	gp := agentGenParams(&AgentConfig{
		Runtime: fa, MaxTurns: 5, MaxFiles: 3, Timeout: time.Minute, GitToken: "git-token",
		OutputLimitBytes: 131072,
		ModelProvider:    testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture"),
		AllowBash:        true,
		CommandPolicy:    runtime.CommandPolicy{Commands: []runtime.ExecutionCommand{{Argv: []string{"go", "test", "./..."}}}},
		ExecutionID:      "request-1", WorkObserver: observer,
	})

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
	if fa.spec.Repo.Owner != "o" || fa.spec.Repo.Name != "r" || fa.spec.Repo.Ref != "ref" || fa.spec.Repo.Token != "git-token" {
		t.Errorf("repo not passed: %+v", fa.spec.Repo)
	}
	if fa.spec.ExecutionID != "request-1" || fa.spec.WorkObserver == nil {
		t.Errorf("runtime work identity not passed: %+v", fa.spec)
	}
	if fa.spec.ExpectedBaseSHA != "ref" || fa.spec.MaxTurns != 5 || fa.spec.MaxSteps != 5 || fa.spec.MaxFiles != 3 || fa.spec.Timeout != time.Minute || fa.spec.OutputLimitBytes != 131072 || fa.spec.ModelProvider != gp.agent.ModelProvider || !fa.spec.AllowBash || !fa.spec.CommandPolicy.AllowShell || len(fa.spec.CommandPolicy.Commands) != 1 {
		t.Errorf("provider-neutral execution policy not passed: %+v", fa.spec)
	}
}

func TestAgentRuntimeSpecOmitsSharedEndpointConfiguration(t *testing.T) {
	observer := func(context.Context, runtime.WorkRef) error { return nil }
	spec := agentRuntimeSpec(&AgentConfig{
		ModelProvider: testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "fixture"),
		ExecutionID:   "request-1", WorkObserver: observer,
	}, runtime.RepoRef{Owner: "o", Name: "r", Ref: "ref", Token: "git-token"}, "fix")
	if spec.Model != "" || spec.Endpoint != "" || spec.Token != "" || len(spec.NetworkDomains) != 0 {
		t.Fatalf("shared endpoint configuration leaked into runtime spec: %+v", spec)
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
	gp := agentGenParams(&AgentConfig{
		Runtime: fa, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands},
	})
	fix, err := generateBuildWithAgent(context.Background(), gp, BuildFailure{
		RootCause: "failed", SuggestedFix: "fix it", SourceFiles: []string{"a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fa.calls != 1 || !slices.Contains(fix.warnings, patchVerifyWarning) {
		t.Fatalf("calls=%d warnings=%v", fa.calls, fix.warnings)
	}
	if fix.rationale != "Guard the retry in `a` so it fails fast." {
		t.Fatalf("rationale = %q", fix.rationale)
	}
}

func TestFixInstructionsShareRepositoryChangeGuidance(t *testing.T) {
	generationContext := validGenerationContext()
	instructions := map[string]string{
		"pattern":  agentInstruction(systemicPattern("etcd"), &generationContext, "", 3, false),
		"build":    buildFailureInstruction(BuildFailure{JobID: "job", BuildID: "1", RootCause: "cause"}, "", 3, false),
		"analysis": analysisFailureInstruction(validAnalysisFailure(), "", 3, false),
	}
	for name, instruction := range instructions {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				repositoryChangeGuidance,
				changeSummaryInstruction,
				"that resolves the failure, verified against the repository",
				"First look for a minimal change that makes the failed operation succeed",
				"correcting a value, version, path, or repository choice this code made",
				"Verify the causal code path against the repository and cite it by file and line",
				"Well-documented external facts",
				"State assumptions and how to validate them",
				"State an observable success criterion",
				"The published suggested fix is one candidate, not the default",
				"Do not substitute a guard for a requested resolving change",
				"Replay the original triggering condition",
				`Start its summary with "Guard only:" and state the remaining failure`,
				"Make no change when no code in this repository is causally involved",
				"Do not edit unrelated code",
				"do not skip or weaken the failing test, just to produce a patch",
				"Do not delete or rename files.",
			} {
				if !strings.Contains(instruction, want) {
					t.Errorf("instruction missing %q:\n%s", want, instruction)
				}
			}
			for _, unwanted := range []string{
				"is a valid fix",
				"prefer a minimal patch with its caveats stated in your summary over making no change",
				"change that repository evidence supports",
				"change supported by repository evidence",
			} {
				if strings.Contains(instruction, unwanted) {
					t.Errorf("instruction contains superseded guidance %q", unwanted)
				}
			}
		})
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

func TestFixPreviewsGenerateOnceAndPreserveVerification(t *testing.T) {
	for _, path := range []string{"pattern", "build", "analysis"} {
		t.Run(path, func(t *testing.T) {
			for _, scenario := range []string{"success", "validator failure", "runtime failure", "integrity failure", "no change"} {
				t.Run(scenario, func(t *testing.T) {
					agent := goodAgent()
					agent.res.BaseSHA = exactAnalysisRevision
					agent.res.CommandResults = sandboxCommandResults()
					switch scenario {
					case "validator failure":
						agent.res.CommandResults[0].ExitCode = 1
					case "runtime failure":
						agent.res.TerminalState = runtime.TerminalFailed
						agent.err = errors.New("executor safety failure")
					case "integrity failure":
						agent.res.CommandResults[0].Argv = []string{"go", "test", "./unexpected"}
					case "no change":
						agent.res.Files = nil
					}
					pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
					manager := newManager(t, pr, agent, Options{})
					manager.opts.Agent.RequireCommandResults = true
					manager.opts.Agent.CommandPolicy.Commands = sandboxVerificationCommands()
					var fix *GeneratedFix
					var err error
					switch path {
					case "pattern":
						fix, err = manager.GeneratePreview(t.Context(), systemicPattern("failure"), "")
					case "build":
						fix, err = manager.GenerateBuildPreview(t.Context(), BuildFailure{ID: "build", JobID: "job", BuildID: "123", RootCause: "failed"}, "")
					case "analysis":
						fix, err = manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
					}
					if agent.calls != 1 || len(pr.opened) != 0 {
						t.Fatalf("generation calls=%d GitHub writes=%d", agent.calls, len(pr.opened))
					}
					if scenario == "runtime failure" || scenario == "integrity failure" || scenario == "no change" {
						if err == nil || fix != nil {
							t.Fatalf("invalid attempt returned fix=%+v err=%v", fix, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					wantStatus := VerifyPassed
					if scenario == "validator failure" {
						wantStatus = VerifyFailed
						if !slices.Contains(fix.Warnings, patchVerifyWarning) {
							t.Fatalf("verification warning missing: %v", fix.Warnings)
						}
					} else if len(fix.Warnings) != 0 {
						t.Fatalf("unexpected warnings: %v", fix.Warnings)
					}
					if fix.Preview.Verify.Status != wantStatus || fix.ValidateExecutionVerification() != nil {
						t.Fatalf("verification=%+v", fix.Preview.Verify)
					}
				})
			}
		})
	}
}
