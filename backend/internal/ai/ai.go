// Package ai provides model transports and the agentic tool-calling analysis
// loop. Service composes the universal Module and Client to analyze a
// single test failure.
package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
)

// callDelay throttles consecutive API calls. It is a var so tests can shrink it;
// production callers should not touch it.
var callDelay = 500 * time.Millisecond

// Client calls an OpenAI chat-completions compatible API for AI analysis.
type Client struct {
	api                *httpAPIClient
	transport          modelTransport
	apiMode            string
	apiURL             string
	model              string
	reasoningEffort    ReasoningEffort
	reasoningEffortErr error
	maxOutputTokens    int
	maxOutputTokensErr error
	cache              *Cache
}

// ReasoningEffort is the normalized provider reasoning-effort contract.
type ReasoningEffort = modelprovider.ReasoningEffort

const (
	ReasoningEffortNone   = modelprovider.ReasoningEffortNone
	ReasoningEffortLow    = modelprovider.ReasoningEffortLow
	ReasoningEffortMedium = modelprovider.ReasoningEffortMedium
	ReasoningEffortHigh   = modelprovider.ReasoningEffortHigh
	ReasoningEffortXHigh  = modelprovider.ReasoningEffortXHigh
	ReasoningEffortMax    = modelprovider.ReasoningEffortMax
)

// NormalizeReasoningEffort normalizes and validates one requested effort.
func NormalizeReasoningEffort(value string) (ReasoningEffort, error) {
	return modelprovider.NormalizeReasoningEffort(value)
}

// Options configures a Client. Endpoint and Model are required; the engine
// assumes no default provider.
type Options struct {
	Token    string
	CacheDir string
	// API selects chat_completions (default) or responses.
	API string
	// Endpoint is the chat-completions URL the provider serves.
	Endpoint string
	// Model is the model identifier the provider expects.
	Model string
	// ReasoningEffort requests provider reasoning depth. Empty preserves the
	// provider default and the historical request and cache identity.
	ReasoningEffort ReasoningEffort
	// ExtraHeaders are merged into every request after the defaults. Use
	// this for provider-specific routing headers or to override the default
	// Authorization scheme.
	ExtraHeaders map[string]string
	// MaxOutputTokens is an optional request output cap. Zero preserves the
	// provider default and historical production behavior.
	MaxOutputTokens int
}

// NewClientWithOptions creates a Client from explicit options. Endpoint and
// Model are used verbatim; callers are responsible for setting them.
func NewClientWithOptions(opts Options) *Client {
	reasoningEffort, reasoningEffortErr := modelprovider.NormalizeReasoningEffort(string(opts.ReasoningEffort))
	var maxOutputTokensErr error
	if opts.MaxOutputTokens < 0 || opts.MaxOutputTokens > 131072 {
		maxOutputTokensErr = fmt.Errorf("max output tokens must be between 0 and 131072")
	}
	api := newHTTPAPIClient(opts.Endpoint, opts.Token, opts.ExtraHeaders)
	apiMode := strings.ToLower(strings.TrimSpace(opts.API))
	if apiMode == "" {
		apiMode = APIChatCompletions
	}
	var transport modelTransport
	switch apiMode {
	case APIChatCompletions:
		transport = newChatCompletionsTransport(api)
	case APIResponses:
		transport = newResponsesTransport(api)
	default:
		transport = unsupportedTransport{api: apiMode}
	}
	return &Client{
		api: api, transport: transport, apiMode: apiMode,
		apiURL: opts.Endpoint, model: opts.Model,
		reasoningEffort: reasoningEffort, reasoningEffortErr: reasoningEffortErr,
		maxOutputTokens: opts.MaxOutputTokens, maxOutputTokensErr: maxOutputTokensErr,
		cache: NewCache(opts.CacheDir),
	}
}

var openAIGPTVersionPattern = regexp.MustCompile(`(?i)^gpt-(\d+)\.(\d+)(?:-|$)`)

// ValidateToolCallingConfiguration rejects provider settings that cannot run
// Aster's tool-using analysis loop.
func ValidateToolCallingConfiguration(apiMode, model string, effort ReasoningEffort) error {
	if apiMode != APIChatCompletions || effort == "" || effort == ReasoningEffortNone {
		return nil
	}
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	match := openAIGPTVersionPattern.FindStringSubmatch(strings.TrimSpace(model))
	if len(match) != 3 {
		return nil
	}
	major := 0
	minor := 0
	_, _ = fmt.Sscanf(match[1]+"."+match[2], "%d.%d", &major, &minor)
	if major < 5 || major == 5 && minor < 4 {
		return nil
	}
	return fmt.Errorf("chat_completions cannot use tool calling with model %q and reasoning effort %q; set reasoning effort to none or use responses", model, effort)
}

