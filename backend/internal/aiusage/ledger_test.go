package aiusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testRecorder(t *testing.T, path string, now time.Time, recent int) *Recorder {
	t.Helper()
	pricing, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.5", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{
		RetentionDays: 2, RecentOperations: recent, Pricing: pricing, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestBeginSeparatesLogicalAndExecutionIDs(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	_, first := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	_, second := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	if first == nil || second == nil || first.usage.LogicalID != second.usage.LogicalID || first.usage.ID == second.usage.ID || first.usage.LogicalID == "request-123" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	_, replay := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", ExecutionID: "execution-456", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	_, sameReplay := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", ExecutionID: "execution-456", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	if replay.usage.ID != sameReplay.usage.ID || replay.usage.ID == "execution-456" {
		t.Fatalf("replay=%+v sameReplay=%+v", replay, sameReplay)
	}
}

func TestOperationRecordsProviderUsage(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(t.Context(), recorder, Metadata{LogicalID: "0011223344556677", ExecutionID: "1111223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 10, ReasoningTokens: 4})
	ObserveModelRequest(ctx, TokenUsage{})
	got := operation.Finish(OutcomeSuccess)
	if got.ModelRequests != 2 || got.ReportedRequests != 1 || got.UnreportedRequests != 1 {
		t.Fatalf("request coverage = %+v", got)
	}
	if got.InputTokens != 100 || got.CachedInputTokens != 20 || got.OutputTokens != 10 || got.ReasoningTokens != 4 {
		t.Fatalf("tokens = %+v", got)
	}
	if got.EstimatedCostNanos == 0 || got.PricingHash == "" {
		t.Fatalf("pricing = %+v", got)
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.Days[0].Totals.ModelRequests != 2 || !snapshot.Days[0].PricingCountsKnown || len(snapshot.RecentOperations) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	dedupe := snapshot.DedupeOperations[got.ID]
	if len(snapshot.DedupeOperations) != 1 || dedupe.Date != "2026-08-03" || len(dedupe.Digest) != 32 {
		t.Fatalf("dedupe operation = %+v", snapshot.DedupeOperations)
	}
}

func TestOperationFinishIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(t.Context(), recorder, Metadata{LogicalID: "0011223344556677", ExecutionID: "1111223344556677", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 10})
	operation.Finish(OutcomeSuccess)
	operation.Finish(OutcomeSuccess)
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 1 || snapshot.Days[0].Totals.InputTokens != 10 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderRejectsConflictingExecutionIDReuse(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	completed := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginAnalyzer, Feature: FeatureFailureAnalysis, StartedAt: completed, CompletedAt: completed, Outcome: OutcomeSuccess, ModelRequests: 1, ReportedRequests: 1, InputTokens: 10})
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginAnalyzer, Feature: FeatureFailureAnalysis, StartedAt: completed, CompletedAt: completed, Outcome: OutcomeSuccess, ModelRequests: 1, ReportedRequests: 1, InputTokens: 20})
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 1 || snapshot.Days[0].Totals.InputTokens != 10 || len(snapshot.RecentOperations) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderPrunesRetentionAndRecentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 1)
	ids := []string{"0000000000000001", "0000000000000002", "0000000000000003"}
	for i, completed := range []time.Time{now.AddDate(0, 0, -2), now.AddDate(0, 0, -1), now} {
		stamp := completed.Format(time.RFC3339Nano)
		recorder.Record(OperationUsage{ID: ids[i], Origin: OriginFetcher, Feature: FeaturePatternAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 2 || snapshot.Days[0].Date != "2026-08-02" || snapshot.Days[1].Date != "2026-08-03" {
		t.Fatalf("days = %+v", snapshot.Days)
	}
	if len(snapshot.RecentOperations) != 1 || snapshot.RecentOperations[0].ID != "0000000000000003" || snapshot.DroppedOperations != 1 {
		t.Fatalf("recent = %+v dropped=%d", snapshot.RecentOperations, snapshot.DroppedOperations)
	}
}

func TestRecorderCanDisableRecentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 0)
	stamp := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 1 || len(snapshot.RecentOperations) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestNewRecorderRejectsMalformedAndNewerLedgers(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(malformed, RecorderOptions{RetentionDays: 30, RecentOperations: 10}); err == nil {
		t.Fatal("expected malformed-ledger error")
	}
	newer := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newer, []byte(`{"version":2,"days":[],"recent_operations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(newer, RecorderOptions{RetentionDays: 30, RecentOperations: 10}); err == nil {
		t.Fatal("expected newer-version error")
	}
	invalidCurrency := filepath.Join(dir, "invalid-currency.json")
	if err := os.WriteFile(invalidCurrency, []byte(`{"version":1,"currency":"123","days":[],"recent_operations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(invalidCurrency, RecorderOptions{RetentionDays: 30, RecentOperations: 10}); err == nil {
		t.Fatal("expected invalid-currency error")
	}
}

func TestRecorderWritesPrivateFile(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "usage.json")
	recorder := testRecorder(t, path, now, 10)
	stamp := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeCacheHit})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := NewRecorder(path, RecorderOptions{RetentionDays: 2, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Snapshot().Days[0].Totals.CacheHits; got != 1 {
		t.Fatalf("cache hits = %d, want 1", got)
	}
}

func TestRecorderWriteFailureIsNonFatal(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	want := errors.New("write failed")
	var logs []string
	recorder, err := NewRecorder("usage.json", RecorderOptions{
		RetentionDays: 2, RecentOperations: 10, Now: func() time.Time { return now },
		Write: func(string, any) error { return want },
		Logf:  func(format string, _ ...any) { logs = append(logs, format) },
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.Format(time.RFC3339Nano)
	got := recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	if got.ID != "0011223344556677" || len(logs) != 1 || recorder.Snapshot().Days[0].Totals.Operations != 1 {
		t.Fatalf("operation=%+v logs=%v snapshot=%+v", got, logs, recorder.Snapshot())
	}
}

func TestRecorderConcurrentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, operation := Begin(t.Context(), recorder, Metadata{Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
			ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 1})
			operation.Finish(OutcomeSuccess)
		}()
	}
	wg.Wait()
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 50 || snapshot.Days[0].Totals.InputTokens != 50 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderPersistsCurrencyAndRejectsChanges(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.json")
	recorder := testRecorder(t, path, now, 10)
	stamp := now.Format(time.RFC3339Nano)
	got := recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess, ReportedRequests: 1, InputTokens: 10})
	if got.Currency != "USD" || recorder.Snapshot().Currency != "USD" {
		t.Fatalf("operation=%+v ledger=%+v", got, recorder.Snapshot())
	}
	eur, err := NewPriceTable(Rates{Currency: "EUR", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(path, RecorderOptions{RetentionDays: 2, RecentOperations: 10, Pricing: eur, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("expected currency-change error")
	}
}

func TestNewRecorderAppliesLoadedPrivacyLimits(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.json")
	pricing, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{RetentionDays: 3, RecentOperations: 3, Pricing: pricing, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i, completed := range []time.Time{now.AddDate(0, 0, -2), now.AddDate(0, 0, -1), now} {
		stamp := completed.Format(time.RFC3339Nano)
		recorder.Record(OperationUsage{ID: fmt.Sprintf("%016x", i+1), Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	}
	loaded, err := NewRecorder(path, RecorderOptions{RetentionDays: 1, RecentOperations: 0, Pricing: pricing, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := loaded.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.Days[0].Date != "2026-08-03" || len(snapshot.RecentOperations) != 0 || len(snapshot.DedupeOperations) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted UsageLedger
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Days) != 1 || len(persisted.RecentOperations) != 0 || len(persisted.DedupeOperations) != 1 || persisted.RetentionDays != 1 {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestRecorderDeduplicatesEntireRetentionWindow(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 0)
	stamp := now.Format(time.RFC3339Nano)
	var first OperationUsage
	for i := 0; i < 1001; i++ {
		operation := recorder.Record(OperationUsage{ID: fmt.Sprintf("%016x", i), Origin: OriginAnalyzer, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess, InputTokens: 1})
		if i == 0 {
			first = operation
		}
	}
	recorder.Record(first)
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 1001 || snapshot.Days[0].Totals.InputTokens != 1001 || len(snapshot.DedupeOperations) != 1001 {
		t.Fatalf("totals=%+v dedupe=%d", snapshot.Days[0].Totals, len(snapshot.DedupeOperations))
	}
}

func TestRecorderCountsRetriedExecutionsSeparately(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, first := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 5})
	firstUsage := first.Finish(OutcomeError)
	ctx, retry := Begin(t.Context(), recorder, Metadata{LogicalID: "request-123", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 7})
	retry.Finish(OutcomeSuccess)
	recorder.Record(firstUsage)
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 2 || snapshot.Days[0].Totals.InputTokens != 12 || len(snapshot.DedupeOperations) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderAllowsCurrencyChangeAfterPricedDataExpires(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.json")
	usd, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{RetentionDays: 3, RecentOperations: 10, Pricing: usd, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	old := now.AddDate(0, 0, -2).Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: old, CompletedAt: old, Outcome: OutcomeSuccess, ReportedRequests: 1, InputTokens: 10})
	eur, err := NewPriceTable(Rates{Currency: "EUR", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewRecorder(path, RecorderOptions{RetentionDays: 1, RecentOperations: 10, Pricing: eur, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := changed.Snapshot(); snapshot.Currency != "EUR" || len(snapshot.Days) != 0 || len(snapshot.DedupeOperations) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestOperationMarksOverflowUnreported(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(t.Context(), recorder, Metadata{LogicalID: "overflow", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: math.MaxInt})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: math.MaxInt})
	got := operation.Finish(OutcomeSuccess)
	if got.ModelRequests != 2 || got.ReportedRequests != 1 || got.UnreportedRequests != 1 || got.InputTokens != int64(math.MaxInt) || !got.UsageInvalid {
		t.Fatalf("operation = %+v", got)
	}
	if got.EstimatedCostNanos != 0 || got.PricingHash != "" {
		t.Fatalf("failed pricing provenance = %+v", got)
	}
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.PricedReportedRequests != 0 || len(snapshot.Days[0].PricingHashes) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestSnapshotExpiresIdleLedgerAndPersistsIt(t *testing.T) {
	current := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.json")
	pricing, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{RetentionDays: 1, RecentOperations: 10, Pricing: pricing, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	stamp := current.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	current = current.AddDate(0, 0, 2)
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 0 || len(snapshot.RecentOperations) != 0 || len(snapshot.DedupeOperations) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted UsageLedger
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Days) != 0 || len(persisted.RecentOperations) != 0 || len(persisted.DedupeOperations) != 0 {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestRecorderReplayDoesNotDisplaceNewerRecentOperation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 1)
	older := now.Add(-time.Minute).Format(time.RFC3339Nano)
	newer := now.Format(time.RFC3339Nano)
	oldUsage := recorder.Record(OperationUsage{ID: "0000000000000001", LogicalID: "1000000000000001", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: older, CompletedAt: older, Outcome: OutcomeError})
	recorder.Record(OperationUsage{ID: "0000000000000002", LogicalID: "1000000000000001", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: newer, CompletedAt: newer, Outcome: OutcomeSuccess})
	recorder.Record(oldUsage)
	snapshot := recorder.Snapshot()
	if len(snapshot.RecentOperations) != 1 || snapshot.RecentOperations[0].ID != "0000000000000002" {
		t.Fatalf("recent operations = %+v", snapshot.RecentOperations)
	}
}
