package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const benchmarkManifestVersion = 1

var benchmarkCaseIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var benchmarkStableIDRE = regexp.MustCompile(`^[0-9a-f]{20}$`)
var benchmarkCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type benchmarkManifest struct {
	Version int                     `json:"version"`
	Cases   []benchmarkManifestCase `json:"cases"`
}

type benchmarkManifestCase struct {
	ID                  string                    `json:"id"`
	StableID            string                    `json:"stable_id"`
	Bucket              string                    `json:"bucket"`
	FixtureAsset        string                    `json:"fixture_asset,omitempty"`
	FixtureSHA256       string                    `json:"fixture_sha256,omitempty"`
	JobType             string                    `json:"job_type"`
	Repo                string                    `json:"repo,omitempty"`
	JobName             string                    `json:"job_name"`
	BuildID             string                    `json:"build_id"`
	PullNumber          string                    `json:"pull_number,omitempty"`
	WebURL              string                    `json:"web_url"`
	Commit              string                    `json:"commit"`
	RepoVersion         string                    `json:"repo_version"`
	RepoRefs            map[string]string         `json:"repo_refs"`
	SourceOwner         string                    `json:"source_owner"`
	SourceName          string                    `json:"source_name"`
	TestName            string                    `json:"test_name"`
	JUnitFile           string                    `json:"junit_file,omitempty"`
	FailureMessage      string                    `json:"failure_message"`
	ConsecutiveFailures int                       `json:"consecutive_failures,omitempty"`
	OppositeDiagnosis   string                    `json:"opposite_diagnosis,omitempty"`
	Signals             []benchmarkManifestSignal `json:"signals"`
}

type benchmarkManifestSignal struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Negated string `json:"negated,omitempty"`
	Must    bool   `json:"must,omitempty"`
}

