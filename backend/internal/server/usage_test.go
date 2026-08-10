package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

func TestAIUsageHandlerAuthenticatedMergedAndFiltered(t *testing.T) {
	dataDir := t.TempDir()
	fetcher := aiusage.UsageLedger{Version: 1, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date: "2026-08-02", Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: 100, OutputTokens: 10, EstimatedCostNanos: 1200},
		Features:      map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: 100, OutputTokens: 10, EstimatedCostNanos: 1200}},
		PricingHashes: []string{"price-a"},
	}}, RecentOperations: []aiusage.OperationUsage{{ID: "1", Feature: aiusage.FeatureFailureAnalysis, CompletedAt: "2026-08-02T12:00:00Z"}}}
	serverLedger := aiusage.UsageLedger{Version: 1, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date: "2026-08-03", Totals: aiusage.UsageTotals{Operations: 1, ExternalUnmeteredOperations: 1, ModelRequests: 1, UnreportedRequests: 1},
		Features: map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureAnalysisChat: {Operations: 1, ExternalUnmeteredOperations: 1, ModelRequests: 1, UnreportedRequests: 1}},
	}}, RecentOperations: []aiusage.OperationUsage{{ID: "2", Feature: aiusage.FeatureAnalysisChat, CompletedAt: "2026-08-03T12:00:00Z"}}}
	if err := statefile.WritePrivateJSONDurable(filepath.Join(dataDir, output.AIUsageFetcherFilename), fetcher); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WritePrivateJSONDurable(filepath.Join(dataDir, output.AIUsageServerFilename), serverLedger); err != nil {
		t.Fatal(err)
	}
	pricing, err := aiusage.NewPriceTable(aiusage.Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev", AIUsageEnabled: true, AIUsageModel: "provider/model", AIUsagePricingRule: "USD input=1 output=2 per million tokens", AIUsagePricing: pricing})
	if err != nil {
		t.Fatal(err)
	}

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/ai-usage", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", unauth.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/ai-usage?start=2026-08-01&end=2026-08-03", nil)
	req.Header.Set("Authorization", "ok")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("status=%d headers=%v", res.Code, res.Header())
	}
	var got usageReport
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.SelectedModel != "provider/model" || !got.PricingConfigured || !strings.Contains(got.PricingRule, "USD") {
		t.Fatalf("usage metadata = %+v", got)
	}
	if got.Currency != "USD" || got.Coverage.Status != "partial" || got.Totals.Operations != 2 || got.Totals.InputTokens != 100 || got.Totals.EstimatedCostNanos != "1200" || len(got.Daily) != 3 || len(got.RecentOperations) != 2 || got.CurrentRateEstimatedCostNanos == "" {
		t.Fatalf("report = %+v", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/ai-usage?start=2026-08-01&end=2026-08-03&feature=analysis_chat", nil)
	req.Header.Set("Authorization", "ok")
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Totals.Operations != 1 || len(got.Features) != 1 || got.Features[0].Feature != aiusage.FeatureAnalysisChat {
		t.Fatalf("filtered report = %+v", got)
	}

	downloadReq := httptest.NewRequest(http.MethodGet, "/api/ai-usage/download?start=2026-08-01&end=2026-08-03", nil)
	downloadReq.Header.Set("Authorization", "ok")
	downloadRes := httptest.NewRecorder()
	h.ServeHTTP(downloadRes, downloadReq)
	var downloaded usageReport
	if downloadRes.Code != http.StatusOK || !strings.Contains(downloadRes.Header().Get("Content-Disposition"), "attachment") || json.NewDecoder(downloadRes.Body).Decode(&downloaded) != nil {
		t.Fatalf("download status=%d headers=%v body=%s", downloadRes.Code, downloadRes.Header(), downloadRes.Body.String())
	}
	if downloaded.Version != aiusage.LedgerVersion || downloaded.Totals.Operations != 2 || downloaded.Coverage.Status != "partial" {
		t.Fatalf("downloaded report = %+v", downloaded)
	}

	publicRes := httptest.NewRecorder()
	h.ServeHTTP(publicRes, httptest.NewRequest(http.MethodGet, "/data/"+output.AIUsageFetcherFilename, nil))
	if publicRes.Code != http.StatusNotFound {
		t.Fatalf("public ledger status = %d, want 404", publicRes.Code)
	}

	capsReq := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	capsRes := httptest.NewRecorder()
	h.ServeHTTP(capsRes, capsReq)
	var caps Capabilities
	if err := json.NewDecoder(capsRes.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Features.AIUsage {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestBuildUsageReportCoverageStatesAndModels(t *testing.T) {
	day := aiusage.DailyUsage{
		Date: "2026-08-10", PricingCountsKnown: true, CoverageCountsKnown: true, ModelCountsKnown: true,
		Totals: aiusage.UsageTotals{
			Operations: 4, ModelRequests: 2, ReportedRequests: 2, PricedReportedRequests: 1,
			CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1, CacheWriteUnreportedRequests: 1,
			ExternalUnmeteredOperations: 1, ModelGatewayExcludedOperations: 1,
			InputTokens: 100, CachedInputTokens: 30, CacheWriteInputTokens: 10, OutputTokens: 20,
		},
		Features: map[aiusage.Feature]aiusage.UsageTotals{
			aiusage.FeatureFailureAnalysis: {Operations: 2, ModelRequests: 2, ReportedRequests: 2, PricedReportedRequests: 1, CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1, CacheWriteUnreportedRequests: 1},
			aiusage.FeatureFixPreview:      {Operations: 1, ModelGatewayExcludedOperations: 1},
			aiusage.FeatureFixCritique:     {Operations: 1, ExternalUnmeteredOperations: 1},
		},
		Models: map[string]aiusage.UsageTotals{
			"claude-sonnet-4.6": {Operations: 2, ModelRequests: 2, ReportedRequests: 2, InputTokens: 100, OutputTokens: 20},
			"gateway-model":     {Operations: 1, ModelGatewayExcludedOperations: 1},
			"unknown":           {Operations: 1, ExternalUnmeteredOperations: 1},
		},
		PricingHashes: []string{"price-a"},
	}
	start, _ := time.Parse(time.DateOnly, "2026-08-10")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Currency: "USD", Days: []aiusage.DailyUsage{day}}}, start, start, nil, start, true)
	for _, state := range []string{"cache_write_unreported", "external_unmetered", "model_gateway_excluded", "pricing_added_after_operation"} {
		if !slices.Contains(report.Coverage.States, state) {
			t.Fatalf("coverage states = %v, missing %s", report.Coverage.States, state)
		}
	}
	if report.Coverage.Status != "partial" || report.Coverage.PricingAddedAfterRequests != 1 || report.ModelCoverage != "complete" || len(report.Models) != 3 ||
		report.Totals.CacheWriteInputTokens != 10 || report.Totals.ModelGatewayExcludedOperations != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportRejectsAggregateOverflow(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-09")
	ledgers := []aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Days: []aiusage.DailyUsage{
		{Date: "2026-08-09", CoverageCountsKnown: true, ModelCountsKnown: true, Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: math.MaxInt64}},
		{Date: "2026-08-10", CoverageCountsKnown: true, ModelCountsKnown: true, Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, InputTokens: 1}},
	}}}
	report := buildUsageReport(ledgers, start, start.AddDate(0, 0, 1), nil, start, false)
	if !report.Coverage.AggregateOverflow || report.Totals.InputTokens != math.MaxInt64 || report.Totals.InputTokens < 0 || !slices.Contains(report.Coverage.States, "aggregate_overflow") || report.PricingCoverage != "unknown" {
		t.Fatalf("overflow report = %+v", report)
	}
}

