package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachePrunesFarFutureEntries(t *testing.T) {
	dir := t.TempDir()
	entries := map[string]CacheEntry{
		"future": {Key: "future", CreatedAt: time.Now().Add(cacheMaxFutureSkew + time.Hour), Data: json.RawMessage(`{"ok":true}`)},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CacheFilename), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(dir)
	if _, ok := cache.Get("future"); ok {
		t.Fatal("far-future cache entry survived load")
	}
}