func loadBenchmarkManifest(path string) ([]benchCase, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 2<<20 {
		return nil, fmt.Errorf("benchmark manifest exceeds 2 MiB")
	}
	decoder := json.NewDecoder(bufio.NewReader(file))
	decoder.DisallowUnknownFields()
	var manifest benchmarkManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode benchmark manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("benchmark manifest must contain one JSON object")
	}
	if manifest.Version != benchmarkManifestVersion {
		return nil, fmt.Errorf("benchmark manifest version %d is unsupported", manifest.Version)
	}
	if len(manifest.Cases) == 0 || len(manifest.Cases) > 50 {
		return nil, fmt.Errorf("benchmark manifest case count must be 1..50")
	}
	seen := map[string]bool{}
	out := make([]benchCase, 0, len(manifest.Cases))
	for index, item := range manifest.Cases {
		if !benchmarkCaseIDRE.MatchString(item.ID) || seen[item.ID] {
			return nil, fmt.Errorf("benchmark manifest case %d has invalid or duplicate id", index)
		}
		seen[item.ID] = true
		if !benchmarkStableIDRE.MatchString(item.StableID) {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid stable_id", item.ID)
		}
		if item.JobType != models.JobTypePeriodic && item.JobType != models.JobTypePresubmit {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid job_type", item.ID)
		}
		if item.JobType == models.JobTypePresubmit && (item.Repo == "" || item.PullNumber == "") {
			return nil, fmt.Errorf("benchmark manifest presubmit case %q requires repo and pull_number", item.ID)
		}
		if item.ConsecutiveFailures < 0 {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid consecutive_failures", item.ID)
		}
		if _, err := strconv.ParseUint(item.BuildID, 10, 64); err != nil {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid build_id", item.ID)
		}
		if !benchmarkCommitRE.MatchString(item.Commit) || item.RepoVersion != item.Commit {
			return nil, fmt.Errorf("benchmark manifest case %q requires matching exact commit and repo_version", item.ID)
		}
		if len(item.RepoRefs) == 0 || len(item.RepoRefs) > 8 {
			return nil, fmt.Errorf("benchmark manifest case %q repo_refs count must be 1..8", item.ID)
		}
		sourceKey := item.SourceOwner + "/" + item.SourceName
		if _, ok := item.RepoRefs[sourceKey]; !ok {
			return nil, fmt.Errorf("benchmark manifest case %q repo_refs omit configured source", item.ID)
		}
		for repo, ref := range item.RepoRefs {
			if repo == "" || ref == "" || len(repo) > 256 || len(ref) > 256 || strings.ContainsAny(repo+ref, "\r\n\x00") {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid repo_refs", item.ID)
			}
		}
		if item.Bucket == "" || item.JobName == "" || item.WebURL == "" || item.TestName == "" || item.FailureMessage == "" || item.SourceOwner == "" || item.SourceName == "" {
			return nil, fmt.Errorf("benchmark manifest case %q is missing required identity", item.ID)
		}
		for label, value := range map[string]string{
			"bucket": item.Bucket, "job_name": item.JobName, "repo": item.Repo, "web_url": item.WebURL,
			"source_owner": item.SourceOwner, "source_name": item.SourceName, "junit_file": item.JUnitFile,
		} {
			if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid %s", item.ID, label)
			}
		}
		if len(item.TestName) > 4096 || len(item.FailureMessage) > 16384 || len(item.OppositeDiagnosis) > 16384 {
			return nil, fmt.Errorf("benchmark manifest case %q text exceeds limits", item.ID)
		}
		if item.FixtureAsset != "" {
			if filepath.Base(item.FixtureAsset) != item.FixtureAsset || !strings.HasSuffix(item.FixtureAsset, ".tar.gz") || len(item.FixtureSHA256) != 64 {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid fixture identity", item.ID)
			}
		}
		if len(item.Signals) == 0 || len(item.Signals) > 32 {
			return nil, fmt.Errorf("benchmark manifest case %q signal count must be 1..32", item.ID)
		}
		signals := make([]benchSignal, 0, len(item.Signals))
		for signalIndex, signal := range item.Signals {
			if signal.Name == "" || signal.Pattern == "" {
				return nil, fmt.Errorf("benchmark manifest case %q signal %d is incomplete", item.ID, signalIndex)
			}
			positive, err := regexp.Compile(signal.Pattern)
			if err != nil {
				return nil, fmt.Errorf("benchmark manifest case %q signal %d pattern: %w", item.ID, signalIndex, err)
			}
			var negative *regexp.Regexp
			if signal.Negated != "" {
				negative, err = regexp.Compile(signal.Negated)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q signal %d negated: %w", item.ID, signalIndex, err)
				}
			}
			signals = append(signals, benchSignal{name: signal.Name, re: positive, negated: negative, must: signal.Must})
		}
		out = append(out, benchCase{
			name: item.ID, stableID: item.StableID, bucket: item.Bucket, fixtureAsset: item.FixtureAsset,
			fixtureSHA256: item.FixtureSHA256, jobType: item.JobType, repo: item.Repo, jobName: item.JobName,
			buildID: item.BuildID, pullNumber: item.PullNumber, webURL: item.WebURL,
			commit: item.Commit, repoVersion: item.RepoVersion, repoRefs: maps.Clone(item.RepoRefs),
			sourceRepo: [2]string{item.SourceOwner, item.SourceName}, testName: item.TestName,
			junitFile: item.JUnitFile, failureMsg: item.FailureMessage, consecutiveFailures: item.ConsecutiveFailures,
			oppositeDiagnosis: item.OppositeDiagnosis, signals: signals,
		})
	}
	return out, nil
}

