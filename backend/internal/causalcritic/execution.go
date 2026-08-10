package causalcritic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	ExecutionSchemaVersion = 1
	RequestEnv             = "PROW_AI_CAUSAL_CRITIC_REQUEST_B64"
	DefaultOutputLimit     = int64(64 << 10)
	maxExecutionRequest    = 95 << 10
)

var decimalCostRE = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,9})?$`)

// RuntimeIdentity fingerprints the immutable executor and non-secret model route.
func RuntimeIdentity(gateway engineruntime.ModelGatewayConfig, sandboxIdentity string, timeout time.Duration, outputLimit int64) string {
	data, _ := json.Marshal(struct {
		ContractVersion string                           `json:"contract_version"`
		Gateway         engineruntime.ModelGatewayConfig `json:"gateway"`
		SandboxIdentity string                           `json:"sandbox_identity"`
		Timeout         string                           `json:"timeout"`
		OutputLimit     int64                            `json:"output_limit"`
	}{ContractVersion, gateway, strings.TrimSpace(sandboxIdentity), timeout.String(), outputLimit})
	return hashString(string(data))
}

// ExecutionRequest is the complete credential-free critic workload input.
type ExecutionRequest struct {
	SchemaVersion   int                              `json:"schema_version"`
	ContractVersion string                           `json:"contract_version"`
	Input           Input                            `json:"input"`
	ModelGateway    engineruntime.ModelGatewayConfig `json:"model_gateway"`
	TimeoutSeconds  int64                            `json:"timeout_seconds"`
	OutputLimit     int64                            `json:"output_limit_bytes"`
}

// GatewayUsage contains only identity and usage reported by the gateway response.
type GatewayUsage struct {
	Status            string `json:"status"`
	Source            string `json:"source"`
	Model             string `json:"model,omitempty"`
	Provider          string `json:"provider,omitempty"`
	InputTokens       int64  `json:"input_tokens,omitempty"`
	CachedInputTokens int64  `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens,omitempty"`
	CostUSD           string `json:"cost_usd,omitempty"`
	NanoAIU           int64  `json:"nano_aiu,omitempty"`
}

// ExecutionResult is the single structured result emitted by the critic executor.
type ExecutionResult struct {
	SchemaVersion   int                         `json:"schema_version"`
	ContractVersion string                      `json:"contract_version"`
	PairHash        string                      `json:"pair_hash"`
	TerminalState   engineruntime.TerminalState `json:"terminal_state"`
	Review          *Review                     `json:"review,omitempty"`
	Usage           GatewayUsage                `json:"usage"`
	DurationMs      int64                       `json:"duration_ms"`
	FailureReason   string                      `json:"failure_reason,omitempty"`
}

// Result adds dashboard-observed lifecycle facts to one executor result.
type Result struct {
	Execution   ExecutionResult
	Resources   engineruntime.ResourceMetadata
	Telemetry   engineruntime.GenerateTelemetry
	CleanupWork *engineruntime.WorkRef
}

// Runtime runs a private critic through the shared Agent Sandbox lifecycle.
type Runtime struct {
	Sandbox          agentsandbox.Runner
	Gateway          engineruntime.ModelGatewayConfig
	Timeout          time.Duration
	OutputLimitBytes int64
}

// RuntimeIdentity seals the critic contract to the full Sandbox workload configuration.
func (r *Runtime) RuntimeIdentity() string {
	if r == nil || r.Sandbox == nil {
		return ""
	}
	limit := r.OutputLimitBytes
	if limit == 0 {
		limit = DefaultOutputLimit
	}
	return RuntimeIdentity(r.Gateway, r.Sandbox.RuntimeIdentity(), r.Timeout, limit)
}

