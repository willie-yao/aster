package runtime

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
)

const (
	// ExecutionContractVersion is the wire contract shared by external fix runtimes.
	ExecutionContractVersion           = 2
	maxExecutionPromptBytes            = 32 << 10
	maxExecutionOutputBytes            = 1 << 20
	maxExecutionCommands               = 32
	maxExecutionArgBytes               = 4 << 10
	maxExecutionSingleArgBytes         = 1 << 10
	maxExecutionResourceMetadataBytes  = 4 << 10
	maxExecutionCommandDurationGraceMs = 5_000
)

// ExecutionCommand is one exact argv allowed by the execution policy.
type ExecutionCommand struct {
	Argv           []string `json:"argv"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
}

// CommandPolicy bounds which commands an executor may run.
type CommandPolicy struct {
	AllowShell bool               `json:"allow_shell"`
	Commands   []ExecutionCommand `json:"commands,omitempty"`
}

// ModelGatewayConfig identifies the tokenless gateway used by the causal critic.
type ModelGatewayConfig struct {
	Endpoint        string `json:"endpoint"`
	Model           string `json:"model"`
	ProtocolVersion string `json:"protocol_version"`
}

// ExecutionRequest is the non-secret contract sent to an external fix executor.
type ExecutionRequest struct {
	Version          int                  `json:"version"`
	RepositoryURL    string               `json:"repository_url"`
	CommitSHA        string               `json:"commit_sha"`
	Prompt           string               `json:"prompt"`
	TimeoutSeconds   int64                `json:"timeout_seconds"`
	MaxSteps         int                  `json:"max_steps"`
	MaxFiles         int                  `json:"max_files"`
	CommandPolicy    CommandPolicy        `json:"command_policy"`
	ModelProvider    modelprovider.Config `json:"model_provider"`
	ExpectedBaseSHA  string               `json:"expected_base_sha"`
	OutputLimitBytes int64                `json:"output_limit_bytes"`
}

// TerminalState is the final state reported by an external fix executor.
type TerminalState string

const (
	TerminalSucceeded TerminalState = "succeeded"
	TerminalFailed    TerminalState = "failed"
	TerminalTimedOut  TerminalState = "timed_out"
	TerminalCancelled TerminalState = "cancelled"
)

// CommandResult records one allowed command execution.
type CommandResult struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
	TimedOut   bool     `json:"timed_out,omitempty"`
}

// ResourceMetadata records the execution placement and enforced resource policy.
type ResourceMetadata struct {
	Backend          string `json:"backend,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	Name             string `json:"name,omitempty"`
	PodName          string `json:"pod_name,omitempty"`
	NodeName         string `json:"node_name,omitempty"`
	RuntimeClassName string `json:"runtime_class_name,omitempty"`
	CPURequest       string `json:"cpu_request,omitempty"`
	CPULimit         string `json:"cpu_limit,omitempty"`
	MemoryRequest    string `json:"memory_request,omitempty"`
	MemoryLimit      string `json:"memory_limit,omitempty"`
	EphemeralStorage string `json:"ephemeral_storage_limit,omitempty"`
	MeasuredUsage    bool   `json:"measured_usage"`
}

// ExecutionResult is the provider-neutral outcome of a bounded fix execution.
type ExecutionResult struct {
	Version        int               `json:"version,omitempty"`
	BaseSHA        string            `json:"base_sha,omitempty"`
	ChangedFiles   []string          `json:"changed_files,omitempty"`
	Files          map[string]string `json:"files,omitempty"`
	Diff           string            `json:"diff,omitempty"`
	CommandResults []CommandResult   `json:"command_results,omitempty"`
	StdoutSummary  string            `json:"stdout_summary,omitempty"`
	StderrSummary  string            `json:"stderr_summary,omitempty"`
	TerminalState  TerminalState     `json:"terminal_state,omitempty"`
	DurationMs     int64             `json:"duration_ms,omitempty"`
	Resources      ResourceMetadata  `json:"resources,omitempty"`
	FailureReason  string            `json:"failure_reason,omitempty"`

	// Output is the legacy bounded summary consumed by the existing Fix PR path.
	Output string `json:"-"`
	// Attempts is the number of external attempts when the backend reports it.
	Attempts  int               `json:"-"`
	Telemetry GenerateTelemetry `json:"-"`
}

// GenerateResult is retained as the Fix PR runtime result name.
type GenerateResult = ExecutionResult

