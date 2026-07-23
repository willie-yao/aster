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

const (
	cacheMaxAge        = 30 * 24 * time.Hour
	cacheMaxFutureSkew = 5 * time.Minute
	// CacheFilename is the private persisted analysis cache.
	CacheFilename = "ai_cache.json"
)

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
	data, err := os.ReadFile(filepath.Join(dir, CacheFilename))
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
	now := time.Now()
	if !validCacheEntryTime(now, entry.CreatedAt) {
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

// Entries returns copies of selected unexpired cache entries.
func (c *Cache) Entries(keys ...string) map[string]CacheEntry {
	out := map[string]CacheEntry{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, key := range keys {
		entry, ok := c.entries[key]
		if !ok || !validCacheEntryTime(now, entry.CreatedAt) {
			continue
		}
		entry.Data = append(json.RawMessage(nil), entry.Data...)
		out[key] = entry
	}
	return out
}

// Merge adds newer valid entries and reports whether the cache changed.
func (c *Cache) Merge(entries map[string]CacheEntry) bool {
	if c == nil || len(entries) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	changed := false
	for key, entry := range entries {
		if key == "" || entry.Key != key || !validCacheEntryTime(now, entry.CreatedAt) || !json.Valid(entry.Data) {
			continue
		}
		current, ok := c.entries[key]
		if ok && !entry.CreatedAt.After(current.CreatedAt) {
			continue
		}
		entry.Data = append(json.RawMessage(nil), entry.Data...)
		c.entries[key] = entry
		changed = true
	}
	if changed {
		c.dirty = true
	}
	return changed
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
	if err := statefile.WriteJSON(filepath.Join(c.dir, CacheFilename), c.entries); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

func validCacheEntryTime(now, created time.Time) bool {
	return !created.IsZero() && !created.After(now.Add(cacheMaxFutureSkew)) && now.Sub(created) <= cacheMaxAge
}

func (c *Cache) pruneExpiredLocked(now time.Time) bool {
	changed := false
	for key, entry := range c.entries {
		if !validCacheEntryTime(now, entry.CreatedAt) {
			delete(c.entries, key)
			changed = true
		}
	}
	return changed
}
