package orka

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	FixerManagedByValue      = "orka-fixer"
	defaultFixNamespace      = "orka-system"
	defaultFixVersion        = "v1"
	defaultFixPriority       = int64(500)
	fixContractLabel         = "prow-ai-dashboard/runtime"
	fixContractLabelValue    = "fix-generation"
	fixContractAnnotation    = "prow-ai-dashboard/fix-contract"
	fixContractVersion       = "v1"
	serviceAccountToken      = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	maxFixResultBytes        = 4 << 20
	actionRequestAnnotation  = "prow-ai-dashboard/action-request"
	defaultFixCleanupTimeout = 30 * time.Second
)

var immutableGitRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

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
	TaskState(context.Context, string, string) (TaskState, error)
	DeleteTaskIfIdentity(context.Context, string, string, string, string) (bool, error)
	Delete(context.Context, schema.GroupVersionResource, string, string) error
}

type resultAPI interface {
	Result(context.Context, string, string) (string, bool, error)
}

// AgentOptions configures the generation-only Orka runtime.
type AgentOptions struct {
	Namespace  string
	AgentRef   string
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
	Namespace                        string
	AgentRef                         string
	API                              string
	APIToken                         string
	Version                          string
	MaxRetries                       int
	KubeContext                      string
	DelegatedServiceAccountName      string
	DelegatedServiceAccountNamespace string
	PodName                          string
	PodUID                           string
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
	kubeConfig := rc
	resultTokens := resultTokenSourceFromEnv(cfg.APIToken)
	delegation := delegatedServiceAccountConfig{
		Namespace: cfg.DelegatedServiceAccountNamespace,
		Name:      cfg.DelegatedServiceAccountName,
		PodName:   cfg.PodName,
		PodUID:    types.UID(cfg.PodUID),
	}
	if delegation.configured() {
		kubeConfig, resultTokens, err = newDelegatedServiceAccountClients(rc, delegation)
		if err != nil {
			return nil, fmt.Errorf("orka fix runtime: delegated identity: %w", err)
		}
	}
	kube, err := NewKubeClient(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("orka fix runtime: kube client: %w", err)
	}
	kube.Manager = FixerManagedByValue
	return NewAgentRuntime(kube, newResultClient(cfg.API, resultTokens), AgentOptions{
		Namespace:  cfg.Namespace,
		AgentRef:   cfg.AgentRef,
		Version:    strings.TrimSpace(cfg.Version),
		MaxRetries: cfg.MaxRetries,
	}), nil
}

func resultTokenSourceFromEnv(explicit string) resultTokenSource {
	if token := strings.TrimSpace(explicit); token != "" {
		return staticResultTokenSource(token)
	}
	if token := strings.TrimSpace(os.Getenv("ORKA_API_TOKEN")); token != "" {
		return staticResultTokenSource(token)
	}
	if file := strings.TrimSpace(os.Getenv("ORKA_API_TOKEN_FILE")); file != "" {
		return fileResultTokenSource{path: file}
	}
	return fileResultTokenSource{path: serviceAccountToken}
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
	_ runtime.AgentRuntime        = (*AgentRuntime)(nil)
	_ runtime.ManagedAgentRuntime = (*AgentRuntime)(nil)
	_ taskAPI                     = (*KubeClient)(nil)
	_ resultAPI                   = (*ResultClient)(nil)
)

