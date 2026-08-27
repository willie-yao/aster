package devmock

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// seedDays is how much history the fabricated usage ledgers and traces cover.
const seedDays = 14

// seedModel names the provider a fabricated record came from.
const seedModel = "mock-model"

// Pricing prices the fabricated usage ledgers. The server advertises the same
// rates, so the cost columns agree with the ledger they summarize.
var Pricing = aiusage.Rates{
	Currency: "USD", InputPerMillion: "1.25", CachedInputPerMillion: "0.13",
	CacheWriteInputPerMillion: "1.56", OutputPerMillion: "10.00",
}

// PricingRule describes Pricing the way the usage API reports a rate.
const PricingRule = "USD input=1.25 cached_input=0.13 cache_write_input=1.56 output=10.00 per million tokens"

// Seed writes the private operational files the authenticated read-only views
// serve. They are deliberately excluded from the public /data tree, so unlike
// the dashboard content they cannot be mirrored from a deployed site and have
// to be fabricated here. Existing files are left alone so a real fetch is never
// overwritten.
func Seed(dataDir string) error {
	// A fixed seed keeps the fabricated numbers stable across restarts, so a
	// chart does not change shape every time the server is rebuilt.
	source := rand.New(rand.NewPCG(1, 2))
	now := time.Now().UTC()
	for _, step := range []struct {
		path  string
		write func() error
	}{
		{filepath.Join(dataDir, output.AITraceFilename), func() error { return seedTraces(dataDir, now) }},
		{filepath.Join(dataDir, ai.CacheFilename), func() error { return seedPatternDiagnostics(dataDir, now) }},
		{fetchprogress.Path(dataDir), func() error { return seedFetchProgress(dataDir, now) }},
		{filepath.Join(dataDir, output.AIUsageFetcherFilename), func() error {
			return seedUsage(dataDir, output.AIUsageFetcherFilename, aiusage.OriginFetcher, now, source)
		}},
		{filepath.Join(dataDir, output.AIUsageServerFilename), func() error {
			return seedUsage(dataDir, output.AIUsageServerFilename, aiusage.OriginServer, now, source)
		}},
	} {
		if _, err := os.Stat(step.path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("devmock: checking %s: %w", step.path, err)
		}
		if err := step.write(); err != nil {
			return err
		}
	}
	return nil
}

// seedTraces writes analysis traces so the analysis health view has content.
func seedTraces(dataDir string, now time.Time) error {
	outcomes := []string{"analyzed", "unavailable", "transient"}
	traces := make([]ai.AnalysisTrace, 0, 12)
	for i := range 12 {
		started := now.Add(-time.Duration(i) * 3 * time.Hour)
		outcome := outcomes[i%len(outcomes)]
		trace := ai.AnalysisTrace{
			JobID:     fmt.Sprintf("periodic-mock-e2e-%d", i%3),
			BuildID:   fmt.Sprintf("18%011d", 1000+i),
			TestName:  fmt.Sprintf("[It] mock suite should converge case %d", i),
			APIMode:   "chat_completions",
			Model:     seedModel,
			StartedAt: timestamp(started),
			ElapsedMs: 12_000 + i*750,
			Outcome:   outcome,
			Events: []ai.TraceEvent{
				{Sequence: 1, Kind: "request", ElapsedMs: 40, ResponseID: fmt.Sprintf("mock-response-%d", i), Status: "ok"},
				{Sequence: 2, Kind: "tool", ElapsedMs: 2_100, Tool: "list_artifacts", DurationMs: 210},
				{Sequence: 3, Kind: "tool", ElapsedMs: 6_400, Tool: "read_artifact", DurationMs: 980},
				{Sequence: 4, Kind: "finalize", ElapsedMs: 11_800, Outcome: outcome, FinishReason: "stop"},
			},
		}
		if outcome == "unavailable" {
			trace.ErrorCode = "insufficient_evidence"
		}
		traces = append(traces, trace)
	}
	return statefile.WritePrivateJSONDurable(filepath.Join(dataDir, output.AITraceFilename), ai.AnalysisTraceFile{
		Version:       1,
		GeneratedAt:   timestamp(now),
		RetainedSince: timestamp(now.Add(-seedDays * 24 * time.Hour)),
		Engine:        &ai.TraceEngine{Version: "mock", Commit: "mock", ImageTag: "mock"},
		Traces:        traces,
	})
}

