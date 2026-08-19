// Package modelprovider defines the non-secret provider contract used by Agent Sandbox OpenCode executors.
package modelprovider

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	APIChatCompletions = "chat_completions"
	APIResponses       = "responses"

	CredentialModeDirect  = "direct"
	CredentialModeGateway = "gateway"

	AuthTypeNone   = "none"
	AuthTypeBearer = "bearer"

	// TokenEnv is the only provider credential environment variable admitted to an executor.
	TokenEnv = "PROW_AI_MODEL_PROVIDER_TOKEN"

	// CopilotIntegrationHeader identifies the calling integration to GitHub Copilot,
	// which answers a request without it with an unexplained 403.
	CopilotIntegrationHeader = "Copilot-Integration-Id"
	CopilotIntegrationID     = "copilot-developer-cli"
)

var ErrCredentialExposure = errors.New("credential-bearing executor output rejected")

// Auth identifies how OpenCode authenticates to the configured provider.
type Auth struct {
	Type     string `json:"type"`
	TokenEnv string `json:"token_env,omitempty"`
}

// Config identifies one non-secret model provider operation endpoint.
type Config struct {
	CredentialMode     string          `json:"credential_mode"`
	API                string          `json:"api"`
	Endpoint           string          `json:"endpoint"`
	Model              string          `json:"model"`
	ReasoningEffort    ReasoningEffort `json:"reasoning_effort,omitempty"`
	Auth               Auth            `json:"auth"`
	PublicCAPrivateDNS bool            `json:"public_ca_private_dns,omitempty"`
}

// Normalize applies configuration defaults before the provider enters a wire contract.
func Normalize(config Config) Config {
	config.CredentialMode = strings.ToLower(strings.TrimSpace(config.CredentialMode))
	if config.CredentialMode == "" {
		config.CredentialMode = CredentialModeDirect
	}
	config.API = strings.ToLower(strings.TrimSpace(config.API))
	if config.API == "" {
		config.API = APIChatCompletions
	}
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	config.Model = strings.TrimSpace(config.Model)
	config.ReasoningEffort = CanonicalReasoningEffort(string(config.ReasoningEffort))
	config.Auth.Type = strings.ToLower(strings.TrimSpace(config.Auth.Type))
	if config.Auth.Type == "" {
		config.Auth.Type = AuthTypeNone
	}
	config.Auth.TokenEnv = strings.TrimSpace(config.Auth.TokenEnv)
	if config.Auth.Type == AuthTypeBearer && config.Auth.TokenEnv == "" {
		config.Auth.TokenEnv = TokenEnv
	}
	return config
}

// EndpointHeaders returns the headers every caller must send to this endpoint,
// whether it runs in process or inside a sandbox executor. Keeping one
// definition stops the two paths from diverging on the same provider.
func EndpointHeaders(endpoint string) map[string]string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "githubcopilot.com" || strings.HasSuffix(host, ".githubcopilot.com") {
		return map[string]string{CopilotIntegrationHeader: CopilotIntegrationID}
	}
	return nil
}

// ValidateOpenCode validates the Agent Sandbox OpenCode provider contract.
func ValidateOpenCode(config Config) error {
	if config != Normalize(config) {
		return fmt.Errorf("model provider configuration must be normalized")
	}
	switch config.CredentialMode {
	case CredentialModeDirect, CredentialModeGateway:
	default:
		return fmt.Errorf("model provider credential mode %q is unsupported", config.CredentialMode)
	}
	if config.API != APIChatCompletions && config.API != APIResponses {
		return fmt.Errorf("model provider API %q is unsupported", config.API)
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("model provider endpoint must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme == "http" {
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		ip := net.ParseIP(host)
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("plain HTTP model provider endpoints are limited to loopback tests")
		}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("model provider endpoint must not contain credentials, a query, or a fragment")
	}
	if config.Model == "" || len(config.Model) > 256 || strings.ContainsAny(config.Model, "\r\n\x00") {
		return fmt.Errorf("model provider model must be non-empty, bounded, and single-line")
	}
	if _, err := NormalizeReasoningEffort(string(config.ReasoningEffort)); err != nil {
		return err
	}
	if config.ReasoningEffort == ReasoningEffortMax {
		return fmt.Errorf("pinned OpenCode 1.18.2 does not support reasoning effort max")
	}
	if config.CredentialMode == CredentialModeGateway {
		if config.Auth.Type != AuthTypeNone || config.Auth.TokenEnv != "" {
			return fmt.Errorf("gateway credential mode must use auth type none")
		}
		if config.API == APIResponses {
			return fmt.Errorf("responses requires direct bearer auth with the pinned OpenCode provider")
		}
		return nil
	}
	if config.PublicCAPrivateDNS {
		return fmt.Errorf("public CA private DNS applies only to gateway credential mode")
	}
	switch config.Auth.Type {
	case AuthTypeNone:
		if config.Auth.TokenEnv != "" {
			return fmt.Errorf("auth type none must not set a token environment variable")
		}
	case AuthTypeBearer:
		if config.Auth.TokenEnv != TokenEnv {
			return fmt.Errorf("bearer auth must use the fixed provider token environment variable")
		}
	default:
		return fmt.Errorf("model provider auth type %q is unsupported", config.Auth.Type)
	}
	if config.API == APIResponses && config.Auth.Type != AuthTypeBearer {
		return fmt.Errorf("responses requires direct bearer auth with the pinned OpenCode provider")
	}
	return nil
}