// Review runs one exact evidence-and-draft pair without publication authority.
func (r *Runtime) Review(ctx context.Context, input Input, executionID string, observer engineruntime.WorkObserver) (result Result, retErr error) {
	if r == nil || r.Sandbox == nil {
		return result, fmt.Errorf("%w: causal critic runtime is unavailable", engineruntime.ErrUnavailable)
	}
	request, err := r.request(input)
	if err != nil {
		return result, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("encode causal critic request: %w", err)
	}
	raw, runErr := r.Sandbox.Run(ctx, agentsandbox.Spec{
		Purpose: "critic", ExecutionID: executionID, RequestEnv: RequestEnv, Request: data,
		Timeout: r.Timeout, OutputLimitBytes: r.OutputLimitBytes, WorkObserver: observer,
	})
	result.Resources = raw.Resources
	result.Telemetry = raw.Telemetry
	result.CleanupWork = raw.Work
	if strings.TrimSpace(raw.Output) == "" {
		return result, errors.Join(fmt.Errorf("%w: causal critic result is empty", engineruntime.ErrMalformedResult), runErr)
	}
	result.Telemetry.FinalizationChecked = true
	parsed, err := DecodeExecutionResult(raw.Output)
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: causal critic result: %v", engineruntime.ErrMalformedResult, err), runErr)
	}
	result.Execution = parsed
	if err := ValidateExecutionResult(parsed, request); err != nil {
		return result, errors.Join(fmt.Errorf("%w: causal critic result: %v", engineruntime.ErrResultContract, err), runErr)
	}
	if parsed.Usage.Status == "reported" || parsed.Usage.Status == "partial" {
		result.Telemetry.TokenUsageAvailable = parsed.Usage.InputTokens != 0 || parsed.Usage.OutputTokens != 0
		result.Telemetry.CostAvailable = parsed.Usage.CostUSD != "" || parsed.Usage.NanoAIU != 0
		result.Telemetry.UsageStatus = "reported_by_gateway"
	} else {
		result.Telemetry.UsageStatus = "unavailable_from_gateway"
	}
	if raw.FinishedReason == "PodSucceeded" && parsed.TerminalState != engineruntime.TerminalSucceeded {
		return result, errors.Join(fmt.Errorf("%w: succeeded Pod reported %q", engineruntime.ErrResultContract, parsed.TerminalState), runErr)
	}
	if raw.FinishedReason == "PodFailed" && parsed.TerminalState == engineruntime.TerminalSucceeded {
		return result, errors.Join(fmt.Errorf("%w: failed Pod reported success", engineruntime.ErrResultContract), runErr)
	}
	result.Telemetry.FinalizationValid = true
	if parsed.TerminalState != engineruntime.TerminalSucceeded {
		switch parsed.TerminalState {
		case engineruntime.TerminalTimedOut:
			return result, errors.Join(context.DeadlineExceeded, runErr)
		case engineruntime.TerminalCancelled:
			return result, errors.Join(engineruntime.ErrCancelled, runErr)
		default:
			return result, errors.Join(fmt.Errorf("causal critic execution failed: %s", parsed.FailureReason), runErr)
		}
	}
	return result, runErr
}

// Cleanup retries one exact persisted Agent Sandbox identity without rerunning the model.
func (r *Runtime) Cleanup(ctx context.Context, work engineruntime.WorkRef) error {
	if r == nil || r.Sandbox == nil {
		return fmt.Errorf("%w: causal critic runtime is unavailable", engineruntime.ErrUnavailable)
	}
	return r.Sandbox.Cleanup(ctx, work)
}

func (r *Runtime) request(input Input) (ExecutionRequest, error) {
	if err := ValidateInput(input); err != nil {
		return ExecutionRequest{}, err
	}
	limit := r.OutputLimitBytes
	if limit == 0 {
		limit = DefaultOutputLimit
	}
	request := ExecutionRequest{
		SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, Input: input,
		ModelGateway: r.Gateway, TimeoutSeconds: int64(r.Timeout.Round(time.Second) / time.Second), OutputLimit: limit,
	}
	if err := ValidateExecutionRequest(request); err != nil {
		return ExecutionRequest{}, err
	}
	return request, nil
}

