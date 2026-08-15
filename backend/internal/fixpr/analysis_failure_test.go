package fixpr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

const exactAnalysisRevision = "0123456789abcdef0123456789abcdef01234567"

func validAnalysisFailure() AnalysisFailure {
	return AnalysisFailure{
		ID: "analysis::id", Project: "capz", JobID: "periodic-capz", JobName: "periodic-capz", BuildID: "123",
		TestName: "TestCluster", AnalysisGeneratedAt: "2026-08-13T01:00:00Z", AnalysisHash: "analysis-hash",
		RootCause: "the reconciler omitted the terminal state", SuggestedFix: "update the reconciler branch",
		AssistantAnswer:  "The artifact shows the terminal branch never calls `markReady`.",
		ChatResponseHash: "chat-hash", PreviewRequestHash: "preview-hash",
		ArtifactCitations: []Evidence{{Path: "artifacts/junit_01.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		SourceRepository:  "up/stream", FailureRevision: exactAnalysisRevision, GenerationBaseRevision: exactAnalysisRevision,
		VerifiedSourceFileHashes: map[string]string{"controllers/cluster_controller.go": strings.Repeat("d", 64)},
		SourceFiles:              []string{"controllers/cluster_controller.go"}, SourceVerification: "source-hash", FindingVerification: "finding-hash",
	}
}

func TestGenerateAnalysisPreviewUsesExactSourceAndCreatesNoWrite(t *testing.T) {
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.GitToken = ""
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "preserve compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("preview performed GitHub write: %+v", pr.opened)
	}
	if agent.spec.Repo.Ref != exactAnalysisRevision || agent.spec.ExpectedBaseSHA != exactAnalysisRevision || agent.spec.Repo.Token != "" {
		t.Fatalf("runtime spec = %+v", agent.spec)
	}
	for _, want := range []string{"exact failed JUnit analysis", "TestCluster", "ArtifactCitations", "source-hash", "preserve compatibility"} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if strings.Contains(agent.spec.Instruction, "recurs systematically") {
		t.Fatalf("instruction claimed recurrence: %s", agent.spec.Instruction)
	}
	snapshot := fix.Snapshot()
	if !snapshot.RequireBaseCurrent || snapshot.Base.HeadSHA != exactAnalysisRevision || !strings.HasPrefix(snapshot.Key, "fix-analysis::") {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGenerateAnalysisPreviewAllowsEmptyOriginalSuggestedFix(t *testing.T) {
	failure := validAnalysisFailure()
	failure.SuggestedFix = ""
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !strings.Contains(agent.spec.Instruction, failure.AssistantAnswer) {
		t.Fatalf("agent calls=%d instruction=%q", agent.calls, agent.spec.Instruction)
	}
	if fix == nil || len(pr.opened) != 0 {
		t.Fatalf("fix=%+v opened=%+v", fix, pr.opened)
	}
}

func TestGenerateAnalysisPreviewUsesCurrentGenerationBase(t *testing.T) {
	failure := validAnalysisFailure()
	failure.FailureRevision = "a866aca055bcaa205648e81d15c67668179fdfab"
	failure.GenerationBaseRevision = "c83d69ab8c572a4c00816076222d65262ee690cc"
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: failure.GenerationBaseRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.spec.Repo.Ref != failure.GenerationBaseRevision || agent.spec.ExpectedBaseSHA != failure.GenerationBaseRevision {
		t.Fatalf("runtime spec = %+v", agent.spec)
	}
	if agent.spec.Repo.Ref == failure.FailureRevision {
		t.Fatal("generation used the failure revision")
	}
	for _, want := range []string{failure.FailureRevision, failure.GenerationBaseRevision, "unchanged at the generation base"} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if snapshot := fix.Snapshot(); snapshot.Base.HeadSHA != failure.GenerationBaseRevision || !snapshot.RequireBaseCurrent {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("preview performed GitHub write: %+v", pr.opened)
	}
}

func TestAnalysisPreviewRequiresCurrentPinnedBaseAtGenerationAndConfirmation(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("b", 40), TreeSHA: "tree-b"}}
	manager := newManager(t, pr, goodAgent(), Options{})
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); err == nil || !strings.Contains(err.Error(), "no longer the current fix base") {
		t.Fatalf("generation drift error = %v", err)
	}

	pr.base = ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree-a"}
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	pr.base = ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("c", 40), TreeSHA: "tree-c"}
	if _, err := manager.OpenFromPreview(t.Context(), fix); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("confirmation drift error = %v", err)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("drifted confirmation wrote PR: %+v", pr.opened)
	}
}

