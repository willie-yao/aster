package causalcritic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	LedgerSchemaVersion = 1
	maxLedgerRecords    = 100
	maxLedgerBytes      = 4 << 20
	ledgerRetention     = 30 * 24 * time.Hour
	pendingRetention    = time.Hour
)

var ErrTrialAlreadyAttempted = errors.New("causal critic trial already attempted")

// TrialStatus classifies one independent critic execution without granting authority.
type TrialStatus string

const (
	TrialPending           TrialStatus = "pending"
	TrialSucceeded         TrialStatus = "succeeded"
	TrialCleanupPending    TrialStatus = "cleanup_pending"
	TrialMalformedResult   TrialStatus = "malformed_result"
	TrialContractViolation TrialStatus = "contract_violation"
	TrialTimeout           TrialStatus = "timeout"
	TrialCancellation      TrialStatus = "cancellation"
	TrialUnavailable       TrialStatus = "unavailable"
	TrialRuntimeFailure    TrialStatus = "runtime_failure"
)

// TrialMetadata identifies one paired cold comparison without answer-bearing data.
type TrialMetadata struct {
	CaseID                     string `json:"case_id"`
	StableID                   string `json:"stable_id"`
	Repetition                 int    `json:"repetition"`
	Arm                        string `json:"arm"`
	AuthoritativeArm           string `json:"authoritative_arm"`
	AuthoritativeElapsedMs     int    `json:"authoritative_elapsed_ms,omitempty"`
	AuthoritativeInputTokens   int    `json:"authoritative_input_tokens,omitempty"`
	AuthoritativeOutputTokens  int    `json:"authoritative_output_tokens,omitempty"`
	AuthoritativeModelRequests int    `json:"authoritative_model_requests,omitempty"`
	SameModelJudgeObjected     bool   `json:"same_model_judge_objected,omitempty"`
	SameModelJudgeRevised      bool   `json:"same_model_judge_revised,omitempty"`
}

// TrialTelemetry records lifecycle facts separately from critic quality.
type TrialTelemetry struct {
	SandboxFinished     bool   `json:"sandbox_finished"`
	SandboxFinishedMs   int64  `json:"sandbox_finished_ms,omitempty"`
	ResultAvailable     bool   `json:"result_available"`
	ResultAvailableMs   int64  `json:"result_available_ms,omitempty"`
	ValidationChecked   bool   `json:"validation_checked"`
	ValidationValid     bool   `json:"validation_valid"`
	CleanupCompleted    bool   `json:"cleanup_completed"`
	CleanupDurationMs   int64  `json:"cleanup_duration_ms,omitempty"`
	TokenUsageAvailable bool   `json:"token_usage_available"`
	CostAvailable       bool   `json:"cost_available"`
	UsageStatus         string `json:"usage_status,omitempty"`
}

// TrialRecord is one private, non-authoritative critic comparison.
type TrialRecord struct {
	ID                string                         `json:"id"`
	CreatedAt         string                         `json:"created_at"`
	AttemptHash       string                         `json:"attempt_hash"`
	RuntimeIdentity   string                         `json:"runtime_identity"`
	Status            TrialStatus                    `json:"status"`
	ErrorCode         string                         `json:"error_code,omitempty"`
	FailureReason     string                         `json:"failure_reason,omitempty"`
	Metadata          TrialMetadata                  `json:"metadata"`
	EvidenceHash      string                         `json:"evidence_hash"`
	DraftHash         string                         `json:"draft_hash"`
	PairHash          string                         `json:"pair_hash"`
	Review            *Review                        `json:"review,omitempty"`
	Usage             GatewayUsage                   `json:"usage"`
	Resources         engineruntime.ResourceMetadata `json:"resources"`
	CleanupWork       *engineruntime.WorkRef         `json:"cleanup_work,omitempty"`
	Telemetry         TrialTelemetry                 `json:"telemetry"`
	RuntimeDurationMs int64                          `json:"runtime_duration_ms,omitempty"`
	Finalized         bool                           `json:"finalized"`
}

