package agentanalysis

import (
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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	LedgerSchemaVersion = 1
	maxLedgerRecords    = 100
	maxLedgerAttempts   = 4096
	maxLedgerBytes      = 4 << 20
	ledgerRetention     = 30 * 24 * time.Hour
)

// ShadowStatus classifies one non-authoritative experiment outcome.
type ShadowStatus string

const (
	ShadowStatusSucceeded      ShadowStatus = "succeeded"
	ShadowStatusCleanupPending ShadowStatus = "cleanup_pending"
	ShadowStatusEvidenceFailed ShadowStatus = "evidence_failed"
	ShadowStatusSetupFailed    ShadowStatus = "setup_failed"
	ShadowStatusRuntimeFailed  ShadowStatus = "runtime_failed"
	ShadowStatusInvalidResult  ShadowStatus = "invalid_result"
	ShadowStatusCancelled      ShadowStatus = "cancelled"
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
	Runtime         string `json:"runtime,omitempty"`
	AgentNamespace  string `json:"agent_namespace,omitempty"`
	AgentRef        string `json:"agent_ref,omitempty"`
	GitSecret       string `json:"git_secret,omitempty"`
	AgentVersion    string `json:"agent_version,omitempty"`
	ContractVersion string `json:"contract_version,omitempty"`
	EvidenceHash    string `json:"evidence_hash,omitempty"`
	SkillHash       string `json:"skill_hash,omitempty"`
	SourceSHA       string `json:"source_sha,omitempty"`
	IdentityHash    string `json:"identity_hash,omitempty"`
	ExecutionID     string `json:"execution_id,omitempty"`
	Timeout         string `json:"timeout,omitempty"`
	MaxTurns        int    `json:"max_turns,omitempty"`
	Retries         int    `json:"retries,omitempty"`
	Attempts        int    `json:"attempts,omitempty"`
	DurationMs      int64  `json:"duration_ms,omitempty"`
}

// ShadowRecord is one private comparison ledger entry.
type ShadowRecord struct {
	ID                string                         `json:"id"`
	CreatedAt         string                         `json:"created_at"`
	AttemptHash       string                         `json:"attempt_hash"`
	ComparisonHash    string                         `json:"comparison_hash,omitempty"`
	Status            ShadowStatus                   `json:"status"`
	ErrorCode         string                         `json:"error_code,omitempty"`
	Subject           Subject                        `json:"subject"`
	Source            sourceinvestigation.Repository `json:"source"`
	RequestHash       string                         `json:"request_hash"`
	AuthoritativeHash string                         `json:"authoritative_hash"`
	Authoritative     AuthoritativeSnapshot          `json:"authoritative"`
	Shadow            *Analysis                      `json:"shadow,omitempty"`
	Scan              *ArtifactScan                  `json:"scan,omitempty"`
	Evidence          []EvidenceManifestEntry        `json:"evidence,omitempty"`
	PlanIDs           []string                       `json:"plan_ids,omitempty"`
	Provenance        Provenance                     `json:"provenance"`
	TotalDurationMs   int64                          `json:"total_duration_ms,omitempty"`
	CleanupPending    bool                           `json:"cleanup_pending,omitempty"`
	CleanupWork       *agentruntime.WorkRef          `json:"cleanup_work,omitempty"`
}

// AttemptRecord retains a compact deduplication identity after detailed records are pruned.
type AttemptRecord struct {
	Hash      string `json:"hash"`
	CreatedAt string `json:"created_at"`
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

// ProvenanceFromResult converts a runtime result to its content-free identity.
func ProvenanceFromResult(result Result) Provenance {
	return Provenance{
		Runtime: result.Runtime, AgentNamespace: result.AgentNamespace, AgentRef: result.AgentRef, AgentVersion: result.AgentVersion,
		ContractVersion: result.ContractVersion, EvidenceHash: result.EvidenceHash, SkillHash: result.SkillHash,
		SourceSHA: result.SourceSHA, IdentityHash: result.IdentityHash, ExecutionID: result.ExecutionID,
		Timeout: result.Timeout.String(), MaxTurns: result.MaxTurns, Retries: result.Retries,
		Attempts: result.Attempts, DurationMs: result.Duration.Milliseconds(),
	}
}

// AttemptIdentity fingerprints an authoritative result and runtime settings before evidence collection.
func AttemptIdentity(subject Subject, requestHash, authoritativeHash, skillSetHash string, source sourceinvestigation.Repository, agentNamespace, agentRef, agentVersion, gitSecret string, timeout time.Duration, maxTurns, retries int) string {
	return hashString(strings.Join([]string{
		ContractVersion, SkillHash(), requestHash, authoritativeHash, skillSetHash,
		source.Owner, source.Name, source.Revision, subject.JobID, subject.BuildID, subject.TestName, subject.TestSource, subject.JUnitFile, subject.SuiteName, subject.ClassName,
		strings.TrimSpace(agentNamespace), strings.TrimSpace(agentRef), strings.TrimSpace(agentVersion), strings.TrimSpace(gitSecret), timeout.String(), fmt.Sprintf("%d", maxTurns), fmt.Sprintf("%d", retries),
	}, "\x00"))
}

// ComparisonIdentity adds the exact frozen evidence hash to one attempted identity.
func ComparisonIdentity(attemptHash string, bundle EvidenceBundle) string {
	return hashString(strings.Join([]string{attemptHash, bundle.Hash, bundle.SkillSetHash, bundle.Source.Revision}, "\x00"))
}

// NewRecordID returns a stable idempotency key for one attempted comparison.
func NewRecordID(subject Subject, createdAt time.Time, identity string) string {
	return "shadow-" + hashString(strings.Join([]string{
		subject.JobID, subject.BuildID, subject.TestName, subject.TestSource, subject.JUnitFile, subject.SuiteName, subject.ClassName,
		createdAt.UTC().Format(time.RFC3339Nano), identity,
	}, "\x00"))[:24]
}

// LedgerAttemptHashes returns the exact comparison contracts already attempted.
func LedgerAttemptHashes(path string) (map[string]bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("agent analysis ledger path is required")
	}
	attempts := map[string]bool{}
	err := withLedgerLock(path, func() error {
		ledger, err := loadLedger(path)
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-ledgerRetention)
		for _, attempt := range ledger.Attempts {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, attempt.CreatedAt)
			if parseErr == nil && !createdAt.Before(cutoff) {
				attempts[attempt.Hash] = true
			}
		}
		for _, record := range ledger.Records {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, record.CreatedAt)
			if parseErr == nil && !createdAt.Before(cutoff) {
				attempts[record.AttemptHash] = true
			}
		}
		return nil
	})
	return attempts, err
}

