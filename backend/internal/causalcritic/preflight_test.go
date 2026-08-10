package causalcritic

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestPreflightIdentityIncludesAllImmutableInputs(t *testing.T) {
	base := PreflightIdentityInput{
		RequestHash: hashString("request"), AuthoritativeHash: hashString("authoritative"),
		SourceRevision: strings.Repeat("a", 40), SkillHash: hashString("skills"), RuntimeIdentity: hashString("runtime"),
	}
	identity, err := PreflightIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	changes := []PreflightIdentityInput{
		{RequestHash: hashString("request-2"), AuthoritativeHash: base.AuthoritativeHash, SourceRevision: base.SourceRevision, SkillHash: base.SkillHash, RuntimeIdentity: base.RuntimeIdentity},
		{RequestHash: base.RequestHash, AuthoritativeHash: hashString("authoritative-2"), SourceRevision: base.SourceRevision, SkillHash: base.SkillHash, RuntimeIdentity: base.RuntimeIdentity},
		{RequestHash: base.RequestHash, AuthoritativeHash: base.AuthoritativeHash, SourceRevision: strings.Repeat("b", 40), SkillHash: base.SkillHash, RuntimeIdentity: base.RuntimeIdentity},
		{RequestHash: base.RequestHash, AuthoritativeHash: base.AuthoritativeHash, SourceRevision: base.SourceRevision, SkillHash: hashString("skills-2"), RuntimeIdentity: base.RuntimeIdentity},
		{RequestHash: base.RequestHash, AuthoritativeHash: base.AuthoritativeHash, SourceRevision: base.SourceRevision, SkillHash: base.SkillHash, RuntimeIdentity: hashString("runtime-2")},
		{RequestHash: base.RequestHash, AuthoritativeHash: base.AuthoritativeHash, SourceRevision: base.SourceRevision, SkillHash: base.SkillHash, RuntimeIdentity: base.RuntimeIdentity, TrialDiscriminator: "arm/2"},
	}
	for index, changed := range changes {
		got, err := PreflightIdentity(changed)
		if err != nil {
			t.Fatalf("change %d: %v", index, err)
		}
		if got == identity {
			t.Fatalf("change %d did not alter identity", index)
		}
	}
}

func TestPreflightAttemptRetention(t *testing.T) {
	tests := []struct {
		name        string
		status      PreflightStatus
		failureCode string
		trialHash   string
		before      time.Duration
		after       time.Duration
	}{
		{name: "pending", status: PreflightPending, before: preflightPendingRetention - time.Second, after: preflightPendingRetention + time.Second},
		{name: "evidence failure", status: PreflightEvidenceFailed, failureCode: "evidence_freeze", before: preflightEvidenceRetention - time.Second, after: preflightEvidenceRetention + time.Second},
		{name: "input rejection", status: PreflightInputInvalid, failureCode: "validation_input_draft", before: preflightAttemptRetention - time.Second, after: preflightAttemptRetention + time.Second},
		{name: "submitted", status: PreflightSubmitted, failureCode: "gateway_timeout", trialHash: hashString("trial"), before: preflightAttemptRetention - time.Second, after: preflightAttemptRetention + time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			publicDir := filepath.Join(root, "public")
			ledgerPath := filepath.Join(root, "private", "critic.json")
			identity := hashString(test.name)
			created := time.Unix(1000, 0).UTC()
			if _, claimed, err := ClaimPreflightAttempt(publicDir, ledgerPath, identity, created); err != nil || !claimed {
				t.Fatalf("initial claim=%v err=%v", claimed, err)
			}
			if test.status != PreflightPending {
				if err := CompletePreflightAttempt(publicDir, ledgerPath, identity, test.status, test.failureCode, test.trialHash, created.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				created = created.Add(time.Minute)
			}
			if _, claimed, err := ClaimPreflightAttempt(publicDir, ledgerPath, identity, created.Add(test.before)); err != nil || claimed {
				t.Fatalf("active claim=%v err=%v", claimed, err)
			}
			got, claimed, err := ClaimPreflightAttempt(publicDir, ledgerPath, identity, created.Add(test.after))
			if err != nil || !claimed || got.Status != PreflightPending {
				t.Fatalf("reclaimed=%+v claimed=%v err=%v", got, claimed, err)
			}
		})
	}
}