func TestAnalysisPreviewRejectsBaseAdvanceDuringGeneration(t *testing.T) {
	failure := validAnalysisFailure()
	initial := ghpr.Base{Branch: "main", HeadSHA: failure.GenerationBaseRevision, TreeSHA: "tree-a"}
	advanced := ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("c", 40), TreeSHA: "tree-c"}
	pr := &fakePR{bases: []ghpr.Base{initial, advanced}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("generation-time base drift error = %v", err)
	}
	if agent.calls != 1 || len(pr.opened) != 0 {
		t.Fatalf("agent calls=%d opened=%d", agent.calls, len(pr.opened))
	}
}

func TestAnalysisPreviewUsesAnyStateDedupBeforeWrite(t *testing.T) {
	pr := &fakePR{
		base:        ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"},
		searchFound: true, searchURL: "https://github.com/up/stream/pull/9",
	}
	manager := newManager(t, pr, goodAgent(), Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := manager.OpenFromPreview(t.Context(), fix)
	if err != nil || url != pr.searchURL || pr.searchAnyCalls != 1 || len(pr.opened) != 0 {
		t.Fatalf("url=%q err=%v searchAny=%d opened=%d", url, err, pr.searchAnyCalls, len(pr.opened))
	}
}

func TestAnalysisPreviewDedupIdentityIncludesSelectedChatAndRequest(t *testing.T) {
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	manager := newManager(t, pr, goodAgent(), Options{})
	first, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	changedChat := validAnalysisFailure()
	changedChat.ChatResponseHash = "other-chat"
	second, err := manager.GenerateAnalysisPreview(t.Context(), changedChat, "")
	if err != nil {
		t.Fatal(err)
	}
	changedRequest := validAnalysisFailure()
	changedRequest.PreviewRequestHash = "other-preview"
	third, err := manager.GenerateAnalysisPreview(t.Context(), changedRequest, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot().Key == second.Snapshot().Key || first.Snapshot().Key == third.Snapshot().Key {
		t.Fatal("selected chat or preview identity did not change Fix PR dedup key")
	}
}

func TestAnalysisPreviewCritiqueConcernWarnsWithoutRetry(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{Critique: &fakeCompleter{critique: `{"issues":["patch needs a narrower condition"]}`}, CritiqueRetries: 3})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !slices.Contains(fix.Warnings, analysisPatchCritiqueWarning) {
		t.Fatalf("calls=%d warnings=%v", agent.calls, fix.Warnings)
	}
}

func TestAnalysisPreviewFailedAuthenticCommandsWarnAndRemainConfirmable(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].ExitCode = 1
	agent.res.BaseSHA = exactAnalysisRevision
	agent.res.CommandResults = results
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.RequireCommandResults = true
	manager.opts.Agent.CommandPolicy.Commands = commands
	reconstructions := 0
	manager.opts.ReconstructPatch = func(context.Context, runtime.RepoRef, string) (map[string]string, string, error) {
		reconstructions++
		return agent.res.Files, agent.res.Diff, nil
	}
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || fix.Preview.Verify.Status != VerifyFailed || !slices.Contains(fix.Warnings, analysisPatchVerifyWarning) {
		t.Fatalf("calls=%d verify=%+v warnings=%v", agent.calls, fix.Preview.Verify, fix.Warnings)
	}
	restored := RestoreGeneratedFix(fix.Snapshot())
	if restored.executionVerification == nil || !restored.executionVerification.AllowFailures || restored.executionVerification.Results[0].ExitCode != 1 {
		t.Fatalf("restored verification = %+v", restored.executionVerification)
	}
	if _, err := manager.OpenFromPreview(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 1 || reconstructions != 1 {
		t.Fatalf("opened=%d reconstructions=%d", len(pr.opened), reconstructions)
	}

	drifted := RestoreGeneratedFix(fix.Snapshot())
	drifted.executionVerification.Results[0].Argv = []string{"go", "test", "./wrong"}
	if _, err := manager.OpenFromPreview(t.Context(), drifted); err == nil || !strings.Contains(err.Error(), "allowed argv") {
		t.Fatalf("integrity error = %v", err)
	}
}

func TestAnalysisPreviewTimedOutAuthenticCommandWarnsWithoutRetry(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].ExitCode = -1
	results[0].TimedOut = true
	results[0].DurationMs = commands[0].TimeoutSeconds * 1000
	agent.res.BaseSHA = exactAnalysisRevision
	agent.res.CommandResults = results
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.RequireCommandResults = true
	manager.opts.Agent.CommandPolicy.Commands = commands
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || fix.Preview.Verify.Status != VerifyFailed || !slices.Contains(fix.Warnings, analysisPatchVerifyWarning) {
		t.Fatalf("calls=%d verify=%+v warnings=%v", agent.calls, fix.Preview.Verify, fix.Warnings)
	}
}

