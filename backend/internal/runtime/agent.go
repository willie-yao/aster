package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// LocalAgentRuntime shallow-clones a repository into temporary directories,
// runs a coding-agent CLI through ProcessSandbox, and returns the changed files.
// NewLocalAgent requires the enforcing srt backend and never falls back to
// direct execution. Returns ErrUnavailable when git or the CLI binary is not on
// PATH.
type LocalAgentRuntime struct {
	// Bin is the coding-agent CLI. Defaults to "opencode".
	Bin string
	// Sandbox constructs the agent process. It is required.
	Sandbox ProcessSandbox
	// buildSpec constructs the CLI invocation and resource policy.
	// Overridable in tests; nil uses the opencode builder.
	buildSpec func(ctx context.Context, spec GenerateSpec, workdir, home, temp string) (SandboxSpec, error)
}

// NewLocalAgent returns a LocalAgentRuntime driving the opencode CLI.
func NewLocalAgent() *LocalAgentRuntime {
	return &LocalAgentRuntime{Bin: "opencode", Sandbox: NewSRTSandboxFromEnv()}
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
	if r.Sandbox == nil {
		return GenerateResult{}, fmt.Errorf("runtime: process sandbox is required")
	}
	build := r.buildSpec
	if build == nil {
		resolvedBin, err := exec.LookPath(bin)
		if err != nil {
			return GenerateResult{}, fmt.Errorf("%w: %s not found", ErrUnavailable, bin)
		}
		build = opencodeSandboxSpec(resolvedBin)
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	work, home, temp, cleanup, err := agentTempDirs(r.Sandbox)
	if err != nil {
		return GenerateResult{}, err
	}
	defer cleanup()

	if err := materialize(ctx, work, spec.Repo); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return GenerateResult{}, fmt.Errorf("%w: clone timed out", ErrUnavailable)
		}
		return GenerateResult{}, err
	}

	processSpec, err := build(ctx, spec, work, home, temp)
	if err != nil {
		return GenerateResult{}, err
	}
	out, runErr := runSandboxProcess(ctx, r.Sandbox, processSpec)
	if ctx.Err() == nil && opencodeMigrationOnly(out) {
		retryOut, retryErr := runSandboxProcess(ctx, r.Sandbox, processSpec)
		out = append(append(out, '\n'), retryOut...)
		runErr = retryErr
	}
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

func runSandboxProcess(ctx context.Context, sandbox ProcessSandbox, spec SandboxSpec) ([]byte, error) {
	if runner, ok := sandbox.(ProcessSandboxRunner); ok {
		return runner.Run(ctx, spec)
	}
	cmd, err := sandbox.Command(ctx, spec)
	if err != nil {
		return nil, err
	}
	return cmd.CombinedOutput()
}

func opencodeMigrationOnly(output []byte) bool {
	text := string(output)
	if !strings.Contains(text, "Performing one time database migration") || !strings.Contains(text, "Database migration complete.") {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			return false
		}
	}
	return true
}

type agentTempBaseProvider interface {
	agentTempBase() (string, error)
}

func agentTempDirs(sandbox ProcessSandbox) (work, home, temp string, cleanup func(), err error) {
	if provider, ok := sandbox.(agentTempBaseProvider); ok {
		base, err := provider.agentTempBase()
		if err != nil {
			return "", "", "", nil, err
		}
		root, err := os.MkdirTemp(base, "agent-*")
		if err != nil {
			return "", "", "", nil, fmt.Errorf("runtime: sandbox temp root: %w", err)
		}
		cleanup := func() { _ = os.RemoveAll(root) }
		work = filepath.Join(root, "work")
		home = filepath.Join(root, "home")
		temp = filepath.Join(root, "tmp")
		for _, dir := range []string{work, home, temp} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				cleanup()
				return "", "", "", nil, fmt.Errorf("runtime: sandbox temp dir: %w", err)
			}
		}
		return work, home, temp, cleanup, nil
	}
	work, err = os.MkdirTemp("", "pad-agent-*")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("runtime: temp dir: %w", err)
	}
	home, err = os.MkdirTemp("", "pad-agent-home-*")
	if err != nil {
		_ = os.RemoveAll(work)
		return "", "", "", nil, fmt.Errorf("runtime: temp home: %w", err)
	}
	temp, err = os.MkdirTemp("", "pad-agent-tmp-*")
	if err != nil {
		_ = os.RemoveAll(work)
		_ = os.RemoveAll(home)
		return "", "", "", nil, fmt.Errorf("runtime: temp runtime dir: %w", err)
	}
	cleanup = func() {
		_ = os.RemoveAll(work)
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(temp)
	}
	return work, home, temp, cleanup, nil
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

