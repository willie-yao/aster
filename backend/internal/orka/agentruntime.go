package orka

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	FixerManagedByValue = "orka-fixer"
	defaultFixNamespace = "orka-system"
	defaultFixVersion   = "v1"
	serviceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	maxFixResultBytes   = 4 << 20
)

// StructuredResult is the generation result returned by an Orka agent Task.
type StructuredResult struct {
	Version    int      `json:"version"`
	Summary    string   `json:"summary"`
	BaseSHA    string   `json:"baseSHA"`
	Diff       string   `json:"diff"`
	Files      []string `json:"files"`
	PushBranch string   `json:"pushBranch,omitempty"`
}

type taskAPI interface {
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	TaskPhase(context.Context, string, string) (string, error)
	Delete(context.Context, schema.GroupVersionResource, string, string) error
}

type resultAPI interface {
	Result(context.Context, string, string) (string, bool, error)
}

// AgentOptions configures the generation-only Orka runtime.
type AgentOptions struct {
	Namespace  string
	AgentRef   string
	GitSecret  string
	Version    string
	MaxRetries int
	PollEvery  time.Duration
}

// AgentRuntime implements runtime.AgentRuntime with an Orka agent Task.
type AgentRuntime struct {
	kube      taskAPI
	results   resultAPI
	opts      AgentOptions
	applyDiff func(context.Context, runtime.RepoRef, string) (map[string]string, string, error)
}

// NewAgentRuntime builds an Orka-backed coding-agent runtime.
func NewAgentRuntime(kube *KubeClient, results *ResultClient, opts AgentOptions) *AgentRuntime {
	return &AgentRuntime{kube: kube, results: results, opts: normalizeAgentOptions(opts)}
}

// FromEnvConfig configures NewAgentRuntimeFromEnv.
type FromEnvConfig struct {
	Namespace   string
	AgentRef    string
	API         string
	APIToken    string
	GitSecret   string
	Version     string
	MaxRetries  int
	KubeContext string
}

// NewAgentRuntimeFromEnv builds Kubernetes and Orka REST clients for fix generation.
func NewAgentRuntimeFromEnv(cfg FromEnvConfig) (*AgentRuntime, error) {
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	if cfg.Namespace == "" {
		cfg.Namespace = defaultFixNamespace
	}
	cfg.AgentRef = strings.TrimSpace(cfg.AgentRef)
	cfg.API = strings.TrimSpace(cfg.API)
	if cfg.AgentRef == "" || cfg.API == "" {
		return nil, fmt.Errorf("orka fix runtime requires agent_ref and api")
	}
	rc, err := RESTConfig(cfg.KubeContext)
	if err != nil {
		return nil, fmt.Errorf("orka fix runtime: kube config: %w", err)
	}
	kube, err := NewKubeClient(rc)
	if err != nil {
		return nil, fmt.Errorf("orka fix runtime: kube client: %w", err)
	}
	kube.Manager = FixerManagedByValue
	return NewAgentRuntime(kube, NewResultClient(cfg.API, resolveOrkaAPIToken(cfg.APIToken)), AgentOptions{
		Namespace:  cfg.Namespace,
		AgentRef:   cfg.AgentRef,
		GitSecret:  strings.TrimSpace(cfg.GitSecret),
		Version:    strings.TrimSpace(cfg.Version),
		MaxRetries: cfg.MaxRetries,
	}), nil
}