// Generate runs the fix agent, validates its structured result, and reapplies
// the captured diff to the pinned base before returning changed files.
func (r *AgentRuntime) Generate(ctx context.Context, spec runtime.GenerateSpec) (result runtime.GenerateResult, retErr error) {
	if r == nil || r.kube == nil || r.results == nil {
		return runtime.GenerateResult{}, fmt.Errorf("%w: Orka runtime is not configured", runtime.ErrUnavailable)
	}
	if strings.TrimSpace(spec.Instruction) == "" {
		return runtime.GenerateResult{}, fmt.Errorf("orka: empty instruction")
	}
	if spec.Repo.Owner == "" || spec.Repo.Name == "" || spec.Repo.Ref == "" || r.opts.AgentRef == "" {
		return runtime.GenerateResult{}, fmt.Errorf("orka: repo owner, name, ref, and agent_ref are required")
	}
	if !immutableGitRevision.MatchString(spec.Repo.Ref) {
		return runtime.GenerateResult{}, fmt.Errorf("orka: repo ref must be a lowercase 40-character commit SHA")
	}
	if spec.MaxTurns < 1 || spec.MaxTurns > 1000 {
		return runtime.GenerateResult{}, fmt.Errorf("orka: max turns must be between 1 and 1000")
	}
	if spec.Timeout <= 0 || spec.Timeout > 30*time.Minute {
		return runtime.GenerateResult{}, fmt.Errorf("orka: timeout must be greater than zero and at most 30m")
	}
	if r.opts.MaxRetries < 0 || r.opts.MaxRetries > 2 {
		return runtime.GenerateResult{}, fmt.Errorf("orka: retries must be between 0 and 2")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	name := FixTaskName(spec, r.opts)
	work := runtime.WorkRef{Backend: "orka", Namespace: r.opts.Namespace, Name: name, ExecutionID: strings.TrimSpace(spec.ExecutionID)}
	if spec.WorkObserver != nil {
		if err := spec.WorkObserver(ctx, work); err != nil {
			return runtime.GenerateResult{}, fmt.Errorf("recording planned fix Task: %w", err)
		}
	}

	state, err := r.kube.TaskState(ctx, r.opts.Namespace, name)
	if err != nil {
		return runtime.GenerateResult{}, fmt.Errorf("%w: reading prior fix Task: %v", runtime.ErrUnavailable, err)
	}
	if state.Exists && work.ExecutionID != "" && state.Annotations[actionRequestAnnotation] != work.ExecutionID {
		return runtime.GenerateResult{}, fmt.Errorf("%w: existing fix Task belongs to another request", runtime.ErrWorkIdentityChanged)
	}
	if state.Exists && (state.Phase == "Failed" || state.Phase == "Cancelled") {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultFixCleanupTimeout)
		err := r.Cleanup(cleanupCtx, runtime.WorkRef{Backend: "orka", Namespace: r.opts.Namespace, Name: name, UID: state.UID, ExecutionID: work.ExecutionID})
		cancel()
		if err != nil {
			return runtime.GenerateResult{}, fmt.Errorf("%w: deleting prior fix Task: %v", runtime.ErrUnavailable, err)
		}
	}
	if err := r.kube.Apply(ctx, TasksGVR, r.opts.Namespace, r.buildTask(name, spec)); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultFixCleanupTimeout)
		cleanupErr := r.Cleanup(cleanupCtx, work)
		cancel()
		return runtime.GenerateResult{}, errors.Join(fmt.Errorf("%w: applying fix Task: %v", runtime.ErrUnavailable, err), cleanupErr)
	}
	state, err = r.kube.TaskState(ctx, r.opts.Namespace, name)
	if err != nil || !state.Exists || strings.TrimSpace(state.UID) == "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultFixCleanupTimeout)
		cleanupErr := r.Cleanup(cleanupCtx, work)
		cancel()
		if err == nil {
			err = fmt.Errorf("applied Task identity is unavailable")
		}
		return runtime.GenerateResult{}, errors.Join(fmt.Errorf("%w: reading applied fix Task: %v", runtime.ErrUnavailable, err), cleanupErr)
	}
	if work.ExecutionID != "" && state.Annotations[actionRequestAnnotation] != work.ExecutionID {
		return runtime.GenerateResult{}, fmt.Errorf("%w: applied fix Task belongs to another request", runtime.ErrWorkIdentityChanged)
	}
	work.UID = state.UID
	if spec.WorkObserver != nil {
		if err := spec.WorkObserver(ctx, work); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultFixCleanupTimeout)
			cleanupErr := r.Cleanup(cleanupCtx, work)
			cancel()
			return runtime.GenerateResult{}, errors.Join(fmt.Errorf("recording observed fix Task: %w", err), cleanupErr)
		}
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultFixCleanupTimeout)
		cleanupErr := r.Cleanup(cleanupCtx, work)
		cancel()
		if cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

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
	var parsed StructuredResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return runtime.GenerateResult{}, fmt.Errorf("orka fix Task %s: parsing result: %w", name, err)
	}
	if err := validateStructuredResult(parsed, spec.Repo.Ref); err != nil {
		return runtime.GenerateResult{Output: parsed.Summary}, fmt.Errorf("orka fix Task %s: %w", name, err)
	}
	if strings.TrimSpace(parsed.Diff) == "" {
		return runtime.GenerateResult{Output: parsed.Summary}, nil
	}

	apply := r.applyDiff
	if apply == nil {
		apply = runtime.ApplyDiff
	}
	files, diff, err := apply(ctx, spec.Repo, parsed.Diff)
	if err != nil {
		return runtime.GenerateResult{Output: parsed.Summary}, fmt.Errorf("reconstructing fix files: %w", err)
	}
	if err := validateResultFiles(parsed.Files, files); err != nil {
		return runtime.GenerateResult{Output: parsed.Summary}, fmt.Errorf("orka fix Task %s: %w", name, err)
	}
	return runtime.GenerateResult{Files: files, Diff: diff, Output: parsed.Summary}, nil
}