// Validate checks the non-secret execution request contract.
func (r ExecutionRequest) Validate() error {
	if r.Version != ExecutionContractVersion {
		return fmt.Errorf("execution request version %d is not supported", r.Version)
	}
	parsed, err := url.Parse(strings.TrimSpace(r.RepositoryURL))
	if err != nil || parsed.Scheme == "" {
		return fmt.Errorf("repository URL must be absolute")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("repository URL must not contain credentials, a query, or a fragment")
	}
	if err := validateExecutionRepositoryURL(parsed); err != nil {
		return err
	}
	if !isImmutableSHA(r.CommitSHA) {
		return fmt.Errorf("commit SHA must be 40 lowercase hexadecimal characters")
	}
	if r.ExpectedBaseSHA != r.CommitSHA {
		return fmt.Errorf("expected base SHA must equal the immutable commit SHA")
	}
	if strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > maxExecutionPromptBytes {
		return fmt.Errorf("prompt must be non-empty and at most %d bytes", maxExecutionPromptBytes)
	}
	if err := modelprovider.ValidateDeploymentEndpoint(r.ModelProvider); err != nil {
		return err
	}
	if _, err := modelprovider.OpenCodeBaseURL(r.ModelProvider); err != nil {
		return err
	}
	if r.ModelProvider.CredentialMode == modelprovider.CredentialModeGateway {
		if err := ValidateModelGatewayTrust(r.ModelProvider.Endpoint, r.ModelProvider.PublicCAPrivateDNS); err != nil {
			return fmt.Errorf("model provider gateway: %w", err)
		}
	}
	if r.TimeoutSeconds <= 0 || time.Duration(r.TimeoutSeconds)*time.Second > 30*time.Minute {
		return fmt.Errorf("timeout must be greater than zero and at most 30m")
	}
	if r.MaxSteps <= 0 || r.MaxSteps > 1000 {
		return fmt.Errorf("max steps must be between 1 and 1000")
	}
	if r.MaxFiles <= 0 || r.MaxFiles > 100 {
		return fmt.Errorf("max files must be between 1 and 100")
	}
	if r.OutputLimitBytes <= 0 || r.OutputLimitBytes > maxExecutionOutputBytes {
		return fmt.Errorf("output limit must be between 1 and %d bytes", maxExecutionOutputBytes)
	}
	if r.CommandPolicy.AllowShell {
		return fmt.Errorf("shell execution is not allowed")
	}
	if err := ValidateExecutionCommands(r.CommandPolicy.Commands, r.TimeoutSeconds, r.MaxSteps); err != nil {
		return err
	}
	if len(r.CommandPolicy.Commands) == 0 {
		return fmt.Errorf("at least one final validation command is required")
	}
	last := r.CommandPolicy.Commands[len(r.CommandPolicy.Commands)-1].Argv
	if !sameArgv(last, []string{"git", "diff", "--cached", "--check"}) {
		return fmt.Errorf("the final validation command must be git diff --cached --check")
	}
	return nil
}

// ValidateExecutionCommands validates exact argv and timeout bounds.
func ValidateExecutionCommands(commands []ExecutionCommand, executionTimeoutSeconds int64, maxSteps int) error {
	if len(commands) > maxExecutionCommands {
		return fmt.Errorf("allowed commands exceed the request bounds")
	}
	if len(commands) >= maxSteps {
		return fmt.Errorf("max steps must reserve at least one coding-agent step after allowed commands")
	}
	for index, command := range commands {
		if len(command.Argv) == 0 {
			return fmt.Errorf("allowed command %d has no argv", index)
		}
		var size int
		for argIndex, arg := range command.Argv {
			if arg == "" {
				return fmt.Errorf("allowed command %d argv %d is empty", index, argIndex)
			}
			if strings.ContainsAny(arg, "\r\n\x00") {
				return fmt.Errorf("allowed command %d argv %d must be single-line and contain no NUL byte", index, argIndex)
			}
			if len(arg) > maxExecutionSingleArgBytes {
				return fmt.Errorf("allowed command %d argv %d exceeds %d bytes", index, argIndex, maxExecutionSingleArgBytes)
			}
			size += len(arg)
		}
		if size > maxExecutionArgBytes {
			return fmt.Errorf("allowed command %d exceeds %d bytes", index, maxExecutionArgBytes)
		}
		if command.TimeoutSeconds <= 0 || command.TimeoutSeconds > executionTimeoutSeconds {
			return fmt.Errorf("allowed command %d timeout must be positive and no greater than the execution timeout", index)
		}
		if err := ValidateExecutionCommandArgv(command.Argv); err != nil {
			return fmt.Errorf("allowed command %d: %w", index, err)
		}
	}
	return nil
}