func TestPreflightDuplicateSuppressionSurvivesDetailedRecordPruning(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	ledgerPath := filepath.Join(root, "private", "critic.json")
	created := time.Unix(2000, 0).UTC()
	preflightHash := hashString("preflight")
	trialHash := hashString("submitted-trial")
	input := criticInput(t)
	ledger := Ledger{
		SchemaVersion: LedgerSchemaVersion,
		Preflights: []PreflightAttempt{{
			Hash: preflightHash, CreatedAt: created.Format(time.RFC3339Nano), UpdatedAt: created.Format(time.RFC3339Nano),
			Status: PreflightSubmitted, TrialAttemptHash: trialHash,
		}},
	}
	for index := 0; index <= maxLedgerRecords; index++ {
		attemptHash := hashString(fmt.Sprintf("trial-%03d", index))
		if index == 0 {
			attemptHash = trialHash
		}
		record := validLedgerRecord(input, attemptHash, created.Add(time.Duration(index)*time.Second))
		ledger.Records = append(ledger.Records, record)
		ledger.Attempts = append(ledger.Attempts, TrialAttempt{Hash: attemptHash, CreatedAt: record.CreatedAt, Status: record.Status})
	}
	if err := writeLedger(ledgerPath, ledger); err != nil {
		t.Fatal(err)
	}
	got, err := loadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range got.Records {
		if record.AttemptHash == trialHash {
			t.Fatal("old detailed trial record was not pruned")
		}
	}
	if _, claimed, err := ClaimPreflightAttempt(publicDir, ledgerPath, preflightHash, created.Add(time.Hour)); err != nil || claimed {
		t.Fatalf("submitted preflight reclaimed=%v err=%v", claimed, err)
	}
}

func TestLedgerPrunesPreflightsByCountAndRecordsByBytes(t *testing.T) {
	created := time.Unix(3000, 0).UTC()
	ledger := Ledger{SchemaVersion: LedgerSchemaVersion}
	for index := 0; index < maxLedgerPreflights+2; index++ {
		when := created.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano)
		ledger.Preflights = append(ledger.Preflights, PreflightAttempt{
			Hash: hashString(fmt.Sprintf("preflight-%05d", index)), CreatedAt: when, UpdatedAt: when,
			Status: PreflightInputInvalid, FailureCode: "validation_input_draft",
		})
	}
	input := criticInput(t)
	for index := 0; index < maxLedgerRecords; index++ {
		record := validLedgerRecord(input, hashString(fmt.Sprintf("large-record-%03d", index)), created.Add(time.Duration(index)*time.Second))
		record.Resources.PodName = strings.Repeat(string(rune('a'+index%26)), 48<<10)
		ledger.Records = append(ledger.Records, record)
	}
	path := filepath.Join(t.TempDir(), "critic.json")
	if err := writeLedger(path, ledger); err != nil {
		t.Fatal(err)
	}
	got, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Preflights) != maxLedgerPreflights {
		t.Fatalf("preflights=%d, want %d", len(got.Preflights), maxLedgerPreflights)
	}
	if len(got.Records) >= maxLedgerRecords {
		t.Fatalf("byte pruning retained %d records", len(got.Records))
	}
	if got.Preflights[0].Hash == ledger.Preflights[0].Hash || got.Preflights[0].Hash == ledger.Preflights[1].Hash {
		t.Fatal("oldest preflight identities were not pruned")
	}
}

func validLedgerRecord(input Input, attemptHash string, created time.Time) TrialRecord {
	return TrialRecord{
		ID: trialRecordID(created, attemptHash), CreatedAt: created.Format(time.RFC3339Nano), AttemptHash: attemptHash,
		RuntimeIdentity: testCriticRuntimeIdentity(), Status: TrialRuntimeFailure, ErrorCode: "runtime_failure",
		Metadata: trialMetadata(), EvidenceHash: input.EvidenceHash, DraftHash: input.DraftHash, PairHash: input.PairHash,
		Usage:     GatewayUsage{Status: "unavailable", Source: "gateway_response"},
		Resources: engineruntime.ResourceMetadata{Backend: "agent-sandbox"},
	}
}
