package promptauthor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type fakeAgent struct {
	got agentruntime.GenerateSpec
	res agentruntime.GenerateResult
	err error
}

func (f *fakeAgent) Generate(_ context.Context, spec agentruntime.GenerateSpec) (agentruntime.GenerateResult, error) {
	f.got = spec
	if spec.WorkObserver != nil {
		_ = spec.WorkObserver(context.Background(), agentruntime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "prompt-task", UID: "task-uid", ExecutionID: spec.ExecutionID})
	}
	return f.res, f.err
}

func validPrompt() string {
	var b strings.Builder
	b.WriteString("# Project prompt\n\n---\n\n")
	for _, heading := range requiredHeadings {
		b.WriteString(heading + "\n\n- Grounded project-specific guidance.\n\n")
	}
	return b.String()
}

func TestOpenCodeRuntimeGenerate(t *testing.T) {
	agent := &fakeAgent{res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validPrompt()}, Diff: "diff", Output: "safe"}}
	r := &OpenCodeRuntime{Agent: agent}
	got, err := r.Generate(context.Background(), Spec{
		Repo:        agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"},
		Instruction: "Generate the prompt.", Model: "model", Endpoint: "https://example.test/v1", Token: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == "" || got.Runtime != "opencode" || got.Output != "safe" {
		t.Fatalf("result = %+v", got)
	}
	if agent.got.AllowBash || !strings.Contains(agent.got.Instruction, SkillName) || agent.got.Skills[SkillName] == "" {
		t.Fatalf("agent spec = %+v", agent.got)
	}
}

func TestOpenCodeRuntimeUsesAgentOwnedProvider(t *testing.T) {
	agent := &fakeAgent{res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validPrompt()}, Diff: "diff"}}
	r := &OpenCodeRuntime{Agent: agent, Runtime: "orka", AgentOwnsProvider: true}
	got, err := r.Generate(context.Background(), Spec{
		Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "Generate.",
		NativeModel: "host/model", UseAmbientAuth: true, Endpoint: "https://host.invalid/v1", Token: "secret",
		NetworkDomains: []string{"host.invalid:443"}, ExecutionID: "onboard-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "orka" || agent.got.NativeModel != "" || agent.got.UseAmbientAuth || agent.got.Endpoint != "" || agent.got.Token != "" || len(agent.got.NetworkDomains) != 0 {
		t.Fatalf("result=%+v spec=%+v", got, agent.got)
	}
	if agent.got.ExecutionID != "onboard-1" || agent.got.Skills[SkillName] == "" {
		t.Fatalf("execution identity or skill missing: %+v", agent.got)
	}
}

