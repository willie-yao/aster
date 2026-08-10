package causalcritic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type trialReviewer struct {
	result Result
	err    error
	calls  int
}

func (r *trialReviewer) Review(context.Context, Input, string, engineruntime.WorkObserver) (Result, error) {
	r.calls++
	return r.result, r.err
}

func TestRunTrialPersistsFinalizedPrivateRecord(t *testing.T) {
	input := criticInput(t)
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	reviewer := &trialReviewer{result: Result{
		Execution: ExecutionResult{Review: &review, Usage: GatewayUsage{Status: "reported", Source: "gateway_response", Model: "critic", InputTokens: 10, OutputTokens: 2}},
		Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true, FinalizationChecked: true, FinalizationValid: true},
	}}
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	ledgerPath := filepath.Join(root, "private", "critic.json")
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-case-1", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != TrialSucceeded || !record.Finalized || record.Review == nil {
		t.Fatalf("record = %+v", record)
	}
	ledger, err := loadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Records) != 1 || !ledger.Records[0].Finalized {
		t.Fatalf("ledger = %+v", ledger)
	}
	info, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("ledger mode = %o", info.Mode().Perm())
	}
	if _, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-case-1", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Unix(101, 0) },
	}); err == nil || reviewer.calls != 1 {
		t.Fatalf("duplicate err=%v calls=%d", err, reviewer.calls)
	}
}

func TestRunTrialPreservesReviewWhenCleanupPending(t *testing.T) {
	input := criticInput(t)
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	reviewer := &trialReviewer{
		result: Result{Execution: ExecutionResult{Review: &review}, CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"}, Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: false, FinalizationChecked: true, FinalizationValid: true}},
		err:    engineruntime.ErrCleanupPending,
	}
	root := t.TempDir()
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: filepath.Join(root, "public"), LedgerPath: filepath.Join(root, "private", "critic.json"),
		Metadata: trialMetadata(), Input: input, ExecutionID: "critic-cleanup", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Unix(200, 0) },
	})
	if !errors.Is(err, engineruntime.ErrCleanupPending) || record.Status != TrialCleanupPending || record.Finalized || record.Review == nil {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestRunTrialRejectsLedgerInsidePublicOutput(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	reviewer := &trialReviewer{}
	_, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: filepath.Join(publicDir, "critic.json"), Metadata: trialMetadata(), Input: criticInput(t), RuntimeIdentity: testCriticRuntimeIdentity(),
	})
	if err == nil {
		t.Fatal("public ledger path was accepted")
	}
}

func testCriticRuntimeIdentity() string {
	return RuntimeIdentity(engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.models.svc.cluster.local/v1", Model: "critic", ProtocolVersion: "openai-chat-completions-v1"}, "example/critic@sha256:"+strings.Repeat("a", 64), time.Minute, DefaultOutputLimit)
}

func trialMetadata() TrialMetadata {
	return TrialMetadata{CaseID: "case", StableID: "0123456789abcdef0123", Repetition: 1, Arm: "agent-sandbox-independent-critic", AuthoritativeArm: "same-model-judge"}
}

type trialCleaner struct {
	work  engineruntime.WorkRef
	calls int
	err   error
}

func (c *trialCleaner) Cleanup(_ context.Context, work engineruntime.WorkRef) error {
	c.calls++
	c.work = work
	return c.err
}

func TestRecoverPendingCleanupFinalizesWithoutRerunningReview(t *testing.T) {
	input := criticInput(t)
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	reviewer := &trialReviewer{
		result: Result{
			Execution:   ExecutionResult{Review: &review},
			CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"},
			Telemetry:   engineruntime.GenerateTelemetry{CleanupCompleted: false, FinalizationChecked: true, FinalizationValid: true},
		},
		err: engineruntime.ErrCleanupPending,
	}
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	if _, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-cleanup", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Now().UTC() },
	}); !errors.Is(err, engineruntime.ErrCleanupPending) {
		t.Fatal(err)
	}
	cleaner := &trialCleaner{}
	if err := RecoverPendingCleanup(t.Context(), cleaner, publicDir, ledgerPath); err != nil {
		t.Fatal(err)
	}
	ledger, err := loadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if cleaner.calls != 1 || cleaner.work.UID != "uid-1" || len(ledger.Records) != 1 || ledger.Records[0].Status != TrialSucceeded || !ledger.Records[0].Finalized || ledger.Records[0].CleanupWork != nil || reviewer.calls != 1 {
		t.Fatalf("cleaner=%+v ledger=%+v reviewer calls=%d", cleaner, ledger, reviewer.calls)
	}
}

func TestRecoverPendingCleanupPreservesFailedReviewStatus(t *testing.T) {
	input := criticInput(t)
	reviewer := &trialReviewer{
		result: Result{
			CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"},
			Telemetry:   engineruntime.GenerateTelemetry{CleanupCompleted: false, FinalizationChecked: true},
		},
		err: errors.Join(engineruntime.ErrMalformedResult, engineruntime.ErrCleanupPending),
	}
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-cleanup-failure", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Now().UTC() },
	})
	if !errors.Is(err, engineruntime.ErrMalformedResult) || record.Status != TrialMalformedResult || record.CleanupWork == nil {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	cleaner := &trialCleaner{}
	if err := RecoverPendingCleanup(t.Context(), cleaner, publicDir, ledgerPath); err != nil {
		t.Fatal(err)
	}
	ledger, err := loadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	got := ledger.Records[0]
	if cleaner.calls != 1 || got.Status != TrialMalformedResult || got.Finalized || !got.Telemetry.CleanupCompleted || got.CleanupWork != nil {
		t.Fatalf("cleaner=%+v record=%+v", cleaner, got)
	}
}