// Validate checks the structured executor result against its request.
func (r ExecutionResult) Validate(request ExecutionRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if r.Version != ExecutionContractVersion {
		return fmt.Errorf("execution result version %d is not supported", r.Version)
	}
	if r.BaseSHA != request.ExpectedBaseSHA {
		return fmt.Errorf("execution result base SHA %q does not match expected %q", r.BaseSHA, request.ExpectedBaseSHA)
	}
	switch r.TerminalState {
	case TerminalSucceeded:
		if strings.TrimSpace(r.FailureReason) != "" {
			return fmt.Errorf("successful execution has a failure reason")
		}
	case TerminalFailed, TerminalTimedOut, TerminalCancelled:
		if strings.TrimSpace(r.FailureReason) == "" {
			return fmt.Errorf("terminal state %q requires a failure reason", r.TerminalState)
		}
	default:
		return fmt.Errorf("terminal state %q is not supported", r.TerminalState)
	}
	if r.DurationMs < 0 || r.DurationMs > request.TimeoutSeconds*1000+30_000 {
		return fmt.Errorf("execution duration is outside the request bound")
	}
	if len(r.CommandResults) > len(request.CommandPolicy.Commands) || len(r.CommandResults) > request.MaxSteps {
		return fmt.Errorf("command results exceed the request policy")
	}
	if r.TerminalState == TerminalSucceeded {
		if err := ValidateSuccessfulCommandResults(request.CommandPolicy.Commands, r.CommandResults); err != nil {
			return err
		}
	} else {
		for index, result := range r.CommandResults {
			if !sameArgv(result.Argv, request.CommandPolicy.Commands[index].Argv) {
				return fmt.Errorf("command result %d does not match the allowed argv", index)
			}
			if err := validateCommandResultTiming(index, request.CommandPolicy.Commands[index], result); err != nil {
				return err
			}
		}
	}
	changed := append([]string(nil), r.ChangedFiles...)
	sort.Strings(changed)
	if !sameStrings(changed, r.ChangedFiles) {
		return fmt.Errorf("changed files must be sorted")
	}
	if len(changed) != len(r.Files) {
		return fmt.Errorf("changed file list and file contents differ")
	}
	if len(changed) > request.MaxFiles {
		return fmt.Errorf("changed files exceed the request max_files bound")
	}
	if len(changed) > 0 && strings.TrimSpace(r.Diff) == "" {
		return fmt.Errorf("changed files require a unified diff")
	}
	for index, name := range changed {
		if index > 0 && changed[index-1] == name {
			return fmt.Errorf("changed file list contains %q more than once", name)
		}
		if !safeExecutionPath(name) {
			return fmt.Errorf("changed file path %q is unsafe", name)
		}
		if _, ok := r.Files[name]; !ok {
			return fmt.Errorf("changed file %q has no content", name)
		}
	}
	payload := r
	payload.Resources = ResourceMetadata{}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode execution result payload: %w", err)
	}
	if int64(len(encodedPayload)+1) > request.OutputLimitBytes {
		return fmt.Errorf("execution result exceeds the %d-byte output limit", request.OutputLimitBytes)
	}
	encodedMetadata, err := json.Marshal(r.Resources)
	if err != nil {
		return fmt.Errorf("encode execution resource metadata: %w", err)
	}
	if len(encodedMetadata) > maxExecutionResourceMetadataBytes {
		return fmt.Errorf("execution resource metadata exceeds %d bytes", maxExecutionResourceMetadataBytes)
	}
	encodedResult, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode enriched execution result: %w", err)
	}
	if int64(len(encodedResult)+1) > request.OutputLimitBytes+maxExecutionResourceMetadataBytes {
		return fmt.Errorf("enriched execution result exceeds its bounded metadata allowance")
	}
	return nil
}

// ValidateSuccessfulCommandResults verifies the complete ordered result set for
// one successful external execution. Every configured command must appear once,
// succeed within its timeout, and end with the exact staged diff check.
func ValidateSuccessfulCommandResults(commands []ExecutionCommand, results []CommandResult) error {
	if len(commands) == 0 {
		return fmt.Errorf("at least one final validation command is required")
	}
	if !sameArgv(commands[len(commands)-1].Argv, []string{"git", "diff", "--cached", "--check"}) {
		return fmt.Errorf("the final validation command must be git diff --cached --check")
	}
	if len(results) != len(commands) {
		return fmt.Errorf("successful execution must report every allowed command exactly once")
	}
	for index, result := range results {
		if commands[index].TimeoutSeconds <= 0 || commands[index].TimeoutSeconds > int64((30*time.Minute)/time.Second) {
			return fmt.Errorf("allowed command %d has an invalid timeout", index)
		}
		if err := ValidateExecutionCommandArgv(commands[index].Argv); err != nil {
			return fmt.Errorf("allowed command %d: %w", index, err)
		}
		if !sameArgv(result.Argv, commands[index].Argv) {
			return fmt.Errorf("command result %d does not match the allowed argv", index)
		}
		if err := validateCommandResultTiming(index, commands[index], result); err != nil {
			return err
		}
		if result.TimedOut {
			return fmt.Errorf("command result %d timed out", index)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("command result %d failed with exit code %d", index, result.ExitCode)
		}
	}
	return nil
}

