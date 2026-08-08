package fixruntime

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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	agentSandboxBackend            = "agent-sandbox"
	agentSandboxRequestEnv         = "PROW_AI_FIX_EXECUTION_REQUEST_B64"
	agentSandboxExecutionLabel     = "prow-ai-dashboard/execution"
	agentSandboxContractAnnotation = "prow-ai-dashboard/execution-contract-sha256"
	agentSandboxIDAnnotation       = "prow-ai-dashboard/execution-id"
	agentSandboxPodAnnotation      = "agents.x-k8s.io/pod-name"
	agentSandboxContainerName      = "executor"
	defaultSandboxPollEvery        = 250 * time.Millisecond
	defaultSandboxCleanupTimeout   = 30 * time.Second
	agentSandboxResultGrace        = 15 * time.Second
	defaultSandboxOutputLimit      = int64(512 << 10)
	defaultSandboxCPURequest       = "100m"
	defaultSandboxCPULimit         = "1"
	defaultSandboxMemoryRequest    = "128Mi"
	defaultSandboxMemoryLimit      = "512Mi"
	defaultSandboxDiskLimit        = "256Mi"
)

var (
	sandboxesGVR                = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	immutableExecutorImage      = regexp.MustCompile(`^[^[:space:]]+@sha256:[0-9a-f]{64}$`)
	agentSandboxInClusterConfig = rest.InClusterConfig
)

// AgentSandboxOptions configure the experimental Agent Sandbox adapter.
type AgentSandboxResources struct {
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EphemeralStorage string
}

type AgentSandboxOptions struct {
	Namespace          string
	Image              string
	ServiceAccountName string
	RuntimeClassName   string
	ModelGateway       engineruntime.ModelGatewayConfig
	PublicCAPrivateDNS bool
	Timeout            time.Duration
	OutputLimitBytes   int64
	PollEvery          time.Duration
	Resources          AgentSandboxResources
	testOnly           bool
	appArmorCapability appArmorCapability
}

type sandboxState struct {
	Exists         bool
	UID            string
	PodName        string
	NodeName       string
	ContractHash   string
	ExecutionID    string
	ShapeHash      string
	ShutdownTime   string
	Finished       bool
	FinishedReason string
}

// agentSandboxAPI is the low-level lifecycle seam intended for a future shared
// Agent Sandbox package. It excludes Fix PR prompt, patch, and result policy.
type agentSandboxAPI interface {
	Create(context.Context, string, map[string]any) (sandboxState, error)
	State(context.Context, string, string) (sandboxState, error)
	Delete(context.Context, string, string, string) error
	PodLogs(context.Context, string, string, int64) (string, error)
	PodExists(context.Context, string, string) (bool, error)
	ExecutionPods(context.Context, string, string) ([]string, error)
}

// AgentSandboxRuntime executes one credential-free request in a v1beta1 Sandbox.
type AgentSandboxRuntime struct {
	api       agentSandboxAPI
	opts      AgentSandboxOptions
	applyDiff func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error)
	now       func() time.Time
}

var (
	_ engineruntime.AgentRuntime        = (*AgentSandboxRuntime)(nil)
	_ engineruntime.ManagedAgentRuntime = (*AgentSandboxRuntime)(nil)
)

// NewAgentSandboxRuntime constructs the production adapter.
func NewAgentSandboxRuntime(api agentSandboxAPI, opts AgentSandboxOptions) (*AgentSandboxRuntime, error) {
	opts = normalizeAgentSandboxOptions(opts)
	if err := validateAgentSandboxOptions(opts); err != nil {
		return nil, err
	}
	return &AgentSandboxRuntime{api: api, opts: opts, now: time.Now}, nil
}

func newAgentSandboxRuntimeForTest(api agentSandboxAPI, opts AgentSandboxOptions) *AgentSandboxRuntime {
	opts.testOnly = true
	opts = normalizeAgentSandboxOptions(opts)
	return &AgentSandboxRuntime{api: api, opts: opts, now: time.Now}
}

