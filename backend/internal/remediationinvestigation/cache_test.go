package remediationinvestigation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNonActionableResult() Result {
	return Result{
		Version: ResultVersion, Classification: ClassificationInsufficientEvidence,
		Reason: "the evidence is ambiguous", CauseAssessment: CauseInconclusive,
		CauseAssessmentReason: "the frozen source does not establish ownership",
		Evidence: []EvidenceCitation{{
			Kind: EvidenceAnalysis, BuildID: "1", AnalysisGeneratedAt: "2026-08-11T00:00:00Z", Quote: "cause",
		}},
	}
}

func testProvenance(now time.Time) Provenance {
	input := testFrozenInput()
	return NewProvenance(input, "model", "responses", EvidenceStats{ToolCalls: 2, SourceReads: 1, ArtifactReads: 1}, Metrics{ModelRequests: 2}, now)
}

func TestCachePersistsPrivateAcceptedResultAndPreservesItOnFailure(t *testing.T) {
	now := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	cache, err := NewCache(path, CacheOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	provenance := testProvenance(now)
	key := cacheKeyForDigest(provenance.InputDigest)
	if err := cache.StoreSuccess(key, testNonActionableResult(), provenance); err != nil {
		t.Fatal(err)
	}
	if err := cache.RecordFailure(key, FailureProvider, errors.New("private provider body with source text")); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := cache.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	if entry.Result.Classification != ClassificationInsufficientEvidence || entry.LastFailure == nil || entry.LastFailure.Category != FailureProvider {
		t.Fatalf("entry=%+v", entry)
	}
	if strings.Contains(entry.LastFailure.Digest, "private") || len(entry.LastFailure.Digest) != 32 {
		t.Fatalf("failure digest=%q", entry.LastFailure.Digest)
	}
	if mode := mustMode(t, filepath.Dir(path)); mode != 0o700 {
		t.Fatalf("directory mode=%o", mode)
	}
	if mode := mustMode(t, path); mode != 0o600 {
		t.Fatalf("cache mode=%o", mode)
	}
	if mode := mustMode(t, filepath.Join(filepath.Dir(path), cacheLockFileName)); mode != 0o600 {
		t.Fatalf("lock mode=%o", mode)
	}

	reloaded, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reloadedEntry, ok, err := reloaded.Lookup(key)
	if err != nil || !ok || reloadedEntry.Result.Classification != entry.Result.Classification {
		t.Fatalf("reloaded=%+v ok=%v err=%v", reloadedEntry, ok, err)
	}
}

func TestCacheFailsClosedOnCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCache(path, CacheOptions{}); err == nil {
		t.Fatal("corrupt cache accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"version":1,"entries":` {
		t.Fatalf("corrupt cache was overwritten: %q", data)
	}
}

func TestCacheSerializesAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	first, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstProvenance := testProvenance(time.Now())
	firstKey := cacheKeyForDigest(firstProvenance.InputDigest)
	if err := first.StoreSuccess(firstKey, testNonActionableResult(), firstProvenance); err != nil {
		t.Fatal(err)
	}
	secondProvenance := testProvenance(time.Now())
	secondProvenance.InputDigest = strings.Repeat("e", 64)
	secondKey := cacheKeyForDigest(secondProvenance.InputDigest)
	if err := second.StoreSuccess(secondKey, testNonActionableResult(), secondProvenance); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{firstKey, secondKey} {
		if _, ok, err := first.Lookup(key); err != nil || !ok {
			t.Fatalf("lookup %s ok=%v err=%v", key, ok, err)
		}
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestCacheRejectsOversizedReplacementAndPreservesPriorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	cache, err := NewCache(path, CacheOptions{MaxEntries: 1024})
	if err != nil {
		t.Fatal(err)
	}
	provenance := testProvenance(time.Now())
	key := cacheKeyForDigest(provenance.InputDigest)
	if err := cache.StoreSuccess(key, testNonActionableResult(), provenance); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	large := testNonActionableResult()
	quote := strings.Repeat("x", 4<<10)
	large.Evidence = make([]EvidenceCitation, 16)
	for index := range large.Evidence {
		large.Evidence[index] = EvidenceCitation{
			Kind: EvidenceAnalysis, BuildID: fmt.Sprintf("build-%d", index),
			AnalysisGeneratedAt: "2026-08-11T00:00:00Z", Quote: quote,
		}
	}
	cache.mu.Lock()
	for index := 0; index < 300; index++ {
		inputDigest := fmt.Sprintf("%064x", index+1)
		entryKey := cacheKeyForDigest(inputDigest)
		entryProvenance := provenance
		entryProvenance.InputDigest = inputDigest
		cache.state.Entries[entryKey] = CacheEntry{
			Key: entryKey, Result: cloneResult(large), Provenance: entryProvenance,
			CreatedAt: "2026-08-11T00:00:00Z", UpdatedAt: "2026-08-11T00:00:00Z",
		}
	}
	err = cache.withFileLockLocked(func() error { return cache.persistLocked() })
	cache.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "would exceed") {
		t.Fatalf("oversized persistence err=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("oversized transition replaced the prior durable cache")
	}
	reloaded, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := reloaded.Lookup(key); err != nil || !ok {
		t.Fatalf("prior result unavailable after rejected replacement: ok=%v err=%v", ok, err)
	}
}