// ValidateDeploymentEndpoint requires the encrypted endpoint used by an Agent Sandbox workload.
func ValidateDeploymentEndpoint(config Config) error {
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("model provider endpoint must be an absolute HTTPS URL")
	}
	return ValidateOpenCode(config)
}

// OpenCodeAdapter describes the native AI SDK package and base URL OpenCode uses.
type OpenCodeAdapter struct {
	NPM     string
	BaseURL string
}

// OpenCodeAdapterFor derives the native provider package from one full operation endpoint.
func OpenCodeAdapterFor(config Config) (OpenCodeAdapter, error) {
	if err := ValidateOpenCode(config); err != nil {
		return OpenCodeAdapter{}, err
	}
	parsed, _ := url.Parse(config.Endpoint)
	path := strings.TrimRight(parsed.Path, "/")
	var suffix, npm string
	switch config.API {
	case APIChatCompletions:
		suffix = "/chat/completions"
		npm = "@ai-sdk/openai-compatible"
	case APIResponses:
		suffix = "/responses"
		npm = "@ai-sdk/openai"
	default:
		return OpenCodeAdapter{}, fmt.Errorf("model provider API %q is unsupported", config.API)
	}
	if !strings.HasSuffix(path, suffix) {
		return OpenCodeAdapter{}, fmt.Errorf("%s endpoint must end with %s", config.API, suffix)
	}
	parsed.Path = strings.TrimSuffix(path, suffix)
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return OpenCodeAdapter{NPM: npm, BaseURL: strings.TrimRight(parsed.String(), "/")}, nil
}

// OpenCodeBaseURL derives the native provider base URL.
func OpenCodeBaseURL(config Config) (string, error) {
	adapter, err := OpenCodeAdapterFor(config)
	return adapter.BaseURL, err
}

// CredentialGuard holds the exact provider credential only inside an executor process.
type CredentialGuard struct {
	value string
}

// NewCredentialGuard resolves the one admitted bearer environment value.
func NewCredentialGuard(config Config, lookup func(string) (string, bool)) (CredentialGuard, error) {
	if err := ValidateOpenCode(config); err != nil {
		return CredentialGuard{}, err
	}
	if config.Auth.Type != AuthTypeBearer {
		return CredentialGuard{}, nil
	}
	value, ok := lookup(TokenEnv)
	// The token arrives from a Kubernetes Secret, so it can carry a trailing
	// newline. Trim before validating: the executor hands this exact value to
	// OpenCode, where a stray byte surfaces only as a provider 401.
	value = strings.TrimSpace(value)
	if !ok || value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return CredentialGuard{}, fmt.Errorf("provider credential environment is unavailable or invalid")
	}
	return CredentialGuard{value: value}, nil
}

// Environment returns the one credential entry OpenCode may inherit.
func (g CredentialGuard) Environment() []string {
	if g.value == "" {
		return nil
	}
	return []string{TokenEnv + "=" + g.value}
}

// CheckStrings rejects any exact provider credential in output-bound strings.
func (g CredentialGuard) CheckStrings(values ...string) error {
	if g.value == "" {
		return nil
	}
	for _, value := range values {
		if strings.Contains(value, g.value) {
			return ErrCredentialExposure
		}
	}
	return nil
}

// CheckBytes rejects any exact provider credential in output-bound bytes.
func (g CredentialGuard) CheckBytes(values ...[]byte) error {
	if g.value == "" {
		return nil
	}
	needle := []byte(g.value)
	for _, value := range values {
		if bytes.Contains(value, needle) {
			return ErrCredentialExposure
		}
	}
	return nil
}

// SanitizeReason returns a fixed reason instead of credential-bearing text.
func (g CredentialGuard) SanitizeReason(value string) string {
	if g.CheckStrings(value) != nil {
		return ErrCredentialExposure.Error()
	}
	return value
}

// Detector finds an exact credential across streaming write boundaries without retaining output.
type Detector struct {
	mu       sync.Mutex
	needle   []byte
	tail     []byte
	detected bool
}

// NewDetector constructs a streaming detector for this credential.
func (g CredentialGuard) NewDetector() *Detector {
	return &Detector{needle: []byte(g.value)}
}

// Write implements io.Writer while retaining only the suffix needed for the next match.
func (d *Detector) Write(value []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.needle) == 0 || d.detected {
		return len(value), nil
	}
	combined := make([]byte, 0, len(d.tail)+len(value))
	combined = append(combined, d.tail...)
	combined = append(combined, value...)
	if bytes.Contains(combined, d.needle) {
		d.detected = true
		d.tail = nil
		return len(value), nil
	}
	keep := len(d.needle) - 1
	if keep > len(combined) {
		keep = len(combined)
	}
	d.tail = append(d.tail[:0], combined[len(combined)-keep:]...)
	return len(value), nil
}

// Detected reports whether the credential appeared in the observed stream.
func (d *Detector) Detected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.detected
}
