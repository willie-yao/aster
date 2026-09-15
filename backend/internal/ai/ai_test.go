package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
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
	oldKey := AgenticCacheKeyForGeneration("universal", "1111111111111111", "job", "1", "test", "failed")
	newKey1 := AgenticCacheKeyForGeneration("universal", "1111111111111111", "job", "2", "test", "failed")
	newKey2 := AgenticCacheKeyForGeneration("universal", "2222222222222222", "job", "2", "test", "failed")
	entries := map[string]CacheEntry{
		oldKey:  {Key: oldKey, CreatedAt: time.Now().Add(-31 * 24 * time.Hour), Data: json.RawMessage(`"old"`)},
		newKey1: {Key: newKey1, CreatedAt: time.Now(), Data: json.RawMessage(`"new-1"`)},
		newKey2: {Key: newKey2, CreatedAt: time.Now(), Data: json.RawMessage(`"new-2"`)},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_cache.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCache(dir)
	if _, ok := c.Get(oldKey); ok {
		t.Fatal("expired entry survived load")
	}
	if _, ok := c.Get(newKey1); !ok {
		t.Fatal("fresh entry was pruned")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewCache(dir)
	if len(reloaded.entries) != 2 {
		t.Fatalf("reloaded entries = %d, want 2", len(reloaded.entries))
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

func TestClientModelProbeValidatesBeforeProviderIO(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":4096}]}`))
	}))
	defer server.Close()
	for _, opts := range []Options{
		{ReasoningEffort: "invalid"},
		{ServiceTier: modelprovider.ServiceTierFlex},
		{MaxOutputTokens: -1},
	} {
		opts.Endpoint, opts.Model = server.URL+"/v1/responses", "m"
		client := NewClientWithOptions(opts)
		if _, ok := client.DetectContextWindowTokens(t.Context()); ok {
			t.Fatalf("invalid configuration returned a model window: %+v", opts)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid configurations made %d provider calls", calls)
	}
	client := NewClientWithOptions(Options{API: APIResponses, Endpoint: server.URL + "/v1/responses", Model: "m"})
	if tokens, ok := client.DetectContextWindowTokens(t.Context()); !ok || tokens != 4096 || calls != 1 {
		t.Fatalf("model probe = %d, %v; calls=%d", tokens, ok, calls)
	}
}

func configureAgenticTestService(service *Service, opts AgenticOptions, factory artifacts.Factory, registry *tools.Registry, enabled []string) {
	service.agenticOpts = opts
	service.browserFactory = factory
	service.registry = registry
	service.enabledTools = enabled
}
