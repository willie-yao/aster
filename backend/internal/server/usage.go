package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/output"
)

const maxAIUsageFileBytes = 64 << 20

type usageReport struct {
	Version                       int                      `json:"version"`
	GeneratedAt                   string                   `json:"generated_at"`
	Range                         usageReportRange         `json:"range"`
	Currency                      string                   `json:"currency,omitempty"`
	MixedCurrency                 bool                     `json:"mixed_currency,omitempty"`
	MixedPricing                  bool                     `json:"mixed_pricing,omitempty"`
	Coverage                      usageReportCoverage      `json:"coverage"`
	Totals                        usageReportTotals        `json:"totals"`
	Daily                         []usageReportDay         `json:"daily"`
	Features                      []usageReportFeature     `json:"features"`
	Models                        []usageReportModel       `json:"models,omitempty"`
	ModelCoverage                 string                   `json:"model_coverage"`
	RecentOperations              []aiusage.OperationUsage `json:"recent_operations"`
	SelectedModel                 string                   `json:"selected_model,omitempty"`
	PricingRule                   string                   `json:"pricing_rule,omitempty"`
	PricingConfigured             bool                     `json:"pricing_configured"`
	RangePriced                   bool                     `json:"range_priced"`
	PricingCoverage               string                   `json:"pricing_coverage"`
	RecordedCostStatus            string                   `json:"recorded_cost_status"`
	CurrentRateStatus             string                   `json:"current_rate_status"`
	CurrentRateCurrency           string                   `json:"current_rate_currency,omitempty"`
	CurrentRateEstimatedCostNanos string                   `json:"current_rate_estimated_cost_nanos,omitempty"`
}

type usageReportRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type usageReportCoverage struct {
	Status                         string   `json:"status"`
	States                         []string `json:"states"`
	ModelRequests                  int      `json:"model_requests"`
	ReportedRequests               int      `json:"reported_requests"`
	PricedReportedRequests         int      `json:"priced_reported_requests"`
	CacheWriteReportedRequests     int      `json:"cache_write_reported_requests"`
	CacheWritePricedRequests       int      `json:"cache_write_priced_requests"`
	CacheWriteUnreportedRequests   int      `json:"cache_write_unreported_requests"`
	InvalidUsageRequests           int      `json:"invalid_usage_requests"`
	UnreportedRequests             int      `json:"unreported_requests"`
	ExternalUnmeteredOperations    int      `json:"external_unmetered_operations"`
	ModelGatewayExcludedOperations int      `json:"model_gateway_excluded_operations"`
	PricingAddedAfterRequests      int      `json:"pricing_added_after_requests"`
	LegacyCoverageUnknown          bool     `json:"legacy_coverage_unknown"`
	AggregateOverflow              bool     `json:"aggregate_overflow"`
}

type usageReportTotals struct {
	Operations                     int    `json:"operations"`
	CacheHits                      int    `json:"cache_hits"`
	SuppressedOperations           int    `json:"suppressed_operations"`
	CooldownRetries                int    `json:"cooldown_retries"`
	Failures                       int    `json:"failures"`
	ExternalUnmeteredOperations    int    `json:"external_unmetered_operations"`
	ModelGatewayExcludedOperations int    `json:"model_gateway_excluded_operations"`
	ModelRequests                  int    `json:"model_requests"`
	ReportedRequests               int    `json:"reported_requests"`
	PricedReportedRequests         int    `json:"priced_reported_requests"`
	CacheWriteReportedRequests     int    `json:"cache_write_reported_requests"`
	CacheWritePricedRequests       int    `json:"cache_write_priced_requests"`
	CacheWriteUnreportedRequests   int    `json:"cache_write_unreported_requests"`
	InvalidUsageRequests           int    `json:"invalid_usage_requests"`
	UnreportedRequests             int    `json:"unreported_requests"`
	InputTokens                    int64  `json:"input_tokens"`
	CachedInputTokens              int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens          int64  `json:"cache_write_input_tokens"`
	OutputTokens                   int64  `json:"output_tokens"`
	ReasoningTokens                int64  `json:"reasoning_tokens"`
	EstimatedCostNanos             string `json:"estimated_cost_nanos"`
}