// ValidateToolConfiguration validates this client for Aster's tool-using loop.
func (c *Client) ValidateToolConfiguration() error {
	return errors.Join(c.ValidateConfiguration(), ValidateToolCallingConfiguration(c.apiMode, c.model, c.reasoningEffort))
}

// Endpoint returns the configured chat-completions URL.
func (c *Client) Endpoint() string { return c.apiURL }

// ModelName returns the configured model identifier.
func (c *Client) ModelName() string { return c.model }

// APIMode returns the selected provider API contract.
func (c *Client) APIMode() string { return c.apiMode }

// ReasoningEffort returns the normalized requested effort. Empty uses the provider default.
func (c *Client) ReasoningEffort() ReasoningEffort { return c.reasoningEffort }

// ValidateConfiguration rejects unsupported client options before provider I/O.
func (c *Client) ValidateConfiguration() error {
	return errors.Join(c.reasoningEffortErr, c.maxOutputTokensErr)
}

// ModelFingerprint hashes the model, endpoint, and non-default API contract.
func ModelFingerprint(apiMode, endpoint, model string) string {
	return ModelFingerprintWithReasoningEffort(apiMode, endpoint, model, "")
}

// ModelFingerprintWithReasoningEffort includes a normalized non-empty effort
// while preserving the historical empty-effort fingerprint exactly.
func ModelFingerprintWithReasoningEffort(apiMode, endpoint, model string, reasoningEffort ReasoningEffort) string {
	apiMode = strings.ToLower(strings.TrimSpace(apiMode))
	if apiMode == "" {
		apiMode = APIChatCompletions
	}
	fingerprint := model + "\x00" + endpoint
	if apiMode != APIChatCompletions {
		fingerprint += "\x00" + apiMode
	}
	if effort := modelprovider.CanonicalReasoningEffort(string(reasoningEffort)); effort != "" {
		fingerprint += "\x00reasoning_effort=" + string(effort)
	}
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:8])
}

// ModelFingerprint returns the current client's safe cache fingerprint.
func (c *Client) ModelFingerprint() string {
	fingerprint := ModelFingerprintWithReasoningEffort(c.apiMode, c.apiURL, c.model, c.reasoningEffort)
	if c.maxOutputTokens == 0 {
		return fingerprint
	}
	sum := sha256.Sum256([]byte(fingerprint + "\x00max_output_tokens=" + fmt.Sprintf("%d", c.maxOutputTokens)))
	return hex.EncodeToString(sum[:8])
}

// modelFingerprint retains the package-local spelling used by older callers.
func (c *Client) modelFingerprint() string { return c.ModelFingerprint() }

// Cache returns the underlying cache so callers can persist it.
func (c *Client) Cache() *Cache {
	return c.cache
}

// Complete sends a tool-free chat completion with system and user messages and
// returns the assistant's text. It is the one-shot generation entry point for
// callers such as prompt drafting. The request is bounded only by ctx.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr(system)},
		{Role: "user", Content: strPtr(user)},
	}
	resp, err := c.callModel(ctx, messages, nil, nil)
	if err != nil {
		return "", err
	}
	if !resp.HasMessage || resp.Message.Content == nil {
		return "", fmt.Errorf("empty completion response")
	}
	return *resp.Message.Content, nil
}

// modelsResponse is the subset of the OpenAI-compatible /v1/models payload we
// care about. Providers report the served model's context window under
// different keys: OpenAI-style servers use top-level context_window; vanilla
// vLLM uses top-level max_model_len; Ray Serve LLM nests it under
// metadata.max_request_context_length. Copilot and some others omit it.
type modelsResponse struct {
	Data []modelEntry `json:"data"`
}

type modelEntry struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window"`
	MaxModelLen   int    `json:"max_model_len"`
	Metadata      struct {
		MaxRequestContextLength int `json:"max_request_context_length"`
		MaxModelLen             int `json:"max_model_len"`
	} `json:"metadata"`
	Capabilities struct {
		Limits struct {
			// MaxPromptTokens is the prompt-side ceiling. GitHub Copilot also
			// reports a larger total window, but the prompt share of it varies
			// from half to nearly all depending on the model, so the total
			// cannot be used to size a request.
			MaxPromptTokens        int `json:"max_prompt_tokens"`
			MaxContextWindowTokens int `json:"max_context_window_tokens"`
		} `json:"limits"`
	} `json:"capabilities"`
}