// NewAgentSandboxRuntimeFromEnv constructs the adapter from deployment environment and Kubernetes config.
func NewAgentSandboxRuntimeFromEnv(expectedGateway engineruntime.ModelGatewayConfig, expectedPublicCAPrivateDNS bool, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	outputLimit, err := parseInt64Env("AGENT_SANDBOX_OUTPUT_LIMIT_BYTES")
	if err != nil {
		return nil, err
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(os.Getenv("AGENT_SANDBOX_TIMEOUT")))
	if err != nil {
		return nil, fmt.Errorf("agent sandbox timeout: %w", err)
	}
	publicCAPrivateDNS := false
	if value := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS")); value != "" {
		publicCAPrivateDNS, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("agent sandbox public CA private DNS setting is invalid")
		}
	}
	opts := AgentSandboxOptions{
		Namespace: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_NAMESPACE")), Image: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_IMAGE")),
		ServiceAccountName: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_SERVICE_ACCOUNT")), RuntimeClassName: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_RUNTIME_CLASS")),
		ModelGateway: engineruntime.ModelGatewayConfig{
			Endpoint: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_GATEWAY_ENDPOINT")), Model: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_GATEWAY_MODEL")),
			ProtocolVersion: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MODEL_GATEWAY_PROTOCOL")),
		},
		PublicCAPrivateDNS: publicCAPrivateDNS, Timeout: timeout, OutputLimitBytes: outputLimit,
		Resources: AgentSandboxResources{
			CPURequest: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_CPU_REQUEST")), CPULimit: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_CPU_LIMIT")),
			MemoryRequest: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MEMORY_REQUEST")), MemoryLimit: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_MEMORY_LIMIT")),
			EphemeralStorage: strings.TrimSpace(os.Getenv("AGENT_SANDBOX_EPHEMERAL_STORAGE_LIMIT")),
		},
	}
	if value := strings.TrimSpace(os.Getenv("AGENT_SANDBOX_POLL_INTERVAL")); value != "" {
		poll, err := time.ParseDuration(value)
		if err != nil || poll <= 0 {
			return nil, fmt.Errorf("agent sandbox poll interval %q is invalid", value)
		}
		opts.PollEvery = poll
	}
	opts = normalizeAgentSandboxOptions(opts)
	if opts.ModelGateway != expectedGateway || opts.PublicCAPrivateDNS != expectedPublicCAPrivateDNS || opts.Timeout != expectedTimeout || opts.OutputLimitBytes != expectedOutputLimit {
		return nil, fmt.Errorf("agent sandbox deployment values do not match project runtime configuration")
	}
	if err := validateAgentSandboxOptions(opts); err != nil {
		return nil, err
	}
	cfg, err := agentSandboxInClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("agent sandbox requires in-cluster Kubernetes configuration: %w", err)
	}
	api, err := newKubeAgentSandboxAPI(cfg)
	if err != nil {
		return nil, fmt.Errorf("agent sandbox Kubernetes client: %w", err)
	}
	return NewAgentSandboxRuntime(api, opts)
}

func normalizeAgentSandboxOptions(opts AgentSandboxOptions) AgentSandboxOptions {
	if opts.PollEvery <= 0 {
		opts.PollEvery = defaultSandboxPollEvery
	}
	if opts.OutputLimitBytes == 0 {
		opts.OutputLimitBytes = defaultSandboxOutputLimit
	}
	if opts.Resources.CPURequest == "" {
		opts.Resources.CPURequest = defaultSandboxCPURequest
	}
	if opts.Resources.CPULimit == "" {
		opts.Resources.CPULimit = defaultSandboxCPULimit
	}
	if opts.Resources.MemoryRequest == "" {
		opts.Resources.MemoryRequest = defaultSandboxMemoryRequest
	}
	if opts.Resources.MemoryLimit == "" {
		opts.Resources.MemoryLimit = defaultSandboxMemoryLimit
	}
	if opts.Resources.EphemeralStorage == "" {
		opts.Resources.EphemeralStorage = defaultSandboxDiskLimit
	}
	return opts
}

