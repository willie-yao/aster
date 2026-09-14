package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func strPtr(s string) *string { return &s }

func TestClientHasNoFixedTimeout(t *testing.T) {
	c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions}, "", nil, "", nil)
	if c.api.httpClient.Timeout != 0 {
		t.Fatalf("chat client must have no fixed Timeout, got %v", c.api.httpClient.Timeout)
	}
}

// TestCopilotHeaderSkippedForNonCopilotEndpoint verifies the integration
// header isn't sent when the configured endpoint isn't api.githubcopilot.com.
func TestCopilotHeaderSkippedForNonCopilotEndpoint(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Copilot-Integration-Id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL, Model: "m"}, "tok", nil, "", nil)
	if _, err := c.Complete(context.Background(), Request{Model: c.model, Messages: []Message{{Role: "user", Content: strPtr("user")}}, Tools: nil, ParallelToolCalls: nil}); err != nil {
		t.Fatalf("callModel: %v", err)
	}
	if got != "" {
		t.Errorf("expected no Copilot-Integration-Id header for %q, got %q", srv.URL, got)
	}
}

func TestRequestHeaders_CustomHeaders(t *testing.T) {
	var gotAuth, gotNIM, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotNIM = r.Header.Get("NIM-Function-Id")
		gotAPIKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL, Model: "m"}, "secret-bearer", map[string]string{
		"NIM-Function-Id": "abc-123",
		"api-key":         "azure-key",
	}, "", nil)
	if _, err := c.Complete(context.Background(), Request{Model: c.model, Messages: []Message{{Role: "user", Content: strPtr("user")}}, Tools: nil, ParallelToolCalls: nil}); err != nil {
		t.Fatalf("callModel: %v", err)
	}

	if gotAuth != "Bearer secret-bearer" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-bearer")
	}
	if gotNIM != "abc-123" {
		t.Errorf("NIM-Function-Id = %q, want %q", gotNIM, "abc-123")
	}
	if gotAPIKey != "azure-key" {
		t.Errorf("api-key = %q, want %q", gotAPIKey, "azure-key")
	}
}

func TestRequestHeaders_ExtraHeadersOverrideAuthorization(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL, Model: "m"}, "ignored", map[string]string{
		"Authorization": "Token custom-scheme",
	}, "", nil)
	if _, err := c.Complete(context.Background(), Request{Model: c.model, Messages: []Message{{Role: "user", Content: strPtr("user")}}, Tools: nil, ParallelToolCalls: nil}); err != nil {
		t.Fatalf("callModel: %v", err)
	}
	if gotAuth != "Token custom-scheme" {
		t.Errorf("Authorization = %q, want extras to win", gotAuth)
	}
}

func TestModelsURLFor(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"http://host:8000/v1/chat/completions", "http://host:8000/v1/models", true},
		{"https://api.example.com/v1/chat/completions", "https://api.example.com/v1/models", true},
		{"https://api.githubcopilot.com/chat/completions", "https://api.githubcopilot.com/models", true},
		{"http://host:8000/v1/responses", "http://host:8000/v1/models", true},
		{"http://host:8000/v1/embeddings", "", false},
		{"http://host:8000/something", "", false},
	}
	for _, tc := range cases {
		got, ok := modelsURLFor(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("modelsURLFor(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestDetectContextWindowTokens(t *testing.T) {
	t.Run("matches configured model", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("expected GET /v1/models, got %s", r.URL.Path)
			}
			w.Write([]byte(`{"data":[{"id":"other","context_window":1000},{"id":"my-model","context_window":262144}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "my-model"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 262144 {
			t.Errorf("got (%d,%v), want (262144,true)", got, ok)
		}
	})

	t.Run("falls back to first with positive window", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"unknown","context_window":40960}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "absent"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 40960 {
			t.Errorf("got (%d,%v), want (40960,true)", got, ok)
		}
	})

	t.Run("GitHub Copilot capabilities.limits", func(t *testing.T) {
		// The exact shape api.githubcopilot.com/models returns. The prompt
		// ceiling is preferred over the larger total window, because a request
		// sized at the window would exceed what the model accepts.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"claude-sonnet-4.6","capabilities":{"limits":{"max_context_window_tokens":1000000,"max_prompt_tokens":936000,"max_output_tokens":64000}}}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "claude-sonnet-4.6"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 936000 {
			t.Errorf("got (%d,%v), want (936000,true)", got, ok)
		}
	})

	t.Run("prompt ceiling wins over a much larger window", func(t *testing.T) {
		// gpt-4o class: the prompt share is half the advertised window.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"gpt-4o","capabilities":{"limits":{"max_context_window_tokens":128000,"max_prompt_tokens":64000}}}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "gpt-4o"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 64000 {
			t.Errorf("got (%d,%v), want (64000,true)", got, ok)
		}
	})

	t.Run("Ray Serve metadata.max_request_context_length", func(t *testing.T) {
		// The exact shape Ray Serve's build_openai_app returns.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"moonshotai/Kimi-K2-Instruct-0905","metadata":{"max_request_context_length":32768}}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "moonshotai/Kimi-K2-Instruct-0905"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 32768 {
			t.Errorf("got (%d,%v), want (32768,true)", got, ok)
		}
	})

	t.Run("vanilla vLLM top-level max_model_len", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"m","max_model_len":131072}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "m"}, "x", nil, "", nil)
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 131072 {
			t.Errorf("got (%d,%v), want (131072,true)", got, ok)
		}
	})

	t.Run("no context_window reported -> not ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "m"}, "x", nil, "", nil)
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false when no context_window reported")
		}
	})

	t.Run("HTTP error -> not ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: srv.URL + "/v1/chat/completions", Model: "m"}, "x", nil, "", nil)
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false on HTTP 404")
		}
	})

	t.Run("non-chat endpoint -> not ok (no models URL)", func(t *testing.T) {
		c := NewClient(modelprovider.Config{API: modelprovider.APIChatCompletions, Endpoint: "http://host/v1/embeddings", Model: "m"}, "x", nil, "", nil)
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false when endpoint isn't a chat-completions URL")
		}
	})
}
