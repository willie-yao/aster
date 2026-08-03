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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

const maxAIUsageFileBytes = 64 << 20

type usageReport struct {
	Version          int                      `json:"version"`
	GeneratedAt      string                   `json:"generated_at"`
	Range            usageReportRange         `json:"range"`
	Currency         string                   `json:"currency,omitempty"`
	MixedCurrency    bool                     `json:"mixed_currency,omitempty"`
	MixedPricing     bool                     `json:"mixed_pricing,omitempty"`
	Coverage         usageReportCoverage      `json:"coverage"`
	Totals           usageReportTotals        `json:"totals"`
	Daily            []usageReportDay         `json:"daily"`
	Features         []usageReportFeature     `json:"features"`
	RecentOperations []aiusage.OperationUsage `json:"recent_operations"`
}

type usageReportRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type usageReportCoverage struct {
	Status                      string `json:"status"`
	ModelRequests               int    `json:"model_requests"`
	ReportedRequests            int    `json:"reported_requests"`
	UnreportedRequests          int    `json:"unreported_requests"`
	ExternalUnmeteredOperations int    `json:"external_unmetered_operations"`
}

type usageReportTotals struct {
	Operations                  int    `json:"operations"`
	CacheHits                   int    `json:"cache_hits"`
	Failures                    int    `json:"failures"`
	ExternalUnmeteredOperations int    `json:"external_unmetered_operations"`
	ModelRequests               int    `json:"model_requests"`
	ReportedRequests            int    `json:"reported_requests"`
	UnreportedRequests          int    `json:"unreported_requests"`
	InputTokens                 int64  `json:"input_tokens"`
	CachedInputTokens           int64  `json:"cached_input_tokens"`
	OutputTokens                int64  `json:"output_tokens"`
	ReasoningTokens             int64  `json:"reasoning_tokens"`
	EstimatedCostNanos          string `json:"estimated_cost_nanos"`
}

type usageReportDay struct {
	Date   string            `json:"date"`
	Totals usageReportTotals `json:"totals"`
}

type usageReportFeature struct {
	Feature aiusage.Feature   `json:"feature"`
	Totals  usageReportTotals `json:"totals"`
}

func aiUsageHandler(dataDir string, attachment bool, now func() time.Time) http.Handler {
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
		report := buildUsageReport(ledgers, start, end, features, now().UTC())
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
	if ledger.Version > aiusage.LedgerVersion {
		return aiusage.UsageLedger{}, fmt.Errorf("usage version %d is newer than supported version %d", ledger.Version, aiusage.LedgerVersion)
	}
	return ledger, nil
}

func buildUsageReport(ledgers []aiusage.UsageLedger, start, end time.Time, featureFilter map[aiusage.Feature]bool, generatedAt time.Time) usageReport {
	dayTotals := map[string]aiusage.UsageTotals{}
	featureTotals := map[aiusage.Feature]aiusage.UsageTotals{}
	currencies := map[string]bool{}
	pricingHashes := map[string]bool{}
	var recent []aiusage.OperationUsage
	for _, ledger := range ledgers {
		for _, day := range ledger.Days {
			date, err := time.Parse(time.DateOnly, day.Date)
			if err != nil || date.Before(start) || date.After(end) {
				continue
			}
			if len(featureFilter) == 0 {
				if day.Totals.Operations > 0 && ledger.Currency != "" {
					currencies[ledger.Currency] = true
				}
				for _, hash := range day.PricingHashes {
					if hash != "" {
						pricingHashes[hash] = true
					}
				}
				totals := dayTotals[day.Date]
				addUsageTotals(&totals, day.Totals)
				dayTotals[day.Date] = totals
			}
			for feature, values := range day.Features {
				if len(featureFilter) > 0 && !featureFilter[feature] {
					continue
				}
				featureTotal := featureTotals[feature]
				addUsageTotals(&featureTotal, values)
				featureTotals[feature] = featureTotal
				if len(featureFilter) > 0 {
					if values.Operations > 0 && ledger.Currency != "" {
						currencies[ledger.Currency] = true
					}
					dayTotal := dayTotals[day.Date]
					addUsageTotals(&dayTotal, values)
					dayTotals[day.Date] = dayTotal
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
	days := make([]usageReportDay, 0, len(dayTotals))
	for date, values := range dayTotals {
		addUsageTotals(&totals, values)
		days = append(days, usageReportDay{Date: date, Totals: reportTotals(values)})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	features := make([]usageReportFeature, 0, len(featureTotals))
	for feature, values := range featureTotals {
		features = append(features, usageReportFeature{Feature: feature, Totals: reportTotals(values)})
	}
	sort.Slice(features, func(i, j int) bool {
		left, _ := strconv.ParseInt(features[i].Totals.EstimatedCostNanos, 10, 64)
		right, _ := strconv.ParseInt(features[j].Totals.EstimatedCostNanos, 10, 64)
		if left != right {
			return left > right
		}
		return features[i].Feature < features[j].Feature
	})
	sort.Slice(recent, func(i, j int) bool { return recent[i].CompletedAt > recent[j].CompletedAt })
	if len(recent) > aiusage.DefaultRecentOperations {
		recent = recent[:aiusage.DefaultRecentOperations]
	}
	currency := ""
	for value := range currencies {
		currency = value
		break
	}
	status := "complete"
	if totals.ModelRequests == 0 || currency == "" || len(pricingHashes) == 0 {
		status = "unavailable"
	} else if totals.UnreportedRequests > 0 || totals.ExternalUnmeteredOperations > 0 || len(currencies) > 1 {
		status = "partial"
	}
	return usageReport{
		Version: aiusage.LedgerVersion, GeneratedAt: generatedAt.Format(time.RFC3339Nano),
		Range:    usageReportRange{Start: start.Format(time.DateOnly), End: end.Format(time.DateOnly)},
		Currency: currency, MixedCurrency: len(currencies) > 1, MixedPricing: len(pricingHashes) > 1,
		Coverage: usageReportCoverage{Status: status, ModelRequests: totals.ModelRequests, ReportedRequests: totals.ReportedRequests, UnreportedRequests: totals.UnreportedRequests, ExternalUnmeteredOperations: totals.ExternalUnmeteredOperations},
		Totals:   reportTotals(totals), Daily: days, Features: features, RecentOperations: recent,
	}
}

func addUsageTotals(target *aiusage.UsageTotals, value aiusage.UsageTotals) {
	target.Operations += value.Operations
	target.CacheHits += value.CacheHits
	target.Failures += value.Failures
	target.ExternalUnmeteredOperations += value.ExternalUnmeteredOperations
	target.ModelRequests += value.ModelRequests
	target.ReportedRequests += value.ReportedRequests
	target.UnreportedRequests += value.UnreportedRequests
	target.InputTokens += value.InputTokens
	target.CachedInputTokens += value.CachedInputTokens
	target.OutputTokens += value.OutputTokens
	target.ReasoningTokens += value.ReasoningTokens
	target.EstimatedCostNanos += value.EstimatedCostNanos
}

func reportTotals(value aiusage.UsageTotals) usageReportTotals {
	return usageReportTotals{
		Operations: value.Operations, CacheHits: value.CacheHits, Failures: value.Failures,
		ExternalUnmeteredOperations: value.ExternalUnmeteredOperations,
		ModelRequests:               value.ModelRequests, ReportedRequests: value.ReportedRequests, UnreportedRequests: value.UnreportedRequests,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens,
		OutputTokens: value.OutputTokens, ReasoningTokens: value.ReasoningTokens,
		EstimatedCostNanos: strconv.FormatInt(value.EstimatedCostNanos, 10),
	}
}
