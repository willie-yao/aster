package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkCacheLoadAndSave(b *testing.B) {
	entries := make(map[string]CacheEntry, 10000)
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("entry-%d", i)
		entries[key] = CacheEntry{Key: key, CreatedAt: time.Now(), Data: json.RawMessage(`{"ok":true}`)}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		dir := b.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ai_cache.json"), data, 0o644); err != nil {
			b.Fatal(err)
		}
		cache := NewCache(dir)
		cache.dirty = true
		if err := cache.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
