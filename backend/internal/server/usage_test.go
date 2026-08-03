package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev", AIUsageEnabled: true})
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
	if got.Currency != "USD" || got.Coverage.Status != "partial" || got.Totals.Operations != 2 || got.Totals.InputTokens != 100 || got.Totals.EstimatedCostNanos != "1200" || len(got.Daily) != 2 || len(got.RecentOperations) != 2 {
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
	report := buildUsageReport(ledgers, start, end, map[aiusage.Feature]bool{aiusage.FeatureAnalysisChat: true}, end)
	if report.Currency != "USD" || report.MixedCurrency || report.MixedPricing || report.Coverage.Status != "complete" {
		t.Fatalf("report = %+v", report)
	}
}
