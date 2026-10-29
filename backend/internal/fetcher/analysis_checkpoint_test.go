package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type checkpointAnalyzerFunc func(context.Context, *http.Client, ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error)

func (f checkpointAnalyzerFunc) AnalyzeFailure(ctx context.Context, client *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	return f(ctx, client, request)
}

// checkpointDeadlineContext lets a worker expire the pass without a wall-clock race.
type checkpointDeadlineContext struct {
	context.Context
	done chan struct{}
}

func (c *checkpointDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *checkpointDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func runCheckpointPass(t *testing.T, p *pipeline, ctx context.Context, watch bool) error {
	t.Helper()
	if watch {
		jobs, err := p.discover(ctx)
		if err != nil {
			return err
		}
		return p.watchPass(ctx, jobs)
	}
	_, err := p.fullPass(ctx)
	return err
}

func TestPassDeadlineCheckpoint(t *testing.T) {
	for _, watch := range []bool{false, true} {
		for _, ending := range []string{"deadline", "cancel", "cache save failure", "trace save failure", "commit failure", "rescued accepted", "rescued preliminary"} {
			t.Run(fmt.Sprintf("watch=%t/%s", watch, ending), func(t *testing.T) {
				dataDir, bucketDir := installRefreshLifecycleFixture(t)
				writeFixtureFile(t, bucketDir, "logs/periodic-test/1/build-log.txt", "configuration rejected\n")
				writeFixtureFile(t, bucketDir, "logs/periodic-test/1/artifacts/junit.xml", `<testsuite name="suite"><testcase name="completed"><failure message="configuration rejected"/></testcase><testcase name="unfinished"><failure message="different failure"/></testcase></testsuite>`)
				before := hashFileTree(t, dataDir)
				deadline := &checkpointDeadlineContext{Context: t.Context(), done: make(chan struct{})}
				var ctx context.Context = deadline
				stop := func() { close(deadline.done) }
				if ending == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(t.Context())
					defer cancel()
					stop = cancel
				}
				rescued := strings.HasPrefix(ending, "rescued")
				preliminary := ending == "rescued preliminary"
				interruptedCall := int64(3)
				if rescued {
					interruptedCall = 5
				}
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					_, _ = io.Copy(io.Discard, request.Body)
					call := calls.Add(1)
					if call == interruptedCall {
						stop()
						<-request.Context().Done()
						return
					}
					if call > interruptedCall {
						call--
					}
					message := map[string]any{"role": "assistant"}
					reason := "stop"
					toolCalls := []any{map[string]any{
						"id": "read", "type": "function",
						"function": map[string]any{"name": "grep_artifact", "arguments": `{"path":"build-log.txt","pattern":"configuration rejected","context_lines":0}`},
					}}
					if call%2 == 1 {
						reason = "tool_calls"
						message["tool_calls"] = toolCalls
					} else {
						message["content"] = `{"summary":"configuration rejected","is_transient":false,"root_cause":"The build configuration was rejected.","severity":"High","suggested_fix":"Correct the rejected build configuration.","relevant_files":[],"evidence_citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"configuration rejected"}]}`
						if preliminary && call >= 4 {
							message["content"] = `{"summary":"configuration rejected","is_transient":false,"root_cause":"The build configuration was rejected.","severity":"High","suggested_fix":"Correct the rejected build configuration.","relevant_files":[],"evidence_citations":[]}`
						}
						if rescued && call == 4 {
							reason = "tool_calls"
							message["tool_calls"] = toolCalls
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"choices": []any{map[string]any{"finish_reason": reason, "message": message}},
					})
				}))
				defer server.Close()

				p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
				p.cfg.AI.Concurrency = 2
				p.cfg.AI.Agentic.MinToolCalls = 1
				configureRefreshLifecycleRuntime(t, p, dataDir)
				p.aiProject.Provider.Endpoint = server.URL
				p.aiRuntime = nil
				p.progress = fetchprogress.New(dataDir, "sha-test")
				p.progress.StartPass(fetchprogress.PassOneShot)

				completed := make(chan struct{})
				var accepted, unfinished ai.FailureAnalysisResult
				var unfinishedErr error
				var completedRequest, unfinishedRequest ai.FailureAnalysisRequest
				var transaction *aiRefreshStateTransaction
				oldAnalyzer := newAnalysisAnalyzer
				newAnalysisAnalyzer = func(service *ai.Service) ai.FailureAnalyzer {
					transaction = p.aiRefreshTransaction
					return checkpointAnalyzerFunc(func(ctx context.Context, client *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
						if strings.Contains(request.TestCase.Name, "unfinished") {
							unfinishedRequest = request
							<-completed
							result, err := service.AnalyzeFailure(ctx, client, request)
							unfinished = result
							unfinishedErr = err
							return result, err
						}
						completedRequest = request
						result, err := service.AnalyzeFailure(ctx, client, request)
						accepted = result
						close(completed)
						return result, err
					})
				}
				oldPatterns, oldWrite, oldEmail := analyzePatternsAcrossBuilds, writeAllOutput, newEmailSender
				var patternCalls, publicationCalls int
				analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error {
					patternCalls++
					return nil
				}
				writeAllOutput = func(string, *project.Config, models.Dashboard, []models.JobDetail, models.FlakinessReport, models.SearchIndex) error {
					publicationCalls++
					return errors.New("unexpected publication")
				}
				sender := &countingNotifySender{}
				newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return sender, nil }
				oldSaveCache, oldSaveTraces := saveAnalysisRuntimeCache, saveAnalysisTraceStore
				saveErr := errors.New("checkpoint disk full")
				switch ending {
				case "cache save failure":
					saveAnalysisRuntimeCache = func(runtime *analysisruntime.Runtime) error {
						if err := oldSaveCache(runtime); err != nil {
							return err
						}
						return saveErr
					}
				case "trace save failure", "commit failure":
					saveAnalysisTraceStore = func(store *ai.TraceStore, path string) error {
						if err := oldSaveTraces(store, path); err != nil {
							return err
						}
						if ending == "commit failure" {
							p.aiRefreshTransaction.outDir = filepath.Join(dataDir, ai.CacheFilename)
							return nil
						}
						return saveErr
					}
				}
				t.Cleanup(func() {
					newAnalysisAnalyzer = oldAnalyzer
					analyzePatternsAcrossBuilds, writeAllOutput, newEmailSender = oldPatterns, oldWrite, oldEmail
					saveAnalysisRuntimeCache, saveAnalysisTraceStore = oldSaveCache, oldSaveTraces
				})

				err := runCheckpointPass(t, p, ctx, watch)
				wantErr := context.DeadlineExceeded
				if ending == "cancel" {
					wantErr = context.Canceled
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("pass error = %v, want %v", err, wantErr)
				}
				if strings.HasSuffix(ending, "save failure") && !errors.Is(err, saveErr) {
					t.Fatalf("pass lost storage error: %v", err)
				}
				if ending == "commit failure" && (!errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "committing completed analysis checkpoint")) {
					t.Fatalf("pass lost checkpoint commit error: %v", err)
				}
				if accepted.Analysis == nil || !ai.AnalysisCitationsVerified(accepted.Analysis) || !accepted.Analysis.CachePersistenceAccepted || calls.Load() != interruptedCall {
					t.Fatalf("completed analysis = %+v, provider calls = %d", accepted.Analysis, calls.Load())
				}
				if rescued {
					disposition := models.AnalysisDispositionCitationsVerified
					wantCacheAccepted := !preliminary
					if preliminary {
						disposition = models.AnalysisDispositionPreliminary
					}
					analysis := unfinished.Analysis
					if unfinishedErr != nil || analysis == nil || analysis.Disposition != disposition || analysis.CachePersistenceAccepted != wantCacheAccepted {
						t.Fatalf("rescued analysis = %+v, error=%v", analysis, unfinishedErr)
					}
					if preliminary && analysis.CachePolicyRejectionReason != string(ai.CacheRejectedCritiqueHardFailure) {
						t.Fatalf("rescued preliminary cache policy = %+v", analysis)
					}
				} else if unfinished.Analysis != nil {
					t.Fatalf("unfinished analysis produced a successful entry: %+v", unfinished.Analysis)
				}
				if p.aiRuntime != nil {
					t.Fatal("failed pass retained the in-memory runtime")
				}
				if patternCalls != 0 || publicationCalls != 0 || sender.calls.Load() != 0 {
					t.Fatalf("post-analysis calls: patterns=%d publication=%d email=%d", patternCalls, publicationCalls, sender.calls.Load())
				}
				committed := ending == "deadline" || rescued
				status := p.progress.Snapshot()
				if transaction == nil || transaction.checkpointCommitted != committed || status.Analyses.CheckpointCommitted != committed {
					t.Fatalf("checkpoint transaction=%+v progress=%+v", transaction, status.Analyses)
				}
				wantCompleted, wantCancelled := 1, 1
				if rescued {
					wantCompleted, wantCancelled = 2, 0
				}
				if status.Analyses.Completed != wantCompleted || status.Analyses.Cancelled != wantCancelled || status.Analyses.Running != 0 || status.Analyses.Queued != 0 {
					t.Fatalf("analysis progress = %+v", status.Analyses)
				}
				after := hashFileTree(t, dataDir)
				if !committed {
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("uncommitted pass changed files\nbefore=%v\nafter=%v", before, after)
					}
					return
				}
				for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
					if after[name] == before[name] {
						t.Fatalf("checkpoint did not update %s", name)
					}
					delete(before, name)
					delete(after, name)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("deadline changed non-checkpoint files\nbefore=%v\nafter=%v", before, after)
				}
				traces, err := ai.LoadTraceStore(filepath.Join(dataDir, output.AITraceFilename))
				if err != nil {
					t.Fatal(err)
				}
				snapshot := traces.Snapshot()
				if len(snapshot.Traces) != 2 {
					t.Fatalf("saved traces = %+v", snapshot.Traces)
				}
				for _, trace := range snapshot.Traces {
					if strings.Contains(trace.TestName, "unfinished") {
						if (trace.Outcome == "success") != rescued {
							t.Fatalf("interrupted analysis trace = %+v", trace)
						}
						if rescued {
							var recovered bool
							for _, event := range trace.Events {
								if event.Kind == "draft_recovery" && event.Outcome == "model_request_error" && event.SelectedAttempt > 0 {
									recovered = true
								}
							}
							if !recovered {
								t.Fatalf("rescued analysis missing draft recovery event: %+v", trace)
							}
						}
					}
					if trace.TestName == completedRequest.TestCase.Name && trace.Outcome != "success" {
						t.Fatalf("completed analysis trace = %+v", trace)
					}
				}
				resultKey := ai.AgenticCacheKeyForGeneration("universal", unfinishedRequest.CacheGeneration, unfinishedRequest.JobID, unfinishedRequest.Build.BuildID, unfinishedRequest.TestCase.Name, unfinishedRequest.TestCase.FailureMessage)
				checkAttempts := func(want int) {
					t.Helper()
					cache := ai.NewCache(dataDir)
					entry, ok := cache.Lookup(resultKey + ":preliminary-attempts")
					var data struct {
						Attempts int `json:"attempts"`
					}
					if ok {
						if err := json.Unmarshal(entry.Data, &data); err != nil {
							t.Fatal(err)
						}
					}
					if data.Attempts != want || ok != (want > 0) {
						t.Fatalf("persisted preliminary attempts=%d present=%t, want %d", data.Attempts, ok, want)
					}
					if preliminary {
						if _, ok := cache.Lookup(resultKey); ok {
							t.Fatal("hard-rejected preliminary draft was saved as an accepted result")
						}
					}
				}
				wantAttempts := 0
				if preliminary {
					wantAttempts = 1
				}
				checkAttempts(wantAttempts)

				reloaded, err := analysisruntime.New(t.Context(), analysisruntime.Options{DataDir: dataDir, Project: p.aiProject})
				if err != nil {
					t.Fatal(err)
				}
				service, err := reloaded.NewService(analysisruntime.ServiceOptions{Backend: p.backend, TraceStore: ai.NewTraceStore()})
				if err != nil {
					t.Fatal(err)
				}
				result, err := service.AnalyzeFailure(t.Context(), p.client, completedRequest)
				if err != nil || result.Analysis == nil || !result.Analysis.CacheHit || calls.Load() != interruptedCall {
					t.Fatalf("completed reload = %+v, error=%v calls=%d", result.Analysis, err, calls.Load())
				}
				result, err = service.AnalyzeFailure(t.Context(), p.client, unfinishedRequest)
				wantCacheHit := rescued && !preliminary
				wantCalls := interruptedCall
				if !wantCacheHit {
					wantCalls += 2
				}
				if err != nil || result.Analysis == nil || result.Analysis.CacheHit != wantCacheHit || calls.Load() != wantCalls {
					t.Fatalf("unfinished retry = %+v, error=%v calls=%d", result.Analysis, err, calls.Load())
				}
				if preliminary {
					if result.Analysis.Disposition != models.AnalysisDispositionPreliminary || result.Analysis.CachePersistenceAccepted {
						t.Fatalf("retried preliminary analysis = %+v", result.Analysis)
					}
					if err := reloaded.SaveCache(); err != nil {
						t.Fatal(err)
					}
					checkAttempts(2)
				} else if !ai.AnalysisCitationsVerified(result.Analysis) {
					t.Fatalf("reloaded analysis disposition = %q", result.Analysis.Disposition)
				}
			})
		}
	}
}

