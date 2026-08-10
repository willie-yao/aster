package causalcritic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentsandbox"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type trialReviewer struct {
	result Result
	err    error
	calls  int
}

type trialReviewerFunc func(context.Context, Input, string, engineruntime.WorkObserver) (Result, error)

func (f trialReviewerFunc) Review(ctx context.Context, input Input, executionID string, observer engineruntime.WorkObserver) (Result, error) {
	return f(ctx, input, executionID, observer)
}

func (trialReviewerFunc) RuntimeIdentity() string { return testCriticRuntimeIdentity() }

func (r *trialReviewer) Review(context.Context, Input, string, engineruntime.WorkObserver) (Result, error) {
	r.calls++
	return r.result, r.err
}

func (r *trialReviewer) RuntimeIdentity() string { return testCriticRuntimeIdentity() }

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
	loaded, found, err := LoadTrialByAttempt(publicDir, ledgerPath, record.AttemptHash)
	if err != nil || !found || loaded.ID != record.ID {
		t.Fatalf("loaded=%+v found=%v err=%v", loaded, found, err)
	}
	info, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("ledger mode = %o", info.Mode().Perm())
	}
	duplicate, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-case-1", RuntimeIdentity: testCriticRuntimeIdentity(), Now: func() time.Time { return time.Unix(101, 0) },
	})
	if err == nil || reviewer.calls != 1 || duplicate.AttemptHash != record.AttemptHash || !duplicate.Finalized {
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

func TestRunTrialTreatsTransientCleanupErrorAsCleanupPending(t *testing.T) {
	input := criticInput(t)
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	reviewer := &trialReviewer{
		result: Result{
			Execution:   ExecutionResult{Review: &review},
			CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"},
			Telemetry:   engineruntime.GenerateTelemetry{CleanupCompleted: false, FinalizationChecked: true, FinalizationValid: true},
		},
		err: errors.New("temporary Kubernetes API failure"),
	}
	root := t.TempDir()
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: filepath.Join(root, "public"), LedgerPath: filepath.Join(root, "private", "critic.json"),
		Metadata: trialMetadata(), Input: input, ExecutionID: "critic-transient-cleanup", RuntimeIdentity: testCriticRuntimeIdentity(),
	})
	if err == nil || record.Status != TrialCleanupPending || record.CleanupWork == nil || record.Finalized {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestRunTrialPersistsObservedWorkWhilePending(t *testing.T) {
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	externalCalls := 0
	reviewer := trialReviewerFunc(func(ctx context.Context, _ Input, _ string, observer engineruntime.WorkObserver) (Result, error) {
		planned := engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run"}
		if err := observer(ctx, planned); err != nil {
			return Result{}, err
		}
		observed := planned
		observed.UID = "uid-1"
		if err := observer(ctx, observed); err != nil {
			return Result{}, err
		}
		ledger, err := loadLedger(ledgerPath)
		if err != nil {
			return Result{}, err
		}
		if len(ledger.Records) != 1 || ledger.Records[0].Status != TrialPending || ledger.Records[0].CleanupWork == nil || ledger.Records[0].CleanupWork.UID != observed.UID {
			t.Fatalf("pending ledger=%+v", ledger)
		}
		return Result{Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, engineruntime.ErrMalformedResult
	})
	_, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: criticInput(t),
		ExecutionID: "critic-observed", RuntimeIdentity: testCriticRuntimeIdentity(),
		Observer: func(context.Context, engineruntime.WorkRef) error {
			externalCalls++
			return nil
		},
	})
	if !errors.Is(err, engineruntime.ErrMalformedResult) || externalCalls != 2 {
		t.Fatalf("err=%v external calls=%d", err, externalCalls)
	}
}