func TestBuildUsageReportFullyPricedProviderUsage(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-10")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date: "2026-08-10", PricingCountsKnown: true, CoverageCountsKnown: true, ModelCountsKnown: true,
		Totals:   aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, PricedReportedRequests: 1, CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1},
		Features: map[aiusage.Feature]aiusage.UsageTotals{}, Models: map[string]aiusage.UsageTotals{"model": {Operations: 1, ModelRequests: 1, ReportedRequests: 1}},
	}}}}, start, start, nil, start, true)
	if report.Coverage.Status != "complete" || !slices.Contains(report.Coverage.States, "fully_priced_provider_reported") {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
}

func TestAIUsageHandlerValidationAndMissing(t *testing.T) {
	dataDir := t.TempDir()
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev", AIUsageEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/api/ai-usage?start=bad", "/api/ai-usage?start=2026-08-03&end=2026-08-01",
		"/api/ai-usage?start=2025-01-01&end=2026-08-03", "/api/ai-usage?feature=bogus",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "ok")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, res.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/ai-usage", nil)
	req.Header.Set("Authorization", "ok")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", res.Code)
	}
}

func TestBuildUsageReportDefaultsToThirtyDays(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/api/ai-usage", nil)
	start, end, _, err := parseUsageQuery(req, now)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.DateOnly) != "2026-07-05" || end.Format(time.DateOnly) != "2026-08-03" {
		t.Fatalf("range=%s..%s", start, end)
	}
}