func TestPerAnalysisDeadlineDoesNotAbortPass(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			dataDir, bucketDir := installRefreshLifecycleFixture(t)
			p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
			p.opts.SkipSideEffects = true
			configureRefreshLifecycleRuntime(t, p, dataDir)
			runtime := p.aiRuntime
			p.progress = fetchprogress.New(dataDir, "sha-test")
			p.progress.StartPass(fetchprogress.PassOneShot)
			oldAnalyzer, oldPatterns := newAnalysisAnalyzer, analyzePatternsAcrossBuilds
			var analysisErr error
			newAnalysisAnalyzer = func(service *ai.Service) ai.FailureAnalyzer {
				return checkpointAnalyzerFunc(func(ctx context.Context, client *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
					analysisCtx, cancel := context.WithTimeout(ctx, -time.Nanosecond)
					defer cancel()
					result, err := service.AnalyzeFailure(analysisCtx, client, request)
					analysisErr = err
					return result, err
				})
			}
			var patternCalls int
			analyzePatternsAcrossBuilds = func(context.Context, *ai.Service, []models.JobDetail, patterns.AnalyzeOptions) error {
				patternCalls++
				return nil
			}
			t.Cleanup(func() {
				newAnalysisAnalyzer, analyzePatternsAcrossBuilds = oldAnalyzer, oldPatterns
			})

			if err := runCheckpointPass(t, p, t.Context(), watch); err != nil {
				t.Fatalf("pass error = %v", err)
			}
			if !errors.Is(analysisErr, context.DeadlineExceeded) || t.Context().Err() != nil {
				t.Fatalf("analysis error=%v parent error=%v", analysisErr, t.Context().Err())
			}
			status := p.progress.Snapshot()
			if patternCalls != 1 || p.aiRuntime != runtime || !status.Analyses.CheckpointCommitted || status.PublicationPhase != fetchprogress.StageCompleted {
				t.Fatalf("live pass patterns=%d runtime retained=%t progress=%+v", patternCalls, p.aiRuntime == runtime, status)
			}
			details, err := loadPublishedJobDetails(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			tc := details["periodic-test"].Runs[0].TestCases[0]
			if tc.AIAnalysis != nil || tc.AISummary == nil || !strings.HasPrefix(tc.AISummary.Summary, "AI analysis unavailable: ") {
				t.Fatalf("timed-out per-failure result = %+v", tc)
			}
		})
	}
}