func TestRunTrialContractViolationOutranksCleanupPending(t *testing.T) {
	input := criticInput(t)
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	reviewer := &trialReviewer{
		result: Result{
			Execution:   ExecutionResult{Review: &review},
			CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"},
			Telemetry:   engineruntime.GenerateTelemetry{CleanupCompleted: false, FinalizationChecked: true},
		},
		err: errors.Join(engineruntime.ErrResultContract, engineruntime.ErrCleanupPending),
	}
	root := t.TempDir()
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: filepath.Join(root, "public"), LedgerPath: filepath.Join(root, "private", "critic.json"),
		Metadata: trialMetadata(), Input: input, ExecutionID: "critic-contract-cleanup", RuntimeIdentity: testCriticRuntimeIdentity(),
	})
	if !errors.Is(err, engineruntime.ErrResultContract) || record.Status != TrialContractViolation || record.CleanupWork == nil || record.Finalized {
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
	return RuntimeIdentity(engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.models.svc.cluster.local/v1", Model: "critic", ProtocolVersion: "openai-chat-completions-v1"}, strings.Repeat("a", 64), time.Minute, DefaultOutputLimit)
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

func TestRecoverPendingCleanupWaitsForStalePendingWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		wantCalls int
	}{
		{name: "recent", age: time.Minute},
		{name: "stale", age: pendingRecoveryAge + time.Minute, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := criticInput(t)
			metadata := trialMetadata()
			created := time.Now().UTC().Add(-tc.age)
			attemptHash := trialAttemptHash(metadata, input, "critic-stale", testCriticRuntimeIdentity())
			record := TrialRecord{
				ID: trialRecordID(created, attemptHash), CreatedAt: created.Format(time.RFC3339Nano), AttemptHash: attemptHash,
				RuntimeIdentity: testCriticRuntimeIdentity(), Status: TrialPending, Metadata: metadata,
				EvidenceHash: input.EvidenceHash, DraftHash: input.DraftHash, PairHash: input.PairHash,
				Usage:       GatewayUsage{Status: "unavailable", Source: "gateway_response"},
				CleanupWork: &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-run", UID: "uid-1"},
			}
			root := t.TempDir()
			publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
			claimed, err := claimTrial(publicDir, ledgerPath, record)
			if err != nil || !claimed {
				t.Fatalf("claim=%v err=%v", claimed, err)
			}
			cleaner := &trialCleaner{}
			if err := RecoverPendingCleanup(t.Context(), cleaner, publicDir, ledgerPath); err != nil {
				t.Fatal(err)
			}
			ledger, err := loadLedger(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if cleaner.calls != tc.wantCalls {
				t.Fatalf("cleanup calls=%d, want %d", cleaner.calls, tc.wantCalls)
			}
			if tc.wantCalls == 1 && (ledger.Records[0].CleanupWork != nil || !ledger.Records[0].Telemetry.CleanupCompleted || ledger.Records[0].Status != TrialPending) {
				t.Fatalf("recovered pending record=%+v", ledger.Records[0])
			}
		})
	}
}