// Ledger stores bounded private critic comparisons.
type Ledger struct {
	SchemaVersion int           `json:"schema_version"`
	UpdatedAt     string        `json:"updated_at,omitempty"`
	Records       []TrialRecord `json:"records"`
}

// TrialSpec configures one persisted private critic run.
type TrialSpec struct {
	PublicDir       string
	LedgerPath      string
	Metadata        TrialMetadata
	Input           Input
	ExecutionID     string
	RuntimeIdentity string
	Observer        engineruntime.WorkObserver
	Now             func() time.Time
}

// Reviewer is the causal critic runtime seam used by the private orchestrator.
type Reviewer interface {
	Review(context.Context, Input, string, engineruntime.WorkObserver) (Result, error)
}

// RunTrial claims, executes, and persists one exact paired critic comparison.
func RunTrial(ctx context.Context, reviewer Reviewer, spec TrialSpec) (TrialRecord, error) {
	if reviewer == nil {
		return TrialRecord{}, fmt.Errorf("causal critic reviewer is unavailable")
	}
	if err := validateTrialMetadata(spec.Metadata); err != nil {
		return TrialRecord{}, err
	}
	if err := ValidateInput(spec.Input); err != nil {
		return TrialRecord{}, err
	}
	if !validSHA256(spec.RuntimeIdentity) {
		return TrialRecord{}, fmt.Errorf("causal critic runtime identity is invalid")
	}
	if err := agentanalysis.ValidatePrivateLedgerPath(spec.PublicDir, spec.LedgerPath); err != nil {
		return TrialRecord{}, fmt.Errorf("causal critic ledger: %w", err)
	}
	now := spec.Now
	if now == nil {
		now = time.Now
	}
	created := now().UTC()
	attemptHash := trialAttemptHash(spec.Metadata, spec.Input, spec.ExecutionID, spec.RuntimeIdentity)
	record := TrialRecord{
		ID: trialRecordID(created, attemptHash), CreatedAt: created.Format(time.RFC3339Nano), AttemptHash: attemptHash,
		Status: TrialPending, Metadata: spec.Metadata, RuntimeIdentity: spec.RuntimeIdentity, EvidenceHash: spec.Input.EvidenceHash,
		DraftHash: spec.Input.DraftHash, PairHash: spec.Input.PairHash,
		Usage: GatewayUsage{Status: "unavailable", Source: "gateway_response"},
	}
	claimed, err := claimTrial(spec.PublicDir, spec.LedgerPath, record)
	if err != nil {
		return TrialRecord{}, err
	}
	if !claimed {
		return TrialRecord{}, fmt.Errorf("%w: %s repetition %d", ErrTrialAlreadyAttempted, spec.Metadata.CaseID, spec.Metadata.Repetition)
	}
	started := now()
	result, runErr := reviewer.Review(ctx, spec.Input, spec.ExecutionID, spec.Observer)
	record.RuntimeDurationMs = max(now().Sub(started).Milliseconds(), 0)
	record.Resources = result.Resources
	if result.CleanupWork != nil && !result.Telemetry.CleanupCompleted {
		work := *result.CleanupWork
		record.CleanupWork = &work
	}
	record.Telemetry = trialTelemetry(result.Telemetry)
	if result.Execution.Usage.Source != "" {
		record.Usage = result.Execution.Usage
	}
	record.FailureReason = strings.TrimSpace(result.Execution.FailureReason)
	if result.Execution.Review != nil {
		review := *result.Execution.Review
		record.Review = &review
	}
	record.Status, record.ErrorCode = classifyTrialResult(result, runErr)
	record.Finalized = record.Status == TrialSucceeded && record.Review != nil && record.Telemetry.CleanupCompleted
	if appendErr := appendTrial(spec.PublicDir, spec.LedgerPath, record); appendErr != nil {
		return record, errors.Join(runErr, appendErr)
	}
	return record, runErr
}