func TestOpenCodeRuntimePreservesValidPromptOnCleanupPending(t *testing.T) {
	agent := &fakeAgent{
		res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validPrompt()}, Diff: "diff"},
		err: fmt.Errorf("%w: deletion pending", agentruntime.ErrCleanupPending),
	}
	got, err := (&OpenCodeRuntime{Agent: agent, Runtime: "orka", AgentOwnsProvider: true}).Generate(context.Background(), Spec{
		Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "Generate.",
	})
	if err != nil || !got.CleanupPending || got.Body == "" || got.CleanupWork == nil || got.CleanupWork.Name != "prompt-task" {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestOpenCodeRuntimePreservesCleanupIdentityWithoutFiles(t *testing.T) {
	agent := &fakeAgent{err: fmt.Errorf("execution failed: %w", agentruntime.ErrCleanupPending)}
	got, err := (&OpenCodeRuntime{Agent: agent, Runtime: "orka", AgentOwnsProvider: true}).Generate(context.Background(), Spec{
		Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "Generate.",
	})
	if err == nil || !got.CleanupPending || got.CleanupWork == nil || got.CleanupWork.Name != "prompt-task" {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestOpenCodeRuntimeRejectsUnsafeChanges(t *testing.T) {
	tests := map[string]agentruntime.GenerateResult{
		"missing prompt": {Files: map[string]string{"other.txt": "x"}},
		"extra file":     {Files: map[string]string{OutputPath: validPrompt(), "other.txt": "x"}},
		"deletion":       {Files: map[string]string{OutputPath: validPrompt()}, Diff: "deleted file mode 100644"},
		"invalid prompt": {Files: map[string]string{OutputPath: "## Architecture\n- only one section"}},
	}
	for name, result := range tests {
		t.Run(name, func(t *testing.T) {
			r := &OpenCodeRuntime{Agent: &fakeAgent{res: result}}
			_, err := r.Generate(context.Background(), Spec{Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "x"})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDiffDestructiveMetadataUsesExactLines(t *testing.T) {
	if diffHasDestructiveChange("+deleted file mode is documentation") {
		t.Fatal("added content was treated as Git deletion metadata")
	}
	if !diffHasDestructiveChange("rename from old.md\nrename to new.md") {
		t.Fatal("rename metadata was not rejected")
	}
}

func TestValidatePromptQuality(t *testing.T) {
	if err := Validate(validPrompt()); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(validPrompt(), "## Architecture\n\n- Grounded project-specific guidance.", "## Architecture\n\n- TODO: fill this.", 1)
	if err := Validate(bad); err == nil {
		t.Fatal("expected TODO-only architecture to fail")
	}
	oversized := strings.Repeat(" ", maxBytes) + validPrompt()
	if err := Validate(oversized); err == nil {
		t.Fatal("leading whitespace bypassed the byte limit")
	}
	fencedMismatch := "````markdown\n" + validPrompt() + "```\n"
	if err := Validate(fencedMismatch); err == nil {
		t.Fatal("shorter fence closer was accepted")
	}
	fencedAnnotatedClose := "```markdown\n" + validPrompt() + "```go\n"
	if err := Validate(fencedAnnotatedClose); err == nil {
		t.Fatal("annotated fence closer was accepted")
	}
	fenced := "```markdown\n" + validPrompt() + "```\n"
	if err := Validate(fenced); err == nil {
		t.Fatal("fenced prompt was accepted")
	}
	otherTODO := strings.Replace(validPrompt(), "## Test and job flavors\n\n- Grounded project-specific guidance.", "## Test and job flavors\n\n- TODO: fill this.", 1)
	if err := Validate(otherTODO); err == nil {
		t.Fatal("TODO-only secondary section was accepted")
	}
	inline := strings.Replace(validPrompt(), "# Project prompt", "# Project prompt\nSee ## Architecture below.", 1)
	inline = strings.Replace(inline, "## Architecture\n\n- Grounded project-specific guidance.", "## Architecture\n", 1)
	if err := Validate(inline); err == nil {
		t.Fatal("inline heading mention bypassed empty section validation")
	}
}

func TestNewOpenCodeRuntimeUsesSandboxedLocalAgent(t *testing.T) {
	r := NewOpenCodeRuntime()
	local, ok := r.Agent.(*agentruntime.LocalAgentRuntime)
	if !ok {
		t.Fatalf("agent = %T, want LocalAgentRuntime", r.Agent)
	}
	if _, ok := local.Sandbox.(*agentruntime.SRTSandbox); !ok {
		t.Fatalf("sandbox = %T, want SRTSandbox", local.Sandbox)
	}
}

func TestOpenCodeRuntimeCopilotNetworkDomains(t *testing.T) {
	agent := &fakeAgent{res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validPrompt()}, Diff: "diff"}}
	r := &OpenCodeRuntime{Agent: agent}
	_, err := r.Generate(context.Background(), Spec{
		Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "Generate.",
		NativeModel: "github-copilot/claude-sonnet-4.6", UseAmbientAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"models.dev:443", "api.githubcopilot.com:443", "github.com:443"}
	if !slices.Equal(agent.got.NetworkDomains, want) {
		t.Fatalf("network domains = %v, want %v", agent.got.NetworkDomains, want)
	}
}

func TestOpenCodeRuntimeRequiresDomainsForOtherNativeProvider(t *testing.T) {
	r := &OpenCodeRuntime{Agent: &fakeAgent{}}
	_, err := r.Generate(context.Background(), Spec{
		Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "Generate.",
		NativeModel: "other/model", UseAmbientAuth: true,
	})
	if err == nil || !strings.Contains(err.Error(), "network domains are required") {
		t.Fatalf("error = %v", err)
	}
}