// opencodeSandboxSpec builds a non-interactive OpenCode invocation and its
// process resource policy. Provider config stays outside the workspace diff.
func opencodeSandboxSpec(bin string) func(context.Context, GenerateSpec, string, string, string) (SandboxSpec, error) {
	return func(_ context.Context, spec GenerateSpec, workdir, home, temp string) (SandboxSpec, error) {
		if spec.UseAmbientAuth {
			if err := writeOpencodeAuth(home, spec.NativeModel); err != nil {
				return SandboxSpec{}, err
			}
		}
		if err := writeOpencodeSkills(home, spec.Skills); err != nil {
			return SandboxSpec{}, err
		}
		if err := writeOpencodeConfig(home, spec); err != nil {
			return SandboxSpec{}, err
		}
		// --dir pins opencode's project root to the clone. opencode's `run` can
		// otherwise attach to an ambient server and ignore the process cwd.
		args := []string{bin, "run", "--dir", workdir, "--format", "json", "--agent", "build"}
		if spec.NativeModel != "" {
			args = append(args, "--model", spec.NativeModel)
		} else if spec.Model != "" {
			args = append(args, "--model", "engine/"+spec.Model)
		}
		args = append(args, spec.Instruction)
		networkDomains, err := opencodeNetworkDomains(spec)
		if err != nil {
			return SandboxSpec{}, err
		}
		return SandboxSpec{
			Command:        args,
			WorkDir:        workdir,
			HomeDir:        home,
			TempDir:        temp,
			Environment:    isolatedOpencodeEnv(home, temp),
			ReadPaths:      opencodeReadPaths(bin, workdir, home, temp),
			WritePaths:     []string{workdir, home, temp},
			NetworkDomains: networkDomains,
		}, nil
	}
}

func opencodeReadPaths(bin, workdir, home, temp string) []string {
	paths := []string{workdir, home, temp, bin}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil && resolved != bin {
		paths = append(paths, resolved)
	}
	for _, name := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			paths = append(paths, value)
		}
	}
	for _, value := range filepath.SplitList(os.Getenv("SSL_CERT_DIR")) {
		if value = strings.TrimSpace(value); value != "" {
			paths = append(paths, value)
		}
	}
	return uniqueStrings(paths)
}

func opencodeNetworkDomains(spec GenerateSpec) ([]string, error) {
	domains := append([]string(nil), spec.NetworkDomains...)
	if strings.TrimSpace(spec.Endpoint) == "" {
		return uniqueStrings(domains), nil
	}
	endpoint, err := url.Parse(spec.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Hostname() == "" {
		return nil, fmt.Errorf("runtime: invalid model endpoint")
	}
	host := endpoint.Hostname()
	if port := endpoint.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	return uniqueStrings(append(domains, host)), nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func isolatedOpencodeEnv(home, temp string) []string {
	env := make([]string, 0, 16)
	for _, name := range []string{
		"PATH",
		"LANG", "LC_ALL", "LC_CTYPE",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return append(env,
		"HOME="+home,
		"TMPDIR="+temp,
		"TMP="+temp,
		"TEMP="+temp,
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
