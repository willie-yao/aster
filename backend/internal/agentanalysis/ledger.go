package agentanalysis

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"golang.org/x/sys/unix"
)

var ledgerCostPattern = regexp.MustCompile(`^[0-9]+(?:[.][0-9]{1,12})?$`)

const (
	LedgerSchemaVersion     = 3
	maxLedgerRecords        = 100
	maxLedgerAttempts       = 4096
	maxLedgerBytes          = 4 << 20
	ledgerRetention         = 30 * 24 * time.Hour
	pendingAttemptRetention = time.Hour
)

// ShadowStatus classifies one non-authoritative experiment outcome.
type ShadowStatus string

const (
	ShadowStatusPending           ShadowStatus = "pending"
	ShadowStatusSucceeded         ShadowStatus = "succeeded"
	ShadowStatusCleanupPending    ShadowStatus = "cleanup_pending"
	ShadowStatusEvidenceFailed    ShadowStatus = "evidence_failed"
	ShadowStatusSetupFailed       ShadowStatus = "setup_failed"
	ShadowStatusRuntimeFailed     ShadowStatus = "runtime_failure"
	ShadowStatusNoResult          ShadowStatus = "no_result"
	ShadowStatusMalformedResult   ShadowStatus = "malformed_result"
	ShadowStatusExtraFile         ShadowStatus = "extra_file"
	ShadowStatusDeletion          ShadowStatus = "deletion"
	ShadowStatusRename            ShadowStatus = "rename"
	ShadowStatusContractViolation ShadowStatus = "contract_violation"
	ShadowStatusTimeout           ShadowStatus = "timeout"
	ShadowStatusCancellation      ShadowStatus = "cancellation"
)

// Subject identifies the failure compared by one private record.
type Subject struct {
	JobID      string `json:"job_id"`
	BuildID    string `json:"build_id"`
	TestName   string `json:"test_name"`
	TestSource string `json:"test_source,omitempty"`
	JUnitFile  string `json:"junit_file,omitempty"`
	SuiteName  string `json:"suite_name,omitempty"`
	ClassName  string `json:"class_name,omitempty"`
}

// AuthoritativeSnapshot is the bounded comparison view of the published result.
type AuthoritativeSnapshot struct {
	Summary           string                    `json:"summary"`
	IsTransient       bool                      `json:"is_transient"`
	RootCause         string                    `json:"root_cause"`
	Severity          string                    `json:"severity"`
	SuggestedFix      string                    `json:"suggested_fix"`
	RelevantFiles     []string                  `json:"relevant_files,omitempty"`
	EvidenceCitations []models.EvidenceCitation `json:"evidence_citations,omitempty"`
	ElapsedMs         int                       `json:"elapsed_ms,omitempty"`
	InputTokens       int                       `json:"input_tokens,omitempty"`
	OutputTokens      int                       `json:"output_tokens,omitempty"`
	ModelRequests     int                       `json:"model_requests,omitempty"`
	ToolCalls         int                       `json:"tool_calls,omitempty"`
	GCSBytes          int                       `json:"gcs_bytes,omitempty"`
	CacheHit          bool                      `json:"cache_hit,omitempty"`
	CritiquePassed    bool                      `json:"critique_passed,omitempty"`
	JudgeRan          bool                      `json:"judge_ran,omitempty"`
	JudgeObjected     bool                      `json:"judge_objected,omitempty"`
	JudgeRevised      bool                      `json:"judge_revised,omitempty"`
}

// EvidenceManifestEntry records frozen evidence identity without its content.
type EvidenceManifestEntry struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	ContentSHA256 string `json:"content_sha256"`
	Truncated     bool   `json:"truncated,omitempty"`
}

