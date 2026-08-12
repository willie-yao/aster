package remediationinvestigation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	CacheRelativePath = ".remediation-investigations/cache.json"
	cacheFileVersion  = 3
	defaultMaxEntries = 256
	maxCacheFileBytes = 16 << 20
	cacheLockFileName = "cache.lock"
)

type FailureCategory string

const (
	FailureInvalidInput      FailureCategory = "invalid_input"
	FailureSourceUnavailable FailureCategory = "source_unavailable"
	FailureProvider          FailureCategory = "provider"
	FailureInvalidResult     FailureCategory = "invalid_result"
	FailureCancelled         FailureCategory = "cancelled"
	FailureTimeout           FailureCategory = "timeout"
	FailureUnknown           FailureCategory = "unknown"
)

type FailureRecord struct {
	Category FailureCategory `json:"category"`
	Digest   string          `json:"digest"`
	At       string          `json:"at"`
}

type CacheEntry struct {
	Key                   string          `json:"key"`
	Result                Result          `json:"result"`
	ResultDigest          string          `json:"result_digest"`
	EvidenceCatalog       EvidenceCatalog `json:"evidence_catalog"`
	EvidenceCatalogDigest string          `json:"evidence_catalog_digest"`
	Provenance            Provenance      `json:"provenance"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	LastFailure           *FailureRecord  `json:"last_failure,omitempty"`
}

type cacheFile struct {
	Version   int                      `json:"version"`
	UpdatedAt string                   `json:"updated_at"`
	Entries   map[string]CacheEntry    `json:"entries"`
	Failures  map[string]FailureRecord `json:"failures,omitempty"`
}

type CacheOptions struct {
	MaxEntries int
	Now        func() time.Time
}

type Cache struct {
	mu         sync.Mutex
	path       string
	maxEntries int
	now        func() time.Time
	state      cacheFile
}

func NewCache(path string, options CacheOptions) (*Cache, error) {
	maxEntries := options.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	cache := &Cache{path: path, maxEntries: maxEntries, now: now, state: newCacheFile()}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err := cache.withFileLockLocked(func() error { return cache.loadLocked() }); err != nil {
		return nil, err
	}
	cache.pruneLocked()
	return cache, nil
}

func newCacheFile() cacheFile {
	return cacheFile{Version: cacheFileVersion, Entries: map[string]CacheEntry{}, Failures: map[string]FailureRecord{}}
}

func (c *Cache) Lookup(key string) (CacheEntry, bool, error) {
	if c == nil {
		return CacheEntry{}, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var entry CacheEntry
	var ok bool
	err := c.withFileLockLocked(func() error {
		if err := c.loadLocked(); err != nil {
			return err
		}
		entry, ok = c.state.Entries[key]
		return nil
	})
	if err != nil || !ok {
		return CacheEntry{}, false, err
	}
	return cloneCacheEntry(entry), true, nil
}

func (c *Cache) StoreSuccess(key string, result Result, catalog EvidenceCatalog, provenance Provenance) error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("cache key is required")
	}
	if err := ValidateResult(result); err != nil {
		return err
	}
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		return err
	}
	if _, err := selectedEvidenceRecords(resultEvidenceIDs(result), catalog); err != nil {
		return err
	}
	if strings.TrimSpace(provenance.InputDigest) == "" || provenance.Versions != CurrentVersions() || provenance.ProviderFingerprint == "" ||
		cacheKeyForDigest(provenance.InputDigest) != key {
		return fmt.Errorf("cache provenance is incomplete or does not match the key")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.withFileLockLocked(func() error {
		if err := c.loadLocked(); err != nil {
			return err
		}
		now := c.now().UTC().Format(time.RFC3339)
		created := now
		if current, ok := c.state.Entries[key]; ok {
			created = current.CreatedAt
		}
		c.state.Entries[key] = CacheEntry{
			Key: key, Result: cloneResult(result), ResultDigest: ResultDigest(result),
			EvidenceCatalog: cloneEvidenceCatalog(catalog), EvidenceCatalogDigest: EvidenceCatalogDigest(catalog),
			Provenance: cloneProvenance(provenance), CreatedAt: created, UpdatedAt: now,
		}
		delete(c.state.Failures, key)
		c.pruneLocked()
		return c.persistLocked()
	})
}

// RecordFailure records sanitized refresh metadata without replacing a valid
// result for the same semantic cache key.
func (c *Cache) RecordFailure(key string, category FailureCategory, err error) error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(key) == "" || !validFailureCategory(category) {
		return fmt.Errorf("valid cache key and failure category are required")
	}
	record := FailureRecord{Category: category, Digest: failureDigest(err), At: c.now().UTC().Format(time.RFC3339)}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.withFileLockLocked(func() error {
		if err := c.loadLocked(); err != nil {
			return err
		}
		if entry, ok := c.state.Entries[key]; ok {
			entry.LastFailure = &record
			entry.UpdatedAt = record.At
			c.state.Entries[key] = entry
		} else {
			c.state.Failures[key] = record
		}
		c.pruneLocked()
		return c.persistLocked()
	})
}

func (c *Cache) withFileLockLocked(fn func() error) error {
	if strings.TrimSpace(c.path) == "" {
		return fn()
	}
	parent := filepath.Dir(c.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create remediation investigation cache directory: %w", err)
	}
	_ = os.Chmod(parent, 0o700)
	lock, err := os.OpenFile(filepath.Join(parent, cacheLockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open remediation investigation cache lock: %w", err)
	}
	defer lock.Close()
	_ = lock.Chmod(0o600)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock remediation investigation cache: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	return fn()
}

func (c *Cache) loadLocked() error {
	if strings.TrimSpace(c.path) == "" {
		return nil
	}
	c.state = newCacheFile()
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read remediation investigation cache: %w", err)
	}
	if len(data) > maxCacheFileBytes {
		return fmt.Errorf("remediation investigation cache exceeds %d bytes", maxCacheFileBytes)
	}
	var loaded cacheFile
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("decode remediation investigation cache: %w", err)
	}
	if loaded.Version != cacheFileVersion {
		return fmt.Errorf("unsupported remediation investigation cache version %d", loaded.Version)
	}
	if loaded.Entries == nil {
		loaded.Entries = map[string]CacheEntry{}
	}
	if loaded.Failures == nil {
		loaded.Failures = map[string]FailureRecord{}
	}
	for key, entry := range loaded.Entries {
		if entry.Provenance.Versions != CurrentVersions() || entry.EvidenceCatalog.Version != EvidenceCatalogVersion {
			delete(loaded.Entries, key)
			continue
		}
		_, evidenceErr := selectedEvidenceRecords(resultEvidenceIDs(entry.Result), entry.EvidenceCatalog)
		if key != entry.Key || ValidateResult(entry.Result) != nil || entry.ResultDigest != ResultDigest(entry.Result) ||
			ValidateEvidenceCatalog(entry.EvidenceCatalog) != nil || evidenceErr != nil ||
			entry.EvidenceCatalogDigest != EvidenceCatalogDigest(entry.EvidenceCatalog) ||
			strings.TrimSpace(entry.Provenance.InputDigest) == "" || entry.Provenance.ProviderFingerprint == "" || cacheKeyForDigest(entry.Provenance.InputDigest) != key {
			return fmt.Errorf("remediation investigation cache entry %q is invalid", key)
		}
	}
	c.state = loaded
	return nil
}

func (c *Cache) pruneLocked() {
	type candidate struct {
		key, updated string
		accepted     bool
	}
	items := make([]candidate, 0, len(c.state.Entries)+len(c.state.Failures))
	for key, entry := range c.state.Entries {
		items = append(items, candidate{key: key, updated: entry.UpdatedAt, accepted: true})
	}
	for key, failure := range c.state.Failures {
		if _, accepted := c.state.Entries[key]; accepted {
			continue
		}
		items = append(items, candidate{key: key, updated: failure.At})
	}
	if len(items) <= c.maxEntries {
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].accepted != items[j].accepted {
			return !items[i].accepted
		}
		if items[i].updated == items[j].updated {
			return items[i].key < items[j].key
		}
		return items[i].updated < items[j].updated
	})
	for _, item := range items[:len(items)-c.maxEntries] {
		delete(c.state.Entries, item.key)
		delete(c.state.Failures, item.key)
	}
}

func (c *Cache) persistLocked() error {
	c.state.Version = cacheFileVersion
	c.state.UpdatedAt = c.now().UTC().Format(time.RFC3339)
	encoded, err := json.Marshal(c.state)
	if err != nil {
		return fmt.Errorf("encode remediation investigation cache: %w", err)
	}
	if len(encoded) > maxCacheFileBytes {
		return fmt.Errorf("remediation investigation cache would exceed %d bytes", maxCacheFileBytes)
	}
	if strings.TrimSpace(c.path) == "" {
		return nil
	}
	if err := statefile.WritePrivateJSONDurable(c.path, c.state); err != nil {
		return fmt.Errorf("write remediation investigation cache: %w", err)
	}
	return nil
}

func validFailureCategory(category FailureCategory) bool {
	switch category {
	case FailureInvalidInput, FailureSourceUnavailable, FailureProvider, FailureInvalidResult,
		FailureCancelled, FailureTimeout, FailureUnknown:
		return true
	default:
		return false
	}
}

func failureDigest(err error) string {
	value := "unknown"
	if err != nil {
		value = err.Error()
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func cloneCacheEntry(entry CacheEntry) CacheEntry {
	entry.Result = cloneResult(entry.Result)
	entry.EvidenceCatalog = cloneEvidenceCatalog(entry.EvidenceCatalog)
	entry.Provenance = cloneProvenance(entry.Provenance)
	if entry.LastFailure != nil {
		failure := *entry.LastFailure
		entry.LastFailure = &failure
	}
	return entry
}

func cloneResult(result Result) Result {
	result.Hypotheses = slices.Clone(result.Hypotheses)
	for index := range result.Hypotheses {
		hypothesis := &result.Hypotheses[index]
		hypothesis.Target = cloneCandidate(hypothesis.Target)
		hypothesis.EvidenceIDs = slices.Clone(hypothesis.EvidenceIDs)
	}
	if result.NonActionable != nil {
		value := *result.NonActionable
		value.EvidenceIDs = slices.Clone(value.EvidenceIDs)
		result.NonActionable = &value
	}
	return result
}

func cloneEvidenceCatalog(catalog EvidenceCatalog) EvidenceCatalog {
	catalog.Records = slices.Clone(catalog.Records)
	for index := range catalog.Records {
		record := &catalog.Records[index]
		if record.Source != nil {
			value := *record.Source
			record.Source = &value
		}
		if record.SourceGrep != nil {
			value := *record.SourceGrep
			record.SourceGrep = &value
		}
		if record.Analysis != nil {
			value := *record.Analysis
			record.Analysis = &value
		}
		if record.Artifact != nil {
			value := *record.Artifact
			record.Artifact = &value
		}
	}
	return catalog
}

func cloneProvenance(provenance Provenance) Provenance { return provenance }