func validateAgentSandboxOptions(opts AgentSandboxOptions) error {
	if opts.appArmorCapability != appArmorRuntimeDefault && (!opts.testOnly || opts.appArmorCapability != appArmorUnavailableForKindTest) {
		return fmt.Errorf("agent sandbox AppArmor capability is invalid")
	}
	if strings.TrimSpace(opts.Namespace) == "" || strings.TrimSpace(opts.Image) == "" || strings.TrimSpace(opts.ServiceAccountName) == "" {
		return fmt.Errorf("agent sandbox namespace, image, and service account are required")
	}
	if !opts.testOnly && !immutableExecutorImage.MatchString(opts.Image) {
		return fmt.Errorf("agent sandbox executor image must use an immutable sha256 digest")
	}
	if !opts.testOnly && strings.TrimSpace(opts.RuntimeClassName) == "" {
		return fmt.Errorf("agent sandbox secure runtime class is required")
	}
	if opts.Timeout <= 0 || opts.Timeout > 30*time.Minute {
		return fmt.Errorf("agent sandbox timeout must be greater than zero and at most 30m")
	}
	if opts.OutputLimitBytes < 4<<10 || opts.OutputLimitBytes > 1<<20 {
		return fmt.Errorf("agent sandbox output limit must be between 4096 and 1048576")
	}
	request := engineruntime.ExecutionRequest{
		Version: engineruntime.ExecutionContractVersion, RepositoryURL: "https://example.invalid/repo.git",
		CommitSHA: strings.Repeat("a", 40), ExpectedBaseSHA: strings.Repeat("a", 40), Prompt: "validate",
		TimeoutSeconds: 1, MaxSteps: 2, MaxFiles: 1,
		CommandPolicy: engineruntime.CommandPolicy{Commands: []engineruntime.ExecutionCommand{{
			Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 1,
		}}},
		ModelGateway: opts.ModelGateway, OutputLimitBytes: opts.OutputLimitBytes,
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("agent sandbox model gateway: %w", err)
	}
	if !opts.testOnly {
		if err := engineruntime.ValidateModelGatewayTrust(opts.ModelGateway.Endpoint, opts.PublicCAPrivateDNS); err != nil {
			return fmt.Errorf("agent sandbox model gateway: %w", err)
		}
	}
	for name, value := range map[string]string{"cpu request": opts.Resources.CPURequest, "cpu limit": opts.Resources.CPULimit, "memory request": opts.Resources.MemoryRequest, "memory limit": opts.Resources.MemoryLimit, "ephemeral storage limit": opts.Resources.EphemeralStorage} {
		if _, err := resource.ParseQuantity(value); err != nil {
			return fmt.Errorf("agent sandbox %s %q is invalid", name, value)
		}
	}
	return nil
}

func parseInt64Env(name string) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return parsed, nil
}