func resolveOrkaAPIToken(explicit string) string {
	if token := strings.TrimSpace(explicit); token != "" {
		return token
	}
	if file := strings.TrimSpace(os.Getenv("ORKA_API_TOKEN_FILE")); file != "" {
		if data, err := os.ReadFile(file); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	if data, err := os.ReadFile(serviceAccountToken); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func normalizeAgentOptions(opts AgentOptions) AgentOptions {
	if strings.TrimSpace(opts.Namespace) == "" {
		opts.Namespace = defaultFixNamespace
	}
	if strings.TrimSpace(opts.Version) == "" {
		opts.Version = defaultFixVersion
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	return opts
}

var (
	_ runtime.AgentRuntime = (*AgentRuntime)(nil)
	_ taskAPI              = (*KubeClient)(nil)
	_ resultAPI            = (*ResultClient)(nil)
)

// Generate runs the fix agent, validates its structured result, and reapplies
// the captured diff to the pinned base before returning changed files.
func (r *AgentRuntime) Generate(ctx context.Context, spec runtime.GenerateSpec) (runtime.GenerateResult, error) {
	if r == nil || r.kube == nil || r.results == nil {
		return runtime.GenerateResult{}, fmt.Errorf("%w: Orka runtime is not configured", runtime.ErrUnavailable)
	}
	if strings.TrimSpace(spec.Instruction) == "" {
		return runtime.GenerateResult{}, fmt.Errorf("orka: empty instruction")
	}
	if spec.Repo.Owner == "" || spec.Repo.Name == "" || spec.Repo.Ref == "" || r.opts.AgentRef == "" {
		return runtime.GenerateResult{}, fmt.Errorf("orka: repo owner, name, ref, and agent_ref are required")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	name := FixTaskName(spec, r.opts)
	if phase, err := r.kube.TaskPhase(ctx, r.opts.Namespace, name); err == nil && (phase == "Failed" || phase == "Cancelled") {
		if err := r.deleteFailedTask(ctx, name); err != nil {
			return runtime.GenerateResult{}, err
		}
	} else if err != nil && !IsNotFound(err) {
		return runtime.GenerateResult{}, fmt.Errorf("%w: reading prior fix Task: %v", runtime.ErrUnavailable, err)
	}
	if err := r.kube.Apply(ctx, TasksGVR, r.opts.Namespace, r.buildTask(name, spec)); err != nil {
		return runtime.GenerateResult{}, fmt.Errorf("%w: applying fix Task: %v", runtime.ErrUnavailable, err)
	}
	phase, err := r.waitTerminal(ctx, name)
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	if phase != "Succeeded" {
		return runtime.GenerateResult{}, fmt.Errorf("orka fix Task %s ended %s", name, phase)
	}

	raw, err := r.waitResult(ctx, name)
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	var result StructuredResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return runtime.GenerateResult{}, fmt.Errorf("orka fix Task %s: parsing result: %w", name, err)
	}
	if err := validateStructuredResult(result, spec.Repo.Ref); err != nil {
		return runtime.GenerateResult{Output: result.Summary}, fmt.Errorf("orka fix Task %s: %w", name, err)
	}
	if strings.TrimSpace(result.Diff) == "" {
		return runtime.GenerateResult{Output: result.Summary}, nil
	}

	apply := r.applyDiff
	if apply == nil {
		apply = runtime.ApplyDiff
	}
	files, diff, err := apply(ctx, spec.Repo, result.Diff)
	if err != nil {
		return runtime.GenerateResult{Output: result.Summary}, fmt.Errorf("reconstructing fix files: %w", err)
	}
	if err := validateResultFiles(result.Files, files); err != nil {
		return runtime.GenerateResult{Output: result.Summary}, fmt.Errorf("orka fix Task %s: %w", name, err)
	}
	return runtime.GenerateResult{Files: files, Diff: diff, Output: result.Summary}, nil
}

func validateStructuredResult(result StructuredResult, expectedBase string) error {
	if result.Version != 1 {
		return fmt.Errorf("unsupported structured result version %d", result.Version)
	}
	if strings.TrimSpace(result.PushBranch) != "" {
		return fmt.Errorf("generation-only Task unexpectedly pushed branch %q", result.PushBranch)
	}
	if strings.TrimSpace(result.BaseSHA) != expectedBase {
		return fmt.Errorf("baseSHA %q does not match pinned base %q", result.BaseSHA, expectedBase)
	}
	if strings.TrimSpace(result.Diff) == "" && len(result.Files) > 0 {
		return fmt.Errorf("result lists files but contains no diff")
	}
	for _, file := range result.Files {
		clean := path.Clean(strings.TrimSpace(file))
		if clean == "." || clean != file || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return fmt.Errorf("unsafe result file path %q", file)
		}
	}
	return nil
}

func validateResultFiles(expected []string, actual map[string]string) error {
	want := append([]string(nil), expected...)
	sort.Strings(want)
	got := make([]string, 0, len(actual))
	for file := range actual {
		got = append(got, file)
	}
	sort.Strings(got)
	if strings.Join(want, "\x00") != strings.Join(got, "\x00") {
		return fmt.Errorf("reported files %v do not match reconstructed files %v", want, got)
	}
	return nil
}

func (r *AgentRuntime) deleteFailedTask(ctx context.Context, name string) error {
	if err := r.kube.Delete(ctx, TasksGVR, r.opts.Namespace, name); err != nil && !IsNotFound(err) {
		return fmt.Errorf("%w: deleting prior fix Task: %v", runtime.ErrUnavailable, err)
	}
	every := r.opts.PollEvery
	if every <= 0 {
		every = time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		_, err := r.kube.TaskPhase(ctx, r.opts.Namespace, name)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: waiting for prior fix Task deletion: %v", runtime.ErrUnavailable, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("deleting prior fix Task %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *AgentRuntime) waitResult(ctx context.Context, name string) (string, error) {
	every := r.opts.PollEvery
	if every <= 0 {
		every = time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		raw, ok, err := r.results.Result(ctx, r.opts.Namespace, name)
		if err != nil {
			return "", fmt.Errorf("%w: reading fix Task result: %v", runtime.ErrUnavailable, err)
		}
		if ok {
			return raw, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("orka fix Task %s result was not durable: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *AgentRuntime) waitTerminal(ctx context.Context, name string) (string, error) {
	every := r.opts.PollEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		phase, err := r.kube.TaskPhase(ctx, r.opts.Namespace, name)
		if err != nil && !IsNotFound(err) {
			return "", fmt.Errorf("%w: reading fix Task phase: %v", runtime.ErrUnavailable, err)
		}
		if TerminalPhase(phase) {
			return phase, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("orka fix Task %s did not finish: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *AgentRuntime) buildTask(name string, spec runtime.GenerateSpec) map[string]any {
	gitRepo := fmt.Sprintf("https://github.com/%s/%s.git", spec.Repo.Owner, spec.Repo.Name)
	if strings.HasPrefix(spec.Repo.CloneURL, "https://") || strings.HasPrefix(spec.Repo.CloneURL, "http://") {
		gitRepo = spec.Repo.CloneURL
	}
	workspace := map[string]any{"gitRepo": gitRepo, "ref": spec.Repo.Ref}
	if r.opts.GitSecret != "" {
		workspace["gitSecretRef"] = map[string]any{"name": r.opts.GitSecret}
	}
	agentRuntime := map[string]any{"workspace": workspace, "allowBash": spec.AllowBash}
	if spec.MaxTurns > 0 {
		agentRuntime["maxTurns"] = int64(spec.MaxTurns)
	}
	taskSpec := map[string]any{
		"type":         "agent",
		"agentRef":     map[string]any{"name": r.opts.AgentRef},
		"prompt":       spec.Instruction,
		"agentRuntime": agentRuntime,
	}
	if spec.Timeout > 0 {
		taskSpec["timeout"] = spec.Timeout.String()
	}
	if r.opts.MaxRetries > 0 {
		taskSpec["retryPolicy"] = map[string]any{"maxRetries": int64(r.opts.MaxRetries)}
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata": map[string]any{
			"name": name, "namespace": r.opts.Namespace,
			"labels": map[string]any{ManagedByLabel: FixerManagedByValue},
		},
		"spec": taskSpec,
	}
}

// FixTaskName fingerprints the pinned repository and complete execution contract.
func FixTaskName(spec runtime.GenerateSpec, opts AgentOptions) string {
	opts = normalizeAgentOptions(opts)
	data := strings.Join([]string{
		spec.Repo.Owner, spec.Repo.Name, spec.Repo.Ref, spec.Instruction,
		opts.AgentRef, opts.GitSecret, opts.Version, fmt.Sprintf("%d", opts.MaxRetries),
		fmt.Sprintf("%t", spec.AllowBash), fmt.Sprintf("%d", spec.MaxTurns), spec.Timeout.String(),
	}, "\x00")
	sum := sha256.Sum256([]byte(data))
	return Sanitize("fix-" + hex.EncodeToString(sum[:8]) + "-" + opts.Version)
}

// ResultClient reads structured Task results from the Orka REST API.
type ResultClient struct {
	base  string
	token string
	http  *http.Client
}

// NewResultClient builds an Orka result client.
func NewResultClient(base, token string) *ResultClient {
	return &ResultClient{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// Result fetches one Task result. A missing or empty result returns ok=false.
func (c *ResultClient) Result(ctx context.Context, namespace, taskName string) (string, bool, error) {
	endpoint := c.base + "/api/v1/tasks/" + url.PathEscape(taskName) + "/result"
	if namespace != "" {
		endpoint += "?namespace=" + url.QueryEscape(namespace)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFixResultBytes+1))
	if readErr != nil {
		return "", false, readErr
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 1024 {
			message = message[:1024]
		}
		return "", false, fmt.Errorf("orka result: HTTP %d: %s", resp.StatusCode, message)
	}
	if len(body) > maxFixResultBytes {
		return "", false, fmt.Errorf("orka result exceeds %d bytes", maxFixResultBytes)
	}
	var wrap struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return "", false, fmt.Errorf("orka result: %w", err)
	}
	if strings.TrimSpace(wrap.Result) == "" {
		return "", false, nil
	}
	return wrap.Result, true, nil
}
