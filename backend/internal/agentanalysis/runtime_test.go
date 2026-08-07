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
		Telemetry: agentruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, FinalizationChecked: true, FinalizationValid: true, CleanupCompleted: true, UsageStatus: "unavailable_from_agent_runtime"},
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
	if got.Status != ShadowStatusSucceeded || got.Attempts != 2 || !got.Telemetry.FinalizationValid || got.AgentNamespace != "orka-system" || got.AgentRef != "analysis-agent" || got.EvidenceHash != bundle.Hash || got.SkillHash != SkillHash() || got.ToolPolicyVersion != ToolPolicyVersion || got.IdentityHash == "" {
		t.Fatalf("result = %+v", got)
	}
	if agent.got.AllowBash || agent.got.Model != "" || agent.got.Endpoint != "" || agent.got.Token != "" || len(agent.got.NetworkDomains) != 0 {
		t.Fatalf("agent spec leaked provider policy: %+v", agent.got)
	}
	if agent.got.Skills[SkillName] == "" || !strings.Contains(agent.got.Instruction, bundle.Hash) || !strings.HasPrefix(agent.got.ExecutionID, "agent-analysis-") {
		t.Fatalf("agent spec = %+v", agent.got)
	}
}

func TestFailureAnalysisSkillIncludesCausalPriorityAnchors(t *testing.T) {
	for _, anchor := range []string{
		"compare specific request, list, watch, or assertion",
		"later successful operation as counterevidence",
		"keep the remaining boundary",
	} {
		if !strings.Contains(failureAnalysisSkill, anchor) {
			t.Errorf("failure analysis skill missing causal-priority anchor %q", anchor)
		}
	}
}

func TestBuildInstructionRejectsOversizedBundle(t *testing.T) {
	bundle := EvidenceBundle{Excerpts: []EvidenceExcerpt{{Content: strings.Repeat("x", maxAgentPromptBytes)}}}
	if _, err := buildInstruction(bundle); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("error = %v", err)
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
	if !errors.Is(err, agentruntime.ErrCleanupPending) || got.Status != ShadowStatusCleanupPending || !got.CleanupPending || got.CleanupWork == nil || got.Analysis.Summary == "" {
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
	if changed := baseRuntime.identityHashWithPolicy(baseSpec, "agent-analysis-tools-other"); changed == base {
		t.Fatal("tool policy version did not change runtime identity")
	}
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

func TestValidateGeneratedOutputStatuses(t *testing.T) {
	validBody := "{}"
	for _, tc := range []struct {
		name   string
		result agentruntime.GenerateResult
		want   ShadowStatus
	}{
		{name: "no result", result: agentruntime.GenerateResult{}, want: ShadowStatusNoResult},
		{name: "extra file", result: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validBody, "extra.txt": "x"}}, want: ShadowStatusExtraFile},
		{name: "wrong file", result: agentruntime.GenerateResult{Files: map[string]string{"other.json": validBody}}, want: ShadowStatusExtraFile},
		{name: "empty file", result: agentruntime.GenerateResult{Files: map[string]string{OutputPath: ""}}, want: ShadowStatusMalformedResult},
		{name: "deletion", result: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validBody}, Diff: "deleted file mode 100644"}, want: ShadowStatusDeletion},
		{name: "rename", result: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validBody}, Diff: "rename from old\nrename to " + OutputPath}, want: ShadowStatusRename},
		{name: "contract", result: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validBody}, Diff: "diff --git a/" + OutputPath + " b/" + OutputPath}, want: ShadowStatusContractViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGeneratedOutput(tc.result)
			if status, ok := shadowStatusFromError(err); !ok || status != tc.want || !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("status=%q ok=%v error=%v", status, ok, err)
			}
		})
	}
}

func TestResolveShadowStatus(t *testing.T) {
	valid := Result{Analysis: Analysis{Summary: "valid"}}
	for _, tc := range []struct {
		name   string
		result Result
		err    error
		want   ShadowStatus
	}{
		{name: "success", want: ShadowStatusSucceeded},
		{name: "malformed", err: agentruntime.ErrMalformedResult, want: ShadowStatusMalformedResult},
		{name: "extra", err: agentruntime.ErrResultExtraFile, want: ShadowStatusExtraFile},
		{name: "deletion", err: agentruntime.ErrResultDeletion, want: ShadowStatusDeletion},
		{name: "rename", err: agentruntime.ErrResultRename, want: ShadowStatusRename},
		{name: "contract", err: agentruntime.ErrResultContract, want: ShadowStatusContractViolation},
		{name: "timeout", err: context.DeadlineExceeded, want: ShadowStatusTimeout},
		{name: "cancellation", err: context.Canceled, want: ShadowStatusCancellation},
		{name: "runtime cancellation", err: agentruntime.ErrCancelled, want: ShadowStatusCancellation},
		{name: "cleanup", result: valid, err: agentruntime.ErrCleanupPending, want: ShadowStatusCleanupPending},
		{name: "runtime", err: agentruntime.ErrUnavailable, want: ShadowStatusRuntimeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveShadowStatus(tc.result, tc.err); got != tc.want {
				t.Fatalf("status=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestRuntimeNoResultPrecedesCleanupPending(t *testing.T) {
	bundle := testBundle(t)
	runtime := &Runtime{
		Agent:          &fakeAgentRuntime{err: agentruntime.ErrCleanupPending},
		AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1",
	}
	got, err := runtime.Generate(t.Context(), Spec{
		Repo:   agentruntime.RepoRef{Owner: bundle.Source.Owner, Name: bundle.Source.Name, Ref: bundle.Source.Revision},
		Bundle: bundle, MaxTurns: 5, Timeout: time.Minute,
	})
	if !errors.Is(err, ErrInvalidResult) || got.Status != ShadowStatusNoResult || !got.CleanupPending {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestRuntimePrimaryFailurePrecedesCleanupPending(t *testing.T) {
	bundle := testBundle(t)
	for _, tc := range []struct {
		name string
		err  error
		want ShadowStatus
	}{
		{name: "malformed", err: agentruntime.ErrMalformedResult, want: ShadowStatusMalformedResult},
		{name: "deletion", err: agentruntime.ErrResultDeletion, want: ShadowStatusDeletion},
		{name: "rename", err: agentruntime.ErrResultRename, want: ShadowStatusRename},
		{name: "timeout", err: context.DeadlineExceeded, want: ShadowStatusTimeout},
		{name: "cancellation", err: context.Canceled, want: ShadowStatusCancellation},
		{name: "runtime", err: agentruntime.ErrUnavailable, want: ShadowStatusRuntimeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &Runtime{
				Agent:          &fakeAgentRuntime{err: errors.Join(tc.err, agentruntime.ErrCleanupPending)},
				AgentNamespace: "orka-system", AgentRef: "analysis-agent", AgentVersion: "v1",
			}
			got, err := runtime.Generate(t.Context(), Spec{
				Repo:   agentruntime.RepoRef{Owner: bundle.Source.Owner, Name: bundle.Source.Name, Ref: bundle.Source.Revision},
				Bundle: bundle, MaxTurns: 5, Timeout: time.Minute,
			})
			if got.Status != tc.want || !got.CleanupPending || !errors.Is(err, tc.err) {
				t.Fatalf("result=%+v error=%v", got, err)
			}
		})
	}
}