// Generate creates one cold-start Sandbox, retrieves its structured result,
// independently reconstructs the patch, and deletes the Sandbox before return.
func (r *AgentSandboxRuntime) Generate(ctx context.Context, spec engineruntime.GenerateSpec) (result engineruntime.GenerateResult, retErr error) {
	if r == nil || r.api == nil {
		return result, fmt.Errorf("%w: agent sandbox runtime is not configured", engineruntime.ErrUnavailable)
	}
	if err := validateAgentSandboxOptions(normalizeAgentSandboxOptions(r.opts)); err != nil {
		return result, fmt.Errorf("%w: %v", engineruntime.ErrUnavailable, err)
	}
	request, err := executionRequest(spec)
	if err != nil {
		return result, err
	}
	if request.ModelGateway != r.opts.ModelGateway || time.Duration(request.TimeoutSeconds)*time.Second != r.opts.Timeout || request.OutputLimitBytes != r.opts.OutputLimitBytes {
		return result, fmt.Errorf("agent sandbox request does not match configured gateway, timeout, or output limit")
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("agent sandbox encode request: %w", err)
	}
	contractHash := agentSandboxContractHash(requestJSON, r.opts)
	executionID := strings.TrimSpace(spec.ExecutionID)
	if executionID == "" {
		executionID = "contract-" + hex.EncodeToString(contractHash[:8])
	}
	name := agentSandboxName(executionID, contractHash[:])
	work := engineruntime.WorkRef{Backend: agentSandboxBackend, Namespace: r.opts.Namespace, Name: name, ExecutionID: executionID}
	if spec.WorkObserver != nil {
		if err := spec.WorkObserver(ctx, work); err != nil {
			return result, fmt.Errorf("recording planned agent Sandbox: %w", err)
		}
	}

	started := r.now()
	result.Telemetry.UsageStatus = "unavailable_from_agent_runtime"
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second+agentSandboxResultGrace+5*time.Second)
	defer cancel()
	object := r.sandboxObject(name, requestJSON, contractHash[:], request, executionID)
	desiredState := sandboxStateFromObject(&unstructured.Unstructured{Object: object})
	state, err := r.api.Create(runCtx, r.opts.Namespace, object)
	if err != nil {
		if state.Exists && state.UID != "" {
			work.UID = state.UID
			return result, errors.Join(err, r.cleanupWork(work))
		}
		if errors.Is(err, engineruntime.ErrWorkIdentityChanged) {
			return result, err
		}
		cleanupErr := r.recoverAmbiguousCreate(work, desiredState)
		return result, errors.Join(fmt.Errorf("%w: create agent Sandbox: %v", engineruntime.ErrUnavailable, err), cleanupErr)
	}
	work.UID = state.UID
	result.Resources = r.resourceMetadata(name, state)
	if work.UID == "" {
		cleanupErr := r.recoverAmbiguousCreate(work, desiredState)
		return result, errors.Join(fmt.Errorf("%w: created agent Sandbox identity is unavailable", engineruntime.ErrUnavailable), cleanupErr)
	}
	if spec.WorkObserver != nil {
		if err := spec.WorkObserver(runCtx, work); err != nil {
			cleanupErr := r.cleanupWork(work)
			return result, errors.Join(fmt.Errorf("recording observed agent Sandbox: %w", err), cleanupErr)
		}
	}
	defer func() {
		cleanupStarted := r.now()
		cleanupErr := r.cleanupWork(work)
		result.Telemetry.CleanupDurationMs = r.now().Sub(cleanupStarted).Milliseconds()
		result.Telemetry.CleanupCompleted = cleanupErr == nil
		if cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	terminal, err := r.waitTerminal(runCtx, work)
	if err != nil {
		result.Version = engineruntime.ExecutionContractVersion
		result.BaseSHA = request.ExpectedBaseSHA
		result.DurationMs = r.now().Sub(started).Milliseconds()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.TerminalState = engineruntime.TerminalTimedOut
			result.FailureReason = "execution deadline exceeded"
			return result, fmt.Errorf("agent Sandbox %s timed out: %w", name, context.DeadlineExceeded)
		}
		if errors.Is(runCtx.Err(), context.Canceled) {
			result.TerminalState = engineruntime.TerminalCancelled
			result.FailureReason = "execution cancelled"
			return result, fmt.Errorf("%w: agent Sandbox %s", engineruntime.ErrCancelled, name)
		}
		return result, err
	}
	result.Telemetry.TaskFinalized = true
	result.Telemetry.TaskFinalizedMs = r.now().Sub(started).Milliseconds()

	logs, err := r.api.PodLogs(runCtx, r.opts.Namespace, terminal.PodName, request.OutputLimitBytes)
	if err != nil {
		result.Version = engineruntime.ExecutionContractVersion
		result.BaseSHA = request.ExpectedBaseSHA
		result.Files = map[string]string{}
		result.TerminalState = engineruntime.TerminalFailed
		result.DurationMs = max(r.now().Sub(started).Milliseconds(), 0)
		result.Resources = r.resourceMetadata(name, terminal)
		result.FailureReason = safeKubernetesDiagnostic(err.Error())
		result.Output = boundedSummary(result.FailureReason)
		if validationErr := result.Validate(request); validationErr != nil {
			result.FailureReason = "agent Sandbox result logs are unavailable"
			result.Output = result.FailureReason
		}
		return result, fmt.Errorf("%w: read agent Sandbox result: %v", engineruntime.ErrMalformedResult, err)
	}
	result.Telemetry.ResultAvailable = true
	result.Telemetry.ResultAvailableMs = r.now().Sub(started).Milliseconds()
	result.Telemetry.FinalizationChecked = true
	parsed, err := decodeExecutionResult(logs)
	if err != nil {
		return result, fmt.Errorf("%w: agent Sandbox result: %v", engineruntime.ErrMalformedResult, err)
	}
	parsed.Resources = r.resourceMetadata(name, terminal)
	parsed.Output = boundedSummary(parsed.StdoutSummary, parsed.StderrSummary, parsed.FailureReason)
	if err := parsed.Validate(request); err != nil {
		return parsed, fmt.Errorf("%w: agent Sandbox result: %v", engineruntime.ErrResultContract, err)
	}
	if terminal.FinishedReason == "PodSucceeded" && parsed.TerminalState != engineruntime.TerminalSucceeded {
		return parsed, fmt.Errorf("%w: succeeded Pod reported %q", engineruntime.ErrResultContract, parsed.TerminalState)
	}
	if terminal.FinishedReason == "PodFailed" && parsed.TerminalState == engineruntime.TerminalSucceeded {
		return parsed, fmt.Errorf("%w: failed Pod reported success", engineruntime.ErrResultContract)
	}
	parsed.Telemetry = result.Telemetry
	parsed.Telemetry.FinalizationValid = true
	if parsed.TerminalState != engineruntime.TerminalSucceeded {
		if parsed.TerminalState == engineruntime.TerminalCancelled {
			return parsed, fmt.Errorf("%w: %s", engineruntime.ErrCancelled, parsed.FailureReason)
		}
		return parsed, fmt.Errorf("agent Sandbox execution %s: %s", parsed.TerminalState, parsed.FailureReason)
	}

	apply := r.applyDiff
	if apply == nil {
		apply = engineruntime.ApplyDiff
	}
	files, diff, err := apply(runCtx, spec.Repo, parsed.Diff)
	if err != nil {
		return parsed, fmt.Errorf("reconstructing agent Sandbox files: %w", err)
	}
	if err := compareExecutionFiles(parsed, files); err != nil {
		return parsed, fmt.Errorf("%w: %v", engineruntime.ErrResultExtraFile, err)
	}
	parsed.Files = files
	parsed.ChangedFiles = sortedFileNames(files)
	parsed.Diff = diff
	return parsed, nil
}