// Provenance records the complete private runtime identity.
type Provenance struct {
	Runtime                     string   `json:"runtime,omitempty"`
	AgentNamespace              string   `json:"agent_namespace,omitempty"`
	AgentRef                    string   `json:"agent_ref,omitempty"`
	GitSecret                   string   `json:"git_secret,omitempty"`
	AgentVersion                string   `json:"agent_version,omitempty"`
	ContractVersion             string   `json:"contract_version,omitempty"`
	ToolPolicyVersion           string   `json:"tool_policy_version,omitempty"`
	EvidenceHash                string   `json:"evidence_hash,omitempty"`
	SkillHash                   string   `json:"skill_hash,omitempty"`
	SourceSHA                   string   `json:"source_sha,omitempty"`
	IdentityHash                string   `json:"identity_hash,omitempty"`
	ExecutionID                 string   `json:"execution_id,omitempty"`
	Timeout                     string   `json:"timeout,omitempty"`
	MaxTurns                    int      `json:"max_turns,omitempty"`
	Retries                     int      `json:"retries,omitempty"`
	Attempts                    int      `json:"attempts,omitempty"`
	RuntimeDurationMs           int64    `json:"runtime_duration_ms,omitempty"`
	DurationMs                  int64    `json:"duration_ms,omitempty"`
	FinalizationDurationMs      int64    `json:"finalization_duration_ms,omitempty"`
	TaskFinalized               bool     `json:"task_finalized,omitempty"`
	TaskFinalizedMs             int64    `json:"task_finalized_ms,omitempty"`
	ResultAvailable             bool     `json:"result_available,omitempty"`
	ResultAvailableMs           int64    `json:"result_available_ms,omitempty"`
	FinalizationChecked         bool     `json:"finalization_checked,omitempty"`
	FinalizationValid           bool     `json:"finalization_valid,omitempty"`
	CleanupCompleted            bool     `json:"cleanup_completed,omitempty"`
	CleanupDurationMs           int64    `json:"cleanup_duration_ms,omitempty"`
	TokenUsageAvailable         bool     `json:"token_usage_available"`
	CostAvailable               bool     `json:"cost_available"`
	UsageStatus                 string   `json:"usage_status,omitempty"`
	InputTokens                 int      `json:"input_tokens,omitempty"`
	CachedInputTokens           int      `json:"cached_input_tokens,omitempty"`
	OutputTokens                int      `json:"output_tokens,omitempty"`
	ReasoningTokens             int      `json:"reasoning_tokens,omitempty"`
	CostUSD                     string   `json:"cost_usd,omitempty"`
	ModelIdentityAvailable      bool     `json:"model_identity_available"`
	ProviderIdentityAvailable   bool     `json:"provider_identity_available"`
	IdentityStatus              string   `json:"identity_status,omitempty"`
	ManifestHash                string   `json:"manifest_hash,omitempty"`
	StageHash                   string   `json:"stage_hash,omitempty"`
	EffectivePromptSHA256       string   `json:"effective_prompt_sha256,omitempty"`
	SkillSetHash                string   `json:"skill_set_hash,omitempty"`
	WorkspacePromptHash         string   `json:"workspace_prompt_hash,omitempty"`
	InputMode                   string   `json:"input_mode,omitempty"`
	MaxSteps                    int      `json:"max_steps,omitempty"`
	ModelContextTokens          int      `json:"model_context_tokens,omitempty"`
	ModelOutputTokens           int      `json:"model_output_tokens,omitempty"`
	ProviderRequests            int      `json:"provider_requests,omitempty"`
	ProviderRequestsKnown       bool     `json:"provider_requests_known"`
	SchedulingAvailable         bool     `json:"scheduling_available"`
	SchedulingMs                int64    `json:"scheduling_ms,omitempty"`
	StagingAvailable            bool     `json:"staging_available"`
	StagingMs                   int64    `json:"staging_ms,omitempty"`
	ExecutionAvailable          bool     `json:"execution_available"`
	ExecutionMs                 int64    `json:"execution_ms,omitempty"`
	ResultPublicationAvailable  bool     `json:"result_publication_available"`
	ResultPublicationMs         int64    `json:"result_publication_ms,omitempty"`
	PhaseTimingStatus           string   `json:"phase_timing_status,omitempty"`
	ProviderCredentialMode      string   `json:"provider_credential_mode,omitempty"`
	ProviderAPI                 string   `json:"provider_api,omitempty"`
	ProviderReasoningEffort     string   `json:"provider_reasoning_effort,omitempty"`
	TerminalState               string   `json:"terminal_state,omitempty"`
	OpenCodeFailureCode         string   `json:"opencode_failure_code,omitempty"`
	OpenCodeErrorClassification string   `json:"opencode_error_classification,omitempty"`
	ResultValidationStatus      string   `json:"result_validation_status,omitempty"`
	ResultValidationCodes       []string `json:"result_validation_codes,omitempty"`
	InputCleanupCompleted       bool     `json:"input_cleanup_completed,omitempty"`
	PublisherRequestHash        string   `json:"publisher_request_hash,omitempty"`
	PublisherJob                string   `json:"publisher_job,omitempty"`
	PublisherPod                string   `json:"publisher_pod,omitempty"`
	PublicationDurationMs       int64    `json:"publication_duration_ms,omitempty"`
	CleanupJob                  string   `json:"input_cleanup_job,omitempty"`
	CleanupPod                  string   `json:"input_cleanup_pod,omitempty"`
	InputCleanupDurationMs      int64    `json:"input_cleanup_duration_ms,omitempty"`
}

