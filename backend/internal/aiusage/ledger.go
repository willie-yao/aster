package aiusage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	DefaultRetentionDays    = 90
	DefaultRecentOperations = 250
)

// RecorderOptions configure one writer-owned private ledger.
type RecorderOptions struct {
	RetentionDays    int
	RecentOperations int
	Pricing          PriceTable
	Now              func() time.Time
	Write            func(string, any) error
	Logf             func(string, ...any)
}

// Recorder owns one ledger file and serializes its in-process updates.
type Recorder struct {
	mu               sync.Mutex
	path             string
	ledger           UsageLedger
	recentOperations int
	pricing          PriceTable
	now              func() time.Time
	write            func(string, any) error
	logf             func(string, ...any)
}

// NewRecorder loads or creates one private usage ledger.
func NewRecorder(path string, options RecorderOptions) (*Recorder, error) {
	if options.RetentionDays <= 0 {
		options.RetentionDays = DefaultRetentionDays
	}
	if options.RecentOperations < 0 {
		return nil, fmt.Errorf("recent operations must be non-negative")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Write == nil {
		options.Write = statefile.WritePrivateJSONDurable
	}
	if options.Logf == nil {
		options.Logf = log.Printf
	}
	ledger, existed, err := loadLedger(path, options.RetentionDays)
	if err != nil {
		return nil, err
	}
	currency := options.Pricing.Currency()
	if ledger.Currency != "" && !validCurrency(ledger.Currency) {
		return nil, fmt.Errorf("usage ledger currency %q is invalid", ledger.Currency)
	}
	if ledger.Currency == "" {
		ledger.Currency = currency
	}
	recorder := &Recorder{
		path: path, ledger: ledger, recentOperations: options.RecentOperations,
		pricing: options.Pricing, now: options.Now, write: options.Write, logf: options.Logf,
	}
	recorder.pruneLocked()
	recorder.truncateRecentLocked()
	if ledger.Currency != "" && currency != "" && ledger.Currency != currency {
		if recorder.hasRetainedCostLocked() {
			return nil, fmt.Errorf("usage ledger currency %q does not match configured currency %q", ledger.Currency, currency)
		}
		recorder.ledger.Currency = currency
	}
	if existed {
		recorder.ledger.UpdatedAt = recorder.now().UTC().Format(time.RFC3339Nano)
		if err := recorder.write(path, recorder.ledger); err != nil {
			return nil, fmt.Errorf("persist usage ledger limits: %w", err)
		}
	}
	return recorder, nil
}

func loadLedger(path string, retentionDays int) (UsageLedger, bool, error) {
	fresh := UsageLedger{Version: LedgerVersion, RetentionDays: retentionDays, Days: []DailyUsage{}, RecentOperations: []OperationUsage{}, DedupeOperations: map[string]DedupeEntry{}}
	if path == "" {
		return fresh, false, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fresh, false, nil
	}
	if err != nil {
		return UsageLedger{}, false, err
	}
	var ledger UsageLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return UsageLedger{}, false, fmt.Errorf("decode usage ledger: %w", err)
	}
	if ledger.Version < 1 || ledger.Version > LedgerVersion {
		return UsageLedger{}, false, fmt.Errorf("usage ledger version %d is unsupported; current version is %d", ledger.Version, LedgerVersion)
	}
	legacyVersion := ledger.Version
	ledger.Version = LedgerVersion
	ledger.RetentionDays = retentionDays
	if ledger.Days == nil {
		ledger.Days = []DailyUsage{}
	}
	if ledger.RecentOperations == nil {
		ledger.RecentOperations = []OperationUsage{}
	}
	if ledger.DedupeOperations == nil {
		ledger.DedupeOperations = map[string]DedupeEntry{}
		for _, operation := range ledger.RecentOperations {
			ledger.DedupeOperations[operation.ID] = dedupeEntry(operation)
		}
	}
	if legacyVersion >= 2 {
		for i := range ledger.Days {
			if ledger.Days[i].Models == nil && ledger.Days[i].ModelCountsKnown {
				ledger.Days[i].Models = map[string]UsageTotals{}
			}
		}
	}
	return ledger, true, nil
}

