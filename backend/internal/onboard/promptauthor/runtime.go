package promptauthor

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	OutputPath = "prompts/system.md"
	SkillName  = "system-prompt-generation"
	maxBytes   = 64 << 10
)

//go:embed skill/system-prompt-generation.md
var systemPromptSkill string

// Spec describes one isolated repository-aware prompt-authoring run.
type Spec struct {
	Repo           agentruntime.RepoRef
	Instruction    string
	Model          string
	NativeModel    string
	UseAmbientAuth bool
	Endpoint       string
	Token          string
	ExtraHeaders   map[string]string
	MaxTurns       int
	Timeout        time.Duration
}

// Result is the validated prompt-authoring output.
type Result struct {
	Body     string
	Runtime  string
	Duration time.Duration
	Output   string
}

// Runtime authors one project prompt in an isolated source workspace.
type Runtime interface {
	Generate(context.Context, Spec) (Result, error)
}

// OpenCodeRuntime delegates prompt authoring to an AgentRuntime.
type OpenCodeRuntime struct {
	Agent agentruntime.AgentRuntime
}

func NewOpenCodeRuntime() *OpenCodeRuntime {
	return &OpenCodeRuntime{Agent: agentruntime.NewLocalAgent()}
}

func diffHasDestructiveChange(diff string) bool {
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "deleted file mode ") || strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to ") {
			return true
		}
	}
	return false
}

func (r *OpenCodeRuntime) Generate(ctx context.Context, spec Spec) (Result, error) {
	if r == nil || r.Agent == nil {
		return Result{}, fmt.Errorf("prompt author: opencode runtime is unavailable")
	}
	if strings.TrimSpace(spec.Instruction) == "" {
		return Result{}, fmt.Errorf("prompt author: instruction is required")
	}
	started := time.Now()
	generated, err := r.Agent.Generate(ctx, agentruntime.GenerateSpec{
		Repo: spec.Repo, Instruction: "Use the " + SkillName + " skill. " + spec.Instruction,
		Model: spec.Model, NativeModel: spec.NativeModel, UseAmbientAuth: spec.UseAmbientAuth,
		Endpoint: spec.Endpoint, Token: spec.Token,
		ExtraHeaders: spec.ExtraHeaders, Skills: map[string]string{SkillName: systemPromptSkill},
		MaxTurns: spec.MaxTurns, AllowBash: false, Timeout: spec.Timeout,
	})
	result := Result{Runtime: "opencode", Duration: time.Since(started), Output: generated.Output}
	if err != nil {
		return result, err
	}
	if diffHasDestructiveChange(generated.Diff) {
		return result, fmt.Errorf("prompt author: agent deleted or renamed repository files")
	}
	if len(generated.Files) != 1 {
		return result, fmt.Errorf("prompt author: agent changed %d files, want only %s", len(generated.Files), OutputPath)
	}
	body, ok := generated.Files[OutputPath]
	if !ok {
		return result, fmt.Errorf("prompt author: agent did not write %s", OutputPath)
	}
	if err := Validate(body); err != nil {
		return result, err
	}
	result.Body = body
	return result, nil
}

// SkillContent returns the engine-owned prompt-generation skill.
func SkillContent() string { return systemPromptSkill }