// Cleanup deletes one exact Sandbox identity and waits for its Pod to disappear.
func (r *AgentSandboxRuntime) Cleanup(ctx context.Context, work engineruntime.WorkRef) error {
	if r == nil || r.api == nil {
		return fmt.Errorf("%w: agent sandbox runtime is not configured", engineruntime.ErrUnavailable)
	}
	if work.Backend != "" && work.Backend != agentSandboxBackend {
		return fmt.Errorf("%w: work backend %q", engineruntime.ErrWorkIdentityChanged, work.Backend)
	}
	if strings.TrimSpace(work.Namespace) == "" || strings.TrimSpace(work.Name) == "" {
		return fmt.Errorf("agent sandbox cleanup requires namespace and name")
	}
	state, err := r.api.State(ctx, work.Namespace, work.Name)
	if err != nil {
		return fmt.Errorf("read agent Sandbox before cleanup: %w", err)
	}
	if !state.Exists {
		pods, podErr := r.api.ExecutionPods(ctx, work.Namespace, work.Name)
		if podErr != nil {
			return fmt.Errorf("confirm missing agent Sandbox Pod cleanup: %w", podErr)
		}
		if len(pods) == 0 {
			return nil
		}
		poll := time.NewTicker(r.opts.PollEvery)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: orphaned agent Sandbox Pods %s/%s: %v", engineruntime.ErrCleanupPending, work.Namespace, work.Name, pods)
			case <-poll.C:
				pods, podErr = r.api.ExecutionPods(ctx, work.Namespace, work.Name)
				if podErr != nil {
					return fmt.Errorf("confirm orphaned agent Sandbox Pod cleanup: %w", podErr)
				}
				if len(pods) == 0 {
					return nil
				}
			}
		}
	}
	if work.UID == "" {
		return fmt.Errorf("%w: agent Sandbox %s/%s cleanup requires its observed UID", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
	}
	if state.UID != work.UID {
		return fmt.Errorf("%w: agent Sandbox %s/%s UID changed", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
	}
	if err := r.api.Delete(ctx, work.Namespace, work.Name, state.UID); err != nil {
		return fmt.Errorf("delete agent Sandbox: %w", err)
	}
	poll := time.NewTicker(r.opts.PollEvery)
	defer poll.Stop()
	podName := state.PodName
	if podName == "" {
		podName = work.Name
	}
	for {
		state, err := r.api.State(ctx, work.Namespace, work.Name)
		if err != nil {
			return fmt.Errorf("confirm agent Sandbox cleanup: %w", err)
		}
		podExists, err := r.api.PodExists(ctx, work.Namespace, podName)
		if err != nil {
			return fmt.Errorf("confirm agent Sandbox Pod cleanup: %w", err)
		}
		pods, err := r.api.ExecutionPods(ctx, work.Namespace, work.Name)
		if err != nil {
			return fmt.Errorf("confirm agent Sandbox execution Pod cleanup: %w", err)
		}
		if !state.Exists && !podExists && len(pods) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: agent Sandbox %s/%s", engineruntime.ErrCleanupPending, work.Namespace, work.Name)
		case <-poll.C:
		}
	}
}

func (r *AgentSandboxRuntime) recoverAmbiguousCreate(work engineruntime.WorkRef, desired sandboxState) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultSandboxCleanupTimeout)
	defer cancel()
	state, err := r.api.State(ctx, work.Namespace, work.Name)
	if err != nil {
		return fmt.Errorf("recover ambiguous agent Sandbox create: %w", err)
	}
	if state.Exists {
		if !compatibleSandboxState(state, desired) {
			return fmt.Errorf("%w: ambiguous agent Sandbox %s/%s has incompatible execution identity or workload shape", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
		}
		if state.UID == "" {
			return fmt.Errorf("%w: ambiguous agent Sandbox %s/%s has no UID", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
		}
		work.UID = state.UID
	}
	return r.Cleanup(ctx, work)
}

func (r *AgentSandboxRuntime) cleanupWork(work engineruntime.WorkRef) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultSandboxCleanupTimeout)
	defer cancel()
	return r.Cleanup(ctx, work)
}

