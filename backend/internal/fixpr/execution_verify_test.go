package fixpr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

func sandboxVerificationCommands() []runtime.ExecutionCommand {
	return []runtime.ExecutionCommand{
		{Argv: []string{"go", "test", "./..."}, TimeoutSeconds: 60},
		{Argv: []string{"go", "vet", "./..."}, TimeoutSeconds: 60},
		{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 30},
	}
}

func sandboxCommandResults() []runtime.CommandResult {
	return []runtime.CommandResult{
		{Argv: []string{"go", "test", "./..."}, ExitCode: 0, DurationMs: 10},
		{Argv: []string{"go", "vet", "./..."}, ExitCode: 0, DurationMs: 10},
		{Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 2},
	}
}

func TestAgentSandboxPreviewAndConfirmationUseExecutorResults(t *testing.T) {
	failure := validAnalysisFailure()
	files := map[string]string{"controllers/cluster_controller.go": "package controllers\n"}
	diff := "diff --git a/controllers/cluster_controller.go b/controllers/cluster_controller.go\n"
	agent := &fakeAgentRuntime{res: runtime.ExecutionResult{
		BaseSHA: failure.GenerationBaseRevision, Files: files, Diff: diff,
		CommandResults: sandboxCommandResults(),
	}}
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: failure.GenerationBaseRevision, TreeSHA: strings.Repeat("b", 40)}}
	reconstructions := 0
	manager := NewManager(pr, t.TempDir()+"/state.json", Options{
		SourceOwner: "up", SourceName: "stream", AuthorName: "Jane", AuthorEmail: "jane@example.com",
		MaxFiles: 3,
		Agent: &AgentConfig{
			Runtime: agent, MaxFiles: 3, MaxTurns: 10, Timeout: time.Minute,
			CommandPolicy: runtime.CommandPolicy{Commands: sandboxVerificationCommands()}, RequireCommandResults: true,
		},
		ReconstructPatch: func(_ context.Context, repo runtime.RepoRef, gotDiff string) (map[string]string, string, error) {
			reconstructions++
			if repo.Ref != failure.GenerationBaseRevision || repo.Token != "" || gotDiff != diff {
				t.Fatalf("reconstruction repo=%+v diff=%q", repo, gotDiff)
			}
			return files, diff, nil
		},
	})

	generated, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 0 {
		t.Fatal("preview performed a GitHub write")
	}
	if agent.calls != 1 || agent.spec.Repo.Token != "" {
		t.Fatalf("agent calls=%d repo=%+v", agent.calls, agent.spec.Repo)
	}
	if generated.Preview.Verify.Status != VerifyPassed || !strings.Contains(generated.Preview.Verify.Summary, "Agent Sandbox") {
		t.Fatalf("verification = %+v", generated.Preview.Verify)
	}
	if reconstructions != 0 {
		t.Fatal("preview confirmation reconstruction ran during generation")
	}

	if _, err := manager.OpenFromPreview(t.Context(), generated); err != nil {
		t.Fatal(err)
	}
	if reconstructions != 1 || len(pr.opened) != 1 {
		t.Fatalf("reconstructions=%d writes=%d", reconstructions, len(pr.opened))
	}
}

func TestAgentSandboxCommandResultsFailClosed(t *testing.T) {
	commands := sandboxVerificationCommands()
	valid := sandboxCommandResults()
	cases := []struct {
		name string
		edit func(*[]runtime.ExecutionCommand, *[]runtime.CommandResult)
		want string
	}{
		{name: "missing", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) { *results = (*results)[:2] }, want: "every allowed command"},
		{name: "reordered", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) {
			(*results)[0], (*results)[1] = (*results)[1], (*results)[0]
		}, want: "allowed argv"},
		{name: "added", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) {
			*results = append(*results, runtime.CommandResult{Argv: []string{"go", "test", "./extra"}})
		}, want: "every allowed command"},
		{name: "failed", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) { (*results)[0].ExitCode = 1 }, want: "failed with exit code"},
		{name: "timed out", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) { (*results)[0].TimedOut = true }, want: "timed out"},
		{name: "malformed", edit: func(_ *[]runtime.ExecutionCommand, results *[]runtime.CommandResult) { (*results)[0].DurationMs = -1 }, want: "negative duration"},
		{name: "malformed command", edit: func(commands *[]runtime.ExecutionCommand, _ *[]runtime.CommandResult) {
			(*commands)[0].TimeoutSeconds = 0
		}, want: "invalid timeout"},
		{name: "missing final diff check", edit: func(commands *[]runtime.ExecutionCommand, _ *[]runtime.CommandResult) { *commands = (*commands)[:2] }, want: "final validation command"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gotCommands := cloneExecutionCommands(commands)
			gotResults := cloneCommandResults(valid)
			testCase.edit(&gotCommands, &gotResults)
			agent := &AgentConfig{RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: gotCommands}}
			_, err := executionVerificationForAgent(agent, runtime.ExecutionResult{BaseSHA: strings.Repeat("a", 40), CommandResults: gotResults}, strings.Repeat("a", 40))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error=%v want=%q", err, testCase.want)
			}
		})
	}
}

func TestAgentSandboxConfirmationRejectsPersistedCommandOrPatchDrift(t *testing.T) {
	commands := sandboxVerificationCommands()
	verification := &ExecutionVerification{BaseSHA: strings.Repeat("a", 40), Commands: commands, Results: sandboxCommandResults()}
	base := ghpr.Base{Branch: "main", HeadSHA: verification.BaseSHA, TreeSHA: strings.Repeat("b", 40)}
	generated := &GeneratedFix{
		Preview: Preview{Diff: "canonical diff", Files: map[string]string{"fix.go": "package fix\n"}, Verify: verification.verifyResult()},
		Title:   "fix: test", Description: "safe", Body: "safe", key: "fix-analysis::test", base: base, requireBaseCurrent: true,
		executionVerification: verification,
	}

	t.Run("command results", func(t *testing.T) {
		copy := RestoreGeneratedFix(generated.Snapshot())
		copy.executionVerification.Results[0].ExitCode = 1
		manager := NewManager(&fakePR{base: base}, t.TempDir()+"/state.json", Options{SourceOwner: "o", SourceName: "r"})
		if _, err := manager.OpenFromPreview(t.Context(), copy); err == nil || !strings.Contains(err.Error(), "exit code") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("patch content", func(t *testing.T) {
		pr := &fakePR{base: base}
		manager := NewManager(pr, t.TempDir()+"/state.json", Options{
			SourceOwner: "o", SourceName: "r",
			ReconstructPatch: func(context.Context, runtime.RepoRef, string) (map[string]string, string, error) {
				return map[string]string{"fix.go": "package changed\n"}, "canonical diff", nil
			},
		})
		if _, err := manager.OpenFromPreview(t.Context(), RestoreGeneratedFix(generated.Snapshot())); err == nil || !strings.Contains(err.Error(), "patch content changed") {
			t.Fatalf("error=%v", err)
		}
		if len(pr.opened) != 0 {
			t.Fatal("confirmation wrote after patch drift")
		}
	})
}