// ShadowQuality records private critique and judge telemetry without changing authority.
type ShadowQuality struct {
	DeterministicStatus string   `json:"deterministic_status"`
	DeterministicPassed bool     `json:"deterministic_passed"`
	RuleIDs             []string `json:"rule_ids,omitempty"`
	HardRules           []string `json:"hard_rules,omitempty"`
	SoftRules           []string `json:"soft_rules,omitempty"`
	SemanticStatus      string   `json:"semantic_status"`
	SemanticValid       bool     `json:"semantic_valid"`
	SemanticObjections  []string `json:"semantic_objections,omitempty"`
	SemanticReason      string   `json:"semantic_reason,omitempty"`
}

// ShadowRecord is one private comparison ledger entry.
type ShadowRecord struct {
	ID                  string                         `json:"id"`
	CreatedAt           string                         `json:"created_at"`
	AttemptHash         string                         `json:"attempt_hash"`
	ComparisonHash      string                         `json:"comparison_hash,omitempty"`
	Status              ShadowStatus                   `json:"status"`
	ErrorCode           string                         `json:"error_code,omitempty"`
	Subject             Subject                        `json:"subject"`
	Source              sourceinvestigation.Repository `json:"source"`
	RequestHash         string                         `json:"request_hash"`
	AuthoritativeHash   string                         `json:"authoritative_hash"`
	Authoritative       AuthoritativeSnapshot          `json:"authoritative"`
	Shadow              *WorkspaceAnalysis             `json:"shadow,omitempty"`
	Scan                *ArtifactScan                  `json:"scan,omitempty"`
	Evidence            []EvidenceManifestEntry        `json:"evidence,omitempty"`
	PlanIDs             []string                       `json:"plan_ids,omitempty"`
	Provenance          Provenance                     `json:"provenance"`
	Quality             ShadowQuality                  `json:"quality"`
	TotalDurationMs     int64                          `json:"total_duration_ms,omitempty"`
	CleanupPending      bool                           `json:"cleanup_pending,omitempty"`
	CleanupWork         *agentruntime.WorkRef          `json:"cleanup_work,omitempty"`
	InputCleanupPending bool                           `json:"input_cleanup_pending,omitempty"`
}

// AttemptRecord retains a compact deduplication identity after detailed records are pruned.
type AttemptRecord struct {
	Hash      string       `json:"hash"`
	CreatedAt string       `json:"created_at"`
	Status    ShadowStatus `json:"status"`
}

// ShadowLedger is the bounded private on-disk comparison history.
type ShadowLedger struct {
	SchemaVersion int             `json:"schema_version"`
	UpdatedAt     string          `json:"updated_at"`
	Attempts      []AttemptRecord `json:"attempts,omitempty"`
	Records       []ShadowRecord  `json:"records"`
}