// LedgerContainsAttempt reports whether this exact comparison contract was already attempted.
func LedgerContainsAttempt(path, attemptHash string) (bool, error) {
	if !validSHA256(attemptHash) {
		return false, fmt.Errorf("agent analysis ledger lookup is invalid")
	}
	attempts, err := LedgerAttemptHashes(path)
	return attempts[attemptHash], err
}

// AppendLedger atomically appends or replaces one bounded private record.
func AppendLedger(path string, record ShadowRecord) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("agent analysis ledger path is required")
	}
	createdAt, err := validateShadowRecord(record)
	if err != nil {
		return fmt.Errorf("agent analysis ledger record: %w", err)
	}
	return withLedgerLock(path, func() error {
		ledger, err := loadLedger(path)
		if err != nil {
			return err
		}
		reference := createdAt
		if previous, parseErr := time.Parse(time.RFC3339Nano, ledger.UpdatedAt); parseErr == nil && previous.After(reference) {
			reference = previous
		}
		cutoff := reference.Add(-ledgerRetention)
		kept := ledger.Records[:0]
		for _, existing := range ledger.Records {
			timestamp, parseErr := time.Parse(time.RFC3339Nano, existing.CreatedAt)
			if parseErr == nil && !timestamp.Before(cutoff) {
				kept = append(kept, existing)
			}
		}
		ledger.Records = kept
		attempts := ledger.Attempts[:0]
		attemptReplaced := false
		for _, attempt := range ledger.Attempts {
			timestamp, parseErr := time.Parse(time.RFC3339Nano, attempt.CreatedAt)
			if parseErr != nil || timestamp.Before(cutoff) {
				continue
			}
			if attempt.Hash == record.AttemptHash {
				attempt = AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt}
				attemptReplaced = true
			}
			attempts = append(attempts, attempt)
		}
		if !attemptReplaced {
			attempts = append(attempts, AttemptRecord{Hash: record.AttemptHash, CreatedAt: record.CreatedAt})
		}
		slices.SortFunc(attempts, func(a, b AttemptRecord) int {
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
		if len(attempts) > maxLedgerAttempts {
			attempts = slices.Clone(attempts[len(attempts)-maxLedgerAttempts:])
		}
		ledger.Attempts = attempts
		replaced := false
		for i := range ledger.Records {
			if ledger.Records[i].ID == record.ID {
				ledger.Records[i] = record
				replaced = true
				break
			}
		}
		if !replaced {
			ledger.Records = append(ledger.Records, record)
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
		ledger.UpdatedAt = reference.UTC().Format(time.RFC3339Nano)
		for {
			data, marshalErr := json.MarshalIndent(ledger, "", "  ")
			if marshalErr != nil {
				return marshalErr
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
	})
}

func loadLedger(path string) (ShadowLedger, error) {
	ledger := ShadowLedger{SchemaVersion: LedgerSchemaVersion}
	file, err := os.Open(filepath.Clean(path))
	if os.IsNotExist(err) {
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
	if ledger.SchemaVersion != LedgerSchemaVersion {
		return ShadowLedger{}, fmt.Errorf("unsupported agent analysis ledger schema %d", ledger.SchemaVersion)
	}
	for _, attempt := range ledger.Attempts {
		if !validSHA256(attempt.Hash) {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis attempt hash")
		}
		if _, err := time.Parse(time.RFC3339Nano, attempt.CreatedAt); err != nil {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis attempt time: %w", err)
		}
	}
	for _, record := range ledger.Records {
		if _, err := validateShadowRecord(record); err != nil {
			return ShadowLedger{}, fmt.Errorf("invalid agent analysis ledger record: %w", err)
		}
	}
	return ledger, nil
}

func validateShadowRecord(record ShadowRecord) (time.Time, error) {
	if record.ID == "" || record.CreatedAt == "" || record.Subject.JobID == "" || record.Subject.BuildID == "" || record.Subject.TestName == "" ||
		!validSHA256(record.AttemptHash) || !validSHA256(record.RequestHash) || !validSHA256(record.AuthoritativeHash) ||
		record.ComparisonHash != "" && !validSHA256(record.ComparisonHash) {
		return time.Time{}, fmt.Errorf("record identity is incomplete")
	}
	switch record.Status {
	case ShadowStatusSucceeded, ShadowStatusCleanupPending, ShadowStatusEvidenceFailed, ShadowStatusSetupFailed, ShadowStatusRuntimeFailed, ShadowStatusInvalidResult, ShadowStatusCancelled:
	default:
		return time.Time{}, fmt.Errorf("unsupported shadow status %q", record.Status)
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

func withLedgerLock(path string, fn func() error) error {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0o700); err != nil && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.ENOSYS) {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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
	return fn()
}