func TestAnalysisGenerationFailureRetainsOnlySafeDiagnostic(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].Stdout = "private stdout"
	results[0].Stderr = "private stderr"
	agent := &fakeAgentRuntime{res: runtime.GenerateResult{
		TerminalState: runtime.TerminalSucceeded, BaseSHA: exactAnalysisRevision,
		Files: map[string]string{}, CommandResults: results,
	}}
	config := &AgentConfig{Runtime: agent, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands}}
	_, err := generateAnalysisWithAgent(t.Context(), genParams{
		owner: "up", repo: "stream", maxFiles: 2, agent: config,
	}, validAnalysisFailure())
	if err == nil {
		t.Fatal("expected no-change failure")
	}
	diagnostic, ok := AnalysisFailureDiagnosticOf(err)
	if !ok || diagnostic.Category != AnalysisFailureNoReviewablePatch || diagnostic.TerminalState != runtime.TerminalSucceeded {
		t.Fatalf("diagnostic = %+v ok=%v", diagnostic, ok)
	}
	if agent.calls != 1 || len(diagnostic.CommandResults) != len(commands) {
		t.Fatalf("calls=%d diagnostic=%+v", agent.calls, diagnostic)
	}
	for _, result := range diagnostic.CommandResults {
		if result.Stdout != "" || result.Stderr != "" {
			t.Fatalf("diagnostic retained command output: %+v", result)
		}
	}
}

func TestAnalysisGenerationFailureClassifiesScopeAndHardOutcomes(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	tests := []struct {
		name     string
		result   runtime.GenerateResult
		err      error
		maxFiles int
		want     AnalysisFailureCategory
	}{
		{
			name: "too broad", maxFiles: 1, want: AnalysisFailureNoReviewablePatch,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalSucceeded, BaseSHA: exactAnalysisRevision,
				Files: map[string]string{"a": "1", "b": "2"}, Diff: "diff", CommandResults: results},
		},
		{
			name: "runtime", maxFiles: 2, want: AnalysisFailureRuntimeInfrastructure,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureRuntime},
			err:    runtime.ErrUnavailable,
		},
		{
			name: "review scope wire outcome", maxFiles: 2, want: AnalysisFailureNoReviewablePatch,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureReviewScope,
				CommandResults: results},
			err: errors.New("agent Sandbox execution failed"),
		},
		{
			name: "result contract", maxFiles: 2, want: AnalysisFailureResultContract,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalFailed, CommandResults: results},
			err:    runtime.ErrMalformedResult,
		},
		{
			name: "safety", maxFiles: 2, want: AnalysisFailureSafetyIntegrity,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureSafetyIntegrity, CommandResults: results},
			err:    errors.New("agent Sandbox execution failed"),
		},
		{
			name: "timeout", maxFiles: 2, want: AnalysisFailureTimedOut,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalTimedOut},
			err:    context.DeadlineExceeded,
		},
		{
			name: "cancelled", maxFiles: 2, want: AnalysisFailureCancelled,
			result: runtime.GenerateResult{TerminalState: runtime.TerminalCancelled},
			err:    runtime.ErrCancelled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgentRuntime{res: tt.result, err: tt.err}
			config := &AgentConfig{Runtime: agent, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands}}
			_, err := generateAnalysisWithAgent(t.Context(), genParams{
				owner: "up", repo: "stream", maxFiles: tt.maxFiles, agent: config,
			}, validAnalysisFailure())
			if err == nil {
				t.Fatal("expected generation failure")
			}
			diagnostic, ok := AnalysisFailureDiagnosticOf(err)
			if !ok || diagnostic.Category != tt.want || agent.calls != 1 {
				t.Fatalf("diagnostic=%+v ok=%v calls=%d err=%v", diagnostic, ok, agent.calls, err)
			}
		})
	}
}