type usageReportDay struct {
	Date                          string               `json:"date"`
	Totals                        usageReportTotals    `json:"totals"`
	Features                      []usageReportFeature `json:"features"`
	Coverage                      usageReportCoverage  `json:"coverage"`
	HasUsage                      bool                 `json:"has_usage"`
	CurrentPartialUTC             bool                 `json:"current_partial_utc"`
	RecordedCostStatus            string               `json:"recorded_cost_status"`
	RecordedCurrency              string               `json:"recorded_currency,omitempty"`
	CurrentRateStatus             string               `json:"current_rate_status"`
	CurrentRateCurrency           string               `json:"current_rate_currency,omitempty"`
	CurrentRateEstimatedCostNanos string               `json:"current_rate_estimated_cost_nanos,omitempty"`
}

type usageReportFeature struct {
	Feature aiusage.Feature   `json:"feature"`
	Totals  usageReportTotals `json:"totals"`
}

type usageReportModel struct {
	Model  string            `json:"model"`
	Totals usageReportTotals `json:"totals"`
}

func aiUsageHandler(dataDir string, attachment bool, now func() time.Time, model, pricingRule string, pricing aiusage.PriceTable) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.SetPrivateResponseHeaders(w.Header())
		start, end, features, err := parseUsageQuery(r, now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ledgers, err := readUsageLedgers(dataDir)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "AI usage unavailable", http.StatusNotFound)
				return
			}
			log.Printf("AI usage: %v", err)
			http.Error(w, "AI usage unavailable", http.StatusInternalServerError)
			return
		}
		pricingConfigured := strings.TrimSpace(pricingRule) != ""
		report := buildUsageReport(ledgers, start, end, features, now().UTC(), pricingConfigured, pricing)
		report.SelectedModel = strings.TrimSpace(model)
		report.PricingRule = strings.TrimSpace(pricingRule)
		report.PricingConfigured = pricingConfigured
		w.Header().Set("Content-Type", "application/json")
		if attachment {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ai-usage-%s-to-%s.json"`, report.Range.Start, report.Range.End))
		}
		_ = json.NewEncoder(w).Encode(report)
	})
}

func parseUsageQuery(r *http.Request, now time.Time) (time.Time, time.Time, map[aiusage.Feature]bool, error) {
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -29)
	var err error
	if value := strings.TrimSpace(r.URL.Query().Get("start")); value != "" {
		start, err = time.Parse(time.DateOnly, value)
		if err != nil {
			return time.Time{}, time.Time{}, nil, fmt.Errorf("start must use YYYY-MM-DD")
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("end")); value != "" {
		end, err = time.Parse(time.DateOnly, value)
		if err != nil {
			return time.Time{}, time.Time{}, nil, fmt.Errorf("end must use YYYY-MM-DD")
		}
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("start must not be after end")
	}
	if end.Sub(start) > 365*24*time.Hour {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("usage range must not exceed 366 days")
	}
	features := map[aiusage.Feature]bool{}
	for _, value := range r.URL.Query()["feature"] {
		feature, ok := aiusage.ParseFeature(strings.TrimSpace(value))
		if !ok {
			return time.Time{}, time.Time{}, nil, fmt.Errorf("unknown AI usage feature %q", value)
		}
		features[feature] = true
	}
	return start, end, features, nil
}

func readUsageLedgers(dataDir string) ([]aiusage.UsageLedger, error) {
	var ledgers []aiusage.UsageLedger
	missing := 0
	for _, name := range []string{output.AIUsageFetcherFilename, output.AIUsageServerFilename} {
		ledger, err := readUsageLedger(filepath.Join(dataDir, name))
		if os.IsNotExist(err) {
			missing++
			continue
		}
		if err != nil {
			return nil, err
		}
		ledgers = append(ledgers, ledger)
	}
	if missing == 2 {
		return nil, os.ErrNotExist
	}
	return ledgers, nil
}

func readUsageLedger(path string) (aiusage.UsageLedger, error) {
	file, err := os.Open(path)
	if err != nil {
		return aiusage.UsageLedger{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAIUsageFileBytes+1))
	if err != nil {
		return aiusage.UsageLedger{}, err
	}
	if len(data) > maxAIUsageFileBytes {
		return aiusage.UsageLedger{}, fmt.Errorf("usage file exceeds %d bytes", maxAIUsageFileBytes)
	}
	var ledger aiusage.UsageLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return aiusage.UsageLedger{}, fmt.Errorf("decode usage file: %w", err)
	}
	if ledger.Version < 1 || ledger.Version > aiusage.LedgerVersion {
		return aiusage.UsageLedger{}, fmt.Errorf("usage version %d is unsupported; current version is %d", ledger.Version, aiusage.LedgerVersion)
	}
	return ledger, nil
}