func trialTelemetry(value engineruntime.GenerateTelemetry) TrialTelemetry {
	return TrialTelemetry{
		SandboxFinished: value.TaskFinalized, SandboxFinishedMs: value.TaskFinalizedMs,
		ResultAvailable: value.ResultAvailable, ResultAvailableMs: value.ResultAvailableMs,
		ValidationChecked: value.FinalizationChecked, ValidationValid: value.FinalizationValid,
		CleanupCompleted: value.CleanupCompleted, CleanupDurationMs: value.CleanupDurationMs,
		TokenUsageAvailable: value.TokenUsageAvailable, CostAvailable: value.CostAvailable, UsageStatus: value.UsageStatus,
	}
}

func classifyTrialResult(result Result, err error) (TrialStatus, string) {
	switch {
	case err == nil && result.Execution.Review != nil && result.Telemetry.CleanupCompleted:
		return TrialSucceeded, ""
	case errors.Is(err, engineruntime.ErrCleanupPending) && result.Execution.Review != nil:
		return TrialCleanupPending, "cleanup_pending"
	case errors.Is(err, engineruntime.ErrMalformedResult):
		return TrialMalformedResult, "malformed_result"
	case errors.Is(err, engineruntime.ErrResultContract):
		return TrialContractViolation, "contract_violation"
	case errors.Is(err, context.DeadlineExceeded):
		return TrialTimeout, "timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, engineruntime.ErrCancelled):
		return TrialCancellation, "cancellation"
	case errors.Is(err, engineruntime.ErrUnavailable):
		return TrialUnavailable, "unavailable"
	default:
		return TrialRuntimeFailure, "runtime_failure"
	}
}

