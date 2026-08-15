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
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	agentSandboxBackend                    = agentsandbox.Backend
	agentSandboxRequestEnv                 = "PROW_AI_FIX_EXECUTION_REQUEST_B64"
	agentSandboxExecutionLabel             = "prow-ai-dashboard/execution"
	agentSandboxContractAnnotation         = "prow-ai-dashboard/execution-contract-sha256"
	agentSandboxIDAnnotation               = "prow-ai-dashboard/execution-id"
	agentSandboxReasoningEffortAnnotation  = "prow-ai-dashboard/model-provider-reasoning-effort"
	agentSandboxCABundleAnnotation         = "prow-ai-dashboard/model-provider-ca-sha256"
	agentSandboxPreparedAnnotation         = "prow-ai-dashboard/prepared-manifest-sha256"
	agentSandboxPreparedIdentityAnnotation = "prow-ai-dashboard/prepared-workspace-sha256"
	agentSandboxPodAnnotation              = "agents.x-k8s.io/pod-name"
	agentSandboxContainerName              = "executor"
	agentSandboxStagerName                 = "stager"
	defaultSandboxPollEvery                = 250 * time.Millisecond
	defaultSandboxCleanupTimeout           = 30 * time.Second
	agentSandboxResultGrace                = 15 * time.Second
	defaultSandboxOutputLimit              = int64(512 << 10)
	defaultSandboxCPURequest               = "100m"
	defaultSandboxCPULimit                 = "1"
	defaultSandboxMemoryRequest            = "128Mi"
	defaultSandboxMemoryLimit              = "512Mi"
	defaultSandboxDiskLimit                = "256Mi"
)

func agentSandboxResultGraceForPurpose(purpose string) time.Duration {
	if purpose == "analysis" {
		return agentanalysis.WorkspacePostModelGrace
	}
	return agentSandboxResultGrace
}

func agentSandboxRunTimeout(spec agentsandbox.Spec) time.Duration {
	return spec.Timeout + agentSandboxResultGraceForPurpose(spec.Purpose) + 5*time.Second
}

var (
	sandboxesGVR                = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	configMapsGVR               = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	immutableExecutorImage      = regexp.MustCompile(`^[^[:space:]]+@sha256:[0-9a-f]{64}$`)
	agentSandboxInClusterConfig = rest.InClusterConfig
)

