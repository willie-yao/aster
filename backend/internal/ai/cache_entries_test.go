package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheEntriesCopiesAndMergeUsesNewest(t *testing.T) {
	cache := NewCache("")
	now := time.Now().UTC()
	newer := CacheEntry{Key: "key", CreatedAt: now, Data: json.RawMessage(`{"value":"new"}`)}
	if !cache.Merge(map[string]CacheEntry{"key": newer}) {
		t.Fatal("initial merge did not change cache")
	}
	entries := cache.Entries("key")
	entry := entries["key"]
	entry.Data[0] = '['
	if got := cache.Entries("key")["key"].Data[0]; got != '{' {
		t.Fatal("Entries returned aliased data")
	}
	older := CacheEntry{Key: "key", CreatedAt: now.Add(-time.Minute), Data: json.RawMessage(`{"value":"old"}`)}
	if cache.Merge(map[string]CacheEntry{"key": older}) {
		t.Fatal("older entry replaced cache")
	}
	invalid := map[string]CacheEntry{
		"wrong-key": {Key: "other", CreatedAt: now.Add(time.Minute), Data: json.RawMessage(`{"value":"bad"}`)},
		"bad-json":  {Key: "bad-json", CreatedAt: now.Add(time.Minute), Data: json.RawMessage(`{`)},
		"future":    {Key: "future", CreatedAt: now.Add(cacheMaxFutureSkew + time.Second), Data: json.RawMessage(`{"value":"future"}`)},
	}
	if cache.Merge(invalid) {
		t.Fatal("invalid entries changed cache")
	}
}

func TestCacheStoreEntryReplacesNewerValueExactly(t *testing.T) {
	cache := NewCache("")
	now := time.Now().UTC()
	newer := CacheEntry{Key: "key", CreatedAt: now, Data: json.RawMessage(`{"value":"new"}`)}
	older := CacheEntry{Key: "key", CreatedAt: now.Add(-time.Minute), Data: json.RawMessage(`{"value":"old"}`)}
	if err := cache.StoreEntry(newer); err != nil {
		t.Fatal(err)
	}
	if err := cache.StoreEntry(older); err != nil {
		t.Fatal(err)
	}
	got, ok := cache.Lookup("key")
	if !ok || !got.CreatedAt.Equal(older.CreatedAt) || string(got.Data) != string(older.Data) {
		t.Fatalf("stored entry = %+v", got)
	}
}

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
	if _, ok := cache.Get("future"); ok || len(cache.Entries("future")) != 0 {
		t.Fatal("far-future cache entry survived load")
	}
}