func buildUsageReport(ledgers []aiusage.UsageLedger, start, end time.Time, featureFilter map[aiusage.Feature]bool, generatedAt time.Time, pricingConfigured bool, priceTables ...aiusage.PriceTable) usageReport {
	var pricing aiusage.PriceTable
	if len(priceTables) > 0 {
		pricing = priceTables[0]
	}
	dayTotals := map[string]aiusage.UsageTotals{}
	dayFeatureTotals := map[string]map[aiusage.Feature]aiusage.UsageTotals{}
	dayCurrencies := map[string]map[string]bool{}
	dayPricingUnknown := map[string]bool{}
	dayCoverageUnknown := map[string]bool{}
	dayAggregateOverflow := map[string]bool{}
	featureTotals := map[aiusage.Feature]aiusage.UsageTotals{}
	modelTotals := map[string]aiusage.UsageTotals{}
	currencies := map[string]bool{}
	pricingHashes := map[string]bool{}
	pricingCountsUnknown := false
	coverageCountsUnknown := false
	modelCountsUnknown := false
	aggregateOverflow := false
	var recent []aiusage.OperationUsage
	for _, ledger := range ledgers {
		for _, day := range ledger.Days {
			date, err := time.Parse(time.DateOnly, day.Date)
			if err != nil || date.Before(start) || date.After(end) {
				continue
			}
			if len(featureFilter) == 0 {
				if !day.CoverageCountsKnown && day.Totals.Operations > 0 {
					coverageCountsUnknown = true
					dayCoverageUnknown[day.Date] = true
				}
				if !day.ModelCountsKnown && day.Totals.Operations > 0 {
					modelCountsUnknown = true
				}
				if !day.PricingCountsKnown && day.Totals.ReportedRequests > 0 && (day.Totals.EstimatedCostNanos > 0 || len(day.PricingHashes) > 0) {
					pricingCountsUnknown = true
					dayPricingUnknown[day.Date] = true
				}
				if day.Totals.Operations > 0 && ledger.Currency != "" {
					currencies[ledger.Currency] = true
					if dayCurrencies[day.Date] == nil {
						dayCurrencies[day.Date] = map[string]bool{}
					}
					dayCurrencies[day.Date][ledger.Currency] = true
				}
				for _, hash := range day.PricingHashes {
					if hash != "" {
						pricingHashes[hash] = true
					}
				}
				totals := dayTotals[day.Date]
				if addUsageTotals(&totals, day.Totals) {
					dayTotals[day.Date] = totals
				} else {
					aggregateOverflow = true
					dayAggregateOverflow[day.Date] = true
				}
				for model, values := range day.Models {
					modelTotal := modelTotals[model]
					if addUsageTotals(&modelTotal, values) {
						modelTotals[model] = modelTotal
					} else {
						aggregateOverflow = true
					}
				}
			}
			for feature, values := range day.Features {
				if len(featureFilter) > 0 && !featureFilter[feature] {
					continue
				}
				featureTotal := featureTotals[feature]
				if addUsageTotals(&featureTotal, values) {
					featureTotals[feature] = featureTotal
				} else {
					aggregateOverflow = true
				}
				if dayFeatureTotals[day.Date] == nil {
					dayFeatureTotals[day.Date] = map[aiusage.Feature]aiusage.UsageTotals{}
				}
				dayFeature := dayFeatureTotals[day.Date][feature]
				if addUsageTotals(&dayFeature, values) {
					dayFeatureTotals[day.Date][feature] = dayFeature
				} else {
					aggregateOverflow = true
					dayAggregateOverflow[day.Date] = true
				}
				if len(featureFilter) > 0 {
					if !day.CoverageCountsKnown && values.Operations > 0 {
						coverageCountsUnknown = true
						dayCoverageUnknown[day.Date] = true
					}
					if !day.PricingCountsKnown && values.ReportedRequests > 0 && (values.EstimatedCostNanos > 0 || len(day.PricingHashes) > 0) {
						pricingCountsUnknown = true
						dayPricingUnknown[day.Date] = true
					}
					if values.Operations > 0 && ledger.Currency != "" {
						currencies[ledger.Currency] = true
						if dayCurrencies[day.Date] == nil {
							dayCurrencies[day.Date] = map[string]bool{}
						}
						dayCurrencies[day.Date][ledger.Currency] = true
					}
					dayTotal := dayTotals[day.Date]
					if addUsageTotals(&dayTotal, values) {
						dayTotals[day.Date] = dayTotal
					} else {
						aggregateOverflow = true
						dayAggregateOverflow[day.Date] = true
					}
				}
			}
		}
		for _, operation := range ledger.RecentOperations {
			completed, err := time.Parse(time.RFC3339Nano, operation.CompletedAt)
			if err != nil || completed.Before(start) || completed.After(end.Add(24*time.Hour-time.Nanosecond)) {
				continue
			}
			if len(featureFilter) > 0 && !featureFilter[operation.Feature] {
				continue
			}
			recent = append(recent, operation)
			if len(featureFilter) > 0 {
				if operation.Currency != "" {
					currencies[operation.Currency] = true
				}
				if operation.PricingHash != "" {
					pricingHashes[operation.PricingHash] = true
				}
			}
		}
	}
	var totals aiusage.UsageTotals
	days := make([]usageReportDay, 0, int(end.Sub(start)/(24*time.Hour))+1)
	for dateCursor := start; !dateCursor.After(end); dateCursor = dateCursor.AddDate(0, 0, 1) {
		date := dateCursor.Format(time.DateOnly)
		values := dayTotals[date]
		if !addUsageTotals(&totals, values) {
			aggregateOverflow = true
		}
		dayCoverage := reportCoverage(values, dayCoverageUnknown[date], dayAggregateOverflow[date], pricingConfigured)
		currentRateStatus, currentRateCurrency, currentRateCost := currentRateEstimate(pricing, values, dayCoverage)
		recordedCurrency := ""
		if len(dayCurrencies[date]) == 1 {
			for value := range dayCurrencies[date] {
				recordedCurrency = value
			}
		}
		days = append(days, usageReportDay{
			Date: date, Totals: reportTotalsWithCost(values, len(dayCurrencies[date]) <= 1), Features: reportFeatureRows(dayFeatureTotals[date], len(dayCurrencies[date]) <= 1), Coverage: dayCoverage,
			HasUsage: values.Operations > 0, CurrentPartialUTC: date == generatedAt.UTC().Format(time.DateOnly),
			RecordedCostStatus: recordedCostStatus(values, len(dayCurrencies[date]), dayPricingUnknown[date], dayCoverageUnknown[date], dayAggregateOverflow[date]),
			RecordedCurrency:   recordedCurrency,
			CurrentRateStatus:  currentRateStatus, CurrentRateCurrency: currentRateCurrency, CurrentRateEstimatedCostNanos: currentRateCost,
		})
	}
	mixedCurrency := len(currencies) > 1
	features := reportFeatureRows(featureTotals, !mixedCurrency)
	models := make([]usageReportModel, 0, len(modelTotals))
	for model, values := range modelTotals {
		models = append(models, usageReportModel{Model: model, Totals: reportTotalsWithCost(values, !mixedCurrency)})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Totals.ModelRequests != models[j].Totals.ModelRequests {
			return models[i].Totals.ModelRequests > models[j].Totals.ModelRequests
		}
		return models[i].Model < models[j].Model
	})
	sort.Slice(recent, func(i, j int) bool { return recent[i].CompletedAt > recent[j].CompletedAt })
	if len(recent) > aiusage.DefaultRecentOperations {
		recent = recent[:aiusage.DefaultRecentOperations]
	}
	currency := ""
	if len(currencies) == 1 {
		for value := range currencies {
			currency = value
		}
	}
	pricingCoverage := "unavailable"
	if totals.ReportedRequests > 0 {
		switch {
		case pricingCountsUnknown || coverageCountsUnknown || aggregateOverflow:
			pricingCoverage = "unknown"
		case totals.PricedReportedRequests == totals.ReportedRequests:
			pricingCoverage = "complete"
			if totals.CacheWriteUnreportedRequests > 0 ||
				totals.CacheWriteInputTokens > 0 && totals.CacheWriteReportedRequests > totals.CacheWritePricedRequests {
				pricingCoverage = "partial"
			}
		case totals.PricedReportedRequests > 0:
			pricingCoverage = "partial"
		}
	}
	coverage := reportCoverage(totals, coverageCountsUnknown, aggregateOverflow, pricingConfigured)
	currentRateStatus, currentRateCurrency, currentRateCost := currentRateEstimate(pricing, totals, coverage)
	modelCoverage := "complete"
	if len(featureFilter) > 0 {
		modelCoverage = "unavailable_for_feature_filter"
		models = nil
	} else if modelCountsUnknown {
		modelCoverage = "partial"
	} else if totals.Operations == 0 {
		modelCoverage = "unavailable"
	}
	return usageReport{
		Version: aiusage.LedgerVersion, GeneratedAt: generatedAt.Format(time.RFC3339Nano),
		Range:    usageReportRange{Start: start.Format(time.DateOnly), End: end.Format(time.DateOnly)},
		Currency: currency, MixedCurrency: mixedCurrency, MixedPricing: len(pricingHashes) > 1,
		RangePriced: pricingCoverage == "complete", PricingCoverage: pricingCoverage,
		RecordedCostStatus: recordedCostStatus(totals, len(currencies), pricingCountsUnknown, coverageCountsUnknown, aggregateOverflow),
		CurrentRateStatus:  currentRateStatus, CurrentRateCurrency: currentRateCurrency, CurrentRateEstimatedCostNanos: currentRateCost,
		Coverage: coverage, Totals: reportTotalsWithCost(totals, !mixedCurrency), Daily: days, Features: features,
		Models: models, ModelCoverage: modelCoverage, RecentOperations: recent,
	}
}

