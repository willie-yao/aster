package tools

import "sync"

// Cache is a thread-safe key→string map used by tools to memoize expensive
// discovery results. NewCache has a caller-bounded lifecycle. Long-running
// callers can use NewBoundedCache to cap retained entries and bytes.
type Cache struct {
	mu         sync.Mutex
	store      map[string]cacheEntry
	clock      uint64
	bytes      int
	maxEntries int
	maxBytes   int
}

type cacheEntry struct {
	value string
	used  uint64
	size  int
}

// NewCache returns an empty Cache without internal eviction.
func NewCache() *Cache {
	return newCache(0, 0)
}

// NewBoundedCache returns an empty LRU cache with entry and byte limits.
func NewBoundedCache(maxEntries, maxBytes int) *Cache {
	return newCache(max(0, maxEntries), max(0, maxBytes))
}

func newCache(maxEntries, maxBytes int) *Cache {
	return &Cache{
		store:      map[string]cacheEntry{},
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// Get returns the stored value and true if present.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.store[key]
	if !ok {
		return "", false
	}
	c.clock++
	entry.used = c.clock
	c.store[key] = entry
	return entry.value, true
}

// Set stores key→value. Last write wins.
func (c *Cache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.store[key]; ok {
		c.bytes -= existing.size
	}
	size := len(key) + len(value)
	if c.maxBytes > 0 && size > c.maxBytes {
		delete(c.store, key)
		return
	}
	c.clock++
	c.store[key] = cacheEntry{value: value, used: c.clock, size: size}
	c.bytes += size
	c.evictLocked()
}

func (c *Cache) evictLocked() {
	for (c.maxEntries > 0 && len(c.store) > c.maxEntries) || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		var oldest string
		var tick uint64
		for key, entry := range c.store {
			if oldest != "" && entry.used >= tick {
				continue
			}
			oldest, tick = key, entry.used
		}
		if oldest == "" {
			return
		}
		c.bytes -= c.store[oldest].size
		delete(c.store, oldest)
	}
}

// Len returns the number of entries. Used by tests + telemetry.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.store)
}

// Bytes returns the retained key and value bytes.
func (c *Cache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