func validateTrialMetadata(metadata TrialMetadata) error {
	if strings.TrimSpace(metadata.CaseID) == "" || len(metadata.CaseID) > 160 || strings.ContainsAny(metadata.CaseID, "\r\n\x00") {
		return fmt.Errorf("causal critic case id is invalid")
	}
	if len(metadata.StableID) != 20 || metadata.StableID != strings.ToLower(metadata.StableID) {
		return fmt.Errorf("causal critic stable id must be 20 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(metadata.StableID); err != nil {
		return fmt.Errorf("causal critic stable id must be hexadecimal")
	}
	if metadata.Repetition < 1 || metadata.Repetition > 1000 {
		return fmt.Errorf("causal critic repetition must be between 1 and 1000")
	}
	if metadata.Arm != "agent-sandbox-independent-critic" {
		return fmt.Errorf("causal critic arm is invalid")
	}
	if strings.TrimSpace(metadata.AuthoritativeArm) == "" || len(metadata.AuthoritativeArm) > 80 || strings.ContainsAny(metadata.AuthoritativeArm, " \t\r\n") {
		return fmt.Errorf("causal critic authoritative arm is invalid")
	}
	for _, value := range []int{metadata.AuthoritativeElapsedMs, metadata.AuthoritativeInputTokens, metadata.AuthoritativeOutputTokens, metadata.AuthoritativeModelRequests} {
		if value < 0 {
			return fmt.Errorf("causal critic authoritative telemetry must be non-negative")
		}
	}
	return nil
}

func trialAttemptHash(metadata TrialMetadata, input Input, executionID, runtimeIdentity string) string {
	data, _ := json.Marshal(struct {
		Metadata        TrialMetadata `json:"metadata"`
		PairHash        string        `json:"pair_hash"`
		ExecutionID     string        `json:"execution_id"`
		RuntimeIdentity string        `json:"runtime_identity"`
	}{metadata, input.PairHash, strings.TrimSpace(executionID), runtimeIdentity})
	return hashString(string(data))
}

func trialRecordID(created time.Time, attemptHash string) string {
	sum := sha256.Sum256([]byte(created.Format(time.RFC3339Nano) + "\x00" + attemptHash))
	return "critic-" + hex.EncodeToString(sum[:10])
}

func claimTrial(publicDir, path string, record TrialRecord) (bool, error) {
	claimed := false
	err := withLedgerLock(publicDir, path, func(resolved string) error {
		ledger, err := loadLedger(resolved)
		if err != nil {
			return err
		}
		pruneLedger(&ledger, createdAtOrNow(record.CreatedAt))
		for _, existing := range ledger.Records {
			if existing.AttemptHash == record.AttemptHash {
				return nil
			}
		}
		ledger.Records = append(ledger.Records, record)
		ledger.UpdatedAt = record.CreatedAt
		if err := writeLedger(resolved, ledger); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

func appendTrial(publicDir, path string, record TrialRecord) error {
	if err := validateTrialRecord(record); err != nil {
		return err
	}
	return withLedgerLock(publicDir, path, func(resolved string) error {
		ledger, err := loadLedger(resolved)
		if err != nil {
			return err
		}
		updated := false
		for index := range ledger.Records {
			if ledger.Records[index].AttemptHash == record.AttemptHash {
				ledger.Records[index] = record
				updated = true
				break
			}
		}
		if !updated {
			return fmt.Errorf("causal critic trial claim is missing")
		}
		ledger.UpdatedAt = record.CreatedAt
		pruneLedger(&ledger, createdAtOrNow(record.CreatedAt))
		return writeLedger(resolved, ledger)
	})
}

func validateTrialRecord(record TrialRecord) error {
	if record.ID == "" || record.CreatedAt == "" || !validSHA256(record.AttemptHash) || !validSHA256(record.RuntimeIdentity) || !validSHA256(record.EvidenceHash) || !validSHA256(record.DraftHash) || !validSHA256(record.PairHash) {
		return fmt.Errorf("causal critic record identity is incomplete")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil {
		return fmt.Errorf("causal critic record time: %w", err)
	}
	if err := validateTrialMetadata(record.Metadata); err != nil {
		return err
	}
	switch record.Status {
	case TrialPending, TrialSucceeded, TrialCleanupPending, TrialMalformedResult, TrialContractViolation, TrialTimeout, TrialCancellation, TrialUnavailable, TrialRuntimeFailure:
	default:
		return fmt.Errorf("unsupported causal critic status %q", record.Status)
	}
	if len(record.FailureReason) > 2<<10 || strings.ContainsRune(record.FailureReason, '\x00') {
		return fmt.Errorf("causal critic failure reason is invalid or oversized")
	}
	if record.Status == TrialSucceeded && record.FailureReason != "" {
		return fmt.Errorf("successful causal critic record has a failure reason")
	}
	if record.Finalized != (record.Status == TrialSucceeded && record.Review != nil && record.Telemetry.CleanupCompleted) {
		return fmt.Errorf("causal critic finalized state is inconsistent")
	}
	if record.Status == TrialCleanupPending && (record.CleanupWork == nil || record.CleanupWork.UID == "") {
		return fmt.Errorf("causal critic cleanup-pending record lacks observed work identity")
	}
	if record.Review != nil && record.Review.PairHash != record.PairHash {
		return fmt.Errorf("causal critic review pair identity changed")
	}
	return nil
}

func loadLedger(path string) (Ledger, error) {
	ledger := Ledger{SchemaVersion: LedgerSchemaVersion, Records: []TrialRecord{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return ledger, err
	}
	if len(data) > maxLedgerBytes {
		return ledger, fmt.Errorf("causal critic ledger exceeds %d bytes", maxLedgerBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return ledger, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ledger, fmt.Errorf("causal critic ledger contains trailing data")
	}
	if ledger.SchemaVersion != LedgerSchemaVersion {
		return ledger, fmt.Errorf("unsupported causal critic ledger schema %d", ledger.SchemaVersion)
	}
	for _, record := range ledger.Records {
		if err := validateTrialRecord(record); err != nil {
			return ledger, err
		}
	}
	return ledger, nil
}

func writeLedger(path string, ledger Ledger) error {
	ledger.SchemaVersion = LedgerSchemaVersion
	slices.SortFunc(ledger.Records, func(left, right TrialRecord) int {
		if left.CreatedAt != right.CreatedAt {
			return strings.Compare(left.CreatedAt, right.CreatedAt)
		}
		return strings.Compare(left.ID, right.ID)
	})
	if len(ledger.Records) > maxLedgerRecords {
		ledger.Records = slices.Clone(ledger.Records[len(ledger.Records)-maxLedgerRecords:])
	}
	for {
		data, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		if len(data) <= maxLedgerBytes {
			break
		}
		if len(ledger.Records) <= 1 {
			return fmt.Errorf("causal critic ledger record exceeds %d bytes", maxLedgerBytes)
		}
		ledger.Records = slices.Clone(ledger.Records[1:])
	}
	return statefile.WritePrivateJSONDurable(path, ledger)
}

func createdAtOrNow(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	return time.Now().UTC()
}

func pruneLedger(ledger *Ledger, reference time.Time) {
	kept := ledger.Records[:0]
	for _, record := range ledger.Records {
		created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		retention := ledgerRetention
		if record.Status == TrialPending {
			retention = pendingRetention
		}
		if err == nil && !created.Before(reference.Add(-retention)) {
			kept = append(kept, record)
		}
	}
	ledger.Records = kept
}

func withLedgerLock(publicDir, ledgerPath string, fn func(string) error) error {
	if err := agentanalysis.ValidatePrivateLedgerPath(publicDir, ledgerPath); err != nil {
		return err
	}
	path := filepath.Clean(ledgerPath)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	path = filepath.Join(realParent, filepath.Base(path))
	if err := agentanalysis.ValidatePrivateLedgerPath(publicDir, path); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	return fn(path)
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// PendingCleaner retries cleanup for a persisted exact Sandbox identity.
type PendingCleaner interface {
	Cleanup(context.Context, engineruntime.WorkRef) error
}

// RecoverPendingCleanup retries cleanup-only work and finalizes persisted valid reviews.
func RecoverPendingCleanup(ctx context.Context, cleaner PendingCleaner, publicDir, ledgerPath string) error {
	if cleaner == nil {
		return fmt.Errorf("causal critic cleanup runtime is unavailable")
	}
	var pending []TrialRecord
	if err := withLedgerLock(publicDir, ledgerPath, func(path string) error {
		ledger, err := loadLedger(path)
		if err != nil {
			return err
		}
		for _, record := range ledger.Records {
			if record.Status == TrialCleanupPending && record.CleanupWork != nil {
				pending = append(pending, record)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var recovered []TrialRecord
	var failures []error
	for _, record := range pending {
		started := time.Now()
		err := cleaner.Cleanup(ctx, *record.CleanupWork)
		record.Telemetry.CleanupDurationMs += max(time.Since(started).Milliseconds(), 0)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		record.Telemetry.CleanupCompleted = true
		record.Status = TrialSucceeded
		record.ErrorCode = ""
		record.Finalized = record.Review != nil && record.Telemetry.ValidationValid
		record.CleanupWork = nil
		recovered = append(recovered, record)
	}
	if len(recovered) > 0 {
		if err := withLedgerLock(publicDir, ledgerPath, func(path string) error {
			ledger, err := loadLedger(path)
			if err != nil {
				return err
			}
			byAttempt := make(map[string]TrialRecord, len(recovered))
			for _, record := range recovered {
				byAttempt[record.AttemptHash] = record
			}
			for index := range ledger.Records {
				if record, ok := byAttempt[ledger.Records[index].AttemptHash]; ok && ledger.Records[index].Status == TrialCleanupPending {
					ledger.Records[index] = record
				}
			}
			ledger.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return writeLedger(path, ledger)
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
