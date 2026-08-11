package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	patternFailureDiagnosticsVersion = 1
	maxPatternDiagnosticEntries      = 500
	maxPatternDiagnosticCacheBytes   = 64 << 20
	maxPatternDiagnosticCount        = 4096
)

var patternDiagnosticJobRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,256}$`)

var knownPatternDiagnosticCodes = map[string]bool{
	"candidate_scan_truncated":      true,
	"canonical_json":                true,
	"confidence":                    true,
	"conflicting_valid_contracts":   true,
	"contract_validation":           true,
	"duplicate_field":               true,
	"empty_output":                  true,
	"field_types":                   true,
	"invalid_json":                  true,
	"missing_message":               true,
	"no_contract":                   true,
	"non_systemic_contract":         true,
	"prow_job_context":              true,
	"remediation_target":            true,
	"remediation_target_limit":      true,
	"remediation_targets":           true,
	"repair_no_contract":            true,
	"required_fields":               true,
	"response_too_large":            true,
	"shared_builds":                 true,
	"summary":                       true,
	"systemic_contract":             true,
	"target_source_missing":         true,
	"target_source_unread":          true,
	"trailing_incomplete_json":      true,
	"trailing_json":                 true,
	"unsafe_conversion_remediation": true,
}

// PatternFailureDiagnostics contains content-free recurring-pattern validation metadata.
type PatternFailureDiagnostics struct {
	Stage                     string `json:"stage,omitempty"`
	ValidationCategory        string `json:"validation_category,omitempty"`
	ValidationCode            string `json:"validation_code,omitempty"`
	CandidateCount            int    `json:"candidate_count,omitempty"`
	ValidCount                int    `json:"valid_count,omitempty"`
	ContractLikeRejectedCount int    `json:"contract_like_rejected_count,omitempty"`
	IncompleteCount           int    `json:"incomplete_count,omitempty"`
	RepairStage               string `json:"repair_stage,omitempty"`
	RepairValidationCode      string `json:"repair_validation_code,omitempty"`
	RepairCount               int    `json:"repair_count,omitempty"`
}

// PatternFailureDiagnosticRecord is one sanitized exact-input cooldown result.
type PatternFailureDiagnosticRecord struct {
	Identity   string                 `json:"identity"`
	JobID      string                 `json:"job_id,omitempty"`
	Category   PatternFailureCategory `json:"category"`
	FailedAt   time.Time              `json:"failed_at"`
	RetryAfter time.Time              `json:"retry_after"`
	PatternFailureDiagnostics
}

// PatternFailureDiagnosticsSnapshot is the private bounded cooldown diagnostic view.
type PatternFailureDiagnosticsSnapshot struct {
	Version     int                              `json:"version"`
	GeneratedAt time.Time                        `json:"generated_at"`
	Entries     []PatternFailureDiagnosticRecord `json:"entries"`
}

type patternClassifiedError struct {
	cause    error
	category PatternFailureCategory
}

func (e *patternClassifiedError) Error() string { return e.cause.Error() }
func (e *patternClassifiedError) Unwrap() error { return e.cause }

type patternFailureDiagnosticsRecorder struct {
	value PatternFailureDiagnostics
}

func (r *patternFailureDiagnosticsRecorder) recordValidation(stage string, stats patternParseStats, err error) {
	if r == nil || err == nil {
		return
	}
	category := patternValidationCategoryOf(err)
	if category == "" {
		return
	}
	candidate := diagnosticsFromValidation(stage, category, patternValidationIssueOf(err), stats)
	current := patternValidationCategory(r.value.ValidationCategory)
	if current == "" || patternDiagnosticValidationRank(category) > patternDiagnosticValidationRank(current) {
		r.value.Stage = candidate.Stage
		r.value.ValidationCategory = candidate.ValidationCategory
		r.value.ValidationCode = candidate.ValidationCode
		r.value.CandidateCount = candidate.CandidateCount
		r.value.ValidCount = candidate.ValidCount
		r.value.ContractLikeRejectedCount = candidate.ContractLikeRejectedCount
		r.value.IncompleteCount = candidate.IncompleteCount
	}
}

func (r *patternFailureDiagnosticsRecorder) beginRepair(stage string) {
	if r == nil {
		return
	}
	r.value.RepairCount++
	r.value.RepairStage = sanitizePatternDiagnosticStage(stage)
	r.value.RepairValidationCode = ""
}

func (r *patternFailureDiagnosticsRecorder) recordRepair(stage string, stats patternParseStats, err error) {
	if r == nil || err == nil {
		return
	}
	r.value.RepairStage = sanitizePatternDiagnosticStage(stage)
	r.value.RepairValidationCode = sanitizePatternDiagnosticCode(patternValidationIssueOf(err))
	if r.value.ValidationCategory == "" {
		r.recordValidation(stage, stats, err)
	}
}

func (r *patternFailureDiagnosticsRecorder) snapshot() PatternFailureDiagnostics {
	if r == nil {
		return PatternFailureDiagnostics{}
	}
	return sanitizePatternFailureDiagnostics(r.value)
}

func patternDiagnosticValidationRank(category patternValidationCategory) int {
	switch category {
	case patternValidationBuilds:
		return 4
	case patternValidationSchema:
		return 3
	case patternValidationJSON:
		return 2
	case patternValidationAmbiguous:
		return 1
	default:
		return 0
	}
}

func diagnosticsFromValidation(stage string, category patternValidationCategory, code string, stats patternParseStats) PatternFailureDiagnostics {
	return sanitizePatternFailureDiagnostics(PatternFailureDiagnostics{
		Stage: stage, ValidationCategory: string(category), ValidationCode: code,
		CandidateCount: stats.CandidateCount, ValidCount: stats.ValidCount,
		ContractLikeRejectedCount: stats.ContractLikeRejectedCount, IncompleteCount: stats.IncompleteCount,
	})
}

func sanitizePatternFailureDiagnostics(value PatternFailureDiagnostics) PatternFailureDiagnostics {
	value.Stage = sanitizePatternDiagnosticStage(value.Stage)
	value.RepairStage = sanitizePatternDiagnosticStage(value.RepairStage)
	value.ValidationCategory = sanitizePatternValidationCategory(value.ValidationCategory)
	value.ValidationCode = sanitizePatternDiagnosticCode(value.ValidationCode)
	value.RepairValidationCode = sanitizePatternDiagnosticCode(value.RepairValidationCode)
	value.CandidateCount = sanitizePatternDiagnosticCount(value.CandidateCount)
	value.ValidCount = sanitizePatternDiagnosticCount(value.ValidCount)
	value.ContractLikeRejectedCount = sanitizePatternDiagnosticCount(value.ContractLikeRejectedCount)
	value.IncompleteCount = sanitizePatternDiagnosticCount(value.IncompleteCount)
	value.RepairCount = sanitizePatternDiagnosticCount(value.RepairCount)
	if value.ValidationCategory == "" {
		value.Stage = ""
		value.ValidationCode = ""
		value.CandidateCount = 0
		value.ValidCount = 0
		value.ContractLikeRejectedCount = 0
		value.IncompleteCount = 0
	}
	if value.RepairCount == 0 {
		value.RepairStage = ""
		value.RepairValidationCode = ""
	}
	return value
}

func sanitizePatternDiagnosticStage(value string) string {
	switch strings.TrimSpace(value) {
	case "tool_free", "grounded", "extraction", "post_validation", "validation", "ambiguity":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizePatternValidationCategory(value string) string {
	category := patternValidationCategory(strings.TrimSpace(value))
	switch category {
	case patternValidationJSON, patternValidationMissing, patternValidationSchema, patternValidationBuilds, patternValidationAmbiguous:
		return string(category)
	default:
		return ""
	}
}

func sanitizePatternDiagnosticCode(value string) string {
	value = strings.TrimSpace(value)
	if knownPatternDiagnosticCodes[value] {
		return value
	}
	return ""
}

func sanitizePatternDiagnosticCount(value int) int {
	if value < 0 || value > maxPatternDiagnosticCount {
		return 0
	}
	return value
}

func patternFailureCategoryWithDiagnostics(err error, diagnostics PatternFailureDiagnostics) PatternFailureCategory {
	category := PatternFailureCategoryOf(err)
	terminal := patternValidationCategory(sanitizePatternValidationCategory(string(category)))
	if terminal == "" {
		return category
	}
	recorded := patternValidationCategory(sanitizePatternValidationCategory(diagnostics.ValidationCategory))
	if recorded != "" && patternDiagnosticValidationRank(recorded) > patternDiagnosticValidationRank(terminal) {
		return PatternFailureCategory(recorded)
	}
	return category
}

// ReadPatternFailureDiagnostics reads sanitized deterministic failure metadata from the private AI cache.
func ReadPatternFailureDiagnostics(path string, now time.Time) (PatternFailureDiagnosticsSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return PatternFailureDiagnosticsSnapshot{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPatternDiagnosticCacheBytes+1))
	if err != nil {
		return PatternFailureDiagnosticsSnapshot{}, err
	}
	if len(data) > maxPatternDiagnosticCacheBytes {
		return PatternFailureDiagnosticsSnapshot{}, fmt.Errorf("AI cache exceeds %d bytes", maxPatternDiagnosticCacheBytes)
	}
	var entries map[string]CacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return PatternFailureDiagnosticsSnapshot{}, fmt.Errorf("decode AI cache: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result := PatternFailureDiagnosticsSnapshot{Version: patternFailureDiagnosticsVersion, GeneratedAt: now, Entries: []PatternFailureDiagnosticRecord{}}
	for key, entry := range entries {
		if !strings.HasPrefix(key, "pattern-failure:") || entry.Key != key || !validCacheEntryTime(now, entry.CreatedAt) {
			continue
		}
		var failure patternFailureCacheData
		if json.Unmarshal(entry.Data, &failure) != nil || !validPatternFailureCacheData(failure, now) {
			continue
		}
		jobID := strings.TrimSpace(failure.JobID)
		if !patternDiagnosticJobRE.MatchString(jobID) {
			jobID = ""
		}
		record := PatternFailureDiagnosticRecord{
			Identity: strings.TrimPrefix(key, "pattern-failure:"), JobID: jobID,
			Category: failure.Category, FailedAt: failure.FailedAt, RetryAfter: failure.RetryAfter,
			PatternFailureDiagnostics: sanitizePatternFailureDiagnostics(failure.PatternFailureDiagnostics),
		}
		result.Entries = append(result.Entries, record)
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if !result.Entries[i].FailedAt.Equal(result.Entries[j].FailedAt) {
			return result.Entries[i].FailedAt.After(result.Entries[j].FailedAt)
		}
		return result.Entries[i].Identity < result.Entries[j].Identity
	})
	if len(result.Entries) > maxPatternDiagnosticEntries {
		result.Entries = result.Entries[:maxPatternDiagnosticEntries]
	}
	return result, nil
}
