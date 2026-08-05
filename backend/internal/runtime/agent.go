package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	opencodeConfigEnv         = "OPENCODE_CONFIG"
	opencodeDisableProjectEnv = "OPENCODE_DISABLE_PROJECT_CONFIG"
	opencodeDisableUpdateEnv  = "OPENCODE_DISABLE_AUTOUPDATE"
	opencodeDisableSkillsEnv  = "OPENCODE_DISABLE_EXTERNAL_SKILLS"
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
	// Skills are engine-owned OpenCode skills injected into the isolated home.
	Skills map[string]string
	// MaxTurns bounds the agent loop; zero uses the CLI default.
	MaxTurns int
	// AllowBash lets the agent run shell commands (build, tests) while fixing.
	AllowBash bool
	// Timeout bounds the whole run (clone plus agent). Zero uses defaultTimeout.
	Timeout time.Duration
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

// GenerateResult is the outcome of a generative run.
type GenerateResult struct {
	// Files maps repo-relative path to full new content for every file the agent
	// added or modified. Deletions are not represented.
	Files map[string]string
	// Diff is the unified diff of the change, for the PR body and preview.
	Diff string
	// Output is the tail of the CLI's own output, redacted and bounded, for
	// debugging.
	Output string
}

// AgentRuntime materializes a workspace and runs a coding agent that edits it,
// returning the changed files. Implementations isolate the workspace from the
// caller and tear it down before returning, mirroring Runtime. LocalAgentRuntime
// ships first; a pod-backed impl can be added later behind this interface.
type AgentRuntime interface {
	Generate(ctx context.Context, spec GenerateSpec) (GenerateResult, error)
}

// ManagedAgentRuntime can stop one exact external execution identity.
type ManagedAgentRuntime interface {
	AgentRuntime
	Cleanup(context.Context, WorkRef) error
}

// LocalAgentRuntime runs a coding-agent CLI on the local host: it shallow-clones
// the repo into a temp directory, runs the CLI against the model endpoint with
// an isolated config, and returns the files the agent changed. It provides no
// isolation beyond a scratch directory, so it suits a trusted CI host; a
// pod-backed AgentRuntime is the path to isolated execution at scale. Returns
// ErrUnavailable when git or the CLI binary is not on PATH.
type LocalAgentRuntime struct {
	// Bin is the coding-agent CLI. Defaults to "opencode".
	Bin string
	// buildCmd constructs the CLI invocation in workdir with an isolated home.
	// Overridable in tests; nil uses the opencode builder.
	buildCmd func(ctx context.Context, spec GenerateSpec, workdir, home string) (*exec.Cmd, error)
}

// NewLocalAgent returns a LocalAgentRuntime driving the opencode CLI.
func NewLocalAgent() *LocalAgentRuntime {
	return &LocalAgentRuntime{Bin: "opencode"}
}