// NewAuthoritativeSnapshot returns the stable published fields and their hash.
func NewAuthoritativeSnapshot(summary *models.AISummary, analysis *models.AIAnalysis) (AuthoritativeSnapshot, string, error) {
	if summary == nil || analysis == nil {
		return AuthoritativeSnapshot{}, "", fmt.Errorf("authoritative analysis is unavailable")
	}
	snapshot := AuthoritativeSnapshot{
		Summary: summary.Summary, IsTransient: summary.IsTransient,
		RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
		RelevantFiles: slices.Clone(analysis.RelevantFiles), EvidenceCitations: slices.Clone(analysis.EvidenceCitations),
		ElapsedMs: analysis.ElapsedMs, InputTokens: analysis.InputTokens, OutputTokens: analysis.OutputTokens,
		ModelRequests: analysis.ModelRequests, ToolCalls: analysis.ToolCalls, GCSBytes: analysis.GCSBytes, CacheHit: analysis.CacheHit,
		CritiquePassed: analysis.CritiquePassed, JudgeRan: analysis.JudgeRan, JudgeObjected: analysis.JudgeObjected, JudgeRevised: analysis.JudgeRevised,
	}
	payload := struct {
		Summary           string                    `json:"summary"`
		IsTransient       bool                      `json:"is_transient"`
		RootCause         string                    `json:"root_cause"`
		Severity          string                    `json:"severity"`
		SuggestedFix      string                    `json:"suggested_fix"`
		RelevantFiles     []string                  `json:"relevant_files,omitempty"`
		EvidenceCitations []models.EvidenceCitation `json:"evidence_citations,omitempty"`
	}{snapshot.Summary, snapshot.IsTransient, snapshot.RootCause, snapshot.Severity, snapshot.SuggestedFix, snapshot.RelevantFiles, snapshot.EvidenceCitations}
	data, err := json.Marshal(payload)
	if err != nil {
		return AuthoritativeSnapshot{}, "", err
	}
	return snapshot, hashString(string(data)), nil
}

// EvidenceManifest returns content-free evidence identity for persistence.
func EvidenceManifest(bundle EvidenceBundle) ([]EvidenceManifestEntry, []string) {
	evidence := make([]EvidenceManifestEntry, 0, len(bundle.Excerpts))
	for _, excerpt := range bundle.Excerpts {
		evidence = append(evidence, EvidenceManifestEntry{
			ID: excerpt.ID, Path: excerpt.Path, Kind: excerpt.Kind,
			ContentSHA256: excerpt.ContentSHA256, Truncated: excerpt.Truncated,
		})
	}
	planIDs := make([]string, 0, len(bundle.Plan))
	for _, planned := range bundle.Plan {
		planIDs = append(planIDs, planned.ID)
	}
	return evidence, planIDs
}

// NewRecordID returns a stable idempotency key for one attempted comparison.
func NewRecordID(subject Subject, createdAt time.Time, identity string) string {
	return "shadow-" + hashString(strings.Join([]string{
		subject.JobID, subject.BuildID, subject.TestName, subject.TestSource, subject.JUnitFile, subject.SuiteName, subject.ClassName,
		createdAt.UTC().Format(time.RFC3339Nano), identity,
	}, "\x00"))[:24]
}

// ValidatePrivateLedgerPath rejects ledger paths that resolve inside public output or through a symlink target.
func ValidatePrivateLedgerPath(publicDir, ledgerPath string) error {
	_, err := resolvePrivateLedgerPath(publicDir, ledgerPath, false)
	return err
}

// LedgerAttemptHashes returns the exact comparison contracts already attempted.
func LedgerAttemptHashes(publicDir, path string) (map[string]bool, error) {
	attempts := map[string]bool{}
	err := withLedgerLock(publicDir, path, func(resolvedPath string) error {
		ledger, err := loadLedger(resolvedPath)
		if err != nil {
			return err
		}
		reference := time.Now().UTC()
		for _, attempt := range ledger.Attempts {
			if attemptActive(attempt, reference) {
				attempts[attempt.Hash] = true
			}
		}
		for _, record := range ledger.Records {
			attempt := AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt, Status: record.Status}
			if attemptActive(attempt, reference) {
				attempts[record.AttemptHash] = true
			}
		}
		return nil
	})
	return attempts, err
}