// RuntimeIdentity fingerprints the normalized non-secret Sandbox workload configuration.
func (r *AgentSandboxRuntime) RuntimeIdentity() string {
	if r == nil {
		return ""
	}
	opts := normalizeAgentSandboxOptions(r.opts)
	var caBundle *modelprovider.CABundleConfig
	if opts.CABundle.Enabled() {
		configured := opts.CABundle
		caBundle = &configured
	}
	payload, _ := json.Marshal(struct {
		Backend            string                           `json:"backend"`
		Namespace          string                           `json:"namespace"`
		Image              string                           `json:"image"`
		ServiceAccountName string                           `json:"service_account_name"`
		RuntimeClassName   string                           `json:"runtime_class_name"`
		ModelProvider      modelprovider.Config             `json:"model_provider,omitempty"`
		ProviderSecretRef  ProviderSecretRef                `json:"provider_secret_ref,omitempty"`
		ModelGateway       engineruntime.ModelGatewayConfig `json:"model_gateway,omitempty"`
		PublicCAPrivateDNS bool                             `json:"public_ca_private_dns,omitempty"`
		CABundle           *modelprovider.CABundleConfig    `json:"ca_bundle,omitempty"`
		CABundleContract   string                           `json:"ca_bundle_contract,omitempty"`
		Timeout            string                           `json:"timeout"`
		OutputLimitBytes   int64                            `json:"output_limit_bytes"`
		PollEvery          string                           `json:"poll_every"`
		Resources          AgentSandboxResources            `json:"resources"`
		AppArmorCapability string                           `json:"app_armor_capability"`
		StagerImage        string                           `json:"stager_image,omitempty"`
		StagerInputClaim   string                           `json:"stager_input_claim,omitempty"`
	}{
		Backend: agentSandboxBackend, Namespace: opts.Namespace, Image: opts.Image,
		ServiceAccountName: opts.ServiceAccountName, RuntimeClassName: opts.RuntimeClassName,
		ModelProvider: opts.ModelProvider, ProviderSecretRef: opts.ProviderSecretRef,
		ModelGateway: opts.ModelGateway, PublicCAPrivateDNS: opts.PublicCAPrivateDNS,
		CABundle: caBundle, CABundleContract: caBundleContract(opts.CABundle),
		Timeout: opts.Timeout.String(), OutputLimitBytes: opts.OutputLimitBytes, PollEvery: opts.PollEvery.String(),
		Resources: opts.Resources, AppArmorCapability: opts.appArmorCapability.String(), StagerImage: opts.StagerImage, StagerInputClaim: opts.StagerInputClaim,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// AgentSandboxOptions configure the experimental Agent Sandbox adapter.
type AgentSandboxResources struct {
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EphemeralStorage string
}

// ProviderSecretRef identifies the one existing Secret key admitted to direct bearer workloads.
type ProviderSecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type AgentSandboxOptions struct {
	Namespace          string
	Image              string
	StagerImage        string
	StagerInputClaim   string
	ServiceAccountName string
	RuntimeClassName   string
	ModelProvider      modelprovider.Config
	ProviderSecretRef  ProviderSecretRef
	ModelGateway       engineruntime.ModelGatewayConfig
	PublicCAPrivateDNS bool
	CABundle           modelprovider.CABundleConfig
	Timeout            time.Duration
	OutputLimitBytes   int64
	PollEvery          time.Duration
	Resources          AgentSandboxResources
	testOnly           bool
	appArmorCapability appArmorCapability
}

type sandboxState struct {
	Exists              bool
	UID                 string
	PodName             string
	NodeName            string
	ContractHash        string
	ExecutionID         string
	ShapeHash           string
	ShutdownTime        string
	Finished            bool
	FinishedReason      string
	PodCreatedAt        time.Time
	ScheduledAt         time.Time
	StageStartedAt      time.Time
	StageFinishedAt     time.Time
	ExecutionStartedAt  time.Time
	ExecutionFinishedAt time.Time
	TimingStatus        string
}

// agentSandboxAPI is the low-level lifecycle seam intended for a future shared
// Agent Sandbox package. It excludes Fix PR prompt, patch, and result policy.
type agentSandboxAPI interface {
	ValidateCABundle(context.Context, string, modelprovider.CABundleConfig) error
	Create(context.Context, string, map[string]any) (sandboxState, error)
	State(context.Context, string, string) (sandboxState, error)
	Delete(context.Context, string, string, string) error
	PodLogs(context.Context, string, string, int64) (string, error)
	PodExists(context.Context, string, string) (bool, error)
	ExecutionPods(context.Context, string, string) ([]string, error)
}

// AgentSandboxRuntime executes one non-secret request in a v1beta1 Sandbox.
type AgentSandboxRuntime struct {
	api       agentSandboxAPI
	opts      AgentSandboxOptions
	applyDiff func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error)
	now       func() time.Time
}

var (
	_ engineruntime.AgentRuntime        = (*AgentSandboxRuntime)(nil)
	_ engineruntime.ManagedAgentRuntime = (*AgentSandboxRuntime)(nil)
	_ agentsandbox.Runner               = (*AgentSandboxRuntime)(nil)
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

// NewAgentSandboxRuntimeFromEnv constructs the Fix PR adapter from deployment environment and Kubernetes config.
func NewAgentSandboxRuntimeFromEnv(expectedProvider modelprovider.Config, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	return newAgentSandboxProviderRunnerFromEnv("AGENT_SANDBOX_", "", true, true, expectedProvider, expectedTimeout, expectedOutputLimit)
}

// NewAgentSandboxProviderRunnerFromEnv constructs an OpenCode lifecycle runner from one reserved environment prefix.
func NewAgentSandboxProviderRunnerFromEnv(prefix string, expectedProvider modelprovider.Config, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	return newAgentSandboxProviderRunnerFromEnv(prefix, "", true, false, expectedProvider, expectedTimeout, expectedOutputLimit)
}

// NewAgentSandboxProviderRunnerForBenchmarkFromEnv allows an explicit disposable kubeconfig context for opt-in analyzer benchmarks.
func NewAgentSandboxProviderRunnerForBenchmarkFromEnv(prefix, kubeContext string, expectedProvider modelprovider.Config, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	if strings.TrimSpace(kubeContext) == "" {
		return nil, fmt.Errorf("agent sandbox benchmark kube context is required")
	}
	return newAgentSandboxProviderRunnerFromEnv(prefix, kubeContext, false, false, expectedProvider, expectedTimeout, expectedOutputLimit)
}

// NewAgentSandboxRunnerFromEnv retains the tokenless causal-critic gateway runner.
func NewAgentSandboxRunnerFromEnv(prefix string, expectedGateway engineruntime.ModelGatewayConfig, expectedPublicCAPrivateDNS bool, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	opts, err := agentSandboxBaseOptionsFromEnv(prefix)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimSpace(prefix)
	env := func(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }
	publicCAPrivateDNS := false
	if value := env("MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS"); value != "" {
		publicCAPrivateDNS, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("agent sandbox public CA private DNS setting is invalid")
		}
	}
	opts.ModelGateway = engineruntime.ModelGatewayConfig{
		Endpoint: env("MODEL_GATEWAY_ENDPOINT"), Model: env("MODEL_GATEWAY_MODEL"), ProtocolVersion: env("MODEL_GATEWAY_PROTOCOL"),
	}
	opts.PublicCAPrivateDNS = publicCAPrivateDNS
	if opts.ModelGateway != expectedGateway || opts.PublicCAPrivateDNS != expectedPublicCAPrivateDNS || opts.Timeout != expectedTimeout || opts.OutputLimitBytes != expectedOutputLimit {
		return nil, fmt.Errorf("agent sandbox deployment values do not match runtime configuration")
	}
	return finishAgentSandboxRunner(opts, "", true)
}

// NewAgentSandboxRunnerForBenchmarkFromEnv retains the tokenless causal-critic benchmark runner.
func NewAgentSandboxRunnerForBenchmarkFromEnv(prefix, kubeContext string, expectedGateway engineruntime.ModelGatewayConfig, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	if strings.TrimSpace(kubeContext) == "" {
		return nil, fmt.Errorf("agent sandbox benchmark kube context is required")
	}
	opts, err := agentSandboxBaseOptionsFromEnv(prefix)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimSpace(prefix)
	env := func(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }
	opts.ModelGateway = engineruntime.ModelGatewayConfig{
		Endpoint: env("MODEL_GATEWAY_ENDPOINT"), Model: env("MODEL_GATEWAY_MODEL"), ProtocolVersion: env("MODEL_GATEWAY_PROTOCOL"),
	}
	if opts.ModelGateway != expectedGateway || opts.Timeout != expectedTimeout || opts.OutputLimitBytes != expectedOutputLimit {
		return nil, fmt.Errorf("agent sandbox deployment values do not match runtime configuration")
	}
	return finishAgentSandboxRunner(opts, kubeContext, false)
}

func newAgentSandboxProviderRunnerFromEnv(prefix, kubeContext string, inClusterOnly, allowCABundle bool, expectedProvider modelprovider.Config, expectedTimeout time.Duration, expectedOutputLimit int64) (*AgentSandboxRuntime, error) {
	opts, err := agentSandboxBaseOptionsFromEnv(prefix)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimSpace(prefix)
	env := func(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }
	publicCAPrivateDNS := false
	if value := env("MODEL_PROVIDER_PUBLIC_CA_PRIVATE_DNS"); value != "" {
		publicCAPrivateDNS, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("agent sandbox provider public CA private DNS setting is invalid")
		}
	}
	opts.ModelProvider = modelprovider.Normalize(modelprovider.Config{
		CredentialMode: env("MODEL_PROVIDER_CREDENTIAL_MODE"), API: env("MODEL_PROVIDER_API"),
		Endpoint: env("MODEL_PROVIDER_ENDPOINT"), Model: env("MODEL_PROVIDER_MODEL"), ReasoningEffort: modelprovider.ReasoningEffort(env("MODEL_PROVIDER_REASONING_EFFORT")),
		Auth: modelprovider.Auth{Type: env("MODEL_PROVIDER_AUTH_TYPE")}, PublicCAPrivateDNS: publicCAPrivateDNS,
	})
	opts.ProviderSecretRef = ProviderSecretRef{Name: env("MODEL_PROVIDER_AUTH_SECRET_NAME"), Key: env("MODEL_PROVIDER_AUTH_SECRET_KEY")}
	if allowCABundle {
		opts.CABundle = modelprovider.CABundleConfig{
			ExistingConfigMap: env("MODEL_PROVIDER_CA_CONFIG_MAP"),
			Key:               env("MODEL_PROVIDER_CA_KEY"),
			SHA256:            env("MODEL_PROVIDER_CA_SHA256"),
		}
	}
	if opts.ModelProvider != expectedProvider || opts.Timeout != expectedTimeout || opts.OutputLimitBytes != expectedOutputLimit {
		return nil, fmt.Errorf("agent sandbox deployment values do not match runtime configuration")
	}
	return finishAgentSandboxRunner(opts, kubeContext, inClusterOnly)
}

func agentSandboxBaseOptionsFromEnv(prefix string) (AgentSandboxOptions, error) {
	prefix = strings.TrimSpace(prefix)
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*_$`).MatchString(prefix) {
		return AgentSandboxOptions{}, fmt.Errorf("agent sandbox environment prefix is invalid")
	}
	env := func(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }
	outputLimit, err := parseInt64Value(prefix+"OUTPUT_LIMIT_BYTES", env("OUTPUT_LIMIT_BYTES"))
	if err != nil {
		return AgentSandboxOptions{}, err
	}
	timeout, err := time.ParseDuration(env("TIMEOUT"))
	if err != nil {
		return AgentSandboxOptions{}, fmt.Errorf("agent sandbox timeout: %w", err)
	}
	opts := AgentSandboxOptions{
		Namespace: env("NAMESPACE"), Image: env("IMAGE"), StagerImage: env("STAGER_IMAGE"), StagerInputClaim: env("STAGER_INPUT_CLAIM"),
		ServiceAccountName: env("SERVICE_ACCOUNT"), RuntimeClassName: env("RUNTIME_CLASS"), Timeout: timeout, OutputLimitBytes: outputLimit,
		Resources: AgentSandboxResources{
			CPURequest: env("CPU_REQUEST"), CPULimit: env("CPU_LIMIT"), MemoryRequest: env("MEMORY_REQUEST"),
			MemoryLimit: env("MEMORY_LIMIT"), EphemeralStorage: env("EPHEMERAL_STORAGE_LIMIT"),
		},
	}
	if value := env("POLL_INTERVAL"); value != "" {
		poll, err := time.ParseDuration(value)
		if err != nil || poll <= 0 {
			return AgentSandboxOptions{}, fmt.Errorf("agent sandbox poll interval %q is invalid", value)
		}
		opts.PollEvery = poll
	}
	return normalizeAgentSandboxOptions(opts), nil
}

func finishAgentSandboxRunner(opts AgentSandboxOptions, kubeContext string, inClusterOnly bool) (*AgentSandboxRuntime, error) {
	if err := validateAgentSandboxOptions(opts); err != nil {
		return nil, err
	}
	var (
		cfg *rest.Config
		err error
	)
	if inClusterOnly {
		cfg, err = agentSandboxInClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("agent sandbox requires in-cluster Kubernetes configuration: %w", err)
		}
	} else {
		cfg, err = agentSandboxKubeconfigContextConfig(kubeContext)
		if err != nil {
			return nil, fmt.Errorf("agent sandbox benchmark Kubernetes configuration: %w", err)
		}
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
	if !opts.testOnly && opts.StagerImage != "" && !immutableExecutorImage.MatchString(opts.StagerImage) {
		return fmt.Errorf("agent sandbox stager image must use an immutable sha256 digest")
	}
	if opts.StagerImage != "" && opts.StagerInputClaim == "" {
		return fmt.Errorf("agent sandbox stager image requires an input claim")
	}
	if opts.StagerInputClaim != "" && len(k8svalidation.IsDNS1123Subdomain(opts.StagerInputClaim)) > 0 {
		return fmt.Errorf("agent sandbox stager input claim is invalid")
	}
	if !opts.testOnly && strings.TrimSpace(opts.RuntimeClassName) == "" {
		return fmt.Errorf("agent sandbox secure runtime class is required")
	}
	if err := modelprovider.ValidateCABundleConfig(opts.CABundle); err != nil {
		return fmt.Errorf("agent sandbox: %w", err)
	}
	if opts.CABundle.Enabled() {
		if len(k8svalidation.IsDNS1123Subdomain(opts.CABundle.ExistingConfigMap)) > 0 || len(k8svalidation.IsConfigMapKey(opts.CABundle.Key)) > 0 {
			return fmt.Errorf("agent sandbox model provider CA ConfigMap name and key are invalid")
		}
		if opts.ModelProvider == (modelprovider.Config{}) {
			return fmt.Errorf("agent sandbox model provider CA bundle is Fix-runtime only")
		}
	}
	if opts.Timeout <= 0 || opts.Timeout > 30*time.Minute {
		return fmt.Errorf("agent sandbox timeout must be greater than zero and at most 30m")
	}
	if opts.OutputLimitBytes < 4<<10 || opts.OutputLimitBytes > 1<<20 {
		return fmt.Errorf("agent sandbox output limit must be between 4096 and 1048576")
	}
	hasProvider := opts.ModelProvider != (modelprovider.Config{})
	hasGateway := opts.ModelGateway != (engineruntime.ModelGatewayConfig{})
	if hasProvider == hasGateway {
		return fmt.Errorf("agent sandbox requires exactly one model provider or legacy critic gateway")
	}
	if hasProvider {
		if err := modelprovider.ValidateDeploymentEndpoint(opts.ModelProvider); err != nil {
			return fmt.Errorf("agent sandbox model provider: %w", err)
		}
		if _, err := modelprovider.OpenCodeBaseURL(opts.ModelProvider); err != nil {
			return fmt.Errorf("agent sandbox model provider: %w", err)
		}
		if opts.ModelProvider.CredentialMode == modelprovider.CredentialModeGateway && !opts.testOnly {
			if err := engineruntime.ValidateModelGatewayTrust(opts.ModelProvider.Endpoint, opts.ModelProvider.PublicCAPrivateDNS); err != nil {
				return fmt.Errorf("agent sandbox model provider gateway: %w", err)
			}
		}
		if opts.ModelProvider.Auth.Type == modelprovider.AuthTypeBearer {
			if len(k8svalidation.IsDNS1123Subdomain(opts.ProviderSecretRef.Name)) > 0 || len(k8svalidation.IsConfigMapKey(opts.ProviderSecretRef.Key)) > 0 {
				return fmt.Errorf("agent sandbox provider Secret name and key are required and must be valid")
			}
		} else if opts.ProviderSecretRef != (ProviderSecretRef{}) {
			return fmt.Errorf("agent sandbox provider Secret settings require direct bearer auth")
		}
	} else {
		if opts.ProviderSecretRef != (ProviderSecretRef{}) {
			return fmt.Errorf("agent sandbox critic gateway must not carry a provider Secret reference")
		}
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

func parseInt64Value(name, value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return parsed, nil
}

// Generate adapts the shared Sandbox lifecycle to the Fix PR contract.
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
	if request.ModelProvider != r.opts.ModelProvider || time.Duration(request.TimeoutSeconds)*time.Second != r.opts.Timeout || request.OutputLimitBytes != r.opts.OutputLimitBytes {
		return result, fmt.Errorf("agent sandbox request does not match configured provider, timeout, or output limit")
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("agent sandbox encode request: %w", err)
	}
	raw, runErr := r.Run(ctx, agentsandbox.Spec{
		Purpose: "fix", ExecutionID: spec.ExecutionID, RequestEnv: agentSandboxRequestEnv, Request: requestJSON,
		Timeout: spec.Timeout, OutputLimitBytes: request.OutputLimitBytes, WritableWorkspace: true, WorkObserver: spec.WorkObserver,
	})
	result.Resources = raw.Resources
	result.Telemetry = raw.Telemetry
	result.DurationMs = max(raw.Duration.Milliseconds(), 0)
	if strings.TrimSpace(raw.Output) == "" {
		if runErr == nil {
			runErr = fmt.Errorf("%w: agent Sandbox result is empty", engineruntime.ErrMalformedResult)
		}
		result.Version = engineruntime.ExecutionContractVersion
		result.BaseSHA = request.ExpectedBaseSHA
		result.Files = map[string]string{}
		switch {
		case errors.Is(runErr, context.DeadlineExceeded):
			result.TerminalState = engineruntime.TerminalTimedOut
			result.FailureReason = "execution deadline exceeded"
		case errors.Is(runErr, engineruntime.ErrCancelled), errors.Is(runErr, context.Canceled):
			result.TerminalState = engineruntime.TerminalCancelled
			result.FailureReason = "execution cancelled"
		default:
			result.TerminalState = engineruntime.TerminalFailed
			if runErr != nil {
				result.FailureReason = safeKubernetesDiagnostic(runErr.Error())
			}
		}
		result.Output = boundedSummary(result.FailureReason)
		return result, runErr
	}
	result.Telemetry.FinalizationChecked = true
	parsed, err := decodeExecutionResult(raw.Output)
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: agent Sandbox result: %v", engineruntime.ErrMalformedResult, err), runErr)
	}
	parsed.Resources = raw.Resources
	parsed.Telemetry = result.Telemetry
	parsed.Output = boundedSummary(parsed.StdoutSummary, parsed.StderrSummary, parsed.FailureReason)
	if err := parsed.Validate(request); err != nil {
		return parsed, errors.Join(fmt.Errorf("%w: agent Sandbox result: %v", engineruntime.ErrResultContract, err), runErr)
	}
	if raw.FinishedReason == "PodSucceeded" && parsed.TerminalState != engineruntime.TerminalSucceeded {
		return parsed, errors.Join(fmt.Errorf("%w: succeeded Pod reported %q", engineruntime.ErrResultContract, parsed.TerminalState), runErr)
	}
	if raw.FinishedReason == "PodFailed" && parsed.TerminalState == engineruntime.TerminalSucceeded {
		return parsed, errors.Join(fmt.Errorf("%w: failed Pod reported success", engineruntime.ErrResultContract), runErr)
	}
	parsed.Telemetry.FinalizationValid = true
	if parsed.TerminalState != engineruntime.TerminalSucceeded {
		if parsed.TerminalState == engineruntime.TerminalCancelled {
			return parsed, errors.Join(fmt.Errorf("%w: %s", engineruntime.ErrCancelled, parsed.FailureReason), runErr)
		}
		return parsed, errors.Join(fmt.Errorf("agent Sandbox execution %s: %s", parsed.TerminalState, parsed.FailureReason), runErr)
	}

	apply := r.applyDiff
	if apply == nil {
		apply = engineruntime.ApplyDiff
	}
	files, diff, err := apply(ctx, spec.Repo, parsed.Diff)
	if err != nil {
		return parsed, errors.Join(fmt.Errorf("reconstructing agent Sandbox files: %w", err), runErr)
	}
	if err := compareExecutionFiles(parsed, files); err != nil {
		return parsed, errors.Join(fmt.Errorf("%w: %v", engineruntime.ErrResultExtraFile, err), runErr)
	}
	parsed.Files = files
	parsed.ChangedFiles = sortedFileNames(files)
	parsed.Diff = diff
	return parsed, runErr
}

// Run owns the business-neutral Agent Sandbox lifecycle and bounded result channel.
func (r *AgentSandboxRuntime) Run(ctx context.Context, spec agentsandbox.Spec) (result agentsandbox.Result, retErr error) {
	if r == nil || r.api == nil {
		return result, fmt.Errorf("%w: agent sandbox runtime is not configured", engineruntime.ErrUnavailable)
	}
	if err := validateAgentSandboxOptions(normalizeAgentSandboxOptions(r.opts)); err != nil {
		return result, fmt.Errorf("%w: %v", engineruntime.ErrUnavailable, err)
	}
	if err := agentsandbox.ValidateSpec(spec); err != nil {
		return result, err
	}
	if spec.StagedWorkspace != nil && (strings.TrimSpace(r.opts.StagerImage) == "" || strings.TrimSpace(r.opts.StagerInputClaim) == "") {
		return result, fmt.Errorf("agent sandbox staged workspace requires a stager image and input claim")
	}
	if spec.PreparedWorkspace != nil && strings.TrimSpace(r.opts.StagerInputClaim) == "" {
		return result, fmt.Errorf("agent sandbox prepared workspace requires an input claim")
	}
	if spec.Timeout != r.opts.Timeout || spec.OutputLimitBytes != r.opts.OutputLimitBytes {
		return result, fmt.Errorf("agent sandbox workload does not match configured timeout or output limit")
	}
	if r.opts.CABundle.Enabled() && spec.Purpose != "fix" {
		return result, fmt.Errorf("agent sandbox model provider CA bundle is Fix-runtime only")
	}
	if r.opts.ModelProvider != (modelprovider.Config{}) {
		result.Telemetry.ProviderCredentialMode = r.opts.ModelProvider.CredentialMode
		result.Telemetry.ProviderAPI = r.opts.ModelProvider.API
		result.Telemetry.ProviderReasoningEffort = string(r.opts.ModelProvider.ReasoningEffort)
	} else {
		result.Telemetry.ProviderCredentialMode = modelprovider.CredentialModeGateway
		result.Telemetry.ProviderAPI = modelprovider.APIChatCompletions
	}
	contractHash := agentSandboxWorkloadHash(spec, r.opts)
	executionID := strings.TrimSpace(spec.ExecutionID)
	if executionID == "" {
		executionID = "contract-" + hex.EncodeToString(contractHash[:8])
	}
	name := agentSandboxPurposeName(spec.Purpose, executionID, contractHash[:])
	work := engineruntime.WorkRef{Backend: agentSandboxBackend, Namespace: r.opts.Namespace, Name: name, ExecutionID: executionID}
	if spec.WorkObserver != nil {
		if err := spec.WorkObserver(ctx, work); err != nil {
			return result, fmt.Errorf("recording planned agent Sandbox: %w", err)
		}
	}

	started := r.now()
	result.Telemetry.UsageStatus = "unavailable_from_model_gateway"
	if result.Telemetry.ProviderCredentialMode == modelprovider.CredentialModeDirect {
		result.Telemetry.UsageStatus = "unavailable_from_direct_provider"
	}
	runCtx, cancel := context.WithTimeout(ctx, agentSandboxRunTimeout(spec))
	defer cancel()
	if r.opts.CABundle.Enabled() {
		if err := r.api.ValidateCABundle(runCtx, r.opts.Namespace, r.opts.CABundle); err != nil {
			return result, fmt.Errorf("%w: validate model provider CA bundle: %v", engineruntime.ErrUnavailable, err)
		}
	}
	object := r.sandboxObjectForSpec(name, spec, contractHash[:], executionID)
	desiredState := sandboxStateFromObject(&unstructured.Unstructured{Object: object})
	state, err := r.api.Create(runCtx, r.opts.Namespace, object)
	if err != nil {
		if state.Exists && state.UID != "" {
			work.UID = state.UID
			observed := work
			result.Work = &observed
			var observerErr error
			if spec.WorkObserver != nil {
				observerErr = spec.WorkObserver(runCtx, work)
			}
			return result, errors.Join(err, observerErr, r.cleanupWork(work))
		}
		if errors.Is(err, engineruntime.ErrWorkIdentityChanged) {
			return result, err
		}
		recovered, cleanupErr := r.recoverAmbiguousCreate(work, desiredState, spec.WorkObserver)
		if recovered.UID != "" {
			result.Work = &recovered
		}
		return result, errors.Join(fmt.Errorf("%w: create agent Sandbox: %v", engineruntime.ErrUnavailable, err), cleanupErr)
	}
	work.UID = state.UID
	if work.UID != "" {
		observed := work
		result.Work = &observed
	}
	result.Resources = r.resourceMetadata(name, state)
	if work.UID == "" {
		recovered, cleanupErr := r.recoverAmbiguousCreate(work, desiredState, spec.WorkObserver)
		if recovered.UID != "" {
			result.Work = &recovered
		}
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
	result.Duration = r.now().Sub(started)
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("agent Sandbox %s timed out: %w", name, context.DeadlineExceeded)
		}
		if errors.Is(runCtx.Err(), context.Canceled) {
			return result, fmt.Errorf("%w: agent Sandbox %s", engineruntime.ErrCancelled, name)
		}
		return result, err
	}
	result.Resources = r.resourceMetadata(name, terminal)
	result.FinishedReason = terminal.FinishedReason
	result.Telemetry.TaskFinalized = true
	result.Telemetry.TaskFinalizedMs = result.Duration.Milliseconds()
	result.Telemetry.SchedulingAvailable = !terminal.PodCreatedAt.IsZero() && !terminal.ScheduledAt.IsZero()
	result.Telemetry.SchedulingMs = durationMilliseconds(terminal.PodCreatedAt, terminal.ScheduledAt)
	result.Telemetry.StagingAvailable = spec.PreparedWorkspace != nil || (!terminal.StageStartedAt.IsZero() && !terminal.StageFinishedAt.IsZero())
	result.Telemetry.StagingMs = durationMilliseconds(terminal.StageStartedAt, terminal.StageFinishedAt)
	result.Telemetry.ExecutionAvailable = !terminal.ExecutionStartedAt.IsZero() && !terminal.ExecutionFinishedAt.IsZero()
	result.Telemetry.ExecutionMs = durationMilliseconds(terminal.ExecutionStartedAt, terminal.ExecutionFinishedAt)
	result.Telemetry.PhaseTimingStatus = terminal.TimingStatus
	if result.Telemetry.SchedulingAvailable && result.Telemetry.StagingAvailable && result.Telemetry.ExecutionAvailable {
		result.Telemetry.PhaseTimingStatus = "available"
	}

	logs, err := r.api.PodLogs(runCtx, r.opts.Namespace, terminal.PodName, spec.OutputLimitBytes)
	if err != nil {
		return result, fmt.Errorf("%w: read agent Sandbox result: %v", engineruntime.ErrMalformedResult, err)
	}
	result.Output = logs
	result.Telemetry.ResultAvailable = true
	result.Telemetry.ResultAvailableMs = r.now().Sub(started).Milliseconds()
	result.Telemetry.PublicationMs = max(result.Telemetry.ResultAvailableMs-result.Telemetry.TaskFinalizedMs, 0)
	result.Telemetry.PublicationAvailable = true
	return result, nil
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

func (r *AgentSandboxRuntime) recoverAmbiguousCreate(work engineruntime.WorkRef, desired sandboxState, observer engineruntime.WorkObserver) (engineruntime.WorkRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultSandboxCleanupTimeout)
	defer cancel()
	state, err := r.api.State(ctx, work.Namespace, work.Name)
	if err != nil {
		return work, fmt.Errorf("recover ambiguous agent Sandbox create: %w", err)
	}
	if state.Exists {
		if !compatibleSandboxState(state, desired) {
			return work, fmt.Errorf("%w: ambiguous agent Sandbox %s/%s has incompatible execution identity or workload shape", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
		}
		if state.UID == "" {
			return work, fmt.Errorf("%w: ambiguous agent Sandbox %s/%s has no UID", engineruntime.ErrWorkIdentityChanged, work.Namespace, work.Name)
		}
		work.UID = state.UID
	}
	var observerErr error
	if work.UID != "" && observer != nil {
		observerErr = observer(ctx, work)
	}
	return work, errors.Join(observerErr, r.Cleanup(ctx, work))
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

func (r *AgentSandboxRuntime) sandboxObjectForSpec(name string, spec agentsandbox.Spec, contractHash []byte, executionID string) map[string]any {
	shutdown := r.now().Add(spec.Timeout + defaultSandboxCleanupTimeout).UTC().Format(time.RFC3339)
	labels := map[string]any{
		"app.kubernetes.io/managed-by": "prow-ai-dashboard",
		"agents.x-k8s.io/created-by":   "prow-ai-dashboard",
		agentSandboxExecutionLabel:     name,
	}
	podLabels := map[string]any{agentSandboxExecutionLabel: name}
	if spec.Purpose != "fix" {
		labels["prow-ai-dashboard/purpose"] = spec.Purpose
		podLabels["prow-ai-dashboard/purpose"] = spec.Purpose
	}
	annotations := map[string]any{
		agentSandboxContractAnnotation: hex.EncodeToString(contractHash),
		agentSandboxIDAnnotation:       strings.TrimSpace(executionID),
	}
	if r.opts.ModelProvider.ReasoningEffort != "" {
		annotations[agentSandboxReasoningEffortAnnotation] = string(r.opts.ModelProvider.ReasoningEffort)
	}
	if r.opts.CABundle.Enabled() {
		annotations[agentSandboxCABundleAnnotation] = r.opts.CABundle.SHA256
	}
	if spec.PreparedWorkspace != nil {
		annotations[agentSandboxPreparedAnnotation] = spec.PreparedWorkspace.ManifestHash
		annotations[agentSandboxPreparedIdentityAnnotation] = spec.PreparedWorkspace.IdentityHash
	}
	return map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1",
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        name,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"service":        false,
			"operatingMode":  "Running",
			"shutdownTime":   shutdown,
			"shutdownPolicy": "Delete",
			"podTemplate": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec":     r.sandboxWorkloadPodSpec(spec),
			},
		},
	}
}

func (r *AgentSandboxRuntime) sandboxObject(name string, requestJSON, contractHash []byte, request engineruntime.ExecutionRequest, executionID string) map[string]any {
	return r.sandboxObjectForSpec(name, agentsandbox.Spec{
		Purpose: "fix", RequestEnv: agentSandboxRequestEnv, Request: requestJSON,
		Timeout: time.Duration(request.TimeoutSeconds) * time.Second, OutputLimitBytes: request.OutputLimitBytes, WritableWorkspace: true,
	}, contractHash, executionID)
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
		ModelProvider:    spec.ModelProvider,
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

func agentSandboxWorkloadHash(spec agentsandbox.Spec, opts AgentSandboxOptions) [sha256.Size]byte {
	stageEnv := ""
	if spec.StagedWorkspace != nil {
		stageEnv = spec.StagedWorkspace.RequestEnv
	}
	preparedHash := ""
	preparedIdentity := ""
	if spec.PreparedWorkspace != nil {
		preparedHash = spec.PreparedWorkspace.ManifestHash
		preparedIdentity = spec.PreparedWorkspace.IdentityHash
	}
	metadata, _ := json.Marshal(struct {
		Purpose              string `json:"purpose"`
		RequestEnv           string `json:"request_env"`
		Timeout              string `json:"timeout"`
		OutputLimitBytes     int64  `json:"output_limit_bytes"`
		WritableWorkspace    bool   `json:"writable_workspace"`
		StageRequestEnv      string `json:"stage_request_env,omitempty"`
		PreparedManifestHash string `json:"prepared_manifest_hash,omitempty"`
		PreparedIdentityHash string `json:"prepared_identity_hash,omitempty"`
	}{spec.Purpose, spec.RequestEnv, spec.Timeout.String(), spec.OutputLimitBytes, spec.WritableWorkspace, stageEnv, preparedHash, preparedIdentity})
	request := append(append(append([]byte(nil), metadata...), 0), spec.Request...)
	if spec.StagedWorkspace != nil {
		request = append(request, 0)
		request = append(request, spec.StagedWorkspace.Request...)
	}
	return agentSandboxContractHash(request, opts)
}

func agentSandboxContractHash(requestJSON []byte, opts AgentSandboxOptions) [sha256.Size]byte {
	hash := sha256.New()
	providerJSON, _ := json.Marshal(opts.ModelProvider)
	secretRefJSON, _ := json.Marshal(opts.ProviderSecretRef)
	values := [][]byte{
		requestJSON, providerJSON, secretRefJSON, []byte(opts.Image), []byte(opts.ServiceAccountName), []byte(opts.RuntimeClassName),
		[]byte(opts.Resources.CPURequest), []byte(opts.Resources.CPULimit), []byte(opts.Resources.MemoryRequest),
		[]byte(opts.Resources.MemoryLimit), []byte(opts.Resources.EphemeralStorage), []byte(opts.appArmorCapability.String()), strconv.AppendBool(nil, opts.PublicCAPrivateDNS),
	}
	if opts.StagerImage != "" {
		values = append(values, []byte(opts.StagerImage))
	}
	if opts.StagerInputClaim != "" {
		values = append(values, []byte(opts.StagerInputClaim))
	}
	if opts.CABundle.Enabled() {
		caBundleJSON, _ := json.Marshal(opts.CABundle)
		values = append(values, caBundleJSON, []byte(modelprovider.CABundleContractVersion))
	}
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(value)
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

func agentSandboxName(executionID string, requestHash []byte) string {
	return agentSandboxPurposeName("fix", executionID, requestHash)
}

func agentSandboxPurposeName(purpose, executionID string, requestHash []byte) string {
	prefix := strings.ToLower(strings.TrimSpace(executionID))
	var b strings.Builder
	for _, r := range prefix {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	prefix = strings.Trim(b.String(), "-")
	if prefix == "" {
		prefix = purpose
	}
	maxPrefix := min(32, 63-len(purpose)-14)
	if len(prefix) > maxPrefix {
		prefix = strings.Trim(prefix[:maxPrefix], "-")
	}
	return purpose + "-" + prefix + "-" + hex.EncodeToString(requestHash[:6])
}

type kubeAgentSandboxAPI struct {
	dynamic      dynamic.Interface
	http         *http.Client
	host         string
	podLifecycle func(context.Context, string, string) string
}

func caBundleContract(config modelprovider.CABundleConfig) string {
	if !config.Enabled() {
		return ""
	}
	return modelprovider.CABundleContractVersion
}

func agentSandboxRESTConfig(contextName string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return agentSandboxKubeconfigContextConfig(contextName)
}

func agentSandboxKubeconfigContextConfig(contextName string) (*rest.Config, error) {
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

func (k *kubeAgentSandboxAPI) ValidateCABundle(ctx context.Context, namespace string, config modelprovider.CABundleConfig) error {
	object, err := k.dynamic.Resource(configMapsGVR).Namespace(namespace).Get(ctx, config.ExistingConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	data, _, err := unstructured.NestedStringMap(object.Object, "data")
	if err != nil {
		return fmt.Errorf("read ConfigMap data: %w", err)
	}
	value, ok := data[config.Key]
	if !ok {
		if binaryData, _, binaryErr := unstructured.NestedStringMap(object.Object, "binaryData"); binaryErr != nil {
			return fmt.Errorf("read ConfigMap binary data: %w", binaryErr)
		} else if _, binary := binaryData[config.Key]; binary {
			return fmt.Errorf("model provider CA bundle key must use ConfigMap data, not binaryData")
		}
		return fmt.Errorf("model provider CA bundle ConfigMap key is missing")
	}
	if err := modelprovider.ValidateCABundle([]byte(value), config.SHA256); err != nil {
		return err
	}
	return nil
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
	state := sandboxStateFromObject(object)
	if state.Finished && state.PodName != "" {
		podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		if pod, podErr := k.dynamic.Resource(podsGVR).Namespace(namespace).Get(ctx, state.PodName, metav1.GetOptions{}); podErr == nil {
			enrichSandboxStateWithPod(&state, pod.Object)
		} else {
			state.TimingStatus = "pod_status_unavailable"
		}
	} else if state.Finished {
		state.TimingStatus = "pod_name_unavailable"
	}
	return state, nil
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

func enrichSandboxStateWithPod(state *sandboxState, pod map[string]any) {
	if state == nil || pod == nil {
		return
	}
	if value, found, _ := unstructured.NestedString(pod, "metadata", "creationTimestamp"); found {
		state.PodCreatedAt, _ = time.Parse(time.RFC3339, value)
	}
	conditions, _, _ := unstructured.NestedSlice(pod, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "PodScheduled" && condition["status"] == "True" {
			state.ScheduledAt, _ = time.Parse(time.RFC3339, stringValue(condition["lastTransitionTime"]))
		}
	}
	state.StageStartedAt, state.StageFinishedAt = containerTiming(pod, "initContainerStatuses", agentSandboxStagerName)
	state.ExecutionStartedAt, state.ExecutionFinishedAt = containerTiming(pod, "containerStatuses", agentSandboxContainerName)
	state.TimingStatus = "timestamps_incomplete"
}

func containerTiming(pod map[string]any, field, name string) (time.Time, time.Time) {
	statuses, _, _ := unstructured.NestedSlice(pod, "status", field)
	for _, raw := range statuses {
		status, _ := raw.(map[string]any)
		if status["name"] != name {
			continue
		}
		state, _, _ := unstructured.NestedMap(status, "state")
		if terminated, ok := state["terminated"].(map[string]any); ok {
			started, _ := time.Parse(time.RFC3339, stringValue(terminated["startedAt"]))
			finished, _ := time.Parse(time.RFC3339, stringValue(terminated["finishedAt"]))
			return started, finished
		}
		if running, ok := state["running"].(map[string]any); ok {
			started, _ := time.Parse(time.RFC3339, stringValue(running["startedAt"]))
			return started, time.Time{}
		}
	}
	return time.Time{}, time.Time{}
}

func durationMilliseconds(start, finish time.Time) int64 {
	if start.IsZero() || finish.IsZero() || finish.Before(start) {
		return 0
	}
	return finish.Sub(start).Milliseconds()
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
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
