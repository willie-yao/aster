// Package aiusage records private, content-free model usage for dashboard AI
// operations.
package aiusage

import "time"

const LedgerVersion = 1

type Feature string

const (
	FeatureFailureAnalysis     Feature = "failure_analysis"
	FeaturePatternAnalysis     Feature = "pattern_analysis"
	FeatureAnalysisChat        Feature = "analysis_chat"
	FeatureIssueDraft          Feature = "issue_draft"
	FeatureFixPreview          Feature = "fix_preview"
	FeatureFixCritique         Feature = "fix_critique"
	FeaturePRTemplate          Feature = "pr_template"
	FeatureSourceInvestigation Feature = "source_investigation"
	FeatureUnknown             Feature = "unknown"
)

type Origin string

const (
	OriginFetcher  Origin = "fetcher"
	OriginAnalyzer Origin = "analyzer"
	OriginServer   Origin = "server"
	OriginUnknown  Origin = "unknown"
)

type Outcome string

const (
	OutcomeSuccess     Outcome = "success"
	OutcomeError       Outcome = "error"
	OutcomeCacheHit    Outcome = "cache_hit"
	OutcomeCancelled   Outcome = "cancelled"
	OutcomeUnavailable Outcome = "unavailable"
)

// TokenUsage is provider-reported usage for one logical model request.
// Reported distinguishes a missing usage object from a present zero value.
type TokenUsage struct {
	Reported          bool `json:"reported,omitempty"`
	InputTokens       int  `json:"input_tokens,omitempty"`
	CachedInputTokens int  `json:"cached_input_tokens,omitempty"`
	OutputTokens      int  `json:"output_tokens,omitempty"`
	ReasoningTokens   int  `json:"reasoning_tokens,omitempty"`
}

// Correlation identifies a dashboard subject without carrying model content.
type Correlation struct {
	JobID    string `json:"job_id,omitempty"`
	BuildID  string `json:"build_id,omitempty"`
	TestName string `json:"test_name,omitempty"`
}

// Metadata identifies one accounting operation.
type Metadata struct {
	LogicalID        string
	ExecutionID      string
	Origin           Origin
	Feature          Feature
	ModelFingerprint string
	Correlation      Correlation
	StartedAt        time.Time
}

// UsageTotals is the additive accounting summary shared by days and features.
type UsageTotals struct {
	Operations                  int   `json:"operations,omitempty"`
	CacheHits                   int   `json:"cache_hits,omitempty"`
	Failures                    int   `json:"failures,omitempty"`
	ExternalUnmeteredOperations int   `json:"external_unmetered_operations,omitempty"`
	ModelRequests               int   `json:"model_requests,omitempty"`
	ReportedRequests            int   `json:"reported_requests,omitempty"`
	UnreportedRequests          int   `json:"unreported_requests,omitempty"`
	InputTokens                 int64 `json:"input_tokens,omitempty"`
	CachedInputTokens           int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens                int64 `json:"output_tokens,omitempty"`
	ReasoningTokens             int64 `json:"reasoning_tokens,omitempty"`
	EstimatedCostNanos          int64 `json:"estimated_cost_nanos,omitempty"`
}

// OperationUsage is one completed, content-free accounting operation.
type OperationUsage struct {
	ID                 string      `json:"id"`
	LogicalID          string      `json:"logical_id,omitempty"`
	Origin             Origin      `json:"origin"`
	Feature            Feature     `json:"feature"`
	StartedAt          string      `json:"started_at"`
	CompletedAt        string      `json:"completed_at"`
	Outcome            Outcome     `json:"outcome"`
	ModelFingerprint   string      `json:"model_fingerprint,omitempty"`
	Currency           string      `json:"currency,omitempty"`
	PricingHash        string      `json:"pricing_hash,omitempty"`
	ModelRequests      int         `json:"model_requests,omitempty"`
	ReportedRequests   int         `json:"reported_requests,omitempty"`
	UnreportedRequests int         `json:"unreported_requests,omitempty"`
	InputTokens        int64       `json:"input_tokens,omitempty"`
	CachedInputTokens  int64       `json:"cached_input_tokens,omitempty"`
	OutputTokens       int64       `json:"output_tokens,omitempty"`
	ReasoningTokens    int64       `json:"reasoning_tokens,omitempty"`
	EstimatedCostNanos int64       `json:"estimated_cost_nanos,omitempty"`
	ExternalUnmetered  bool        `json:"external_unmetered,omitempty"`
	UsageInvalid       bool        `json:"usage_invalid,omitempty"`
	Correlation        Correlation `json:"correlation,omitempty"`
}

// DailyUsage is one UTC day of totals and feature breakdowns.
type DailyUsage struct {
	Date          string                  `json:"date"`
	Totals        UsageTotals             `json:"totals"`
	Features      map[Feature]UsageTotals `json:"features"`
	PricingHashes []string                `json:"pricing_hashes,omitempty"`
}

// DedupeEntry is the minimal state needed to ignore exact persistence replays.
type DedupeEntry struct {
	Date   string `json:"date"`
	Digest string `json:"digest"`
}

// UsageLedger is one writer-owned private usage snapshot.
type UsageLedger struct {
	Version           int                    `json:"version"`
	UpdatedAt         string                 `json:"updated_at"`
	Currency          string                 `json:"currency,omitempty"`
	RetentionDays     int                    `json:"retention_days"`
	DroppedOperations int                    `json:"dropped_operations,omitempty"`
	Days              []DailyUsage           `json:"days"`
	RecentOperations  []OperationUsage       `json:"recent_operations"`
	DedupeOperations  map[string]DedupeEntry `json:"dedupe_operations,omitempty"`
}

func validFeature(value Feature) bool {
	switch value {
	case FeatureFailureAnalysis, FeaturePatternAnalysis, FeatureAnalysisChat,
		FeatureIssueDraft, FeatureFixPreview, FeatureFixCritique, FeaturePRTemplate,
		FeatureSourceInvestigation, FeatureUnknown:
		return true
	default:
		return false
	}
}

func validOrigin(value Origin) bool {
	switch value {
	case OriginFetcher, OriginAnalyzer, OriginServer, OriginUnknown:
		return true
	default:
		return false
	}
}

func validOutcome(value Outcome) bool {
	switch value {
	case OutcomeSuccess, OutcomeError, OutcomeCacheHit, OutcomeCancelled, OutcomeUnavailable:
		return true
	default:
		return false
	}
}

// ParseFeature validates one API feature filter.
func ParseFeature(value string) (Feature, bool) {
	feature := Feature(value)
	return feature, validFeature(feature) && feature != FeatureUnknown
}