type benchmarkJSONLResult struct {
	CaseID                 string                     `json:"case_id"`
	StableID               string                     `json:"stable_id"`
	Repetition             int                        `json:"repetition"`
	ModelLabel             string                     `json:"model_label"`
	JobName                string                     `json:"job_name"`
	BuildID                string                     `json:"build_id"`
	CheckoutCommit         string                     `json:"checkout_commit"`
	SourceRevision         string                     `json:"source_revision,omitempty"`
	SourceUnavailable      bool                       `json:"source_unavailable,omitempty"`
	TestName               string                     `json:"test_name"`
	ElapsedMS              int64                      `json:"elapsed_ms"`
	Usable                 bool                       `json:"usable"`
	Summary                string                     `json:"summary,omitempty"`
	RootCause              string                     `json:"root_cause,omitempty"`
	SuggestedFix           string                     `json:"suggested_fix,omitempty"`
	Severity               string                     `json:"severity,omitempty"`
	Evidence               []models.EvidenceCitation  `json:"evidence_citations,omitempty"`
	FileLinks              map[string]string          `json:"file_links,omitempty"`
	SignalHits             int                        `json:"signal_hits"`
	SignalTotal            int                        `json:"signal_total"`
	MissingMust            []string                   `json:"missing_must,omitempty"`
	SelectedAttempt        int                        `json:"selected_attempt,omitempty"`
	ToolNames              []string                   `json:"tool_names,omitempty"`
	ToolCounts             []string                   `json:"tool_counts,omitempty"`
	GCSBytes               int                        `json:"gcs_bytes,omitempty"`
	EvidencePlanCovered    bool                       `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted bool                       `json:"gcs_floor_retry_exhausted,omitempty"`
	CritiquePassed         bool                       `json:"critique_passed,omitempty"`
	BudgetExhausted        bool                       `json:"budget_exhausted,omitempty"`
	FloorNudges            int                        `json:"floor_nudges,omitempty"`
	FloorNudgeReasons      []string                   `json:"floor_nudge_reasons,omitempty"`
	CacheGeneration        string                     `json:"cache_generation,omitempty"`
	CacheVerification      benchmarkCacheVerification `json:"cache_verification"`
	Trace                  benchmarkJSONLTrace        `json:"trace"`
}

type benchmarkCacheVerification struct {
	Attempted              bool                    `json:"attempted"`
	Saved                  bool                    `json:"saved"`
	Accepted               bool                    `json:"accepted"`
	RejectionReason        ai.CacheRejectionReason `json:"rejection_reason,omitempty"`
	CacheHit               bool                    `json:"cache_hit"`
	ProviderRequests       int                     `json:"provider_requests"`
	EvidencePlanCovered    bool                    `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted bool                    `json:"gcs_floor_retry_exhausted,omitempty"`
	CacheGeneration        string                  `json:"cache_generation,omitempty"`
}

type benchmarkJSONLTrace struct {
	ModelRequests     int            `json:"model_requests"`
	ModelFailures     int            `json:"model_failures"`
	ToolCalls         int            `json:"tool_calls"`
	ToolFailures      int            `json:"tool_failures"`
	InputTokens       int            `json:"input_tokens"`
	CachedInputTokens int            `json:"cached_input_tokens"`
	OutputTokens      int            `json:"output_tokens"`
	Finalize          map[string]int `json:"finalize"`
	FinalizeRecovery  map[string]int `json:"finalize_recovery"`
	Critique          map[string]int `json:"critique"`
}