// contextTokens returns the entry's reported context window in tokens, checking
// the known provider-specific fields in order, or 0 when none report it.
func (m modelEntry) contextTokens() int {
	for _, v := range []int{
		m.ContextWindow,
		m.MaxModelLen,
		m.Metadata.MaxRequestContextLength,
		m.Metadata.MaxModelLen,
		// Prompt tokens before the total window: a model whose prompt share is
		// half its window would otherwise be sized at twice what it accepts.
		m.Capabilities.Limits.MaxPromptTokens,
		m.Capabilities.Limits.MaxContextWindowTokens,
	} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// DetectContextWindowTokens queries the endpoint's /v1/models and returns the
// served model's context window in tokens. Returns ok=false when the endpoint
// does not expose /v1/models, does not report a context window, or errors.
// Best effort: one short GET, no retries.
func (c *Client) DetectContextWindowTokens(ctx context.Context) (int, bool) {
	if c.ValidateConfiguration() != nil {
		return 0, false
	}
	modelsURL, ok := modelsURLFor(c.apiURL)
	if !ok {
		return 0, false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return 0, false
	}
	c.api.setRequestHeaders(req)
	resp, err := c.api.httpClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false
	}
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	// Prefer the entry matching the configured model; else the first entry
	// that reports a positive window.
	best := 0
	for _, m := range out.Data {
		win := m.contextTokens()
		if win <= 0 {
			continue
		}
		if m.ID == c.model {
			return win, true
		}
		if best == 0 {
			best = win
		}
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// modelsURLFor derives the /v1/models URL from a chat-completions URL by
// swapping the trailing "/chat/completions" for "/models". Returns ok=false
// when the URL doesn't look like a chat-completions endpoint.
func modelsURLFor(chatURL string) (string, bool) {
	for _, suffix := range []string{"/chat/completions", "/responses"} {
		if base, found := strings.CutSuffix(chatURL, suffix); found {
			return base + "/models", true
		}
	}
	return "", false
}

// analysisResponse is the expected JSON structure from the analysis model.
// Combines the headline summary, transient classification, and deep root-cause
// fields in a single response so the list view and detail view always agree.
type analysisResponse struct {
	Summary           string                        `json:"summary"`
	IsTransient       bool                          `json:"is_transient"`
	RootCause         string                        `json:"root_cause"`
	Severity          string                        `json:"severity"`
	SuggestedFix      string                        `json:"suggested_fix"`
	RelevantFiles     []string                      `json:"relevant_files"`
	SearchSuggestions []string                      `json:"search_suggestions,omitempty"`
	CauseLocation     *models.AnalysisCauseLocation `json:"cause_location,omitempty"`
	EvidenceCitations []models.EvidenceCitation     `json:"evidence_citations,omitempty"`
}

// proseFields returns RootCause + Summary + SuggestedFix + RelevantFiles
// for callers that scan across every textual field of the draft.
func (r analysisResponse) proseFields() []string {
	out := make([]string, 0, 3+len(r.RelevantFiles))
	out = append(out, r.RootCause, r.Summary, r.SuggestedFix)
	out = append(out, r.RelevantFiles...)
	return out
}

// firstSentence returns the first sentence of s, capped at 200 chars. It derives
// a list-view summary when the model omits "summary".
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for i, r := range s {
		if r == '.' || r == '\n' {
			return strings.TrimSpace(s[:i+1])
		}
		if i >= 200 {
			break
		}
	}
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

var whitespaceRe = regexp.MustCompile(`\s+`)

func normalizeError(msg string) string {
	// Collapse whitespace and remove hex addresses/UUIDs for stable hashing.
	s := whitespaceRe.ReplaceAllString(msg, " ")
	s = regexp.MustCompile(`0x[0-9a-fA-F]+`).ReplaceAllString(s, "<addr>")
	s = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).ReplaceAllString(s, "<uuid>")
	return strings.TrimSpace(s)
}

// extractJSON tries to pull a JSON object from text that may include markdown fences.
func extractJSON(s string) string {
	re := regexp.MustCompile("(?s)```(?:json)?\\s*({.*?})\\s*```")
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