func (r *AgentSandboxRuntime) waitTerminal(ctx context.Context, work engineruntime.WorkRef) (sandboxState, error) {
	poll := time.NewTicker(r.opts.PollEvery)
	defer poll.Stop()
	for {
		state, err := r.api.State(ctx, work.Namespace, work.Name)
		if err != nil {
			return sandboxState{}, fmt.Errorf("read agent Sandbox state: %w", err)
		}
		if !state.Exists {
			return sandboxState{}, fmt.Errorf("%w: agent Sandbox disappeared", engineruntime.ErrWorkIdentityChanged)
		}
		if work.UID != "" && state.UID != work.UID {
			return sandboxState{}, fmt.Errorf("%w: agent Sandbox UID changed", engineruntime.ErrWorkIdentityChanged)
		}
		if state.Finished {
			if state.PodName == "" {
				state.PodName = work.Name
			}
			return state, nil
		}
		select {
		case <-ctx.Done():
			return sandboxState{}, ctx.Err()
		case <-poll.C:
		}
	}
}

func (r *AgentSandboxRuntime) sandboxObject(name string, requestJSON, contractHash []byte, request engineruntime.ExecutionRequest, executionID string) map[string]any {
	shutdown := r.now().Add(time.Duration(request.TimeoutSeconds)*time.Second + defaultSandboxCleanupTimeout).UTC().Format(time.RFC3339)
	return map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1",
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "prow-ai-dashboard",
				"agents.x-k8s.io/created-by":   "prow-ai-dashboard",
				agentSandboxExecutionLabel:     name,
			},
			"annotations": map[string]any{
				agentSandboxContractAnnotation: hex.EncodeToString(contractHash),
				agentSandboxIDAnnotation:       strings.TrimSpace(executionID),
			},
		},
		"spec": map[string]any{
			"service":        false,
			"operatingMode":  "Running",
			"shutdownTime":   shutdown,
			"shutdownPolicy": "Delete",
			"podTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{agentSandboxExecutionLabel: name}},
				"spec":     r.workloadPodSpec(requestJSON, request),
			},
		},
	}
}

func (r *AgentSandboxRuntime) resourceMetadata(name string, state sandboxState) engineruntime.ResourceMetadata {
	return engineruntime.ResourceMetadata{
		Backend:          agentSandboxBackend,
		Namespace:        r.opts.Namespace,
		Name:             name,
		PodName:          state.PodName,
		NodeName:         state.NodeName,
		RuntimeClassName: r.opts.RuntimeClassName,
		CPURequest:       r.opts.Resources.CPURequest,
		CPULimit:         r.opts.Resources.CPULimit,
		MemoryRequest:    r.opts.Resources.MemoryRequest,
		MemoryLimit:      r.opts.Resources.MemoryLimit,
		EphemeralStorage: r.opts.Resources.EphemeralStorage,
		MeasuredUsage:    false,
	}
}

func executionRequest(spec engineruntime.GenerateSpec) (engineruntime.ExecutionRequest, error) {
	if spec.Repo.Token != "" {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("agent sandbox does not accept a repository token")
	}
	if spec.Model != "" || spec.NativeModel != "" || spec.UseAmbientAuth || spec.Endpoint != "" || spec.Token != "" || len(spec.ExtraHeaders) > 0 || len(spec.Skills) > 0 || len(spec.NetworkDomains) > 0 {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("agent sandbox does not accept model or provider credentials")
	}
	if spec.AllowBash || spec.CommandPolicy.AllowShell {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("agent sandbox shell execution is not enabled by this prototype")
	}
	repositoryURL := strings.TrimSpace(spec.Repo.CloneURL)
	if repositoryURL == "" {
		if strings.TrimSpace(spec.Repo.Owner) == "" || strings.TrimSpace(spec.Repo.Name) == "" {
			return engineruntime.ExecutionRequest{}, fmt.Errorf("agent sandbox repository owner and name are required")
		}
		repositoryURL = "https://github.com/" + spec.Repo.Owner + "/" + spec.Repo.Name + ".git"
	}
	maxSteps := spec.MaxSteps
	if maxSteps == 0 {
		maxSteps = spec.MaxTurns
	}
	outputLimit := spec.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = defaultSandboxOutputLimit
	}
	request := engineruntime.ExecutionRequest{
		Version:          engineruntime.ExecutionContractVersion,
		RepositoryURL:    repositoryURL,
		CommitSHA:        strings.TrimSpace(spec.Repo.Ref),
		Prompt:           spec.Instruction,
		TimeoutSeconds:   int64(spec.Timeout.Round(time.Second) / time.Second),
		MaxSteps:         maxSteps,
		MaxFiles:         spec.MaxFiles,
		CommandPolicy:    spec.CommandPolicy,
		ModelGateway:     spec.ModelGateway,
		ExpectedBaseSHA:  strings.TrimSpace(spec.ExpectedBaseSHA),
		OutputLimitBytes: outputLimit,
	}
	if err := request.Validate(); err != nil {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("agent sandbox request: %w", err)
	}
	return request, nil
}