func TestBuildUsageReportScopesProvenanceToFilters(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	ledgers := []aiusage.UsageLedger{
		{Version: 1, Currency: "USD", Days: []aiusage.DailyUsage{{Date: "2026-08-02", Totals: aiusage.UsageTotals{Operations: 2, ModelRequests: 2, ReportedRequests: 2}, Features: map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 1, ModelRequests: 1, ReportedRequests: 1}, aiusage.FeatureAnalysisChat: {Operations: 1, ModelRequests: 1, ReportedRequests: 1}}, PricingHashes: []string{"failure-price"}}}, RecentOperations: []aiusage.OperationUsage{{ID: "chat", Feature: aiusage.FeatureAnalysisChat, Currency: "USD", PricingHash: "chat-price", CompletedAt: "2026-08-02T12:00:00Z"}}},
		{Version: 1, Currency: "EUR", Days: []aiusage.DailyUsage{{Date: "2026-07-01", Totals: aiusage.UsageTotals{Operations: 1}, Features: map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 1}}, PricingHashes: []string{"eur-price"}}}},
	}
	report := buildUsageReport(ledgers, start, end, map[aiusage.Feature]bool{aiusage.FeatureAnalysisChat: true}, end, false)
	if report.Currency != "USD" || report.MixedCurrency || report.MixedPricing {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportMarksFilteredLegacyPricingUnknown(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: 1, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date:   "2026-08-02",
		Totals: aiusage.UsageTotals{Operations: 2, ModelRequests: 2, ReportedRequests: 2},
		Features: map[aiusage.Feature]aiusage.UsageTotals{
			aiusage.FeatureFailureAnalysis: {Operations: 1, ModelRequests: 1, ReportedRequests: 1},
			aiusage.FeatureAnalysisChat:    {Operations: 1, ModelRequests: 1, ReportedRequests: 1},
		},
		PricingHashes: []string{"zero-price"},
	}}}}, start, end, map[aiusage.Feature]bool{aiusage.FeatureAnalysisChat: true}, end, false)
	if report.PricingCoverage != "unknown" || report.RangePriced {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportSeparatesCoverageFromPricing(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Days: []aiusage.DailyUsage{{
		Date: "2026-08-02", CoverageCountsKnown: true, ModelCountsKnown: true,
		Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1, InputTokens: 10},
	}}}}, start, end, nil, end, false)
	if report.Coverage.Status != "partial" || !slices.Contains(report.Coverage.States, "pricing_unavailable") || report.RangePriced || report.PricingCoverage != "unavailable" {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportIncludesSuppressedOperations(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-10")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Days: []aiusage.DailyUsage{{
		Date:                "2026-08-10",
		CoverageCountsKnown: true, ModelCountsKnown: true,
		Totals: aiusage.UsageTotals{Operations: 2, SuppressedOperations: 1, CooldownRetries: 1},
		Features: map[aiusage.Feature]aiusage.UsageTotals{
			aiusage.FeaturePatternAnalysis: {Operations: 2, SuppressedOperations: 1, CooldownRetries: 1},
		},
	}}}}, start, start, nil, start, false)
	if report.Totals.Operations != 2 || report.Totals.SuppressedOperations != 1 || report.Totals.CooldownRetries != 1 ||
		len(report.Daily) != 1 || report.Daily[0].Totals.SuppressedOperations != 1 || report.Daily[0].Totals.CooldownRetries != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportIgnoresUnappliedPricingHash(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date:                "2026-08-02",
		PricingCountsKnown:  true,
		CoverageCountsKnown: true,
		ModelCountsKnown:    true,
		Totals:              aiusage.UsageTotals{Operations: 2, ModelRequests: 1, ReportedRequests: 1},
		PricingHashes:       []string{"unused-price"},
	}}}}, start, end, nil, end, false)
	if report.PricingCoverage != "unavailable" || report.RangePriced {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportRetainsPricingCoverageCounts(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Currency: "USD", Days: []aiusage.DailyUsage{{
		Date:                "2026-08-02",
		PricingCountsKnown:  true,
		CoverageCountsKnown: true,
		ModelCountsKnown:    true,
		Totals:              aiusage.UsageTotals{Operations: 2, ModelRequests: 2, ReportedRequests: 2, PricedReportedRequests: 1, EstimatedCostNanos: 100},
		Features: map[aiusage.Feature]aiusage.UsageTotals{
			aiusage.FeatureFailureAnalysis: {Operations: 1, ModelRequests: 1, ReportedRequests: 1, PricedReportedRequests: 1, EstimatedCostNanos: 100},
			aiusage.FeatureAnalysisChat:    {Operations: 1, ModelRequests: 1, ReportedRequests: 1},
		},
		PricingHashes: []string{"price"},
	}}}}, start, end, nil, end, false)
	if report.PricingCoverage != "partial" || report.RangePriced || report.Totals.PricedReportedRequests != 1 || report.Coverage.PricedReportedRequests != 1 {
		t.Fatalf("report = %+v", report)
	}
	var pricedDay *usageReportDay
	for index := range report.Daily {
		if report.Daily[index].Date == "2026-08-02" {
			pricedDay = &report.Daily[index]
		}
	}
	if len(report.Daily) != 3 || pricedDay == nil || pricedDay.Totals.PricedReportedRequests != 1 {
		t.Fatalf("daily = %+v", report.Daily)
	}
	pricedByFeature := map[aiusage.Feature]int{}
	for _, feature := range report.Features {
		pricedByFeature[feature.Feature] = feature.Totals.PricedReportedRequests
	}
	if len(report.Features) != 2 || pricedByFeature[aiusage.FeatureFailureAnalysis] != 1 || pricedByFeature[aiusage.FeatureAnalysisChat] != 0 {
		t.Fatalf("features = %+v", report.Features)
	}
}