// Cleanup deletes only the exact observed Orka Task and waits for completion.
func (r *AgentRuntime) Cleanup(ctx context.Context, work runtime.WorkRef) error {
	if r == nil || r.kube == nil {
		return fmt.Errorf("%w: Orka runtime is not configured", runtime.ErrUnavailable)
	}
	if work.Backend != "" && work.Backend != "orka" {
		return fmt.Errorf("%w: backend %q", runtime.ErrWorkIdentityChanged, work.Backend)
	}
	namespace := strings.TrimSpace(work.Namespace)
	if namespace == "" {
		namespace = r.opts.Namespace
	}
	if strings.TrimSpace(work.Name) == "" {
		return fmt.Errorf("%w: runtime work name is empty", runtime.ErrWorkIdentityChanged)
	}
	every := r.opts.PollEvery
	if every <= 0 {
		every = time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	uid := strings.TrimSpace(work.UID)
	for {
		state, err := r.kube.TaskState(ctx, namespace, work.Name)
		if err != nil {
			return fmt.Errorf("%w: reading fix Task cleanup state: %v", runtime.ErrCleanupPending, err)
		}
		if !state.Exists {
			return nil
		}
		if uid == "" {
			if strings.TrimSpace(work.ExecutionID) == "" || state.Annotations[actionRequestAnnotation] != work.ExecutionID || strings.TrimSpace(state.UID) == "" {
				return fmt.Errorf("%w: Task %s cannot be adopted safely", runtime.ErrWorkIdentityChanged, work.Name)
			}
			uid = state.UID
		} else if state.UID != uid {
			return fmt.Errorf("%w: Task %s UID changed", runtime.ErrWorkIdentityChanged, work.Name)
		}
		_, err = r.kube.DeleteTaskIfIdentity(ctx, namespace, work.Name, uid, state.ResourceVersion)
		if err != nil {
			return fmt.Errorf("%w: deleting fix Task: %v", runtime.ErrCleanupPending, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", runtime.ErrCleanupPending, ctx.Err())
		case <-ticker.C:
		}
	}
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
	workspace := map[string]any{
		"gitRepo": fmt.Sprintf("https://github.com/%s/%s.git", spec.Repo.Owner, spec.Repo.Name),
		"ref":     spec.Repo.Ref,
	}
	agentRuntime := map[string]any{
		"workspace": workspace,
		"maxTurns":  int64(spec.MaxTurns),
		"allowBash": spec.AllowBash,
	}
	taskSpec := map[string]any{
		"type":         "agent",
		"agentRef":     map[string]any{"name": r.opts.AgentRef, "namespace": r.opts.Namespace},
		"prompt":       spec.Instruction,
		"agentRuntime": agentRuntime,
		"timeout":      spec.Timeout.String(),
		"priority":     defaultFixPriority,
		"retryPolicy":  map[string]any{"maxRetries": int64(r.opts.MaxRetries)},
	}
	metadata := map[string]any{
		"name": name, "namespace": r.opts.Namespace,
		"labels": map[string]any{
			ManagedByLabel:   FixerManagedByValue,
			fixContractLabel: fixContractLabelValue,
		},
		"annotations": map[string]any{fixContractAnnotation: fixContractVersion},
	}
	if executionID := strings.TrimSpace(spec.ExecutionID); executionID != "" {
		metadata["annotations"].(map[string]any)[actionRequestAnnotation] = executionID
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata":   metadata,
		"spec":       taskSpec,
	}
}

// FixTaskName fingerprints the pinned repository and complete execution contract.
func FixTaskName(spec runtime.GenerateSpec, opts AgentOptions) string {
	opts = normalizeAgentOptions(opts)
	parts := []string{
		spec.Repo.Owner, spec.Repo.Name, spec.Repo.Ref, spec.Instruction,
		opts.AgentRef, opts.Namespace, opts.Version, fmt.Sprintf("%d", opts.MaxRetries),
		fmt.Sprintf("%t", spec.AllowBash), fmt.Sprintf("%d", spec.MaxTurns), spec.Timeout.String(),
	}
	if executionID := strings.TrimSpace(spec.ExecutionID); executionID != "" {
		parts = append(parts, executionID)
	}
	data := strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(data))
	return Sanitize("fix-" + hex.EncodeToString(sum[:8]) + "-" + opts.Version)
}

// ResultClient reads structured Task results from the Orka REST API.
type ResultClient struct {
	base   string
	tokens resultTokenSource
	http   *http.Client
}

// ResultHTTPError identifies a non-success response from the Orka result API.
type ResultHTTPError struct {
	StatusCode int
}

func (e *ResultHTTPError) Error() string {
	return fmt.Sprintf("orka result: HTTP %d", e.StatusCode)
}

// IsResultAuthorizationError reports a systemic result API authorization failure.
func IsResultAuthorizationError(err error) bool {
	var httpErr *ResultHTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}

type resultTokenSource interface {
	Token() (string, error)
}

type staticResultTokenSource string

func (s staticResultTokenSource) Token() (string, error) {
	return strings.TrimSpace(string(s)), nil
}

type fileResultTokenSource struct {
	path string
}

func (s fileResultTokenSource) Token() (string, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return "", errors.New("read Orka result API token file")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("orka result API token file is empty")
	}
	return token, nil
}

