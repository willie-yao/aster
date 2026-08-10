package aiusage

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOperationAccountsCacheReadWriteAndModelProvenance(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	pricing, err := NewPriceTable(Rates{
		Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1",
		CacheWriteInputPerMillion: "1.25", OutputPerMillion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder("", RecorderOptions{RetentionDays: 30, RecentOperations: 10, Pricing: pricing, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, operation := Begin(context.Background(), recorder, Metadata{
		LogicalID: "operation", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now,
	})
	ObserveModelRequestWithModel(ctx, TokenUsage{
		Reported: true, InputTokens: 1000, CachedInputTokens: 400,
		CacheWriteInputTokens: 100, CacheWriteInputTokensReported: true,
		OutputTokens: 250, ReasoningTokens: 50,
	}, "claude-sonnet-4.6", "0123456789abcdef")
	got := operation.Finish(OutcomeSuccess)
	if got.Model != "claude-sonnet-4.6" || got.ModelFingerprint != "0123456789abcdef" || got.UsageSource != UsageSourceProviderResponse ||
		got.CacheWriteReportedRequests != 1 || got.CacheWriteUnreportedRequests != 0 || got.CacheWritePricedRequests != 1 ||
		got.CacheWriteInputTokens != 100 || got.EstimatedCostNanos != 1_165_000 || !got.CoverageCountsKnown {
		t.Fatalf("operation = %+v", got)
	}
	snapshot := recorder.Snapshot()
	if snapshot.Version != LedgerVersion || len(snapshot.Days) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	day := snapshot.Days[0]
	if !day.CoverageCountsKnown || !day.ModelCountsKnown || day.Totals.CacheWriteInputTokens != 100 ||
		day.Totals.CacheWriteReportedRequests != 1 || day.Totals.CacheWritePricedRequests != 1 ||
		day.Models["claude-sonnet-4.6"].ModelRequests != 1 {
		t.Fatalf("day = %+v", day)
	}
}

func TestOperationDistinguishesMissingAndPresentZeroCacheWriteUsage(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(context.Background(), recorder, Metadata{LogicalID: "operation", Origin: OriginFetcher, Feature: FeaturePatternAnalysis, StartedAt: now})
	ObserveModelRequestWithModel(ctx, TokenUsage{Reported: true, InputTokens: 10}, "model", "0123456789abcdef")
	ObserveModelRequestWithModel(ctx, TokenUsage{Reported: true, InputTokens: 10, CacheWriteInputTokensReported: true}, "model", "0123456789abcdef")
	got := operation.Finish(OutcomeSuccess)
	if got.ReportedRequests != 2 || got.CacheWriteUnreportedRequests != 1 || got.CacheWriteReportedRequests != 1 || got.CacheWriteInputTokens != 0 {
		t.Fatalf("operation = %+v", got)
	}
}

func TestOperationRejectsInvalidAndOverflowingUsage(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(context.Background(), recorder, Metadata{LogicalID: "operation", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 2, CachedInputTokens: 1, CacheWriteInputTokens: 2, CacheWriteInputTokensReported: true})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: math.MaxInt})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 1})
	got := operation.Finish(OutcomeError)
	if got.ModelRequests != 3 || got.ReportedRequests != 1 || got.UnreportedRequests != 2 || got.InvalidUsageRequests != 2 ||
		!got.UsageInvalid || got.InputTokens != int64(math.MaxInt) {
		t.Fatalf("operation = %+v", got)
	}
}

func TestModelGatewayExclusionIsSpecific(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(context.Background(), recorder, Metadata{LogicalID: "operation", Origin: OriginServer, Feature: FeatureFixPreview, StartedAt: now})
	MarkModelGatewayExcluded(ctx, "gateway-model")
	got := operation.Finish(OutcomeSuccess)
	if !got.ExternalUnmetered || !got.ModelGatewayExcluded || got.UsageSource != UsageSourceModelGateway || got.Model != "gateway-model" || !got.CoverageCountsKnown {
		t.Fatalf("operation = %+v", got)
	}
	totals := recorder.Snapshot().Days[0].Totals
	if totals.ExternalUnmeteredOperations != 1 || totals.ModelGatewayExcludedOperations != 1 || totals.ModelRequests != 0 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestModelProvenanceRejectsEndpointLikeValues(t *testing.T) {
	if got := safeModelID("https://gateway.internal/v1"); got != "" {
		t.Fatalf("endpoint-like model was retained: %q", got)
	}
	if got := safeModelID("provider/claude-sonnet-4.6"); got != "provider/claude-sonnet-4.6" {
		t.Fatalf("safe model = %q", got)
	}
}

func TestLegacyVersionOneLedgerMigratesWithoutDataLoss(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := UsageLedger{
		Version: 1, Currency: "USD", RetentionDays: 90,
		Days: []DailyUsage{{
			Date: "2026-08-10", Totals: UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: 25, EstimatedCostNanos: 100},
			Features: map[Feature]UsageTotals{FeatureFailureAnalysis: {Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: 25, EstimatedCostNanos: 100}},
		}},
		RecentOperations: []OperationUsage{{ID: "0011223344556677", LogicalID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now.Format(time.RFC3339Nano), CompletedAt: now.Format(time.RFC3339Nano), Outcome: OutcomeSuccess, ModelRequests: 1, ReportedRequests: 1, InputTokens: 25, EstimatedCostNanos: 100, Currency: "USD", PricingHash: "legacy"}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{RetentionDays: 90, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	got := recorder.Snapshot()
	if got.Version != LedgerVersion || len(got.Days) != 1 || got.Days[0].Totals.InputTokens != 25 || got.Days[0].Totals.EstimatedCostNanos != 100 ||
		got.Days[0].CoverageCountsKnown || got.Days[0].ModelCountsKnown || len(got.RecentOperations) != 1 {
		t.Fatalf("migrated ledger = %+v", got)
	}
}