func TestBuildUsageReportMarksMixedLegacyPricingUnknown(t *testing.T) {
	start, _ := time.Parse(time.DateOnly, "2026-08-01")
	end, _ := time.Parse(time.DateOnly, "2026-08-03")
	report := buildUsageReport([]aiusage.UsageLedger{{Version: 1, Currency: "USD", Days: []aiusage.DailyUsage{
		{Date: "2026-08-01", Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, EstimatedCostNanos: 100}, PricingHashes: []string{"legacy-price"}},
		{Date: "2026-08-02", PricingCountsKnown: true, Totals: aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, PricedReportedRequests: 1, EstimatedCostNanos: 100}, PricingHashes: []string{"current-price"}},
	}}}, start, end, nil, end, false)
	if report.PricingCoverage != "unknown" || report.RangePriced {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildUsageReportHistoricalDaysAndCurrentRepricing(t *testing.T) {
	pricing, err := aiusage.NewPriceTable(aiusage.Rates{
		Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1", CacheWriteInputPerMillion: "1.25", OutputPerMillion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	day := func(date string, totals aiusage.UsageTotals, features map[aiusage.Feature]aiusage.UsageTotals) aiusage.DailyUsage {
		return aiusage.DailyUsage{Date: date, Totals: totals, Features: features, PricingCountsKnown: true, CoverageCountsKnown: true, ModelCountsKnown: true}
	}
	ledgers := []aiusage.UsageLedger{{Version: aiusage.LedgerVersion, Currency: "USD", Days: []aiusage.DailyUsage{
		day("2026-08-04", aiusage.UsageTotals{Operations: 10, ModelRequests: 10, ReportedRequests: 10, PricedReportedRequests: 10, CacheWriteReportedRequests: 10, CacheWritePricedRequests: 10, InputTokens: 1_000_000, CachedInputTokens: 100_000, CacheWriteInputTokens: 200_000, OutputTokens: 50_000, EstimatedCostNanos: 1_000_000_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 10, ModelRequests: 10, ReportedRequests: 10, InputTokens: 1_000_000, OutputTokens: 50_000}}),
		day("2026-08-05", aiusage.UsageTotals{Operations: 20, CacheHits: 18, ModelRequests: 2, ReportedRequests: 2, PricedReportedRequests: 2, CacheWriteReportedRequests: 2, CacheWritePricedRequests: 2, InputTokens: 100_000, CachedInputTokens: 80_000, OutputTokens: 5_000, EstimatedCostNanos: 40_000_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 20, CacheHits: 18, ModelRequests: 2}}),
		day("2026-08-06", aiusage.UsageTotals{Operations: 6, Failures: 5, ModelRequests: 6, ReportedRequests: 6, PricedReportedRequests: 6, CacheWriteUnreportedRequests: 6, InputTokens: 11_800, OutputTokens: 1_400, EstimatedCostNanos: 20_000_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeaturePatternAnalysis: {Operations: 6, Failures: 5, ModelRequests: 6, ReportedRequests: 6, InputTokens: 11_800, OutputTokens: 1_400}}),
		day("2026-08-07", aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, CacheWriteReportedRequests: 1, InputTokens: 30_000, OutputTokens: 2_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureAnalysisChat: {Operations: 1, ModelRequests: 1, ReportedRequests: 1}}),
		day("2026-08-08", aiusage.UsageTotals{Operations: 1, ExternalUnmeteredOperations: 1}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureSourceInvestigation: {Operations: 1, ExternalUnmeteredOperations: 1}}),
		day("2026-08-09", aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, PricedReportedRequests: 1, CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1, InputTokens: 50_000, CacheWriteInputTokens: 10_000, OutputTokens: 1_000, EstimatedCostNanos: 60_000_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFixPreview: {Operations: 1, ModelRequests: 1, ReportedRequests: 1, CacheWriteInputTokens: 10_000}}),
		day("2026-08-10", aiusage.UsageTotals{Operations: 2, ModelRequests: 2, ReportedRequests: 1, PricedReportedRequests: 1, CacheWriteReportedRequests: 1, UnreportedRequests: 1, InputTokens: 20_000, OutputTokens: 1_000, EstimatedCostNanos: 25_000_000}, map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 2, ModelRequests: 2, ReportedRequests: 1, UnreportedRequests: 1}}),
	}}}
	start := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	generated := time.Date(2026, time.August, 10, 14, 0, 0, 0, time.UTC)
	report := buildUsageReport(ledgers, start, generated.Truncate(24*time.Hour), nil, generated, true, pricing)
	if len(report.Daily) != 8 || report.Daily[0].Date != "2026-08-03" || report.Daily[0].HasUsage || report.Daily[7].Date != "2026-08-10" || !report.Daily[7].CurrentPartialUTC {
		t.Fatalf("daily range = %+v", report.Daily)
	}
	if report.Daily[4].RecordedCostStatus != "unavailable" || report.Daily[7].RecordedCostStatus != "partial" || report.Daily[6].CurrentRateEstimatedCostNanos == "" {
		t.Fatalf("cost statuses = %+v", report.Daily)
	}
	if len(report.Daily[3].Features) != 1 || report.Daily[3].Features[0].Feature != aiusage.FeaturePatternAnalysis {
		t.Fatalf("pattern day features = %+v", report.Daily[3].Features)
	}
	var reconciled aiusage.UsageTotals
	for _, value := range report.Daily {
		parsedCost, err := strconv.ParseInt(value.Totals.EstimatedCostNanos, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if !aiusage.AddTotals(&reconciled, aiusage.UsageTotals{
			Operations: value.Totals.Operations, ModelRequests: value.Totals.ModelRequests, ReportedRequests: value.Totals.ReportedRequests,
			InputTokens: value.Totals.InputTokens, CachedInputTokens: value.Totals.CachedInputTokens, CacheWriteInputTokens: value.Totals.CacheWriteInputTokens,
			OutputTokens: value.Totals.OutputTokens, EstimatedCostNanos: parsedCost,
		}) {
			t.Fatal("fixture totals overflowed")
		}
	}
	if reconciled.Operations != report.Totals.Operations || reconciled.ModelRequests != report.Totals.ModelRequests || reconciled.InputTokens != report.Totals.InputTokens || strconv.FormatInt(reconciled.EstimatedCostNanos, 10) != report.Totals.EstimatedCostNanos {
		t.Fatalf("daily totals do not reconcile: daily=%+v report=%+v", reconciled, report.Totals)
	}
}