func writeBenchmarkJSONL(t *testing.T, path string, bc benchCase, repetition int, tc *models.TestCase, elapsed time.Duration, snapshot ai.AnalysisTraceFile, selectedAttempt int, toolUsage benchmarkToolUsage, traceSummary benchmarkTraceSummary, cacheGeneration string, cacheVerification benchmarkCacheVerification) {
	t.Helper()
	if path == "" {
		return
	}
	if !benchmarkStableIDRE.MatchString(bc.stableID) {
		t.Fatalf("external benchmark results require a stable case id")
	}
	label := strings.TrimSpace(os.Getenv("BENCH_MODEL_LABEL"))
	if !benchmarkCaseIDRE.MatchString(label) {
		t.Fatalf("BENCH_MODEL_LABEL must be a stable anonymous label when BENCH_RESULTS_JSONL is set")
	}
	result := benchmarkJSONLResult{
		CaseID: bc.name, StableID: bc.stableID, Repetition: repetition, ModelLabel: label,
		JobName: bc.jobName, BuildID: bc.buildID, CheckoutCommit: bc.commit, TestName: bc.testName, ElapsedMS: elapsed.Milliseconds(),
		FileLinks: map[string]string{}, SignalTotal: len(bc.signals), SelectedAttempt: selectedAttempt,
		ToolNames: append([]string(nil), toolUsage.names...), ToolCounts: append([]string(nil), toolUsage.counts...),
		FloorNudges: traceSummary.floorNudges, FloorNudgeReasons: append([]string(nil), traceSummary.floorNudgeReasons...),
		CacheGeneration: cacheGeneration, CacheVerification: cacheVerification,
		Trace: benchmarkJSONLTrace{Finalize: map[string]int{}, FinalizeRecovery: map[string]int{}, Critique: map[string]int{}},
	}
	build := models.BuildInfo{Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs)}
	if source, ok := ai.ResolveBuildSource(build, bc.sourceRepo[0], bc.sourceRepo[1]); ok {
		result.SourceRevision = source.Revision
	} else {
		result.SourceUnavailable = true
	}
	if tc != nil && tc.AIAnalysis != nil && tc.AISummary != nil {
		result.Usable = true
		result.Summary, result.RootCause, result.SuggestedFix, result.Severity = tc.AISummary.Summary, tc.AIAnalysis.RootCause, tc.AIAnalysis.SuggestedFix, tc.AIAnalysis.Severity
		result.GCSBytes = tc.AIAnalysis.GCSBytes
		result.EvidencePlanCovered = tc.AIAnalysis.EvidencePlanCovered
		result.GCSFloorRetryExhausted = tc.AIAnalysis.GCSFloorRetryExhausted
		result.CritiquePassed = tc.AIAnalysis.CritiquePassed
		result.BudgetExhausted = tc.AIAnalysis.BudgetExhausted
		result.Evidence = append([]models.EvidenceCitation(nil), tc.AIAnalysis.EvidenceCitations...)
		for key, value := range tc.AIAnalysis.FileLinks {
			result.FileLinks[key] = value
		}
		scored := strings.ToLower(strings.Join([]string{result.Summary, result.RootCause, result.SuggestedFix}, "\n"))
		for _, signal := range bc.signals {
			if signal.matches(scored) {
				result.SignalHits++
			} else if signal.must {
				result.MissingMust = append(result.MissingMust, signal.name)
			}
		}
	}
	for _, trace := range snapshot.Traces {
		for _, event := range trace.Events {
			switch event.Kind {
			case "model_request":
				result.Trace.ModelRequests++
				if event.Outcome == "error" {
					result.Trace.ModelFailures++
				}
				result.Trace.InputTokens += event.InputTokens
				result.Trace.CachedInputTokens += event.CachedInputTokens
				result.Trace.OutputTokens += event.OutputTokens
			case "tool_call":
				result.Trace.ToolCalls++
				if event.Outcome == "error" {
					result.Trace.ToolFailures++
				}
			case "finalize":
				result.Trace.Finalize[event.Outcome+":"+event.ErrorCode]++
			case "finalize_recovery":
				result.Trace.FinalizeRecovery[event.Outcome]++
			case "critique":
				result.Trace.Critique["outcome:"+event.Outcome]++
				result.Trace.Critique["punts"] += event.CritiquePunts
				result.Trace.Critique["unread"] += event.CritiqueUnread
				result.Trace.Critique["citations"] += event.CritiqueCitations
				result.Trace.Critique["skills"] += event.CritiqueSkills
				result.Trace.Critique["groups"] += event.CritiqueGroups
				result.Trace.Critique["transient"] += event.CritiqueTransient
			}
		}
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open BENCH_RESULTS_JSONL: %v", err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(result); err != nil {
		t.Fatalf("write BENCH_RESULTS_JSONL: %v", err)
	}
}