// Record prices and persists one completed operation. Persistence failures are
// logged and do not change the caller's AI result.
func (r *Recorder) Record(operation OperationUsage) OperationUsage {
	if r == nil {
		return operation
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	operation = normalizeOperation(operation, r.now())
	if operation.Currency == "" {
		operation.Currency = r.pricing.Currency()
		if operation.Currency == "" {
			operation.Currency = r.ledger.Currency
		}
	}
	if r.ledger.Currency == "" {
		r.ledger.Currency = operation.Currency
	}
	if operation.EstimatedCostNanos > 0 && operation.Currency != r.ledger.Currency {
		r.logf("⚠ AI usage cost currency %q does not match ledger currency %q", operation.Currency, r.ledger.Currency)
		operation.EstimatedCostNanos = 0
	}
	if operation.PricingHash == "" {
		operation.PricingHash = r.pricing.Hash()
	}
	if operation.EstimatedCostNanos == 0 && operation.PricingHash == r.pricing.Hash() && r.pricing.Configured() && operation.ReportedRequests > 0 {
		cost, err := r.pricing.Estimate(TokenUsage{
			Reported:    true,
			InputTokens: boundedInt(operation.InputTokens), CachedInputTokens: boundedInt(operation.CachedInputTokens),
			CacheWriteInputTokens: boundedInt(operation.CacheWriteInputTokens), CacheWriteInputTokensReported: operation.CacheWriteReportedRequests > 0,
			OutputTokens: boundedInt(operation.OutputTokens), ReasoningTokens: boundedInt(operation.ReasoningTokens),
		})
		if err != nil {
			r.logf("⚠ AI usage cost estimate failed: %v", err)
			operation.PricingHash = ""
		} else {
			operation.Currency = r.pricing.Currency()
			operation.EstimatedCostNanos = cost
			if r.pricing.CacheWriteConfigured() {
				operation.CacheWritePricedRequests = operation.CacheWriteReportedRequests
			}
		}
	}
	if !r.recordLocked(operation) {
		return operation
	}
	r.ledger.UpdatedAt = r.now().UTC().Format(time.RFC3339Nano)
	if r.path != "" {
		if err := r.write(r.path, r.ledger); err != nil {
			r.logf("⚠ AI usage ledger write failed: %v", err)
		}
	}
	return operation
}

func (r *Recorder) recordLocked(operation OperationUsage) bool {
	entry := dedupeEntry(operation)
	if existing, ok := r.ledger.DedupeOperations[operation.ID]; ok {
		if existing.Digest != entry.Digest {
			r.logf("⚠ AI usage execution ID %q was reused with different accounting data", operation.ID)
		}
		return false
	}
	r.applyLocked(operation, 1)
	r.ledger.DedupeOperations[operation.ID] = entry
	for i, existing := range r.ledger.RecentOperations {
		if existing.ID == operation.ID {
			r.ledger.RecentOperations = append(r.ledger.RecentOperations[:i], r.ledger.RecentOperations[i+1:]...)
			break
		}
	}
	if r.recentOperations > 0 {
		r.ledger.RecentOperations = append([]OperationUsage{operation}, r.ledger.RecentOperations...)
	}
	r.truncateRecentLocked()
	r.pruneLocked()
	return true
}

func dedupeEntry(operation OperationUsage) DedupeEntry {
	data, _ := json.Marshal(operation)
	digest := sha256.Sum256(data)
	completed, err := time.Parse(time.RFC3339Nano, operation.CompletedAt)
	date := ""
	if err == nil {
		date = completed.UTC().Format(time.DateOnly)
	}
	return DedupeEntry{Date: date, Digest: hex.EncodeToString(digest[:16])}
}

func (r *Recorder) truncateRecentLocked() {
	if r.recentOperations == 0 {
		r.ledger.DroppedOperations += len(r.ledger.RecentOperations)
		r.ledger.RecentOperations = []OperationUsage{}
		return
	}
	sort.Slice(r.ledger.RecentOperations, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339Nano, r.ledger.RecentOperations[i].CompletedAt)
		right, rightErr := time.Parse(time.RFC3339Nano, r.ledger.RecentOperations[j].CompletedAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		return r.ledger.RecentOperations[i].ID < r.ledger.RecentOperations[j].ID
	})
	if len(r.ledger.RecentOperations) > r.recentOperations {
		dropped := len(r.ledger.RecentOperations) - r.recentOperations
		r.ledger.RecentOperations = r.ledger.RecentOperations[:r.recentOperations]
		r.ledger.DroppedOperations += dropped
	}
}

