package remediationinvestigation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func testEvidenceCatalog() EvidenceCatalog {
	input := testFrozenInput()
	record := EvidenceRecord{
		Kind: EvidenceAnalysis,
		Analysis: &AnalysisEvidenceIdentity{
			BuildID: input.Analyses[0].BuildID, GeneratedAt: input.Analyses[0].GeneratedAt,
			RootCauseDigest: HashText(input.Analyses[0].RootCause),
		},
	}
	record.ID = evidenceRecordID(record)
	return EvidenceCatalog{Version: EvidenceCatalogVersion, Records: []EvidenceRecord{record}}
}

func testNonActionableResult() Result {
	catalog := testEvidenceCatalog()
	reason := NonActionableInsufficientEvidence
	return Result{
		Version: ResultVersion, Reason: "the evidence is ambiguous", CauseAssessment: CauseInconclusive,
		EvidenceIDs: []string{catalog.Records[0].ID}, NonActionableReason: &reason,
	}
}

func testProvenance(now time.Time) Provenance {
	input := testFrozenInput()
	return NewProvenance(input, "model", "responses", "", EvidenceStats{ToolCalls: 2, SourceReads: 1, ArtifactReads: 1}, Metrics{ModelRequests: 2}, now)
}

func TestCacheDropsSemanticallyStalePrivateEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	cache, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provenance := testProvenance(time.Now())
	key := cacheKeyForDigest(provenance.InputDigest)
	catalog := testEvidenceCatalog()
	if err := cache.StoreSuccess(key, testNonActionableResult(), catalog, provenance); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored cacheFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	entry := stored.Entries[key]
	entry.Provenance.Versions.Prompt--
	entry.EvidenceCatalog.Version--
	stored.Entries[key] = entry
	raw, _ = json.Marshal(stored)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewCache(path, CacheOptions{})
	if err != nil {
		t.Fatalf("stale private entry blocked cache load: %v", err)
	}
	if _, ok, err := reloaded.Lookup(key); err != nil || ok {
		t.Fatalf("stale lookup ok=%v err=%v", ok, err)
	}
}

func TestCachePersistsPrivateAcceptedResultAndPreservesItOnFailure(t *testing.T) {
	now := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	cache, err := NewCache(path, CacheOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	provenance := testProvenance(now)
	key := cacheKeyForDigest(provenance.InputDigest)
	catalog := testEvidenceCatalog()
	if err := cache.StoreSuccess(key, testNonActionableResult(), catalog, provenance); err != nil {
		t.Fatal(err)
	}
	if err := cache.RecordFailure(key, FailureProvider, errors.New("private provider body with source text")); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := cache.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	if entry.Result.NonActionableReason == nil || *entry.Result.NonActionableReason != NonActionableInsufficientEvidence || entry.LastFailure == nil || entry.LastFailure.Category != FailureProvider {
		t.Fatalf("entry=%+v", entry)
	}
	if entry.EvidenceCatalogDigest != EvidenceCatalogDigest(entry.EvidenceCatalog) {
		t.Fatal("evidence catalog digest mismatch")
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
	if err != nil || !ok || ResultDigest(reloadedEntry.Result) != ResultDigest(entry.Result) {
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
	catalog := testEvidenceCatalog()
	firstProvenance := testProvenance(time.Now())
	firstKey := cacheKeyForDigest(firstProvenance.InputDigest)
	if err := first.StoreSuccess(firstKey, testNonActionableResult(), catalog, firstProvenance); err != nil {
		t.Fatal(err)
	}
	secondProvenance := testProvenance(time.Now())
	secondProvenance.InputDigest = strings.Repeat("e", 64)
	secondKey := cacheKeyForDigest(secondProvenance.InputDigest)
	if err := second.StoreSuccess(secondKey, testNonActionableResult(), catalog, secondProvenance); err != nil {
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
	catalog := testEvidenceCatalog()
	if err := cache.StoreSuccess(key, testNonActionableResult(), catalog, provenance); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	largeCatalog := EvidenceCatalog{Version: EvidenceCatalogVersion}
	for index := 0; index < 256; index++ {
		largeCatalog.Records = append(largeCatalog.Records, EvidenceRecord{
			ID: strings.Repeat("x", 1024), Kind: EvidenceSource,
			Source: &SourceEvidenceIdentity{
				Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
				Path:       strings.Repeat("p", 1024), ContentDigest: strings.Repeat("a", 64),
			},
		})
	}
	cache.mu.Lock()
	for index := 0; index < 100; index++ {
		inputDigest := fmt.Sprintf("%064x", index+1)
		entryKey := cacheKeyForDigest(inputDigest)
		entryProvenance := provenance
		entryProvenance.InputDigest = inputDigest
		cache.state.Entries[entryKey] = CacheEntry{
			Key: entryKey, Result: cloneResult(testNonActionableResult()), ResultDigest: ResultDigest(testNonActionableResult()),
			EvidenceCatalog: largeCatalog, EvidenceCatalogDigest: EvidenceCatalogDigest(largeCatalog), Provenance: entryProvenance,
			CreatedAt: "2026-08-12T00:00:00Z", UpdatedAt: "2026-08-12T00:00:00Z",
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

func TestCacheRejectsLegacyVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCache(path, CacheOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("legacy cache err=%v", err)
	}
}

func TestCacheLookupDeepClonesCandidateAndEvidenceCatalog(t *testing.T) {
	cache, err := NewCache("", CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	catalog := testEvidenceCatalog()
	grepRecord := EvidenceRecord{
		Kind: EvidenceSourceGrep,
		SourceGrep: &SourceGrepEvidenceIdentity{
			Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
			Path:       "config/defaults.yaml", LineStart: 1, LineEnd: 1, ContentDigest: HashText("feature: true\n"), Match: "feature: true",
		},
	}
	grepRecord.ID = evidenceRecordID(grepRecord)
	catalog.Records = append(catalog.Records, grepRecord)
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports, Reason: "one configuration identity",
		Candidate: &ConfigurationFieldCandidate{
			Kind: CandidateConfigurationField, Path: "config/defaults.yaml", FieldPath: []string{"feature", "enabled"}, Value: "true",
		},
		EvidenceIDs: []string{catalog.Records[0].ID},
	}
	provenance := testProvenance(time.Now())
	key := cacheKeyForDigest(provenance.InputDigest)
	if err := cache.StoreSuccess(key, result, catalog, provenance); err != nil {
		t.Fatal(err)
	}
	first, ok, err := cache.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	first.CandidateFieldPathForTest()[0] = "mutated"
	first.EvidenceCatalog.Records[0].Analysis.BuildID = "mutated"
	first.EvidenceCatalog.Records[1].SourceGrep.Match = "mutated"
	second, ok, err := cache.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("second lookup ok=%v err=%v", ok, err)
	}
	candidate := second.Result.Candidate.(*ConfigurationFieldCandidate)
	if candidate.FieldPath[0] != "feature" || second.EvidenceCatalog.Records[0].Analysis.BuildID == "mutated" ||
		second.EvidenceCatalog.Records[1].SourceGrep.Match == "mutated" || second.ResultDigest != ResultDigest(second.Result) {
		t.Fatalf("cached private state mutated: %+v", second)
	}
}

func (entry *CacheEntry) CandidateFieldPathForTest() []string {
	return entry.Result.Candidate.(*ConfigurationFieldCandidate).FieldPath
}