// seedPatternDiagnostics writes an AI cache holding only recurring-pattern
// rejection records, which is all the diagnostics view reads out of it.
func seedPatternDiagnostics(dataDir string, now time.Time) error {
	entries := map[string]ai.CacheEntry{}
	for i, category := range []string{"schema", "missing", "ambiguous"} {
		failedAt := now.Add(-time.Duration(i+1) * time.Hour)
		data, err := json.Marshal(map[string]any{
			"version":                      1,
			"job_id":                       fmt.Sprintf("periodic-mock-e2e-%d", i),
			"category":                     category,
			"failed_at":                    failedAt,
			"retry_after":                  failedAt.Add(6 * time.Hour),
			"stage":                        "validation",
			"validation_category":          category,
			"validation_code":              "mock_" + category,
			"candidate_count":              4,
			"valid_count":                  i,
			"contract_like_rejected_count": 1,
			"incomplete_count":             1,
		})
		if err != nil {
			return fmt.Errorf("devmock: encoding pattern diagnostics: %w", err)
		}
		key := fmt.Sprintf("pattern-failure:mock-pattern-%d", i)
		entries[key] = ai.CacheEntry{Key: key, CreatedAt: failedAt, Data: data}
	}
	return statefile.WritePrivateJSONDurable(filepath.Join(dataDir, ai.CacheFilename), entries)
}

// seedFetchProgress writes a completed fetch pass so the operator status banner
// has something to report.
func seedFetchProgress(dataDir string, now time.Time) error {
	started := now.Add(-9 * time.Minute)
	completed := now.Add(-30 * time.Second)
	status := fetchprogress.Status{
		SchemaVersion:               fetchprogress.SchemaVersion,
		RunID:                       "mock-run",
		PassID:                      "mock-pass",
		PassType:                    fetchprogress.PassOneShot,
		EngineVersion:               "mock",
		Phase:                       fetchprogress.PhaseIdle,
		RunStartedAt:                started,
		PassStartedAt:               started,
		PhaseStartedAt:              completed,
		LastProgressAt:              completed,
		LastCheckedAt:               &completed,
		LastSuccessfulPublicationAt: &completed,
		Outcome:                     fetchprogress.OutcomeSucceeded,
		PatternPhase:                fetchprogress.StageCompleted,
		PublicationPhase:            fetchprogress.StageCompleted,
		SideEffectPhase:             fetchprogress.StageSkipped,
	}
	status.Jobs = fetchprogress.JobProgress{Total: 28, Completed: 28}
	status.Builds = fetchprogress.BuildProgress{Cached: 61, Fetched: 23}
	status.Analyses = fetchprogress.AnalysisProgress{
		LogicalTotal: 46, AcceptedCacheHits: 39, NewWork: 7,
		Completed: 46, FreshAnalysesCompleted: 7, ResultsRetrieved: 7,
	}
	status.Patterns = fetchprogress.PatternProgress{Eligible: 7, Completed: 7, Attempts: 7, Current: 7}
	return fetchprogress.Write(fetchprogress.Path(dataDir), status)
}

// seedUsage writes one private usage ledger through the real recorder, so the
// ledger it produces is the same shape the usage API reads in production.
func seedUsage(dataDir, filename string, origin aiusage.Origin, now time.Time, source *rand.Rand) error {
	pricing, err := aiusage.NewPriceTable(Pricing)
	if err != nil {
		return fmt.Errorf("devmock: pricing %s: %w", filename, err)
	}
	clock := now
	recorder, err := aiusage.NewRecorder(filepath.Join(dataDir, filename), aiusage.RecorderOptions{
		RetentionDays:    seedDays * 2,
		RecentOperations: 50,
		Pricing:          pricing,
		Now:              func() time.Time { return clock },
	})
	if err != nil {
		return fmt.Errorf("devmock: creating %s: %w", filename, err)
	}
	features := usageFeatures(origin)
	for day := seedDays - 1; day >= 0; day-- {
		for operation := range 3 {
			clock = now.AddDate(0, 0, -day).Add(time.Duration(operation) * time.Hour)
			input := int64(4_000 + source.IntN(9_000))
			output := int64(400 + source.IntN(1_400))
			recorder.Record(aiusage.OperationUsage{
				ID:            fmt.Sprintf("mock-%s-%d-%d", origin, day, operation),
				Origin:        origin,
				Feature:       features[operation%len(features)],
				StartedAt:     timestamp(clock),
				CompletedAt:   timestamp(clock.Add(30 * time.Second)),
				Outcome:       aiusage.OutcomeSuccess,
				Model:         seedModel,
				ModelRequests: 1, ReportedRequests: 1,
				InputTokens:         input,
				CachedInputTokens:   input / 3,
				OutputTokens:        output,
				ReasoningTokens:     output / 2,
				CoverageCountsKnown: true,
			})
		}
	}
	return nil
}

// usageFeatures are the features one origin plausibly bills for.
func usageFeatures(origin aiusage.Origin) []aiusage.Feature {
	if origin == aiusage.OriginServer {
		return []aiusage.Feature{aiusage.FeatureAnalysisChat, aiusage.FeatureIssueDraft, aiusage.FeatureFixPreview}
	}
	return []aiusage.Feature{aiusage.FeatureFailureAnalysis, aiusage.FeaturePatternAnalysis, aiusage.FeatureSourceInvestigation}
}