func validateResultAPI(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute http or https URL", field)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, a query, or a fragment", field)
	}
	return nil
}

// NewResultClient builds an Orka result client with a static token.
func NewResultClient(base, token string) *ResultClient {
	return newResultClient(base, staticResultTokenSource(token))
}

// NewFileResultClient builds an Orka result client that reloads its token for every request.
func NewFileResultClient(base, tokenFile string) *ResultClient {
	return newResultClient(base, fileResultTokenSource{path: strings.TrimSpace(tokenFile)})
}

func newResultClientFromEnv(base, explicitToken string) *ResultClient {
	return newResultClient(base, resultTokenSourceFromEnv(explicitToken))
}

func newResultClient(base string, tokens resultTokenSource) *ResultClient {
	return &ResultClient{base: strings.TrimRight(base, "/"), tokens: tokens, http: &http.Client{Timeout: 30 * time.Second}}
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
	if c.tokens != nil {
		token, tokenErr := c.tokens.Token()
		if tokenErr != nil {
			return "", false, tokenErr
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusUnauthorized {
			if invalidator, ok := c.tokens.(interface{ invalidate() }); ok {
				invalidator.invalidate()
			}
		}
		return "", false, &ResultHTTPError{StatusCode: resp.StatusCode}
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFixResultBytes+1))
	if readErr != nil {
		return "", false, readErr
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
