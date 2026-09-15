package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

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
	modelsURL, ok := modelsURLFor(c.api.endpoint)
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
