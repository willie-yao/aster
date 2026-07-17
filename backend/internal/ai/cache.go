package ai

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const cacheMaxAge = 30 * 24 * time.Hour

// CacheEntry holds a single cached AI response.
type CacheEntry struct {
	Key       string          `json:"key"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// Cache provides a simple file-backed key/value store for AI responses.
type Cache struct {
	dir     string
	mu      sync.Mutex
	entries map[string]CacheEntry
	dirty   bool
}

// NewCache creates a cache, loading existing entries from dir/ai_cache.json.
func NewCache(dir string) *Cache {
	c := &Cache{
		dir:     dir,
		entries: make(map[string]CacheEntry),
	}
	if dir == "" {
		return c
	}
	data, err := os.ReadFile(filepath.Join(dir, "ai_cache.json"))
	if err == nil {
		var entries map[string]CacheEntry
		if err := json.Unmarshal(data, &entries); err == nil {
			c.entries = entries
			c.dirty = c.pruneExpiredLocked(time.Now())
		} else {
			log.Printf("Warning: failed to parse AI cache: %v", err)
			c.dirty = true
		}
	}
	return c
}

// Get returns the cached data if the key exists and is not older than 30 days.
func (c *Cache) Get(key string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.CreatedAt) > cacheMaxAge {
		delete(c.entries, key)
		c.dirty = true
		return nil, false
	}
	return entry.Data, true
}

// Set stores data under the given key.
func (c *Cache) Set(key string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = CacheEntry{
		Key:       key,
		CreatedAt: time.Now(),
		Data:      raw,
	}
	c.dirty = true
	return nil
}

// Save writes the cache to dir/ai_cache.json.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == "" {
		return nil
	}
	if c.pruneExpiredLocked(time.Now()) {
		c.dirty = true
	}
	if !c.dirty {
		return nil
	}
	if err := statefile.WriteJSON(filepath.Join(c.dir, "ai_cache.json"), c.entries); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

func (c *Cache) pruneExpiredLocked(now time.Time) bool {
	changed := false
	for key, entry := range c.entries {
		if now.Sub(entry.CreatedAt) > cacheMaxAge {
			delete(c.entries, key)
			changed = true
		}
	}
	return changed
}
