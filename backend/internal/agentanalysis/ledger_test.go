package agentanalysis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
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
	record.Shadow = &WorkspaceAnalysis{RootCause: strings.Repeat("x", maxLedgerBytes)}
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