// ValidateExecutionRequest enforces an internal credential-free gateway and bounded input.
func ValidateExecutionRequest(request ExecutionRequest) error {
	if request.SchemaVersion != ExecutionSchemaVersion || request.ContractVersion != ContractVersion {
		return fmt.Errorf("unsupported causal critic execution contract")
	}
	if err := ValidateInput(request.Input); err != nil {
		return err
	}
	if err := ValidateGatewayConfig(request.ModelGateway); err != nil {
		return err
	}
	if err := engineruntime.ValidateModelGatewayTrust(request.ModelGateway.Endpoint, false); err != nil {
		return fmt.Errorf("causal critic gateway: %w", err)
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > int64((30*time.Minute)/time.Second) {
		return fmt.Errorf("causal critic timeout must be between 1 and 1800 seconds")
	}
	if request.OutputLimit < 4<<10 || request.OutputLimit > 1<<20 {
		return fmt.Errorf("causal critic output limit must be between 4096 and 1048576")
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > maxExecutionRequest {
		return fmt.Errorf("causal critic execution request exceeds %d bytes", maxExecutionRequest)
	}
	return nil
}

// ValidateExecutionResult applies only deterministic schema and evidence-reference checks.
func ValidateExecutionResult(result ExecutionResult, request ExecutionRequest) error {
	if result.SchemaVersion != ExecutionSchemaVersion || result.ContractVersion != ContractVersion || result.PairHash != request.Input.PairHash {
		return fmt.Errorf("execution result identity mismatch")
	}
	if result.DurationMs < 0 || result.DurationMs > request.TimeoutSeconds*1000+5000 {
		return fmt.Errorf("execution duration is outside the request bound")
	}
	if err := validateGatewayUsage(result.Usage); err != nil {
		return err
	}
	switch result.TerminalState {
	case engineruntime.TerminalSucceeded:
		if result.Review == nil || strings.TrimSpace(result.FailureReason) != "" {
			return fmt.Errorf("successful critic result requires one review and no failure reason")
		}
		if err := ValidateReview(*result.Review, request.Input); err != nil {
			return err
		}
	case engineruntime.TerminalFailed, engineruntime.TerminalTimedOut, engineruntime.TerminalCancelled:
		if result.Review != nil || strings.TrimSpace(result.FailureReason) == "" || len(result.FailureReason) > 2<<10 {
			return fmt.Errorf("failed critic result must contain only a bounded failure reason")
		}
	default:
		return fmt.Errorf("unsupported critic terminal state %q", result.TerminalState)
	}
	return nil
}

// DecodeExecutionResult rejects prose, code fences, unknown fields, and trailing values.
func DecodeExecutionResult(raw string) (ExecutionResult, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var result ExecutionResult
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return result, fmt.Errorf("result contains trailing data")
	}
	return result, nil
}

// ValidateGatewayConfig validates the credential-free critic gateway identity.
func ValidateGatewayConfig(gateway engineruntime.ModelGatewayConfig) error {
	parsed, err := url.Parse(strings.TrimSpace(gateway.Endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("causal critic gateway must be an absolute credential-free HTTPS URL")
	}
	if strings.TrimSpace(gateway.Model) == "" || len(gateway.Model) > 256 || strings.ContainsAny(gateway.Model, "\r\n\x00") {
		return fmt.Errorf("causal critic gateway model is invalid")
	}
	if gateway.ProtocolVersion != "openai-chat-completions-v1" {
		return fmt.Errorf("causal critic gateway protocol %q is unsupported", gateway.ProtocolVersion)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if net.ParseIP(host) != nil || !strings.HasSuffix(host, ".svc") && !strings.HasSuffix(host, ".svc.cluster.local") && !strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("causal critic gateway must use internal service DNS")
	}
	return nil
}

func validateGatewayUsage(usage GatewayUsage) error {
	if usage.Source != "gateway_response" {
		return fmt.Errorf("critic usage source must be gateway_response")
	}
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens > usage.InputTokens || usage.NanoAIU < 0 {
		return fmt.Errorf("critic gateway token usage is invalid")
	}
	for _, value := range []string{usage.Model, usage.Provider} {
		if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("critic gateway identity is invalid")
		}
	}
	if usage.CostUSD != "" && !decimalCostRE.MatchString(usage.CostUSD) {
		return fmt.Errorf("critic gateway cost is invalid")
	}
	hasUsage := usage.Model != "" || usage.Provider != "" || usage.InputTokens != 0 || usage.CachedInputTokens != 0 || usage.OutputTokens != 0 || usage.CostUSD != "" || usage.NanoAIU != 0
	switch usage.Status {
	case "unavailable":
		if hasUsage {
			return fmt.Errorf("unavailable critic usage contains reported fields")
		}
	case "partial":
		if !hasUsage {
			return fmt.Errorf("partial critic usage contains no reported fields")
		}
	case "reported":
		if usage.Model == "" || usage.InputTokens == 0 && usage.OutputTokens == 0 {
			return fmt.Errorf("reported critic usage lacks model or token counts")
		}
	default:
		return fmt.Errorf("unsupported critic usage status %q", usage.Status)
	}
	return nil
}