func validateCommandResultTiming(index int, command ExecutionCommand, result CommandResult) error {
	if result.DurationMs < 0 {
		return fmt.Errorf("command result %d has a negative duration", index)
	}
	commandTimeoutMs := command.TimeoutSeconds * 1000
	if result.ExitCode == 0 && !result.TimedOut && result.DurationMs > commandTimeoutMs {
		return fmt.Errorf("successful command result %d duration exceeds its configured timeout", index)
	}
	if result.DurationMs > commandTimeoutMs+maxExecutionCommandDurationGraceMs {
		return fmt.Errorf("command result %d duration exceeds its configured timeout and cleanup grace", index)
	}
	return nil
}

// ValidateModelGatewayTrust validates the deployed gateway TLS and host policy.
func ValidateModelGatewayTrust(endpoint string, publicCAPrivateDNS bool) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return fmt.Errorf("endpoint must be an absolute https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must not contain credentials, a query, or a fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	internal := isInternalModelGatewayHost(host)
	if publicCAPrivateDNS {
		if internal {
			return fmt.Errorf("public CA private DNS applies only to a privately resolved public FQDN")
		}
		if !isPublicCAPrivateDNSHost(host) {
			return fmt.Errorf("public CA private DNS requires a non-provider DNS FQDN")
		}
	} else if !internal {
		return fmt.Errorf("endpoint host must be internal or public CA private DNS must be enabled")
	}
	return nil
}

func isInternalModelGatewayHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") || strings.HasSuffix(host, ".internal") || ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

func isPublicCAPrivateDNSHost(host string) bool {
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") {
		return false
	}
	for _, suffix := range []string{
		"openai.com", "openai.azure.com", "services.ai.azure.com", "anthropic.com",
		"githubcopilot.com", "copilot.microsoft.com", "moonshot.cn", "kimi.com",
		"generativelanguage.googleapis.com", "api.nvidia.com", "mistral.ai", "cohere.ai",
		"groq.com", "together.xyz", "deepseek.com", "x.ai",
	} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	return true
}

// ValidateExecutionCommandArgv validates executable-specific argv policy.
func ValidateExecutionCommandArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("has no argv")
	}
	if err := ValidateExecutionCommandExecutable(argv[0]); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(argv[0]), "git") && !sameArgv(argv, []string{"git", "diff", "--cached", "--check"}) {
		return fmt.Errorf("git is reserved for the exact final diff check")
	}
	return nil
}

// ValidateExecutionCommandExecutable rejects shell and generic command dispatchers.
func ValidateExecutionCommandExecutable(value string) error {
	if strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("must use a PATH-resolved executable")
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sh", "bash", "dash", "zsh", "ksh", "fish", "cmd", "cmd.exe", "powershell", "pwsh":
		return fmt.Errorf("must not invoke a shell")
	case "env", "busybox", "toybox":
		return fmt.Errorf("must not use a command dispatcher")
	case "opencode", "fixexecutor":
		return fmt.Errorf("must not invoke a coding agent or executor")
	default:
		return nil
	}
}

func validateExecutionRepositoryURL(parsed *url.URL) error {
	switch parsed.Scheme {
	case "file":
		if parsed.Host != "" || !filepath.IsAbs(parsed.Path) {
			return fmt.Errorf("file repository URL must use an absolute local path without a host")
		}
		return nil
	case "https":
		host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if host == "" {
			return fmt.Errorf("https repository URL must include a host")
		}
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") {
			return fmt.Errorf("https repository URL must identify a public host")
		}
		if ip := net.ParseIP(host); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			return fmt.Errorf("https repository URL must identify a public host")
		}
		return nil
	default:
		return fmt.Errorf("repository URL scheme %q is not allowed", parsed.Scheme)
	}
}

func isImmutableSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func safeExecutionPath(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." || strings.HasPrefix(value, "..") {
		return false
	}
	return !strings.ContainsRune(value, '\\')
}

func sameArgv(left, right []string) bool {
	return sameStrings(left, right)
}

func sameStrings(left, right []string) bool {
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