func TestBuildUsageReportKeepsMixedCurrenciesSafe(t *testing.T) {
	pricing, err := aiusage.NewPriceTable(aiusage.Rates{Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	day := func(currency string, cost int64) aiusage.UsageLedger {
		return aiusage.UsageLedger{Version: aiusage.LedgerVersion, Currency: currency, Days: []aiusage.DailyUsage{{
			Date: "2026-08-10", PricingCountsKnown: true, CoverageCountsKnown: true, ModelCountsKnown: true,
			Totals:   aiusage.UsageTotals{Operations: 1, ModelRequests: 1, ReportedRequests: 1, PricedReportedRequests: 1, CacheWriteReportedRequests: 1, CacheWritePricedRequests: 1, InputTokens: 10, OutputTokens: 2, EstimatedCostNanos: cost},
			Features: map[aiusage.Feature]aiusage.UsageTotals{aiusage.FeatureFailureAnalysis: {Operations: 1, EstimatedCostNanos: cost}},
		}}}
	}
	date := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	report := buildUsageReport([]aiusage.UsageLedger{day("USD", 100), day("EUR", 200)}, date, date, nil, date, true, pricing)
	if !report.MixedCurrency || report.RecordedCostStatus != "mixed_currency" || report.Totals.EstimatedCostNanos != "0" || report.Daily[0].RecordedCostStatus != "mixed_currency" || report.Daily[0].RecordedCurrency != "" || report.Daily[0].Totals.EstimatedCostNanos != "0" || report.CurrentRateStatus != "available" {
		t.Fatalf("mixed-currency report = %+v", report)
	}
}