func decodeExecutionResult(raw string) (engineruntime.ExecutionResult, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var result engineruntime.ExecutionResult
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return result, fmt.Errorf("result contains trailing data")
	}
	return result, nil
}

func compareExecutionFiles(result engineruntime.ExecutionResult, files map[string]string) error {
	actual := sortedFileNames(files)
	if !equalStrings(actual, result.ChangedFiles) {
		return fmt.Errorf("reported files %v do not match reconstructed files %v", result.ChangedFiles, actual)
	}
	for name, content := range files {
		if result.Files[name] != content {
			return fmt.Errorf("reported content for %s does not match reconstructed content", name)
		}
	}
	return nil
}

func sortedFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func boundedSummary(values ...string) string {
	value := strings.TrimSpace(strings.Join(values, "\n"))
	const limit = 8 << 10
	if len(value) > limit {
		return value[len(value)-limit:]
	}
	return value
}

func agentSandboxContractHash(requestJSON []byte, opts AgentSandboxOptions) [sha256.Size]byte {
	hash := sha256.New()
	for _, value := range [][]byte{
		requestJSON, []byte(opts.Image), []byte(opts.ServiceAccountName), []byte(opts.RuntimeClassName),
		[]byte(opts.Resources.CPURequest), []byte(opts.Resources.CPULimit), []byte(opts.Resources.MemoryRequest),
		[]byte(opts.Resources.MemoryLimit), []byte(opts.Resources.EphemeralStorage), []byte(opts.appArmorCapability.String()), strconv.AppendBool(nil, opts.PublicCAPrivateDNS),
	} {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(value)
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

func agentSandboxName(executionID string, requestHash []byte) string {
	prefix := strings.ToLower(strings.TrimSpace(executionID))
	var b strings.Builder
	for _, r := range prefix {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	prefix = strings.Trim(b.String(), "-")
	if prefix == "" {
		prefix = "fix"
	}
	if len(prefix) > 32 {
		prefix = strings.Trim(prefix[:32], "-")
	}
	return "fix-" + prefix + "-" + hex.EncodeToString(requestHash[:6])
}

type kubeAgentSandboxAPI struct {
	dynamic      dynamic.Interface
	http         *http.Client
	host         string
	podLifecycle func(context.Context, string, string) string
}

func agentSandboxRESTConfig(contextName string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func newKubeAgentSandboxAPI(cfg *rest.Config) (*kubeAgentSandboxAPI, error) {
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeAgentSandboxAPI{dynamic: dynamicClient, http: httpClient, host: strings.TrimRight(cfg.Host, "/")}, nil
}

func (k *kubeAgentSandboxAPI) Create(ctx context.Context, namespace string, object map[string]any) (sandboxState, error) {
	desired := &unstructured.Unstructured{Object: object}
	created, err := k.dynamic.Resource(sandboxesGVR).Namespace(namespace).Create(ctx, desired, metav1.CreateOptions{})
	desiredState := sandboxStateFromObject(desired)
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := k.dynamic.Resource(sandboxesGVR).Namespace(namespace).Get(ctx, desired.GetName(), metav1.GetOptions{})
		if getErr != nil {
			return sandboxState{}, getErr
		}
		existingState := sandboxStateFromObject(existing)
		if !compatibleSandboxState(existingState, desiredState) {
			return sandboxState{}, fmt.Errorf("%w: existing agent Sandbox execution or workload shape changed", engineruntime.ErrWorkIdentityChanged)
		}
		return existingState, nil
	}
	if err != nil {
		return sandboxState{}, err
	}
	createdState := sandboxStateFromObject(created)
	if !compatibleSandboxState(createdState, desiredState) {
		return createdState, fmt.Errorf("%w: created agent Sandbox workload shape changed", engineruntime.ErrWorkIdentityChanged)
	}
	return createdState, nil
}

func (k *kubeAgentSandboxAPI) State(ctx context.Context, namespace, name string) (sandboxState, error) {
	object, err := k.dynamic.Resource(sandboxesGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return sandboxState{}, nil
	}
	if err != nil {
		return sandboxState{}, err
	}
	return sandboxStateFromObject(object), nil
}

func (k *kubeAgentSandboxAPI) Delete(ctx context.Context, namespace, name, uid string) error {
	uidValue := types.UID(uid)
	err := k.dynamic.Resource(sandboxesGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uidValue}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *kubeAgentSandboxAPI) PodLogs(ctx context.Context, namespace, podName string, limit int64) (string, error) {
	endpoint := k.host + "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(podName) + "/log"
	query := url.Values{"container": {agentSandboxContainerName}, "limitBytes": {fmt.Sprintf("%d", limit+1)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("construct Pod log request: %w", err)
	}
	response, err := k.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("request Pod logs: %s", safeKubernetesDiagnostic(err.Error()))
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		lifecycle := k.podLogLifecycleContext(ctx, namespace, podName)
		body, bodyErr := readBoundedKubernetesBody(response.Body)
		if errors.Is(bodyErr, errKubernetesErrorBodyOversized) {
			return "", fmt.Errorf("pod logs for %s/%s unavailable: %s; Kubernetes API HTTP %d status response exceeds %d bytes", namespace, podName, lifecycle, response.StatusCode, maxKubernetesErrorBodyBytes)
		}
		if bodyErr != nil {
			return "", fmt.Errorf("pod logs for %s/%s unavailable: %s; read Kubernetes API HTTP %d status response: %s", namespace, podName, lifecycle, response.StatusCode, safeKubernetesDiagnostic(bodyErr.Error()))
		}
		return "", fmt.Errorf("pod logs for %s/%s unavailable: %s; Kubernetes API HTTP %d: %s", namespace, podName, lifecycle, response.StatusCode, kubernetesStatusDetail(body))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+2))
	if err != nil {
		return "", fmt.Errorf("read pod logs: %s", safeKubernetesDiagnostic(err.Error()))
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("pod logs response exceeds %d bytes", limit)
	}
	return string(data), nil
}

func (k *kubeAgentSandboxAPI) PodExists(ctx context.Context, namespace, podName string) (bool, error) {
	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	_, err := k.dynamic.Resource(podsGVR).Namespace(namespace).Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (k *kubeAgentSandboxAPI) ExecutionPods(ctx context.Context, namespace, executionName string) ([]string, error) {
	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	list, err := k.dynamic.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: agentSandboxExecutionLabel + "=" + executionName})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, pod := range list.Items {
		names = append(names, pod.GetName())
	}
	sort.Strings(names)
	return names, nil
}

func compatibleSandboxState(existing, desired sandboxState) bool {
	if existing.ContractHash != desired.ContractHash || existing.ExecutionID != desired.ExecutionID || existing.ShapeHash != desired.ShapeHash {
		return false
	}
	existingShutdown, existingErr := time.Parse(time.RFC3339, existing.ShutdownTime)
	desiredShutdown, desiredErr := time.Parse(time.RFC3339, desired.ShutdownTime)
	return existingErr == nil && desiredErr == nil && !existingShutdown.After(desiredShutdown)
}

func sandboxShapeHash(object *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	delete(spec, "shutdownTime")
	value := struct {
		Labels map[string]string `json:"labels"`
		Spec   map[string]any    `json:"spec"`
	}{Labels: object.GetLabels(), Spec: spec}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sandboxStateFromObject(object *unstructured.Unstructured) sandboxState {
	annotations := object.GetAnnotations()
	state := sandboxState{
		Exists: true, UID: string(object.GetUID()), PodName: object.GetName(),
		ContractHash: annotations[agentSandboxContractAnnotation], ExecutionID: annotations[agentSandboxIDAnnotation],
		ShapeHash: sandboxShapeHash(object),
	}
	state.ShutdownTime, _, _ = unstructured.NestedString(object.Object, "spec", "shutdownTime")
	if value := annotations[agentSandboxPodAnnotation]; value != "" {
		state.PodName = value
	}
	state.NodeName, _, _ = unstructured.NestedString(object.Object, "status", "nodeName")
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Finished" && condition["status"] == "True" {
			state.Finished = true
			state.FinishedReason, _ = condition["reason"].(string)
		}
	}
	return state
}
