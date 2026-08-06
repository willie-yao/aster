package agentanalysis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type fakeAgentRuntime struct {
	got agentruntime.GenerateSpec
	res agentruntime.GenerateResult
	err error
}

func (f *fakeAgentRuntime) Generate(ctx context.Context, spec agentruntime.GenerateSpec) (agentruntime.GenerateResult, error) {
	f.got = spec
	if spec.WorkObserver != nil {
		_ = spec.WorkObserver(ctx, agentruntime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "analysis-task"})
		_ = spec.WorkObserver(ctx, agentruntime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "analysis-task", UID: "uid-1"})
	}
	return f.res, f.err
}

func TestRuntimeGenerate(t *testing.T) {
	bundle := testBundle(t)
	body := validAnalysisJSON(bundle)
	agent := &fakeAgentRuntime{res: agentruntime.GenerateResult{
		Files: map[string]string{OutputPath: body}, Diff: validOutputDiff(body), Attempts: 2,
	}}
	runtime := &Runtime{Agent: agent, Name: "orka", AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1", Retries: 1}
	reader := &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}
	got, err := runtime.Generate(t.Context(), Spec{
		Repo:   agentruntime.RepoRef{Owner: bundle.Source.Owner, Name: bundle.Source.Name, Ref: bundle.Source.Revision},
		Bundle: bundle, SourceReader: reader, MaxTurns: 12, Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 2 || got.AgentNamespace != "orka-system" || got.AgentRef != "analysis-agent" || got.EvidenceHash != bundle.Hash || got.SkillHash != SkillHash() || got.IdentityHash == "" {
		t.Fatalf("result = %+v", got)
	}
	if agent.got.AllowBash || agent.got.Model != "" || agent.got.Endpoint != "" || agent.got.Token != "" || len(agent.got.NetworkDomains) != 0 {
		t.Fatalf("agent spec leaked provider policy: %+v", agent.got)
	}
	if agent.got.Skills[SkillName] == "" || !strings.Contains(agent.got.Instruction, bundle.Hash) || !strings.HasPrefix(agent.got.ExecutionID, "agent-analysis-") {
		t.Fatalf("agent spec = %+v", agent.got)
	}
}

func TestRuntimePreservesValidResultOnCleanupPending(t *testing.T) {
	bundle := testBundle(t)
	body := validAnalysisJSON(bundle)
	agent := &fakeAgentRuntime{
		res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: body}, Diff: validOutputDiff(body), Attempts: 1},
		err: agentruntime.ErrCleanupPending,
	}
	runtime := &Runtime{Agent: agent, AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1"}
	got, err := runtime.Generate(t.Context(), Spec{
		Repo:   agentruntime.RepoRef{Owner: bundle.Source.Owner, Name: bundle.Source.Name, Ref: bundle.Source.Revision},
		Bundle: bundle, SourceReader: &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}},
		MaxTurns: 5, Timeout: time.Minute,
	})
	if !errors.Is(err, agentruntime.ErrCleanupPending) || !got.CleanupPending || got.CleanupWork == nil || got.Analysis.Summary == "" {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestRuntimeRejectsUnsafeOutputAndRepo(t *testing.T) {
	bundle := testBundle(t)
	runtime := &Runtime{Agent: &fakeAgentRuntime{}, AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1"}
	_, err := runtime.Generate(t.Context(), Spec{
		Repo:   agentruntime.RepoRef{Owner: "other", Name: bundle.Source.Name, Ref: bundle.Source.Revision},
		Bundle: bundle, MaxTurns: 5, Timeout: time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("repo error = %v", err)
	}

	body := validAnalysisJSON(bundle)
	agent := &fakeAgentRuntime{res: agentruntime.GenerateResult{
		Files: map[string]string{OutputPath: body}, Diff: "diff --git a/" + OutputPath + " b/" + OutputPath + "\n--- a/" + OutputPath + "\n+++ b/" + OutputPath,
	}}
	runtime.Agent = agent
	_, err = runtime.Generate(t.Context(), Spec{
		Repo:   agentruntime.RepoRef{Owner: bundle.Source.Owner, Name: bundle.Source.Name, Ref: bundle.Source.Revision},
		Bundle: bundle, MaxTurns: 5, Timeout: time.Minute,
	})
	if !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("output error = %v", err)
	}
}

func TestRuntimeIdentityIncludesExecutionContract(t *testing.T) {
	bundle := testBundle(t)
	baseRuntime := &Runtime{AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1", Retries: 1}
	baseSpec := Spec{Bundle: bundle, MaxTurns: 10, Timeout: 5 * time.Minute}
	base := baseRuntime.identityHash(baseSpec)
	tests := []struct {
		name    string
		runtime Runtime
		spec    Spec
	}{
		{name: "namespace", runtime: Runtime{AgentNamespace: "other-system", AgentRef: "analysis-agent", AgentVersion: "v1", Retries: 1}, spec: baseSpec},
		{name: "agent", runtime: Runtime{AgentNamespace: "orka-system", AgentRef: "other-agent", AgentVersion: "v1", Retries: 1}, spec: baseSpec},
		{name: "agent version", runtime: Runtime{AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v2", Retries: 1}, spec: baseSpec},
		{name: "retries", runtime: Runtime{AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1", Retries: 2}, spec: baseSpec},
		{name: "turns", runtime: *baseRuntime, spec: Spec{Bundle: bundle, MaxTurns: 11, Timeout: baseSpec.Timeout}},
		{name: "timeout", runtime: *baseRuntime, spec: Spec{Bundle: bundle, MaxTurns: baseSpec.MaxTurns, Timeout: 6 * time.Minute}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.runtime.identityHash(test.spec); got == base {
				t.Fatalf("identity did not change: %s", got)
			}
		})
	}
}