func TestLedgerRetainsAttemptTombstonesAndCleanupRecords(t *testing.T) {
	input := criticInput(t)
	created := time.Now().UTC().Add(-time.Hour)
	ledger := Ledger{SchemaVersion: LedgerSchemaVersion}
	for index := 0; index <= maxLedgerRecords; index++ {
		when := created.Add(time.Duration(index) * time.Second)
		attemptHash := hashString(fmt.Sprintf("attempt-%d", index))
		record := TrialRecord{
			ID: fmt.Sprintf("critic-%020d", index), CreatedAt: when.Format(time.RFC3339Nano), AttemptHash: attemptHash,
			RuntimeIdentity: testCriticRuntimeIdentity(), Status: TrialRuntimeFailure, ErrorCode: "runtime_failure",
			Metadata: trialMetadata(), EvidenceHash: input.EvidenceHash, DraftHash: input.DraftHash, PairHash: input.PairHash,
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"},
		}
		if index == 0 {
			record.CleanupWork = &engineruntime.WorkRef{Backend: "agent-sandbox", Namespace: "critic", Name: "critic-old", UID: "uid-old"}
		}
		ledger.Records = append(ledger.Records, record)
		ledger.Attempts = append(ledger.Attempts, TrialAttempt{Hash: attemptHash, CreatedAt: record.CreatedAt, Status: record.Status})
	}
	path := filepath.Join(t.TempDir(), "critic.json")
	if err := writeLedger(path, ledger); err != nil {
		t.Fatal(err)
	}
	got, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != maxLedgerRecords || len(got.Attempts) != maxLedgerRecords+1 {
		t.Fatalf("records=%d attempts=%d", len(got.Records), len(got.Attempts))
	}
	oldAttempt := ledger.Attempts[0].Hash
	foundRecord, foundAttempt := false, false
	for _, record := range got.Records {
		if record.AttemptHash == oldAttempt && record.CleanupWork != nil {
			foundRecord = true
		}
	}
	for _, attempt := range got.Attempts {
		if attempt.Hash == oldAttempt {
			foundAttempt = true
		}
	}
	if !foundRecord || !foundAttempt {
		t.Fatalf("old cleanup record=%v attempt=%v", foundRecord, foundAttempt)
	}
	publicDir := filepath.Join(t.TempDir(), "public")
	claimed, err := claimTrial(publicDir, path, ledger.Records[0])
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("retained attempt tombstone was reclaimed")
	}
}

func TestRunTrialSeparatesLifecycleAndFailureCodes(t *testing.T) {
	input := criticInput(t)
	reviewer := &trialReviewer{
		result: Result{Execution: ExecutionResult{
			FailureCode: "gateway_request", FailureReason: "model gateway request failed",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"},
		}, Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true, FinalizationChecked: true, FinalizationValid: true}},
		err: errors.New("causal critic execution failed"),
	}
	root := t.TempDir()
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: filepath.Join(root, "public"), LedgerPath: filepath.Join(root, "private", "critic.json"),
		Metadata: trialMetadata(), Input: input, ExecutionID: "critic-failure-code", RuntimeIdentity: testCriticRuntimeIdentity(),
	})
	if err == nil || record.Status != TrialRuntimeFailure || record.ErrorCode != "runtime_failure" || record.FailureCode != "gateway_request" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestTrialRecordRejectsFailureCodeOnSuccess(t *testing.T) {
	input := criticInput(t)
	created := time.Unix(4000, 0).UTC()
	review := Review{SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: input.PairHash, Verdict: "pass", Findings: []Finding{}, Confidence: "medium"}
	record := TrialRecord{
		ID: "critic-success", CreatedAt: created.Format(time.RFC3339Nano), AttemptHash: hashString("attempt"),
		RuntimeIdentity: testCriticRuntimeIdentity(), Status: TrialSucceeded, FailureCode: "stale_failure", Metadata: trialMetadata(),
		EvidenceHash: input.EvidenceHash, DraftHash: input.DraftHash, PairHash: input.PairHash, Review: &review,
		Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, Telemetry: TrialTelemetry{CleanupCompleted: true}, Finalized: true,
	}
	if err := validateTrialRecord(record); err == nil {
		t.Fatal("successful trial retained a failure code")
	}
}

func TestLoadLedgerMigratesPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "critic.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.SchemaVersion != LedgerSchemaVersion || len(ledger.Preflights) != 0 || len(ledger.Records) != 0 {
		t.Fatalf("ledger=%+v", ledger)
	}
}