// LedgerContainsAttempt reports whether this exact comparison contract was already attempted.
func LedgerContainsAttempt(publicDir, path, attemptHash string) (bool, error) {
	if !validSHA256(attemptHash) {
		return false, fmt.Errorf("agent analysis ledger lookup is invalid")
	}
	attempts, err := LedgerAttemptHashes(publicDir, path)
	return attempts[attemptHash], err
}

// ClaimLedgerAttempt atomically reserves one comparison identity before external work starts.
func ClaimLedgerAttempt(publicDir, path string, record ShadowRecord) (bool, error) {
	normalizeShadowQuality(&record.Quality)
	record.Status = ShadowStatusPending
	record.ErrorCode = ""
	record.ComparisonHash = ""
	record.Shadow = nil
	record.TotalDurationMs = 0
	createdAt, err := validateShadowRecord(record)
	if err != nil {
		return false, fmt.Errorf("agent analysis ledger claim: %w", err)
	}
	claimed := false
	err = withLedgerLock(publicDir, path, func(resolvedPath string) error {
		ledger, err := loadLedger(resolvedPath)
		if err != nil {
			return err
		}
		reference := ledgerReference(ledger, createdAt)
		pruneLedger(&ledger, reference)
		for _, attempt := range ledger.Attempts {
			if attempt.Hash == record.AttemptHash && attemptActive(attempt, reference) {
				return nil
			}
		}
		for _, existing := range ledger.Records {
			attempt := AttemptRecord{Hash: existing.AttemptHash, CreatedAt: existing.CreatedAt, Status: existing.Status}
			if attempt.Hash == record.AttemptHash && attemptActive(attempt, reference) {
				return nil
			}
		}
		upsertAttempt(&ledger, AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt, Status: ShadowStatusPending})
		upsertRecord(&ledger, record)
		ledger.UpdatedAt = reference.UTC().Format(time.RFC3339Nano)
		if err := writeLedger(resolvedPath, &ledger); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

// AppendLedger atomically replaces a claimed attempt with its final bounded record.
func AppendLedger(publicDir, path string, record ShadowRecord) error {
	normalizeShadowQuality(&record.Quality)
	createdAt, err := validateShadowRecord(record)
	if err != nil {
		return fmt.Errorf("agent analysis ledger record: %w", err)
	}
	return withLedgerLock(publicDir, path, func(resolvedPath string) error {
		ledger, err := loadLedger(resolvedPath)
		if err != nil {
			return err
		}
		reference := ledgerReference(ledger, createdAt)
		pruneLedger(&ledger, reference)
		upsertAttempt(&ledger, AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt, Status: record.Status})
		upsertRecord(&ledger, record)
		ledger.UpdatedAt = reference.UTC().Format(time.RFC3339Nano)
		return writeLedger(resolvedPath, &ledger)
	})
}

func ledgerReference(ledger ShadowLedger, createdAt time.Time) time.Time {
	reference := createdAt
	if previous, err := time.Parse(time.RFC3339Nano, ledger.UpdatedAt); err == nil && previous.After(reference) {
		reference = previous
	}
	return reference
}

func pruneLedger(ledger *ShadowLedger, reference time.Time) {
	records := ledger.Records[:0]
	for _, record := range ledger.Records {
		attempt := AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt, Status: record.Status}
		if attemptActive(attempt, reference) {
			records = append(records, record)
		}
	}
	ledger.Records = records
	attempts := ledger.Attempts[:0]
	for _, attempt := range ledger.Attempts {
		if attemptActive(attempt, reference) {
			attempts = append(attempts, attempt)
		}
	}
	ledger.Attempts = attempts
}

func attemptActive(attempt AttemptRecord, reference time.Time) bool {
	createdAt, err := time.Parse(time.RFC3339Nano, attempt.CreatedAt)
	if err != nil {
		return false
	}
	retention := ledgerRetention
	if attempt.Status == ShadowStatusPending {
		retention = pendingAttemptRetention
	}
	return !createdAt.Before(reference.Add(-retention))
}