func TestPatternDeadlineCheckpointSurvivesRollback(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			dataDir, bucketDir := installRefreshLifecycleFixture(t)
			analyzer := &resultLifecycleAnalyzer{result: ai.FailureAnalysisResult{
				Summary: &models.AISummary{Summary: "analyzed"},
				Analysis: &models.AIAnalysis{
					RootCause: "shared cause", SuggestedFix: "update configuration", Severity: "High",
					Mode: ai.AgenticMode, Disposition: models.AnalysisDispositionCitationsVerified,
				},
			}}
			p := refreshLifecyclePipeline(t, dataDir, bucketDir, analyzer)
			configureRefreshLifecycleRuntime(t, p, dataDir)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{
						"finish_reason": "tool_calls",
						"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
							"id": "pattern", "type": "function", "function": map[string]any{
								"name":      "submit_causal_groups",
								"arguments": `{"groups":[{"builds":["3","2"],"root_cause":"shared cause","confidence":"high"}],"unclassified_builds":["1"],"summary":"shared failure"}`,
							},
						}}},
					}},
				})
			}))
			defer server.Close()
			p.aiProject.Provider.Endpoint = server.URL
			p.aiRuntime = nil
			ctx := &checkpointDeadlineContext{Context: t.Context(), done: make(chan struct{})}
			oldPatterns := analyzePatternsAcrossBuilds
			analyzePatternsAcrossBuilds = func(ctx context.Context, service *ai.Service, _ []models.JobDetail, _ patterns.AnalyzeOptions) error {
				stats, err := patterns.AnalyzeWithOptions(ctx, service, []models.JobDetail{patternCheckpointJob("completed")}, patterns.AnalyzeOptions{})
				if err != nil || stats.Completed != 1 {
					return fmt.Errorf("pattern result %+v: %w", stats, err)
				}
				return context.DeadlineExceeded
			}
			oldSave := saveAnalysisRuntimeCache
			var saves int
			saveAnalysisRuntimeCache = func(runtime *analysisruntime.Runtime) error {
				saves++
				if saves == 2 {
					close(ctx.done)
				}
				return oldSave(runtime)
			}
			t.Cleanup(func() {
				analyzePatternsAcrossBuilds, saveAnalysisRuntimeCache = oldPatterns, oldSave
			})
			before := hashFileTree(t, dataDir)

			if err := runCheckpointPass(t, p, ctx, watch); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pass error = %v", err)
			}
			if p.aiRuntime != nil || calls.Load() != 1 || saves != 2 {
				t.Fatalf("runtime invalidated=%t provider calls=%d saves=%d", p.aiRuntime == nil, calls.Load(), saves)
			}
			after := hashFileTree(t, dataDir)
			for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
				delete(before, name)
				delete(after, name)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("pattern deadline changed non-checkpoint files")
			}
			runtime, err := p.ensureAnalysisRuntime(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			service, err := runtime.NewService(analysisruntime.ServiceOptions{Backend: p.backend, TraceStore: ai.NewTraceStore()})
			if err != nil {
				t.Fatal(err)
			}
			stats, err := patterns.AnalyzeWithOptions(t.Context(), service, []models.JobDetail{patternCheckpointJob("completed")}, patterns.AnalyzeOptions{})
			if err != nil || stats.CacheHits != 1 || calls.Load() != 1 {
				t.Fatalf("reloaded pattern=%+v error=%v provider calls=%d", stats, err, calls.Load())
			}
		})
	}
}