func reportFeatureRows(totals map[aiusage.Feature]aiusage.UsageTotals, includeCost bool) []usageReportFeature {
	rows := make([]usageReportFeature, 0, len(totals))
	for feature, values := range totals {
		rows = append(rows, usageReportFeature{Feature: feature, Totals: reportTotalsWithCost(values, includeCost)})
	}
	sort.Slice(rows, func(i, j int) bool {
		left, _ := strconv.ParseInt(rows[i].Totals.EstimatedCostNanos, 10, 64)
		right, _ := strconv.ParseInt(rows[j].Totals.EstimatedCostNanos, 10, 64)
		if left != right {
			return left > right
		}
		return rows[i].Feature < rows[j].Feature
	})
	return rows
}

func reportTotalsWithCost(value aiusage.UsageTotals, includeCost bool) usageReportTotals {
	if !includeCost {
		value.EstimatedCostNanos = 0
	}
	return reportTotals(value)
}

func recordedCostStatus(totals aiusage.UsageTotals, currencyCount int, pricingUnknown, coverageUnknown, aggregateOverflow bool) string {
	if aggregateOverflow {
		return "unavailable"
	}
	if currencyCount > 1 {
		return "mixed_currency"
	}
	if totals.ModelRequests == 0 && totals.ExternalUnmeteredOperations == 0 && totals.ModelGatewayExcludedOperations == 0 {
		return "available"
	}
	if pricingUnknown || coverageUnknown {
		return "unknown"
	}
	if totals.ReportedRequests == 0 || totals.PricedReportedRequests == 0 {
		return "unavailable"
	}
	if totals.PricedReportedRequests < totals.ReportedRequests || totals.UnreportedRequests > 0 || totals.InvalidUsageRequests > 0 ||
		totals.ExternalUnmeteredOperations > 0 || totals.ModelGatewayExcludedOperations > 0 || totals.CacheWriteUnreportedRequests > 0 ||
		totals.CacheWriteInputTokens > 0 && totals.CacheWritePricedRequests < totals.CacheWriteReportedRequests {
		return "partial"
	}
	return "available"
}

