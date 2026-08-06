package agentanalysis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func ledgerPublicDir(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(path)), "public")
}

func ledgerRecord(id string, when time.Time) ShadowRecord {
	return ShadowRecord{
		ID: id, CreatedAt: when.Format(time.RFC3339Nano), AttemptHash: hashString("attempt-" + id),
		RequestHash: hashString("request-" + id), AuthoritativeHash: hashString("authoritative-" + id),
		Status: ShadowStatusSucceeded, Subject: Subject{JobID: "job", BuildID: "1", TestName: "test"},
		Source: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
	}
}

func TestAppendLedgerWritesPrivateBoundedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	created := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < maxLedgerRecords+1; i++ {
		when := created.Add(time.Duration(i) * time.Second)
		record := ledgerRecord(NewRecordID(Subject{JobID: "job", BuildID: "1", TestName: "test"}, when, "identity"), when)
		if err := AppendLedger(ledgerPublicDir(path), path, record); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger ShadowLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.SchemaVersion != LedgerSchemaVersion || len(ledger.Records) != maxLedgerRecords || len(ledger.Attempts) != maxLedgerRecords+1 {
		t.Fatalf("ledger = %+v", ledger)
	}
	if ledger.Records[0].CreatedAt != created.Add(time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("oldest record was not pruned: %+v", ledger.Records[0])
	}
}

func TestAuthoritativeSnapshotExcludesPrivateTelemetry(t *testing.T) {
	summary := &models.AISummary{GeneratedAt: "now", Summary: "summary", IsTransient: false}
	analysis := &models.AIAnalysis{
		GeneratedAt: "now", Model: "private-model", RootCause: "cause", Severity: "High", SuggestedFix: "fix",
		ToolCalls: 4, CritiqueHardFailures: []string{"private-rule"}, RelevantFiles: []string{"build-log.txt"},
	}
	snapshot, hash, err := NewAuthoritativeSnapshot(summary, analysis)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{"private-model", "private-rule", "generated_at"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("snapshot contains %q: %s", forbidden, data)
		}
	}
	if hash == "" || snapshot.RootCause != "cause" || snapshot.ToolCalls != 4 {
		t.Fatalf("snapshot=%+v hash=%q", snapshot, hash)
	}
	analysis.ToolCalls = 9
	_, secondHash, err := NewAuthoritativeSnapshot(summary, analysis)
	if err != nil || secondHash != hash {
		t.Fatalf("telemetry changed semantic hash: first=%s second=%s error=%v", hash, secondHash, err)
	}
}

func TestEvidenceManifestOmitsContent(t *testing.T) {
	bundle := testBundle(t)
	evidence, _ := EvidenceManifest(bundle)
	data, _ := json.Marshal(evidence)
	if strings.Contains(string(data), "failure text") || !strings.Contains(string(data), bundle.Excerpts[0].ContentSHA256) {
		t.Fatalf("manifest = %s", data)
	}
}

func TestLedgerContainsAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	attempt := hashString("attempt")
	record := ledgerRecord("record", time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC))
	record.AttemptHash = attempt
	record.Status = ShadowStatusRuntimeFailed
	if err := AppendLedger(ledgerPublicDir(path), path, record); err != nil {
		t.Fatal(err)
	}
	found, err := LedgerContainsAttempt(ledgerPublicDir(path), path, attempt)
	if err != nil || !found {
		t.Fatalf("found=%v error=%v", found, err)
	}
	found, err = LedgerContainsAttempt(ledgerPublicDir(path), path, hashString("other"))
	if err != nil || found {
		t.Fatalf("found=%v error=%v", found, err)
	}
}

func TestAppendLedgerPrunesExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	oldTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		id   string
		when time.Time
	}{{"old", oldTime}, {"new", newTime}} {
		if err := AppendLedger(ledgerPublicDir(path), path, ledgerRecord(item.id, item.when)); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Records) != 1 || ledger.Records[0].ID != "new" {
		t.Fatalf("records = %+v", ledger.Records)
	}
}

func TestAppendLedgerRejectsOversizedSingleRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	record := ledgerRecord("large", time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC))
	record.Shadow = &Analysis{RootCause: strings.Repeat("x", maxLedgerBytes)}
	if err := AppendLedger(ledgerPublicDir(path), path, record); err == nil {
		t.Fatal("oversized ledger record was accepted")
	}
}

func TestAppendLedgerSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			when := time.Date(2026, 8, 6, 12, 0, i, 0, time.UTC)
			record := ledgerRecord(fmt.Sprintf("record-%d", i), when)
			record.Subject.TestName = fmt.Sprintf("test-%d", i)
			if err := AppendLedger(ledgerPublicDir(path), path, record); err != nil {
				t.Errorf("AppendLedger: %v", err)
			}
		}()
	}
	wg.Wait()
	ledger, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Records) != 10 {
		t.Fatalf("records = %d, want 10", len(ledger.Records))
	}
}

func TestAttemptIdentityIncludesRequestAndRuntimeContract(t *testing.T) {
	subject := Subject{JobID: "job", BuildID: "1", TestName: "test"}
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	base := AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "agent", "v1", "source-readonly", time.Minute, 12, 0)
	variants := []string{
		AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "other-system", "agent", "v1", "source-readonly", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "agent", "v1", "other-secret", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("other-request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "agent", "v1", "source-readonly", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("request"), hashString("other-authoritative"), hashString("skills"), source, "orka-system", "agent", "v1", "source-readonly", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "other", "v1", "source-readonly", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "agent", "v2", "source-readonly", time.Minute, 12, 0),
		AttemptIdentity(subject, hashString("request"), hashString("authoritative"), hashString("skills"), source, "orka-system", "agent", "v1", "source-readonly", 2*time.Minute, 12, 0),
	}
	for i, variant := range variants {
		if variant == base {
			t.Fatalf("variant %d did not change identity", i)
		}
	}
}

func TestLedgerAttemptHashesIgnoresExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	record := ledgerRecord("old", time.Now().UTC().Add(-ledgerRetention-time.Hour))
	if err := AppendLedger(ledgerPublicDir(path), path, record); err != nil {
		t.Fatal(err)
	}
	attempts, err := LedgerAttemptHashes(ledgerPublicDir(path), path)
	if err != nil {
		t.Fatal(err)
	}
	if attempts[record.AttemptHash] {
		t.Fatal("expired attempt still blocks a fresh comparison")
	}
}

func TestClaimLedgerAttemptIsAtomic(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	path := filepath.Join(root, "private", "ledger.json")
	record := ledgerRecord("claim", time.Now().UTC())
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := ClaimLedgerAttempt(publicDir, path, record)
			if err != nil {
				t.Errorf("ClaimLedgerAttempt: %v", err)
				return
			}
			if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("claimed = %d, want 1", claimed.Load())
	}
	ledger, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Records) != 1 || ledger.Records[0].Status != ShadowStatusPending || len(ledger.Attempts) != 1 || ledger.Attempts[0].Status != ShadowStatusPending {
		t.Fatalf("ledger = %+v", ledger)
	}
	record.Status = ShadowStatusSucceeded
	if err := AppendLedger(publicDir, path, record); err != nil {
		t.Fatal(err)
	}
	ledger, err = loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Records[0].Status != ShadowStatusSucceeded || ledger.Attempts[0].Status != ShadowStatusSucceeded {
		t.Fatalf("final ledger = %+v", ledger)
	}
}

func TestClaimLedgerAttemptReclaimsStalePending(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	path := filepath.Join(root, "private", "ledger.json")
	old := ledgerRecord("old", time.Now().UTC().Add(-pendingAttemptRetention-time.Minute))
	if claimed, err := ClaimLedgerAttempt(publicDir, path, old); err != nil || !claimed {
		t.Fatalf("old claim=%v error=%v", claimed, err)
	}
	fresh := ledgerRecord("fresh", time.Now().UTC())
	fresh.AttemptHash = old.AttemptHash
	if claimed, err := ClaimLedgerAttempt(publicDir, path, fresh); err != nil || !claimed {
		t.Fatalf("fresh claim=%v error=%v", claimed, err)
	}
}

func TestValidatePrivateLedgerPathRejectsSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	privateDir := filepath.Join(root, "private")
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(privateDir, "ledger.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateLedgerPath(publicDir, path); err == nil {
		t.Fatal("ledger symlink was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateLedgerPath(publicDir, path); err == nil {
		t.Fatal("ledger lock symlink was accepted")
	}
}