func TestLoadBenchmarkManifest(t *testing.T) {
	valid := `{
  "version": 1,
  "cases": [{
    "id": "case-one",
    "stable_id": "0123456789abcdef0123",
    "bucket": "kubernetes-ci-logs",
    "job_type": "periodic",
    "job_name": "periodic-example",
    "build_id": "123456789",
    "web_url": "https://example.invalid/build/123456789/",
    "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "repo_version": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "repo_refs": {"example/project":"main"},
    "source_owner": "example",
    "source_name": "project",
    "test_name": "Example test",
    "junit_file": "junit.xml",
    "failure_message": "failed",
    "consecutive_failures": 2,
    "signals": [{"name":"cause","pattern":"(?i)root cause","must":true}]
  }]
}`
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cases, err := loadBenchmarkManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].name != "case-one" || cases[0].stableID != "0123456789abcdef0123" || !cases[0].signals[0].must {
		t.Fatalf("cases=%+v", cases)
	}

	for name, mutate := range map[string]func(string) string{
		"unknown field": func(value string) string {
			return strings.Replace(value, `"version": 1`, `"version": 1, "extra": true`, 1)
		},
		"bad stable id": func(value string) string { return strings.Replace(value, "0123456789abcdef0123", "model-name", 1) },
		"bad regexp":    func(value string) string { return strings.Replace(value, "(?i)root cause", "[", 1) },
		"second object": func(value string) string { return value + `{}` },
	} {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(bad, []byte(mutate(valid)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadBenchmarkManifest(bad); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestWriteBenchmarkJSONLIsBlindedAndPrivate(t *testing.T) {
	t.Setenv("BENCH_MODEL_LABEL", "model-a")
	path := filepath.Join(t.TempDir(), "results.jsonl")
	bc := benchCase{
		name: "case-one", stableID: "0123456789abcdef0123", jobName: "job", buildID: "123", testName: "test",
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": "main"},
		sourceRepo: [2]string{"example", "project"},
		signals:    []benchSignal{{name: "cause", re: regexp.MustCompile(`root cause`), must: true}},
	}
	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "summary"},
		AIAnalysis: &models.AIAnalysis{
			Model: "PRIVATE_MODEL", RootCause: "root cause", SuggestedFix: "fix", Severity: "High",
			FileLinks: map[string]string{"file.go": "https://example.invalid/file.go"}, ToolCalls: 3, GCSBytes: 42,
			EvidencePlanCovered: true, GCSFloorRetryExhausted: true, CritiquePassed: true, BudgetExhausted: true,
		},
	}
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Events: []ai.TraceEvent{
		{Kind: "model_request", Outcome: "success", InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2},
		{Kind: "tool_call", Outcome: "success"},
		{Kind: "finalize", Outcome: "empty", ErrorCode: "unexpected_tool_call"},
		{Kind: "finalize_recovery", Outcome: "retained_draft"},
		{Kind: "critique", Outcome: "objected", CritiquePunts: 1},
	}}}}
	cacheVerification := benchmarkCacheVerification{Attempted: true, Saved: true, Accepted: true, CacheHit: true, EvidencePlanCovered: true, GCSFloorRetryExhausted: true, CacheGeneration: "generation"}
	writeBenchmarkJSONL(t, path, bc, 2, tc, 3*time.Second, snapshot, 1,
		benchmarkToolUsage{names: []string{"read_artifact"}, counts: []string{"read_artifact=1"}},
		benchmarkTraceSummary{floorNudges: 1, floorNudgeReasons: []string{"gcs_bytes"}}, "generation", cacheVerification)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE_MODEL") {
		t.Fatalf("JSONL leaked model identity: %s", data)
	}
	var result benchmarkJSONLResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ModelLabel != "model-a" || result.Repetition != 2 || result.SignalHits != 1 || result.SourceRevision != strings.Repeat("a", 40) || result.SourceUnavailable ||
		result.Trace.Finalize["empty:unexpected_tool_call"] != 1 || result.Trace.Critique["punts"] != 1 || result.GCSBytes != 42 ||
		!result.EvidencePlanCovered || !result.GCSFloorRetryExhausted || !result.CritiquePassed || !result.BudgetExhausted ||
		result.FloorNudges != 1 || !slices.Equal(result.FloorNudgeReasons, []string{"gcs_bytes"}) ||
		!slices.Equal(result.ToolNames, []string{"read_artifact"}) || !slices.Equal(result.ToolCounts, []string{"read_artifact=1"}) ||
		!result.CacheVerification.Accepted || !result.CacheVerification.CacheHit || result.CacheGeneration != "generation" {
		t.Fatalf("result=%+v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode=%o", info.Mode().Perm())
	}
}