func TestSuccessfulPatternCacheSurvivesAnotherJobFailure(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "65536")
	valid := `{"groups":[{"builds":["3","2"],"root_cause":"shared cause","confidence":"high"}],"unclassified_builds":["1"],"summary":"shared failure"}`
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"finish_reason": "tool_calls",
					"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
						"id": "pattern", "type": "function", "function": map[string]any{"name": "submit_causal_groups", "arguments": valid},
					}}},
				}},
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		AI:      &project.AI{Agentic: project.Agentic{Tools: []string{"filesystem"}}},
		Storage: project.Storage{Provider: string(storage.ProviderLocal), Base: bucketDir},
	}
	aiProject := &analysisruntime.Project{
		Config: cfg,
		Provider: project.AIProvider{
			API: project.AIAPIChatCompletions, Endpoint: server.URL, Model: "checkpoint-test",
		},
		SystemPrompt: "test prompt",
	}
	runtime, err := analysisruntime.New(t.Context(), analysisruntime.Options{Token: "token", DataDir: dataDir, Project: aiProject})
	if err != nil {
		t.Fatal(err)
	}
	traces := ai.NewTraceStore()
	service, err := runtime.NewService(analysisruntime.ServiceOptions{Backend: backend, TraceStore: traces})
	if err != nil {
		t.Fatal(err)
	}
	details := []models.JobDetail{patternCheckpointJob("job-a"), patternCheckpointJob("job-b")}
	stats, patternErr := patterns.AnalyzeWithOptions(t.Context(), service, details, patterns.AnalyzeOptions{})
	if patternErr == nil || stats.Completed != 1 || stats.Failed != 1 {
		t.Fatalf("stats=%+v error=%v", stats, patternErr)
	}
	producer := ai.TraceEngine{Version: "v1.2.3", Commit: "0123456789abcdef", ImageTag: "sha-0123456"}
	if err := (&pipeline{opts: Options{OutDir: dataDir, TraceEngine: producer}}).persistRuntimeAnalysisState(runtime, traces); err != nil {
		t.Fatal(err)
	}
	persistedTraces, err := ai.LoadTraceStore(filepath.Join(dataDir, output.AITraceFilename))
	if err != nil {
		t.Fatal(err)
	}
	if engine := persistedTraces.Snapshot().Engine; engine == nil || *engine != producer {
		t.Fatalf("trace engine = %+v", engine)
	}

	callsBeforeReload := calls.Load()
	reloaded, err := analysisruntime.New(t.Context(), analysisruntime.Options{Token: "token", DataDir: dataDir, Project: aiProject})
	if err != nil {
		t.Fatal(err)
	}
	reloadedService, err := reloaded.NewService(analysisruntime.ServiceOptions{Backend: backend, TraceStore: ai.NewTraceStore()})
	if err != nil {
		t.Fatal(err)
	}
	reloadedDetails := []models.JobDetail{patternCheckpointJob("job-a")}
	reloadedStats, err := patterns.AnalyzeWithOptions(t.Context(), reloadedService, reloadedDetails, patterns.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reloadedStats.CacheHits != 1 || reloadedStats.Completed != 1 || calls.Load() != callsBeforeReload {
		t.Fatalf("reloaded stats=%+v calls=%d before=%d", reloadedStats, calls.Load(), callsBeforeReload)
	}
}

func patternCheckpointJob(jobID string) models.JobDetail {
	detail := models.JobDetail{Name: jobID, JobID: jobID, JobType: models.JobTypePeriodic}
	for _, buildID := range []string{"3", "2", "1"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name: "failed test", Status: "failed", FailureMessage: fmt.Sprintf("failure %s", buildID),
				AISummary: &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{
					RootCause: "shared cause", SuggestedFix: "update configuration", Severity: "High", Mode: "agentic",
					Disposition: models.AnalysisDispositionCitationsVerified,
				},
			}},
		})
	}
	return detail
}
