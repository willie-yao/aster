package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

// GenerateSpec is a one-shot code-generation run: materialize Repo at its ref,
// run a coding-agent CLI with Instruction in the workspace, and return the files
// the agent changed. It is the generative counterpart to Spec (which runs a
// fixed command for verification).
type GenerateSpec struct {
	Repo RepoRef
	// Instruction is the fix task handed to the coding agent.
	Instruction string
	// Model is the custom endpoint model id. Empty uses the CLI's own default.
	Model string
	// NativeModel is an OpenCode provider/model reference such as
	// github-copilot/claude-sonnet-4.6. It is mutually exclusive with Model.
	NativeModel string
	// UseAmbientAuth copies only NativeModel's configured OpenCode credential into
	// the isolated home.
	UseAmbientAuth bool
	// Endpoint is the OpenAI-compatible base the model is served from. A full
	// chat-completions URL is accepted; the trailing /chat/completions is
	// stripped to the base the CLI expects.
	Endpoint string
	// Token authenticates the model endpoint.
	Token string
	// ExtraHeaders are sent by the isolated model provider.
	ExtraHeaders map[string]string
	// Skills are engine-owned instructions made available through the backend's
	// bounded native mechanism. Local OpenCode writes skill files; remote
	// backends may include the validated contents in their trusted prompt.
	Skills map[string]string
	// MaxTurns bounds the agent loop; zero uses the CLI default.
	MaxTurns int
	// AllowBash lets the agent run shell commands (build, tests) while fixing.
	AllowBash bool
	// NetworkDomains are additional outbound destinations required by the task.
	// The custom endpoint host is added automatically.
	NetworkDomains []string
	// Timeout bounds the whole run (clone plus agent). Zero uses defaultTimeout.
	Timeout time.Duration
	// ExpectedBaseSHA is the immutable base the caller will independently verify.
	ExpectedBaseSHA string
	// MaxSteps bounds provider-neutral executor actions. Zero uses MaxTurns.
	MaxSteps int
	// MaxFiles bounds the changed-file result.
	MaxFiles int
	// ModelProvider is non-secret configuration for the Agent Sandbox provider.
	ModelProvider modelprovider.Config
	// CommandPolicy lists the exact commands an external executor may run.
	CommandPolicy CommandPolicy
	// OutputLimitBytes bounds the structured executor result.
	OutputLimitBytes int64
	// ExecutionID scopes externally managed work to one action request.
	ExecutionID string
	// WorkObserver records planned and observed external runtime identities.
	WorkObserver WorkObserver
}

// WorkRef identifies one externally managed runtime execution.
type WorkRef struct {
	Backend     string `json:"backend"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	UID         string `json:"uid,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
}

// WorkObserver persists external runtime identity as it becomes available.
type WorkObserver func(context.Context, WorkRef) error

// GenerateTelemetry records lifecycle facts observable through the runtime contract.
type GenerateTelemetry struct {
	ProviderCredentialMode  string
	ProviderAPI             string
	ProviderReasoningEffort string
	TaskFinalized           bool
	TaskFinalizedMs         int64
	ResultAvailable         bool
	ResultAvailableMs       int64
	SchedulingMs            int64
	SchedulingAvailable     bool
	StagingMs               int64
	StagingAvailable        bool
	ExecutionMs             int64
	ExecutionAvailable      bool
	PublicationMs           int64
	PublicationAvailable    bool
	PhaseTimingStatus       string
	FailurePhase            string
	FailureCode             string
	ExecutorStarted         bool
	FinalizationChecked     bool
	FinalizationValid       bool
	CleanupCompleted        bool
	CleanupDurationMs       int64
	TokenUsageAvailable     bool
	CostAvailable           bool
	UsageStatus             string
}

// AgentRuntime materializes a disposable workspace and runs a coding agent that
// edits it, returning the changed files. Each backend defines its own process
// isolation and tears down the workspace before returning.
type AgentRuntime interface {
	Generate(ctx context.Context, spec GenerateSpec) (GenerateResult, error)
}

// ManagedAgentRuntime can stop one exact external execution identity.
type ManagedAgentRuntime interface {
	AgentRuntime
	Cleanup(context.Context, WorkRef) error
}

func gitChanges(ctx context.Context, dir, token string) (map[string]string, string, error) {
	if err := gitRun(ctx, dir, "add", "-A"); err != nil {
		return nil, "", err
	}
	names, err := gitOut(ctx, dir, "diff", "--no-ext-diff", "--cached", "--name-only")
	if err != nil {
		return nil, "", err
	}
	files := map[string]string{}
	for _, p := range strings.Split(strings.TrimSpace(names), "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			// A staged deletion has no file on disk; skip it.
			if os.IsNotExist(err) {
				continue
			}
			return nil, "", fmt.Errorf("runtime: read changed %s: %w", p, err)
		}
		files[p] = string(b)
	}
	diff, err := gitOut(ctx, dir, "diff", "--no-ext-diff", "--cached")
	if err != nil {
		return nil, "", err
	}
	return files, redactToken(diff, token), nil
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	gitArgs := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir = dir
	cmd.Env = gitSafeEnvironment()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime: git %s: %w: %s", args[0], err, tail(buf.String(), 2048))
	}
	return nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	gitArgs := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir = dir
	cmd.Env = gitSafeEnvironment()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("runtime: git %s: %w: %s", args[0], err, tail(errb.String(), 2048))
	}
	return out.String(), nil
}

// opencodeSandboxSpec builds a non-interactive OpenCode invocation and its
// process resource policy. Provider config stays outside the workspace diff.