func (r *Recorder) applyLocked(operation OperationUsage, direction int64) {
	completed, err := time.Parse(time.RFC3339Nano, operation.CompletedAt)
	if err != nil {
		return
	}
	date := completed.UTC().Format(time.DateOnly)
	index := sort.Search(len(r.ledger.Days), func(i int) bool { return r.ledger.Days[i].Date >= date })
	if index == len(r.ledger.Days) || r.ledger.Days[index].Date != date {
		if direction < 0 {
			return
		}
		coverageKnown := operationCoverageKnown(operation)
		modelKnown := operationModelKnown(operation)
		r.ledger.Days = append(r.ledger.Days, DailyUsage{})
		copy(r.ledger.Days[index+1:], r.ledger.Days[index:])
		r.ledger.Days[index] = DailyUsage{
			Date: date, Features: map[Feature]UsageTotals{}, Models: map[string]UsageTotals{},
			PricingCountsKnown: true, CoverageCountsKnown: coverageKnown, ModelCountsKnown: modelKnown,
		}
	}
	day := &r.ledger.Days[index]
	if direction > 0 {
		day.CoverageCountsKnown = day.CoverageCountsKnown && operationCoverageKnown(operation)
		day.ModelCountsKnown = day.ModelCountsKnown && operationModelKnown(operation)
	}
	if day.Features == nil {
		day.Features = map[Feature]UsageTotals{}
	}
	if day.Models == nil && day.ModelCountsKnown {
		day.Models = map[string]UsageTotals{}
	}
	if direction > 0 && operation.PricingHash != "" && !slices.Contains(day.PricingHashes, operation.PricingHash) {
		day.PricingHashes = append(day.PricingHashes, operation.PricingHash)
		sort.Strings(day.PricingHashes)
	}
	applyTotals(&day.Totals, operationTotals(operation), direction)
	feature := day.Features[operation.Feature]
	applyTotals(&feature, operationTotals(operation), direction)
	if emptyTotals(feature) {
		delete(day.Features, operation.Feature)
	} else {
		day.Features[operation.Feature] = feature
	}
	if day.ModelCountsKnown {
		model := operationModelKey(operation)
		modelTotals := day.Models[model]
		applyTotals(&modelTotals, operationTotals(operation), direction)
		if emptyTotals(modelTotals) {
			delete(day.Models, model)
		} else {
			day.Models[model] = modelTotals
		}
	}
	if emptyTotals(day.Totals) {
		r.ledger.Days = append(r.ledger.Days[:index], r.ledger.Days[index+1:]...)
	}
}

func (r *Recorder) pruneLocked() {
	cutoff := r.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(r.ledger.RetentionDays - 1)).Format(time.DateOnly)
	first := sort.Search(len(r.ledger.Days), func(i int) bool { return r.ledger.Days[i].Date >= cutoff })
	if first > 0 {
		r.ledger.Days = append([]DailyUsage(nil), r.ledger.Days[first:]...)
	}
	kept := r.ledger.RecentOperations[:0]
	for _, operation := range r.ledger.RecentOperations {
		completed, err := time.Parse(time.RFC3339Nano, operation.CompletedAt)
		if err == nil && completed.UTC().Format(time.DateOnly) >= cutoff {
			kept = append(kept, operation)
		}
	}
	r.ledger.RecentOperations = kept
	for id, entry := range r.ledger.DedupeOperations {
		if entry.Date == "" || entry.Date < cutoff {
			delete(r.ledger.DedupeOperations, id)
		}
	}
}

func (r *Recorder) hasRetainedCostLocked() bool {
	for _, day := range r.ledger.Days {
		if day.Totals.EstimatedCostNanos != 0 {
			return true
		}
	}
	return false
}