// Generate materializes spec.Repo, runs the coding agent against it, and returns
// the files it changed. The workspace is removed before returning.
func (r *LocalAgentRuntime) Generate(ctx context.Context, spec GenerateSpec) (GenerateResult, error) {
	if strings.TrimSpace(spec.Instruction) == "" {
		return GenerateResult{}, fmt.Errorf("runtime: empty instruction")
	}
	if spec.Repo.Owner == "" || spec.Repo.Name == "" || spec.Repo.Ref == "" {
		return GenerateResult{}, fmt.Errorf("runtime: repo owner, name, and ref are required")
	}
	if spec.Model != "" && spec.NativeModel != "" {
		return GenerateResult{}, fmt.Errorf("runtime: model and native model are mutually exclusive")
	}
	if spec.UseAmbientAuth && spec.NativeModel == "" {
		return GenerateResult{}, fmt.Errorf("runtime: ambient auth requires a native model")
	}
	bin := r.Bin
	if bin == "" {
		bin = "opencode"
	}
	if _, err := exec.LookPath("git"); err != nil {
		return GenerateResult{}, fmt.Errorf("%w: git not found", ErrUnavailable)
	}
	build := r.buildCmd
	if build == nil {
		build = opencodeCmd(bin)
		if _, err := exec.LookPath(bin); err != nil {
			return GenerateResult{}, fmt.Errorf("%w: %s not found", ErrUnavailable, bin)
		}
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	work, err := os.MkdirTemp("", "pad-agent-*")
	if err != nil {
		return GenerateResult{}, fmt.Errorf("runtime: temp dir: %w", err)
	}
	defer os.RemoveAll(work)
	home, err := os.MkdirTemp("", "pad-agent-home-*")
	if err != nil {
		return GenerateResult{}, fmt.Errorf("runtime: temp home: %w", err)
	}
	defer os.RemoveAll(home)

	if err := materialize(ctx, work, spec.Repo); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return GenerateResult{}, fmt.Errorf("%w: clone timed out", ErrUnavailable)
		}
		return GenerateResult{}, err
	}

	cmd, err := build(ctx, spec, work, home)
	if err != nil {
		return GenerateResult{}, err
	}
	out, runErr := cmd.CombinedOutput()
	output := redactToken(tail(string(out), maxOutputBytes), spec.Token)
	if ctx.Err() == context.DeadlineExceeded {
		return GenerateResult{Output: output}, fmt.Errorf("runtime: agent timed out")
	}
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return GenerateResult{Output: output}, fmt.Errorf("runtime: running %s: %w", bin, runErr)
		}
		// A non-zero CLI exit still may have produced edits; fall through and let
		// the diff decide. If nothing changed the caller sees an empty result.
	}

	files, diff, err := gitChanges(ctx, work, spec.Repo.Token)
	if err != nil {
		return GenerateResult{Output: output}, err
	}
	return GenerateResult{Files: files, Diff: diff, Output: output}, nil
}

// gitChanges stages every change in dir and returns the modified/added files as
// a path->content map plus the unified diff. Deletions are dropped from the map
// (the fix path overlays content, it does not remove files).
func gitChanges(ctx context.Context, dir, token string) (map[string]string, string, error) {
	if err := gitRun(ctx, dir, "add", "-A"); err != nil {
		return nil, "", err
	}
	names, err := gitOut(ctx, dir, "diff", "--cached", "--name-only")
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
	diff, err := gitOut(ctx, dir, "diff", "--cached")
	if err != nil {
		return nil, "", err
	}
	return files, redactToken(diff, token), nil
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime: git %s: %w: %s", args[0], err, tail(buf.String(), 2048))
	}
	return nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("runtime: git %s: %w: %s", args[0], err, tail(errb.String(), 2048))
	}
	return out.String(), nil
}

// opencodeCmd returns a buildCmd that runs the opencode CLI in non-interactive
// mode against the spec's model endpoint. Provider config is written to an
// isolated home so it never lands in the workspace or the resulting diff.
func opencodeCmd(bin string) func(ctx context.Context, spec GenerateSpec, workdir, home string) (*exec.Cmd, error) {
	return func(ctx context.Context, spec GenerateSpec, workdir, home string) (*exec.Cmd, error) {
		if spec.UseAmbientAuth {
			if err := writeOpencodeAuth(home, spec.NativeModel); err != nil {
				return nil, err
			}
		}
		if err := writeOpencodeSkills(home, spec.Skills); err != nil {
			return nil, err
		}
		if err := writeOpencodeConfig(home, spec); err != nil {
			return nil, err
		}
		// --dir pins opencode's project root to the clone. opencode's `run` can
		// otherwise attach to an ambient server and ignore the process cwd,
		// writing edits outside the workspace.
		args := []string{"run", "--dir", workdir, "--format", "json", "--agent", "build"}
		if spec.NativeModel != "" {
			args = append(args, "--model", spec.NativeModel)
		} else if spec.Model != "" {
			args = append(args, "--model", "engine/"+spec.Model)
		}
		args = append(args, spec.Instruction)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = workdir
		cmd.Env = isolatedOpencodeEnv(home)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = waitDelay
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return os.ErrProcessDone
		}
		return cmd, nil
	}
}

func isolatedOpencodeEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+9)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, "OPENCODE_") {
			continue
		}
		switch name {
		case "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		opencodeConfigEnv+"="+filepath.Join(home, ".config", "opencode", "opencode.json"),
		opencodeDisableProjectEnv+"=true",
		opencodeDisableUpdateEnv+"=true",
		opencodeDisableSkillsEnv+"=true",
	)
}

func writeOpencodeAuth(home, nativeModel string) error {
	provider, _, ok := strings.Cut(nativeModel, "/")
	if !ok || provider == "" {
		return fmt.Errorf("runtime: invalid native model %q", nativeModel)
	}
	dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("runtime: resolve opencode auth home: %w", err)
		}
		dataHome = filepath.Join(userHome, ".local", "share")
	}
	source := filepath.Join(dataHome, "opencode", "auth.json")
	raw, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("runtime: read opencode auth: %w", err)
	}
	var credentials map[string]json.RawMessage
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return fmt.Errorf("runtime: decode opencode auth: %w", err)
	}
	credential, ok := credentials[provider]
	if !ok {
		return fmt.Errorf("runtime: opencode auth has no credential for %q", provider)
	}
	targetDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return fmt.Errorf("runtime: opencode auth dir: %w", err)
	}
	filtered, err := json.Marshal(map[string]json.RawMessage{provider: credential})
	if err != nil {
		return fmt.Errorf("runtime: encode opencode auth: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "auth.json"), filtered, 0o600); err != nil {
		return fmt.Errorf("runtime: write opencode auth: %w", err)
	}
	return nil
}

func writeOpencodeSkills(home string, skills map[string]string) error {
	for name, content := range skills {
		if !validSkillName(name) {
			return fmt.Errorf("runtime: invalid opencode skill name %q", name)
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("runtime: empty opencode skill %q", name)
		}
		dir := filepath.Join(home, ".config", "opencode", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("runtime: opencode skill dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
			return fmt.Errorf("runtime: write opencode skill: %w", err)
		}
	}
	return nil
}

func validSkillName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// writeOpencodeConfig writes an opencode config to home's XDG config dir defining
// a single OpenAI-compatible provider "engine" pointed at the spec's endpoint,
// with edit and (optionally) bash permissions pre-approved for non-interactive
// runs.
func writeOpencodeConfig(home string, spec GenerateSpec) error {
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("runtime: opencode config dir: %w", err)
	}
	bashPerm := "deny"
	if spec.AllowBash {
		bashPerm = "allow"
	}
	models := map[string]any{}
	if spec.Model != "" {
		// opencode requires context+output together when a limit is set. Cap
		// output so opencode does not send an oversized max_tokens default a
		// metadata-less custom model may reject; ample for a minimal fix. Context
		// is opencode's compaction threshold only; the model enforces its own.
		models[spec.Model] = map[string]any{
			"limit": map[string]any{"context": 128000, "output": 8192},
		}
	}
	agents := map[string]any{}
	if spec.MaxTurns > 0 {
		agents["build"] = map[string]any{"steps": spec.MaxTurns}
	}
	providerOptions := map[string]any{
		"baseURL": openAIBase(spec.Endpoint),
		"apiKey":  spec.Token,
	}
	if len(spec.ExtraHeaders) > 0 {
		headers := make(map[string]string, len(spec.ExtraHeaders))
		for key, value := range spec.ExtraHeaders {
			headers[key] = value
		}
		providerOptions["headers"] = headers
	}
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"share":      "disabled",
		"autoupdate": false,
		"snapshot":   false,
		"agent":      agents,
		"permission": map[string]any{
			"edit": "allow",
			"bash": bashPerm,
		},
	}
	if spec.NativeModel == "" {
		cfg["provider"] = map[string]any{
			"engine": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"name":    "engine",
				"options": providerOptions,
				"models":  models,
			},
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: opencode config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), b, 0o600); err != nil {
		return fmt.Errorf("runtime: write opencode config: %w", err)
	}
	return nil
}

// openAIBase reduces a chat-completions endpoint to the base URL the AI SDK's
// openai-compatible provider expects, which appends /chat/completions itself.
func openAIBase(endpoint string) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	e = strings.TrimSuffix(e, "/chat/completions")
	e = strings.TrimSuffix(e, "/responses")
	return e
}
