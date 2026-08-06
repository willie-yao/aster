package promptauthor

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// ErrOutputValidation marks an agent result that failed the output contract.
var ErrOutputValidation = errors.New("prompt author output validation failed")

const (
	OutputPath = "prompts/system.md"
	SkillName  = "system-prompt-generation"
	maxBytes   = 64 << 10
)

var githubCopilotNetworkDomains = []string{
	"models.dev:443",
	"api.githubcopilot.com:443",
	"github.com:443",
}

//go:embed skill/system-prompt-generation.md
var systemPromptSkill string

// Spec describes one temporary repository-aware prompt-authoring run.
type Spec struct {
	Repo           agentruntime.RepoRef
	Instruction    string
	Model          string
	NativeModel    string
	UseAmbientAuth bool
	Endpoint       string
	Token          string
	ExtraHeaders   map[string]string
	NetworkDomains []string
	MaxTurns       int
	Timeout        time.Duration
	ExecutionID    string
}

// Result is the validated prompt-authoring output.
type Result struct {
	Body           string
	Runtime        string
	Duration       time.Duration
	Output         string
	CleanupPending bool
	CleanupWork    *agentruntime.WorkRef
}

// Runtime authors one project prompt in an isolated source workspace.
type Runtime interface {
	Generate(context.Context, Spec) (Result, error)
}

// OpenCodeRuntime delegates prompt authoring to an AgentRuntime.
type OpenCodeRuntime struct {
	Agent             agentruntime.AgentRuntime
	Runtime           string
	AgentOwnsProvider bool
}

func NewOpenCodeRuntime() *OpenCodeRuntime {
	return &OpenCodeRuntime{Agent: agentruntime.NewLocalAgent(), Runtime: "opencode"}
}

func promptNetworkDomains(spec Spec) ([]string, error) {
	domains := append([]string(nil), spec.NetworkDomains...)
	if spec.NativeModel != "" {
		provider, _, ok := strings.Cut(spec.NativeModel, "/")
		if !ok || provider == "" {
			return nil, fmt.Errorf("prompt author: invalid native model %q", spec.NativeModel)
		}
		if provider == "github-copilot" {
			domains = append(domains, githubCopilotNetworkDomains...)
		} else if len(domains) == 0 {
			return nil, fmt.Errorf("prompt author: network domains are required for native provider %q", provider)
		}
	}
	normalized, err := agentruntime.NormalizeNetworkDomains(domains)
	if err != nil {
		return nil, fmt.Errorf("prompt author: network domains: %w", err)
	}
	return normalized, nil
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
	var networkDomains []string
	var err error
	if !r.AgentOwnsProvider {
		networkDomains, err = promptNetworkDomains(spec)
		if err != nil {
			return Result{}, err
		}
	}
	started := time.Now()
	generateSpec := agentruntime.GenerateSpec{
		Repo: spec.Repo, Instruction: "Use the " + SkillName + " skill. " + spec.Instruction,
		Skills:   map[string]string{SkillName: systemPromptSkill},
		MaxTurns: spec.MaxTurns, AllowBash: false, Timeout: spec.Timeout,
		ExecutionID: spec.ExecutionID,
	}
	var observedWork agentruntime.WorkRef
	if r.AgentOwnsProvider {
		generateSpec.WorkObserver = func(_ context.Context, work agentruntime.WorkRef) error {
			observedWork = work
			return nil
		}
	}
	if !r.AgentOwnsProvider {
		generateSpec.Model, generateSpec.NativeModel, generateSpec.UseAmbientAuth = spec.Model, spec.NativeModel, spec.UseAmbientAuth
		generateSpec.Endpoint, generateSpec.Token, generateSpec.ExtraHeaders = spec.Endpoint, spec.Token, spec.ExtraHeaders
		generateSpec.NetworkDomains = networkDomains
	}
	generated, err := r.Agent.Generate(ctx, generateSpec)
	runtimeName := strings.TrimSpace(r.Runtime)
	if runtimeName == "" {
		runtimeName = "opencode"
	}
	result := Result{Runtime: runtimeName, Duration: time.Since(started), Output: generated.Output}
	cleanupPending := errors.Is(err, agentruntime.ErrCleanupPending)
	result.CleanupPending = cleanupPending
	if cleanupPending && observedWork.Name != "" {
		work := observedWork
		result.CleanupWork = &work
	}
	if err != nil && (!cleanupPending || len(generated.Files) == 0) {
		return result, err
	}
	if diffHasDestructiveChange(generated.Diff) {
		return result, fmt.Errorf("%w: agent deleted or renamed repository files", ErrOutputValidation)
	}
	if len(generated.Files) != 1 {
		return result, fmt.Errorf("%w: agent changed %d files, want only %s", ErrOutputValidation, len(generated.Files), OutputPath)
	}
	body, ok := generated.Files[OutputPath]
	if !ok {
		return result, fmt.Errorf("%w: agent did not write %s", ErrOutputValidation, OutputPath)
	}
	if err := Validate(body); err != nil {
		return result, fmt.Errorf("%w: %v", ErrOutputValidation, err)
	}
	result.Body = body
	return result, nil
}

// SkillContent returns the engine-owned prompt-generation skill.
func SkillContent() string { return systemPromptSkill }