func upsertAttempt(ledger *ShadowLedger, attempt AttemptRecord) {
	for i := range ledger.Attempts {
		if ledger.Attempts[i].Hash == attempt.Hash {
			ledger.Attempts[i] = attempt
			return
		}
	}
	ledger.Attempts = append(ledger.Attempts, attempt)
}

func upsertRecord(ledger *ShadowLedger, record ShadowRecord) {
	for i := range ledger.Records {
		if ledger.Records[i].ID == record.ID {
			ledger.Records[i] = record
			return
		}
	}
	ledger.Records = append(ledger.Records, record)
}

func writeLedger(path string, ledger *ShadowLedger) error {
	slices.SortFunc(ledger.Attempts, func(a, b AttemptRecord) int {
		aTime, _ := time.Parse(time.RFC3339Nano, a.CreatedAt)
		bTime, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		if !aTime.Equal(bTime) {
			if aTime.Before(bTime) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Hash, b.Hash)
	})
	if len(ledger.Attempts) > maxLedgerAttempts {
		ledger.Attempts = slices.Clone(ledger.Attempts[len(ledger.Attempts)-maxLedgerAttempts:])
	}
	slices.SortFunc(ledger.Records, func(a, b ShadowRecord) int {
		aTime, _ := time.Parse(time.RFC3339Nano, a.CreatedAt)
		bTime, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		if !aTime.Equal(bTime) {
			if aTime.Before(bTime) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(ledger.Records) > maxLedgerRecords {
		ledger.Records = slices.Clone(ledger.Records[len(ledger.Records)-maxLedgerRecords:])
	}
	for {
		data, err := json.MarshalIndent(ledger, "", "  ")
		if err != nil {
			return err
		}
		if len(data)+1 <= maxLedgerBytes {
			break
		}
		if len(ledger.Records) <= 1 {
			return fmt.Errorf("agent analysis ledger record exceeds %d bytes", maxLedgerBytes)
		}
		ledger.Records = slices.Clone(ledger.Records[1:])
	}
	return statefile.WritePrivateJSONDurable(path, ledger)
}

func loadLedger(path string) (ShadowLedger, error) {
	ledger := ShadowLedger{SchemaVersion: LedgerSchemaVersion}
	file, err := openNoFollow(path, unix.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return ledger, nil
	}
	if err != nil {
		return ShadowLedger{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLedgerBytes+1))
	if err != nil {
		return ShadowLedger{}, err
	}
	if len(data) > maxLedgerBytes {
		return ShadowLedger{}, fmt.Errorf("agent analysis ledger exceeds %d bytes", maxLedgerBytes)
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return ShadowLedger{}, fmt.Errorf("parse agent analysis ledger: %w", err)
	}
	if ledger.SchemaVersion != 1 && ledger.SchemaVersion != 2 && ledger.SchemaVersion != LedgerSchemaVersion {
		return ShadowLedger{}, fmt.Errorf("unsupported agent analysis ledger schema %d", ledger.SchemaVersion)
	}
	if ledger.SchemaVersion == 1 || ledger.SchemaVersion == 2 {
		normalizeLegacyLedger(&ledger)
	}
	for _, attempt := range ledger.Attempts {
		if !validSHA256(attempt.Hash) {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis attempt hash")
		}
		if _, err := time.Parse(time.RFC3339Nano, attempt.CreatedAt); err != nil {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis attempt time: %w", err)
		}
		if !validShadowStatus(attempt.Status) {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis attempt status %q", attempt.Status)
		}
	}
	for _, record := range ledger.Records {
		if _, err := validateShadowRecord(record); err != nil {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis ledger record: %w", err)
		}
	}
	return ledger, nil
}

func normalizeLegacyLedger(ledger *ShadowLedger) {
	if ledger == nil {
		return
	}
	ledger.SchemaVersion = LedgerSchemaVersion
	for i := range ledger.Attempts {
		ledger.Attempts[i].Status = normalizeLegacyShadowStatus(ledger.Attempts[i].Status)
	}
	for i := range ledger.Records {
		record := &ledger.Records[i]
		record.Status = normalizeLegacyShadowStatus(record.Status)
		if record.Provenance.RuntimeDurationMs == 0 {
			record.Provenance.RuntimeDurationMs = record.Provenance.DurationMs
		}
		record.Provenance.DurationMs = 0
		normalizeShadowQuality(&record.Quality)
	}
}

func normalizeLegacyShadowStatus(status ShadowStatus) ShadowStatus {
	switch status {
	case "runtime_failed":
		return ShadowStatusRuntimeFailed
	case "invalid_result":
		return ShadowStatusContractViolation
	case "cancelled":
		return ShadowStatusCancellation
	default:
		return status
	}
}

func validateShadowRecord(record ShadowRecord) (time.Time, error) {
	if record.ID == "" || record.CreatedAt == "" || record.Subject.JobID == "" || record.Subject.BuildID == "" || record.Subject.TestName == "" ||
		!validSHA256(record.AttemptHash) || !validSHA256(record.RequestHash) || !validSHA256(record.AuthoritativeHash) ||
		record.ComparisonHash != "" && !validSHA256(record.ComparisonHash) {
		return time.Time{}, fmt.Errorf("record identity is incomplete")
	}
	if !validShadowStatus(record.Status) {
		return time.Time{}, fmt.Errorf("unsupported shadow status %q", record.Status)
	}
	if !validShadowQuality(record.Quality) {
		return time.Time{}, fmt.Errorf("invalid shadow quality telemetry")
	}
	if !validShadowProvenance(record.Provenance) {
		return time.Time{}, fmt.Errorf("invalid shadow provenance telemetry")
	}
	if err := sourceinvestigation.ValidateRepository(record.Source); err != nil {
		return time.Time{}, err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("record time: %w", err)
	}
	return createdAt, nil
}

func validShadowProvenance(value Provenance) bool {
	for _, count := range []int{value.Attempts, value.InputTokens, value.CachedInputTokens, value.OutputTokens, value.ReasoningTokens, value.ProviderRequests} {
		if count < 0 {
			return false
		}
	}
	for _, duration := range []int64{
		value.RuntimeDurationMs, value.DurationMs, value.FinalizationDurationMs, value.TaskFinalizedMs, value.ResultAvailableMs,
		value.CleanupDurationMs, value.SchedulingMs, value.StagingMs, value.ExecutionMs, value.ResultPublicationMs,
		value.PublicationDurationMs, value.InputCleanupDurationMs,
	} {
		if duration < 0 {
			return false
		}
	}
	if value.CachedInputTokens > value.InputTokens || value.ReasoningTokens > value.OutputTokens {
		return false
	}
	if value.CostAvailable != (value.CostUSD != "") || value.CostUSD != "" && !ledgerCostPattern.MatchString(value.CostUSD) {
		return false
	}
	for _, phase := range []struct {
		available bool
		duration  int64
	}{
		{value.SchedulingAvailable, value.SchedulingMs},
		{value.StagingAvailable, value.StagingMs},
		{value.ExecutionAvailable, value.ExecutionMs},
		{value.ResultPublicationAvailable, value.ResultPublicationMs},
	} {
		if !phase.available && phase.duration != 0 {
			return false
		}
	}
	switch value.TerminalState {
	case "", string(agentruntime.TerminalSucceeded), string(agentruntime.TerminalFailed), string(agentruntime.TerminalCancelled), string(agentruntime.TerminalTimedOut):
	default:
		return false
	}
	if !validWorkspaceFailureCode(value.OpenCodeFailureCode) || len(value.OpenCodeErrorClassification) > 64 {
		return false
	}
	switch value.ResultValidationStatus {
	case "", WorkspaceResultAccepted, WorkspaceResultAcceptedWithWarnings, WorkspaceResultRejected:
	default:
		return false
	}
	if len(value.ResultValidationCodes) > 32 {
		return false
	}
	for _, code := range value.ResultValidationCodes {
		if !validWorkspaceFailureCode(code) {
			return false
		}
	}
	return true
}

func normalizeShadowQuality(quality *ShadowQuality) {
	if quality == nil {
		return
	}
	if quality.DeterministicStatus == "" {
		quality.DeterministicStatus = "not_run"
	}
	if quality.SemanticStatus == "" {
		quality.SemanticStatus = "not_run"
	}
}

func validShadowQuality(quality ShadowQuality) bool {
	switch quality.DeterministicStatus {
	case "not_run", "unavailable":
		if quality.DeterministicPassed || len(quality.RuleIDs) > 0 || len(quality.HardRules) > 0 || len(quality.SoftRules) > 0 {
			return false
		}
	case "passed":
		if !quality.DeterministicPassed || len(quality.HardRules) > 0 {
			return false
		}
	case "objected":
		if quality.DeterministicPassed || len(quality.RuleIDs) == 0 {
			return false
		}
	default:
		return false
	}
	switch quality.SemanticStatus {
	case "not_run", "unavailable", "passed", "objected", "error":
	default:
		return false
	}
	if quality.SemanticStatus == "passed" && !quality.SemanticValid || quality.SemanticStatus != "passed" && quality.SemanticValid {
		return false
	}
	return true
}

func validShadowStatus(status ShadowStatus) bool {
	switch status {
	case ShadowStatusPending, ShadowStatusSucceeded, ShadowStatusCleanupPending, ShadowStatusEvidenceFailed, ShadowStatusSetupFailed,
		ShadowStatusRuntimeFailed, ShadowStatusNoResult, ShadowStatusMalformedResult, ShadowStatusExtraFile, ShadowStatusDeletion,
		ShadowStatusRename, ShadowStatusContractViolation, ShadowStatusTimeout, ShadowStatusCancellation:
		return true
	default:
		return false
	}
}

func withLedgerLock(publicDir, path string, fn func(string) error) error {
	resolvedPath, err := resolvePrivateLedgerPath(publicDir, path, true)
	if err != nil {
		return err
	}
	lock, err := openNoFollow(resolvedPath+".lock", unix.O_CREAT|unix.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	if fn == nil {
		return nil
	}
	return fn(resolvedPath)
}

func resolvePrivateLedgerPath(publicDir, ledgerPath string, createParent bool) (string, error) {
	if !filepath.IsAbs(ledgerPath) {
		return "", fmt.Errorf("agent analysis ledger path must be absolute")
	}
	publicResolved, err := resolvePathWithMissing(publicDir)
	if err != nil {
		return "", err
	}
	parentResolved, err := resolvePathWithMissing(filepath.Dir(filepath.Clean(ledgerPath)))
	if err != nil {
		return "", err
	}
	resolvedPath := filepath.Join(parentResolved, filepath.Base(filepath.Clean(ledgerPath)))
	if pathWithin(publicResolved, resolvedPath) {
		return "", fmt.Errorf("agent analysis ledger resolves inside public output")
	}
	if createParent {
		if err := os.MkdirAll(parentResolved, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(parentResolved, 0o700); err != nil && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.ENOSYS) {
			return "", err
		}
		realParent, err := filepath.EvalSymlinks(parentResolved)
		if err != nil {
			return "", err
		}
		resolvedPath = filepath.Join(realParent, filepath.Base(resolvedPath))
		publicResolved, err = resolvePathWithMissing(publicDir)
		if err != nil {
			return "", err
		}
		if pathWithin(publicResolved, resolvedPath) {
			return "", fmt.Errorf("agent analysis ledger resolves inside public output")
		}
	}
	for _, target := range []string{resolvedPath, resolvedPath + ".lock"} {
		info, err := os.Lstat(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return "", fmt.Errorf("agent analysis ledger target must be a regular file")
		}
	}
	return resolvedPath, nil
}

func resolvePathWithMissing(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func openNoFollow(path string, flags int, perm uint32) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW, perm)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
