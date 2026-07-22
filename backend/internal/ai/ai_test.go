package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheSetAndGet(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	val := map[string]string{"hello": "world"}
	if err := c.Set("k1", val); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, ok := c.Get("k1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["hello"] != "world" {
		t.Fatalf("unexpected value: %v", got)
	}
}

func TestCacheMiss(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestCacheExpiry(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	_ = c.Set("old", "data")
	c.mu.Lock()
	entry := c.entries["old"]
	entry.CreatedAt = time.Now().Add(-31 * 24 * time.Hour)
	c.entries["old"] = entry
	c.mu.Unlock()

	_, ok := c.Get("old")
	if ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestCacheSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	_ = c.Set("persist", "yes")
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "ai_cache.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("cache file is empty")
	}

	c2 := NewCache(dir)
	raw, ok := c2.Get("persist")
	if !ok {
		t.Fatal("expected cache hit after reload")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != "yes" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestCachePrunesExpiredEntriesOnLoad(t *testing.T) {
	dir := t.TempDir()
	entries := map[string]CacheEntry{
		"old": {Key: "old", CreatedAt: time.Now().Add(-31 * 24 * time.Hour), Data: json.RawMessage(`"old"`)},
		"new": {Key: "new", CreatedAt: time.Now(), Data: json.RawMessage(`"new"`)},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_cache.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCache(dir)
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired entry survived load")
	}
	if _, ok := c.Get("new"); !ok {
		t.Fatal("fresh entry was pruned")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewCache(dir)
	if len(reloaded.entries) != 1 {
		t.Fatalf("reloaded entries = %d, want 1", len(reloaded.entries))
	}
}

func TestNormalizeError(t *testing.T) {
	input := "error at 0xDEADBEEF with id 12345678-1234-1234-1234-123456789abc foo"
	got := normalizeError(input)
	if got != "error at <addr> with id <uuid> foo" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestExtractJSON(t *testing.T) {
	fenced := "Here is the analysis:\n```json\n{\"root_cause\": \"test\"}\n```\nDone."
	got := extractJSON(fenced)
	if got != `{"root_cause": "test"}` {
		t.Fatalf("unexpected: %q", got)
	}

	bare := `Some text {"key": "val"} more text`
	got = extractJSON(bare)
	if got != `{"key": "val"}` {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestNewClientWithOptionsNoDefaulting(t *testing.T) {
	// Endpoint and Model are used verbatim; the engine applies no default
	// provider, so empty options stay empty.
	c := NewClientWithOptions(Options{Token: "x", CacheDir: t.TempDir()})
	if c.Endpoint() != "" {
		t.Errorf("endpoint = %q, want empty (no default)", c.Endpoint())
	}
	if c.ModelName() != "" {
		t.Errorf("model = %q, want empty (no default)", c.ModelName())
	}
}

func TestNewClientWithOptionsOverrides(t *testing.T) {
	c := NewClientWithOptions(Options{
		Token:    "x",
		CacheDir: t.TempDir(),
		Endpoint: "https://example.com/v1/chat/completions",
		Model:    "my-model",
	})
	if c.Endpoint() != "https://example.com/v1/chat/completions" {
		t.Errorf("endpoint = %q", c.Endpoint())
	}
	if c.ModelName() != "my-model" {
		t.Errorf("model = %q", c.ModelName())
	}
}

func TestIsCopilotEndpoint(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://api.githubcopilot.com/chat/completions", true},
		{"https://api.githubcopilot.com:443/chat/completions", true},
		{"https://api.openai.com/v1/chat/completions", false},
		{"https://integrate.api.nvidia.com/v1/chat/completions", false},
		{"https://my.openai.azure.com/openai/deployments/gpt4/chat/completions", false},
		{"http://localhost:11434/v1/chat/completions", false},
		{"://broken", false},
	}
	for _, tt := range tests {
		if got := isCopilotEndpoint(tt.url); got != tt.want {
			t.Errorf("isCopilotEndpoint(%q) = %v, want %v", tt.url, got, tt.want)
		}
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

	c := NewClientWithOptions(Options{Token: "tok", CacheDir: t.TempDir(), Endpoint: srv.URL, Model: "m"})
	if _, err := c.callModel(context.Background(), []modelMessage{{Role: "user", Content: strPtr("user")}}, nil, nil); err != nil {
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

	c := NewClientWithOptions(Options{
		Token:    "secret-bearer",
		CacheDir: t.TempDir(),
		Endpoint: srv.URL,
		Model:    "m",
		ExtraHeaders: map[string]string{
			"NIM-Function-Id": "abc-123",
			"api-key":         "azure-key",
		},
	})
	if _, err := c.callModel(context.Background(), []modelMessage{{Role: "user", Content: strPtr("user")}}, nil, nil); err != nil {
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

	c := NewClientWithOptions(Options{
		Token:    "ignored",
		CacheDir: t.TempDir(),
		Endpoint: srv.URL,
		Model:    "m",
		ExtraHeaders: map[string]string{
			"Authorization": "Token custom-scheme",
		},
	})
	if _, err := c.callModel(context.Background(), []modelMessage{{Role: "user", Content: strPtr("user")}}, nil, nil); err != nil {
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
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "my-model", Token: "x"})
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
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "absent", Token: "x"})
		got, ok := c.DetectContextWindowTokens(context.Background())
		if !ok || got != 40960 {
			t.Errorf("got (%d,%v), want (40960,true)", got, ok)
		}
	})

	t.Run("Ray Serve metadata.max_request_context_length", func(t *testing.T) {
		// The exact shape Ray Serve's build_openai_app returns.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"moonshotai/Kimi-K2-Instruct-0905","metadata":{"max_request_context_length":32768}}]}`))
		}))
		defer srv.Close()
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "moonshotai/Kimi-K2-Instruct-0905", Token: "x"})
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
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "m", Token: "x"})
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
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "m", Token: "x"})
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false when no context_window reported")
		}
	})

	t.Run("HTTP error -> not ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		c := NewClientWithOptions(Options{Endpoint: srv.URL + "/v1/chat/completions", Model: "m", Token: "x"})
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false on HTTP 404")
		}
	})

	t.Run("non-chat endpoint -> not ok (no models URL)", func(t *testing.T) {
		c := NewClientWithOptions(Options{Endpoint: "http://host/v1/embeddings", Model: "m", Token: "x"})
		if _, ok := c.DetectContextWindowTokens(context.Background()); ok {
			t.Error("expected ok=false when endpoint isn't a chat-completions URL")
		}
	})
}

func TestModelFingerprintSeparatesResponsesWithoutChangingChat(t *testing.T) {
	chat := NewClientWithOptions(Options{Endpoint: "https://example/v1/chat/completions", Model: "m"})
	explicitChat := NewClientWithOptions(Options{API: APIChatCompletions, Endpoint: "https://example/v1/chat/completions", Model: "m"})
	responses := NewClientWithOptions(Options{API: APIResponses, Endpoint: "https://example/v1/chat/completions", Model: "m"})
	if chat.modelFingerprint() != explicitChat.modelFingerprint() {
		t.Fatal("explicit Chat mode changed the existing fingerprint")
	}
	if chat.modelFingerprint() == responses.modelFingerprint() {
		t.Fatal("Responses mode reused the Chat fingerprint")
	}
}