func currentRateEstimate(pricing aiusage.PriceTable, totals aiusage.UsageTotals, coverage usageReportCoverage) (string, string, string) {
	if !pricing.Configured() || coverage.AggregateOverflow || totals.ReportedRequests == 0 && (totals.ExternalUnmeteredOperations > 0 || totals.ModelGatewayExcludedOperations > 0) {
		return "unavailable", "", ""
	}
	cost, err := pricing.EstimateTotals(totals)
	if err != nil {
		return "unavailable", pricing.Currency(), ""
	}
	status := "available"
	if coverage.Status != "complete" || totals.CacheWriteInputTokens > 0 && !pricing.CacheWriteConfigured() {
		status = "partial"
	}
	return status, pricing.Currency(), strconv.FormatInt(cost, 10)
}

func reportCoverage(totals aiusage.UsageTotals, legacyUnknown, aggregateOverflow, pricingConfigured bool) usageReportCoverage {
	coverage := usageReportCoverage{
		Status: "complete", States: []string{}, ModelRequests: totals.ModelRequests, ReportedRequests: totals.ReportedRequests,
		PricedReportedRequests: totals.PricedReportedRequests, CacheWriteReportedRequests: totals.CacheWriteReportedRequests,
		CacheWritePricedRequests: totals.CacheWritePricedRequests, CacheWriteUnreportedRequests: totals.CacheWriteUnreportedRequests,
		InvalidUsageRequests: totals.InvalidUsageRequests, UnreportedRequests: totals.UnreportedRequests,
		ExternalUnmeteredOperations: totals.ExternalUnmeteredOperations, ModelGatewayExcludedOperations: totals.ModelGatewayExcludedOperations,
		LegacyCoverageUnknown: legacyUnknown, AggregateOverflow: aggregateOverflow,
	}
	addState := func(state string) { coverage.States = append(coverage.States, state) }
	if totals.Operations == 0 {
		coverage.Status = "unavailable"
		addState("no_usage_records")
		return coverage
	}
	if totals.ModelRequests == 0 && totals.ExternalUnmeteredOperations == 0 && totals.ModelGatewayExcludedOperations == 0 && !aggregateOverflow {
		addState("no_provider_usage")
		return coverage
	}
	partial := false
	if totals.UnreportedRequests > 0 || totals.InvalidUsageRequests > 0 {
		partial = true
		addState("partial_token_usage")
	}
	if totals.CacheWriteUnreportedRequests > 0 {
		partial = true
		addState("cache_write_unreported")
	}
	if totals.CacheWriteInputTokens > 0 && totals.CacheWriteReportedRequests > totals.CacheWritePricedRequests {
		partial = true
		addState("cache_write_pricing_missing")
	}
	if totals.ExternalUnmeteredOperations > 0 {
		partial = true
		addState("external_unmetered")
	}
	if totals.ModelGatewayExcludedOperations > 0 {
		partial = true
		addState("model_gateway_excluded")
	}
	if legacyUnknown {
		partial = true
		addState("legacy_coverage_unknown")
	}
	if aggregateOverflow {
		partial = true
		addState("aggregate_overflow")
	}
	unpriced := max(totals.ReportedRequests-totals.PricedReportedRequests, 0)
	if unpriced > 0 {
		partial = true
		if pricingConfigured {
			coverage.PricingAddedAfterRequests = unpriced
			addState("pricing_added_after_operation")
		} else {
			addState("pricing_unavailable")
		}
	}
	if totals.ModelRequests > 0 && totals.ReportedRequests == totals.ModelRequests && !partial {
		addState("fully_priced_provider_reported")
	} else if totals.ReportedRequests == 0 && (totals.ExternalUnmeteredOperations > 0 || totals.ModelGatewayExcludedOperations > 0) {
		coverage.Status = "unavailable"
		return coverage
	}
	if partial {
		coverage.Status = "partial"
	}
	return coverage
}