func TestRunTrialPersistsDashboardValidationCode(t *testing.T) {
	input := criticInput(t)
	reviewer := &trialReviewer{
		result: Result{
			Execution: ExecutionResult{FailureCode: "INVALID-CODE", FailureReason: "untrusted failure"},
			Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true, FinalizationChecked: true},
		},
		err: errors.Join(engineruntime.ErrResultContract, validationError(ValidationResultTerminal, ErrInvalidReview, "invalid failure code")),
	}
	root := t.TempDir()
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: filepath.Join(root, "public"), LedgerPath: filepath.Join(root, "private", "critic.json"),
		Metadata: trialMetadata(), Input: input, ExecutionID: "critic-invalid-code", RuntimeIdentity: testCriticRuntimeIdentity(),
	})
	if !errors.Is(err, engineruntime.ErrResultContract) || record.Status != TrialContractViolation || record.FailureCode != "validation_result_terminal" || record.FailureReason != "" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestRunTrialReturnsIdentityForAttemptTombstone(t *testing.T) {
	input := criticInput(t)
	metadata := trialMetadata()
	runtimeIdentity := testCriticRuntimeIdentity()
	executionID := "critic-tombstone"
	attemptHash := trialAttemptHash(metadata, input, executionID, runtimeIdentity)
	created := time.Unix(5000, 0).UTC()
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	if err := writeLedger(ledgerPath, Ledger{
		SchemaVersion: LedgerSchemaVersion,
		Attempts:      []TrialAttempt{{Hash: attemptHash, CreatedAt: created.Format(time.RFC3339Nano), Status: TrialSucceeded}},
		Records:       []TrialRecord{},
	}); err != nil {
		t.Fatal(err)
	}
	reviewer := &trialReviewer{}
	record, err := RunTrial(t.Context(), reviewer, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: metadata, Input: input, ExecutionID: executionID, RuntimeIdentity: runtimeIdentity,
		Now: func() time.Time { return created.Add(time.Minute) },
	})
	if !errors.Is(err, ErrTrialAlreadyAttempted) || !errors.Is(err, ErrTrialDetailsPruned) || record.AttemptHash != attemptHash || record.PairHash != input.PairHash || record.Status != TrialSucceeded || reviewer.calls != 0 {
		t.Fatalf("record=%+v reviewer=%d err=%v", record, reviewer.calls, err)
	}
	if _, found, err := LoadTrialByAttempt(publicDir, ledgerPath, attemptHash); !errors.Is(err, ErrTrialDetailsPruned) || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestRunTrialPersistsNULFailureReasonAsContractViolation(t *testing.T) {
	input := criticInput(t)
	runner := fakeSandboxRunner{run: func(spec agentsandbox.Spec) (agentsandbox.Result, error) {
		var request ExecutionRequest
		if err := json.Unmarshal(spec.Request, &request); err != nil {
			t.Fatal(err)
		}
		execution := ExecutionResult{
			SchemaVersion: ExecutionSchemaVersion, ContractVersion: ContractVersion, PairHash: request.Input.PairHash,
			TerminalState: engineruntime.TerminalFailed, FailureCode: "gateway_request", FailureReason: "gateway\x00failed",
			Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"}, DurationMs: 100,
		}
		data, _ := json.Marshal(execution)
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	runtime := &Runtime{
		Sandbox: runner, Gateway: engineruntime.ModelGatewayConfig{Endpoint: "https://gateway.platform.svc.cluster.local/v1", Model: "critic-model", ProtocolVersion: "openai-chat-completions-v1"},
		Timeout: time.Minute, OutputLimitBytes: DefaultOutputLimit,
	}
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	record, err := RunTrial(t.Context(), runtime, TrialSpec{
		PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: trialMetadata(), Input: input,
		ExecutionID: "critic-nul-reason", RuntimeIdentity: runtime.RuntimeIdentity(),
	})
	if !errors.Is(err, engineruntime.ErrResultContract) || ValidationCodeOf(err) != ValidationResultTerminal || record.Status != TrialContractViolation || record.FailureCode != "validation_result_terminal" || record.FailureReason != "" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	ledger, loadErr := loadLedger(ledgerPath)
	if loadErr != nil || len(ledger.Records) != 1 || ledger.Records[0].Status != TrialContractViolation {
		t.Fatalf("ledger=%+v err=%v", ledger, loadErr)
	}
}
