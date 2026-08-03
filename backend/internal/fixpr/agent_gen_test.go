package fixpr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// fakeAgentRuntime is a stand-in AgentRuntime that returns canned results and
// records the spec it was called with.
type fakeAgentRuntime struct {
	res  runtime.GenerateResult
	err  error
	spec runtime.GenerateSpec
}

func (f *fakeAgentRuntime) Generate(_ context.Context, spec runtime.GenerateSpec) (runtime.GenerateResult, error) {
	f.spec = spec
	return f.res, f.err
}

func agentGenParams(ar *AgentConfig) genParams {
	return genParams{owner: "o", repo: "r", ref: "ref", maxFiles: 3, agent: ar}
}

// goodAgent returns a fake agent runtime that proposes a canned single-file fix,
// used by the Reconcile tests to drive the (only) generation path.
func goodAgent() *fakeAgentRuntime {
	return &fakeAgentRuntime{res: runtime.GenerateResult{
		Files: map[string]string{"templates/cluster.yaml": strings.Replace(sampleFile, "StandardSSD_LRS", "Premium_LRS", 1)},
		Diff:  "--- a/templates/cluster.yaml\n+++ b/templates/cluster.yaml\n@@\n-  diskType: StandardSSD_LRS\n+  diskType: Premium_LRS\n",
	}}
}

func TestGenerateWithAgent_HappyPath(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{
		Files: map[string]string{"templates/cluster.yaml": "diskType: Premium_LRS\n"},
		Diff:  "--- a/templates/cluster.yaml\n+++ b/templates/cluster.yaml\n",
	}}
	observer := func(context.Context, runtime.WorkRef) error { return nil }
	gp := agentGenParams(&AgentConfig{Runtime: fa, Model: "m", Endpoint: "e", ModelToken: "t", AllowBash: true, ExecutionID: "request-1", WorkObserver: observer})

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
	if fa.spec.ExecutionID != "request-1" || fa.spec.WorkObserver == nil {
		t.Errorf("runtime work identity not passed: %+v", fa.spec)
	}
}

func TestGenerateWithAgent_NoChangeIsNotFixable(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{Files: map[string]string{}}}
	_, err := generateWithAgent(context.Background(), agentGenParams(&AgentConfig{Runtime: fa}), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "no code change") {
		t.Errorf("expected a not-auto-fixable error, got %v", err)
	}
}

func TestGenerateWithAgent_RejectsTooManyFiles(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{Files: map[string]string{
		"a": "1", "b": "2", "c": "3", "d": "4",
	}}}
	gp := agentGenParams(&AgentConfig{Runtime: fa}) // maxFiles 3
	_, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "exceeding max_files") {
		t.Errorf("expected a max_files rejection, got %v", err)
	}
}

func TestGenerateWithAgent_UnavailableSurfaces(t *testing.T) {
	// Wrap the sentinel so errors.Is matches through the fixpr error.
	fa := &fakeAgentRuntime{err: errWrap{runtime.ErrUnavailable}}
	_, err := generateWithAgent(context.Background(), agentGenParams(&AgentConfig{Runtime: fa}), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("expected an unavailable error, got %v", err)
	}
}

type errWrap struct{ err error }

func (e errWrap) Error() string { return "agent: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

func TestGenerateWithAgent_CritiqueApproves(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{
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

func TestGenerateWithAgent_CritiqueRejectsThenExhausts(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{
		Files: map[string]string{"a.yaml": "still wrong\n"}, Diff: "diff",
	}}
	rev := &fakeCompleter{critique: `{"issues": ["wrong value"]}`}
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = rev
	gp.critiqueRetries = 1

	_, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "rejected by review") {
		t.Errorf("expected a review rejection, got %v", err)
	}
	// The reviewer's objection must be fed back into the retry instruction.
	if !strings.Contains(fa.spec.Instruction, "wrong value") {
		t.Errorf("retry instruction missing reviewer feedback: %q", fa.spec.Instruction)
	}
}

func TestGenerateWithAgent_CritiqueErrorFailsClosed(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{
		Files: map[string]string{"a.yaml": "fixed\n"}, Diff: "diff",
	}}
	rev := &fakeCompleter{critiqueErr: errors.New("review endpoint down")}
	gp := agentGenParams(&AgentConfig{Runtime: fa})
	gp.critique = rev
	gp.critiqueRetries = 1

	if _, err := generateWithAgent(context.Background(), gp, systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "review failed") {
		t.Errorf("a review error should drop the fix (fail closed), got %v", err)
	}
}

func TestGenerateBuildWithAgentPassesRuntimeIdentity(t *testing.T) {
	fa := &fakeAgentRuntime{res: runtime.GenerateResult{Files: map[string]string{"a": "b"}, Diff: "diff"}}
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