func addUsageTotals(target *aiusage.UsageTotals, value aiusage.UsageTotals) bool {
	return aiusage.AddTotals(target, value)
}

func reportTotals(value aiusage.UsageTotals) usageReportTotals {
	return usageReportTotals{
		Operations: value.Operations, CacheHits: value.CacheHits, SuppressedOperations: value.SuppressedOperations, CooldownRetries: value.CooldownRetries, Failures: value.Failures,
		ExternalUnmeteredOperations: value.ExternalUnmeteredOperations, ModelGatewayExcludedOperations: value.ModelGatewayExcludedOperations,
		ModelRequests: value.ModelRequests, ReportedRequests: value.ReportedRequests, PricedReportedRequests: value.PricedReportedRequests, UnreportedRequests: value.UnreportedRequests,
		CacheWriteReportedRequests: value.CacheWriteReportedRequests, CacheWritePricedRequests: value.CacheWritePricedRequests,
		CacheWriteUnreportedRequests: value.CacheWriteUnreportedRequests, InvalidUsageRequests: value.InvalidUsageRequests,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens, CacheWriteInputTokens: value.CacheWriteInputTokens,
		OutputTokens: value.OutputTokens, ReasoningTokens: value.ReasoningTokens,
		EstimatedCostNanos: strconv.FormatInt(value.EstimatedCostNanos, 10),
	}
}