// Snapshot enforces retention and returns an independent deterministic copy.
func (r *Recorder) Snapshot() UsageLedger {
	if r == nil {
		return UsageLedger{Version: LedgerVersion, Days: []DailyUsage{}, RecentOperations: []OperationUsage{}, DedupeOperations: map[string]DedupeEntry{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	beforeDays, beforeRecent, beforeDedupe := len(r.ledger.Days), len(r.ledger.RecentOperations), len(r.ledger.DedupeOperations)
	r.pruneLocked()
	r.truncateRecentLocked()
	if beforeDays != len(r.ledger.Days) || beforeRecent != len(r.ledger.RecentOperations) || beforeDedupe != len(r.ledger.DedupeOperations) {
		r.ledger.UpdatedAt = r.now().UTC().Format(time.RFC3339Nano)
		if r.path != "" {
			if err := r.write(r.path, r.ledger); err != nil {
				r.logf("⚠ AI usage ledger retention write failed: %v", err)
			}
		}
	}
	data, _ := json.Marshal(r.ledger)
	var out UsageLedger
	_ = json.Unmarshal(data, &out)
	return out
}

func normalizeOperation(operation OperationUsage, now time.Time) OperationUsage {
	operation.ID = safeOperationID(operation.ID)
	if operation.ID == "" {
		operation.ID = randomID()
	}
	operation.LogicalID = safeOperationID(operation.LogicalID)
	if operation.LogicalID == "" {
		operation.LogicalID = operation.ID
	}
	if !validFeature(operation.Feature) {
		operation.Feature = FeatureUnknown
	}
	if !validOrigin(operation.Origin) {
		operation.Origin = OriginUnknown
	}
	if !validOutcome(operation.Outcome) {
		operation.Outcome = OutcomeError
	}
	operation.ModelFingerprint = safeFingerprint(operation.ModelFingerprint)
	operation.Model = safeModelID(operation.Model)
	if !validUsageSource(operation.UsageSource) {
		operation.UsageSource = ""
	}
	if operation.ModelGatewayExcluded {
		operation.ExternalUnmetered = true
		if operation.UsageSource == "" {
			operation.UsageSource = UsageSourceModelGateway
		}
	} else if operation.ExternalUnmetered && operation.UsageSource == "" {
		operation.UsageSource = UsageSourceExternalRuntime
	}
	if !validCurrency(operation.Currency) {
		operation.Currency = ""
	}
	operation.ModelRequests = max(operation.ModelRequests, 0)
	operation.ReportedRequests = max(operation.ReportedRequests, 0)
	operation.CacheWriteReportedRequests = min(max(operation.CacheWriteReportedRequests, 0), operation.ReportedRequests)
	operation.CacheWriteUnreportedRequests = min(max(operation.CacheWriteUnreportedRequests, 0), operation.ReportedRequests-operation.CacheWriteReportedRequests)
	operation.CacheWritePricedRequests = min(max(operation.CacheWritePricedRequests, 0), operation.CacheWriteReportedRequests)
	operation.UnreportedRequests = max(operation.UnreportedRequests, 0)
	operation.InvalidUsageRequests = min(max(operation.InvalidUsageRequests, 0), operation.UnreportedRequests)
	operation.InputTokens = max(operation.InputTokens, 0)
	operation.CachedInputTokens = max(operation.CachedInputTokens, 0)
	if operation.CachedInputTokens > operation.InputTokens {
		operation.CachedInputTokens = operation.InputTokens
	}
	operation.CacheWriteInputTokens = max(operation.CacheWriteInputTokens, 0)
	if operation.CacheWriteInputTokens > operation.InputTokens-operation.CachedInputTokens {
		operation.CacheWriteInputTokens = operation.InputTokens - operation.CachedInputTokens
	}
	operation.OutputTokens = max(operation.OutputTokens, 0)
	operation.ReasoningTokens = max(operation.ReasoningTokens, 0)
	if operation.ReasoningTokens > operation.OutputTokens {
		operation.ReasoningTokens = operation.OutputTokens
	}
	operation.EstimatedCostNanos = max(operation.EstimatedCostNanos, 0)
	if _, err := time.Parse(time.RFC3339Nano, operation.StartedAt); err != nil {
		operation.StartedAt = now.UTC().Format(time.RFC3339Nano)
	}
	if _, err := time.Parse(time.RFC3339Nano, operation.CompletedAt); err != nil {
		operation.CompletedAt = operation.StartedAt
	}
	return operation
}

func operationTotals(operation OperationUsage) UsageTotals {
	totals := UsageTotals{
		Operations: 1, ModelRequests: operation.ModelRequests,
		ReportedRequests: operation.ReportedRequests, CacheWriteReportedRequests: operation.CacheWriteReportedRequests,
		CacheWritePricedRequests: operation.CacheWritePricedRequests, CacheWriteUnreportedRequests: operation.CacheWriteUnreportedRequests,
		InvalidUsageRequests: operation.InvalidUsageRequests, UnreportedRequests: operation.UnreportedRequests,
		InputTokens: operation.InputTokens, CachedInputTokens: operation.CachedInputTokens,
		CacheWriteInputTokens: operation.CacheWriteInputTokens,
		OutputTokens:          operation.OutputTokens, ReasoningTokens: operation.ReasoningTokens,
		EstimatedCostNanos: operation.EstimatedCostNanos,
	}
	if operation.PricingHash != "" && operation.Currency != "" {
		totals.PricedReportedRequests = operation.ReportedRequests
	}
	if operation.Outcome == OutcomeCacheHit {
		totals.CacheHits = 1
	}
	if operation.Outcome == OutcomeSuppressed {
		totals.SuppressedOperations = 1
	}
	if operation.CooldownRetry {
		totals.CooldownRetries = 1
	}
	if operation.Outcome == OutcomeError || operation.Outcome == OutcomeCancelled || operation.Outcome == OutcomeUnavailable {
		totals.Failures = 1
	}
	if operation.ExternalUnmetered {
		totals.ExternalUnmeteredOperations = 1
	}
	if operation.ModelGatewayExcluded {
		totals.ModelGatewayExcludedOperations = 1
	}
	return totals
}

func applyTotals(target *UsageTotals, value UsageTotals, direction int64) {
	target.Operations += int(int64(value.Operations) * direction)
	target.CacheHits += int(int64(value.CacheHits) * direction)
	target.SuppressedOperations += int(int64(value.SuppressedOperations) * direction)
	target.CooldownRetries += int(int64(value.CooldownRetries) * direction)
	target.Failures += int(int64(value.Failures) * direction)
	target.ExternalUnmeteredOperations += int(int64(value.ExternalUnmeteredOperations) * direction)
	target.ModelGatewayExcludedOperations += int(int64(value.ModelGatewayExcludedOperations) * direction)
	target.ModelRequests += int(int64(value.ModelRequests) * direction)
	target.ReportedRequests += int(int64(value.ReportedRequests) * direction)
	target.PricedReportedRequests += int(int64(value.PricedReportedRequests) * direction)
	target.CacheWriteReportedRequests += int(int64(value.CacheWriteReportedRequests) * direction)
	target.CacheWritePricedRequests += int(int64(value.CacheWritePricedRequests) * direction)
	target.CacheWriteUnreportedRequests += int(int64(value.CacheWriteUnreportedRequests) * direction)
	target.InvalidUsageRequests += int(int64(value.InvalidUsageRequests) * direction)
	target.UnreportedRequests += int(int64(value.UnreportedRequests) * direction)
	target.InputTokens += value.InputTokens * direction
	target.CachedInputTokens += value.CachedInputTokens * direction
	target.CacheWriteInputTokens += value.CacheWriteInputTokens * direction
	target.OutputTokens += value.OutputTokens * direction
	target.ReasoningTokens += value.ReasoningTokens * direction
	target.EstimatedCostNanos += value.EstimatedCostNanos * direction
}

func operationModelKey(operation OperationUsage) string {
	if operation.MixedModels {
		return "mixed"
	}
	if operation.Model != "" {
		return operation.Model
	}
	if operation.ModelFingerprint != "" {
		return "fingerprint:" + operation.ModelFingerprint
	}
	return "unknown"
}

func operationCoverageKnown(operation OperationUsage) bool {
	return operation.CoverageCountsKnown || operation.ModelRequests == 0
}

func operationModelKnown(operation OperationUsage) bool {
	return operation.MixedModels || operation.Model != "" || operation.ModelFingerprint != "" ||
		(operation.ModelRequests == 0 && !operation.ExternalUnmetered)
}

func emptyTotals(t UsageTotals) bool {
	return t == (UsageTotals{})
}

func boundedInt(value int64) int {
	maxInt := int64(^uint(0) >> 1)
	if value < 0 {
		return 0
	}
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}
