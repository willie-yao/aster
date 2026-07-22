// Command orka-ingestor patches Orka analysis results back into the fetcher's
// dashboard skeleton. For every failing test in jobs/*.json it re-derives the
// producer's content-addressed Task name, fetches that Task's result from the
// Orka API, parses the analysis JSON, and writes ai_summary/ai_analysis onto the
// test case. In batch mode it also correlates analyzed builds into job-level
// recurring patterns before the frontend reads the completed dashboard data.
//
// Idempotent: re-running patches whatever results are now available and leaves
// the rest untouched, so it can run repeatedly as Tasks complete.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patterns"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir to patch in place (holds jobs/*.json)")
	projectDir := flag.String("project-dir", ".", "consumer dir with project.yaml for post-finalization side effects")
	apiBase := flag.String("api", "http://localhost:8080", "Orka API base URL")
	token := flag.String("token", "", "bearer token for the Orka API (or set -token-file)")
	tokenFile := flag.String("token-file", "", "file holding the bearer token")
	version := flag.String("version", "v1", "manual cache-bust version (must match the producer manifest)")
	provider := flag.String("provider", "copilot", "Orka Provider name for job-level pattern analysis")
	model := flag.String("model", "claude-sonnet-4.6", "model label recorded on each analysis")
	wait := flag.Duration("wait", 0, "keep polling until every failing test is patched or this deadline elapses (0 = single pass)")
	poll := flag.Duration("poll", 15*time.Second, "interval between passes when -wait is set")
	patternWait := flag.Duration("pattern-wait", 25*time.Minute, "total deadline for job-level pattern analysis (0 disables it)")
	patternPoll := flag.Duration("pattern-poll", 5*time.Second, "poll interval for job-level pattern Tasks")
	patternTimeout := flag.String("pattern-timeout", "10m", "per-Task timeout for job-level pattern analysis")
	patternRetries := flag.Int("pattern-retries", 1, "pattern Task retryPolicy maxRetries")
	taskExecutionJSON := flag.String("task-execution", "", "JSON Task.spec.execution placement with nodeSelector, tolerations, and affinity")
	namespace := flag.String("namespace", "orka-system", "namespace holding the Tasks and Tools")
	kubeContext := flag.String("context", "", "kubeconfig context for Task-status checks and GC (in-cluster config is used when empty)")
	gc := flag.Bool("gc", false, "after ingesting, delete per-build Tools whose Tasks are all terminal")
	skipSideEffects := flag.Bool("skip-side-effects", false, "finalize dashboard data without notifications or GitHub writes")
	serve := flag.Bool("serve", false, "run as a webhook receiver: patch each result as its Task completes (Task.webhookURL)")
	addr := flag.String("addr", ":8080", "listen address in -serve mode")
	flag.Parse()

	taskExecution, err := orka.ParseTaskExecution(*taskExecutionJSON)
	if err != nil {
		log.Fatalf("task execution: %v", err)
	}

	tok := strings.TrimSpace(*token)
	if tok == "" && *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("read token-file: %v", err)
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		// In-cluster default: the pod's ServiceAccount token (TokenReview accepts it).
		if b, err := os.ReadFile(saTokenPath); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}

	client := &orkaClient{base: strings.TrimRight(*apiBase, "/"), token: tok, http: &http.Client{Timeout: 30 * time.Second}}

	if *serve {
		srv := &webhookServer{client: client, dataDir: *dataDir, namespace: *namespace}
		srv.rebuildIndex()
		http.HandleFunc("/webhook", srv.handle)
		http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
			srv.mu.Lock()
			manifest := srv.manifest
			srv.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(skeletonStatus(*dataDir, manifest))
		})
		st := skeletonStatus(*dataDir, srv.manifest)
		log.Printf("🔔 webhook receiver on %s (data=%s version=%s); %d failing: %d analyzed, %d unavailable, %d pending",
			*addr, *dataDir, *version, st.Failing, st.Analyzed, st.Unavailable, st.Pending)
		server := &http.Server{Addr: *addr, Handler: http.DefaultServeMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		log.Fatal(server.ListenAndServe())
	}

	manifest, err := orka.LoadAnalysisManifest(*dataDir)
	if err != nil {
		log.Fatalf("load analysis manifest: %v", err)
	}
	if manifest.Provider != *provider || manifest.Model != *model || manifest.Version != *version {
		log.Fatalf("analysis manifest provider/model/version %s/%s/%s does not match ingestor flags %s/%s/%s",
			manifest.Provider, manifest.Model, manifest.Version, *provider, *model, *version)
	}

	// A kube client enables phase-based unavailable reasons and Tool GC. It is
	// best-effort: without cluster access the ingestor still patches results and
	// marks anything unresolved at the deadline as unavailable.
	kube := newKubeClient(*kubeContext)

	deadline := time.Now().Add(*wait)
	builds := map[string]bool{}
	for {
		final := *wait > 0 && !time.Now().Before(deadline)
		patched, failedTests, missing := ingestPass(client, kube, *namespace, *dataDir, manifest, *model, final, builds)
		log.Printf("patched %d/%d failing tests (%d results missing/unavailable)", patched, failedTests, missing)
		if *wait <= 0 || missing == 0 || final {
			break
		}
		time.Sleep(*poll)
	}

	runSideEffects := finalizedSideEffects(*skipSideEffects, *projectDir, *dataDir)
	if runSideEffects == nil {
		log.Println("Side effects: skipped (-skip-side-effects)")
	}
	if *patternWait > 0 {
		if kube == nil {
			log.Fatal("pattern finalization requires cluster access")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), *patternWait)
			analyzer := &patternTaskAnalyzer{
				kube: kube, client: client, namespace: *namespace,
				provider: *provider, model: *model, apiMode: manifest.APIMode, version: *version,
				projectScope: manifest.ProjectScope,
				timeout:      *patternTimeout, retries: *patternRetries, poll: *patternPoll,
				execution: taskExecution,
			}
			stats, err := finalizeBatch(ctx, *dataDir, analyzer, runSideEffects)
			cancel()
			if err != nil {
				log.Fatalf("pattern finalization failed: %v", err)
			} else {
				log.Printf("🔗 finalized %d pattern analyses (%d systemic, %d failed) across %d jobs",
					stats.PatternAnalyses, stats.RecurringPatterns, stats.PatternFailures, stats.Jobs)
			}
		}
	} else if _, err := finalizeBatch(context.Background(), *dataDir, nil, runSideEffects); err != nil {
		log.Fatalf("post-finalization failed: %v", err)
	}

	if *gc && kube != nil {
		gcTools(kube, *namespace, builds)
	}

	st := skeletonStatus(*dataDir, manifest)
	log.Printf("📊 %d failing tests: %d analyzed, %d unavailable, %d pending",
		st.Failing, st.Analyzed, st.Unavailable, st.Pending)
}

func finalizedSideEffects(skip bool, projectDir, dataDir string) func(context.Context) error {
	if skip {
		return nil
	}
	return func(ctx context.Context) error {
		return fetcher.RunFinalizedSideEffects(ctx, fetcher.FinalizedSideEffectsOptions{
			ProjectDir: projectDir,
			DataDir:    dataDir,
		})
	}
}

func finalizeBatch(ctx context.Context, dataDir string, analyzer patterns.Analyzer, sideEffects func(context.Context) error) (orka.FinalizeStats, error) {
	if analyzer != nil {
		return orka.FinalizePatternsAndRun(ctx, dataDir, analyzer, sideEffects)
	}
	if sideEffects != nil {
		if err := sideEffects(ctx); err != nil {
			return orka.FinalizeStats{}, fmt.Errorf("side effects: %w", err)
		}
	}
	return orka.FinalizeStats{}, nil
}

// status is the analysis coverage of the skeleton, for logs and the /status endpoint.
type status struct {
	Failing     int `json:"failing"`
	Analyzed    int `json:"analyzed"`
	Unavailable int `json:"unavailable"`
	Pending     int `json:"pending"`
}

// skeletonStatus counts, across all failing tests, how many have a real analysis,
// how many are marked unavailable, and how many are still awaiting a result.
func skeletonStatus(dataDir string, manifest *orka.AnalysisManifest) status {
	var st status
	jobFiles, _ := filepath.Glob(filepath.Join(dataDir, "jobs", "*.json"))
	for _, jf := range jobFiles {
		raw, err := os.ReadFile(jf)
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(raw, &detail) != nil {
			continue
		}
		if manifest != nil && !manifest.Jobs[detail.JobID] {
			continue
		}
		for ri := range detail.Runs {
			for ti := range detail.Runs[ri].TestCases {
				tc := &detail.Runs[ri].TestCases[ti]
				if tc.Status != "failed" {
					continue
				}
				st.Failing++
				switch {
				case tc.AIAnalysis != nil:
					st.Analyzed++
				case tc.AISummary != nil && strings.HasPrefix(tc.AISummary.Summary, unavailablePrefix):
					st.Unavailable++
				default:
					st.Pending++
				}
			}
		}
	}
	return st
}

// newKubeClient builds a KubeClient, or returns nil (logged) when unavailable.
func newKubeClient(kubeContext string) *orka.KubeClient {
	cfg, err := orka.RESTConfig(kubeContext)
	if err != nil {
		log.Printf("⚠ no cluster access (%v); running result-only, no phase reasons or GC", err)
		return nil
	}
	kc, err := orka.NewKubeClient(cfg)
	if err != nil {
		log.Printf("⚠ kube client init failed (%v); running result-only", err)
		return nil
	}
	return kc
}

// saTokenPath is the standard in-cluster ServiceAccount token mount.
const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// unavailablePrefix matches the engine's marker for a failure whose analysis
// could not complete and has no model result (internal/ai/service.go).
const unavailablePrefix = "AI analysis unavailable: "

// ingestPass patches every available result into the skeleton once. When final
// is set, still-missing failing tests are marked unavailable (with a Task-phase
// reason when a kube client is present). Distinct scoped builds are recorded
// for Tool garbage collection. Returns patched, seen, and unresolved counts.
func ingestPass(client *orkaClient, kube *orka.KubeClient, namespace, dataDir string, manifest *orka.AnalysisManifest, model string, final bool, builds map[string]bool) (patched, failedTests, missing int) {
	jobFiles, _ := filepath.Glob(filepath.Join(dataDir, "jobs", "*.json"))
	rejectionCounts := map[string]int{}
	var rejectionMu sync.Mutex
	recordRejection := func(reason string) {
		if !final || reason == "" {
			return
		}
		rejectionMu.Lock()
		rejectionCounts[reason]++
		rejectionMu.Unlock()
	}
	for _, jf := range jobFiles {
		raw, err := os.ReadFile(jf)
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(raw, &detail) != nil {
			continue
		}
		if !manifest.Jobs[detail.JobID] {
			continue
		}
		type pendingTest struct {
			tc                   *models.TestCase
			name                 string
			stale                bool
			evidencePlanComplete bool
		}
		var pending []pendingTest
		var changed atomic.Bool
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			for ti := range run.TestCases {
				tc := &run.TestCases[ti]
				if tc.Status != "failed" {
					continue
				}
				failedTests++
				ref, err := manifest.TaskRef(detail.JobID, *run, ti, *tc)
				if err != nil {
					missing++
					recordRejection("analysis Task identity is missing")
					if final && setUnavailable(tc, "analysis Task identity is missing") {
						changed.Store(true)
					}
					continue
				}
				builds[ref.ToolScope] = true
				if analysisCurrent(tc.AIAnalysis, manifest.ContractHash, ref.Name) {
					continue // already patched by an earlier pass
				}
				pending = append(pending, pendingTest{
					tc: tc, name: ref.Name, stale: tc.AIAnalysis != nil,
					evidencePlanComplete: manifest.TaskEvidencePlanComplete(ref.Name),
				})
			}
		}
		var patchedCount, missingCount atomic.Int64
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, item := range pending {
			wg.Add(1)
			sem <- struct{}{}
			go func(item pendingTest) {
				defer wg.Done()
				defer func() { <-sem }()
				accepted, rejection := applyResult(item.tc, client, namespace, item.name, model, manifest.ContractHash, manifest.APIMode, manifest.MinToolCalls, manifest.MinGCSBytes, manifest.SkillSetHash, item.evidencePlanComplete, manifest.ValidationKey)
				if accepted {
					patchedCount.Add(1)
					changed.Store(true)
					return
				}
				missingCount.Add(1)
				if final {
					if item.stale {
						item.tc.AISummary = nil
						item.tc.AIAnalysis = nil
						changed.Store(true)
					}
					if rejection != "" {
						recordRejection(rejection)
						if setUnavailable(item.tc, rejection) {
							changed.Store(true)
						}
					} else {
						updated, reason := markUnavailable(item.tc, kube, namespace, item.name)
						recordRejection(reason)
						if updated {
							changed.Store(true)
						}
					}
				}
			}(item)
		}
		wg.Wait()
		patched += int(patchedCount.Load())
		missing += int(missingCount.Load())
		if changed.Load() {
			if err := statefile.WriteJSON(jf, detail); err != nil {
				log.Printf("write %s: %v", jf, err)
			}
		}
	}
	if final && len(rejectionCounts) > 0 {
		reasons := make([]string, 0, len(rejectionCounts))
		for reason := range rejectionCounts {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			log.Printf("⚠ Orka rejection summary: %d x %s", rejectionCounts[reason], reason)
		}
	}
	return patched, failedTests, missing
}

// applyResult fetches taskName's result, parses the analysis, and patches it
// onto tc. Returns true if it patched (result available and parseable).
func applyResult(tc *models.TestCase, client *orkaClient, namespace, taskName, model, contractHash, apiMode string, minToolCalls, minGCSBytes int, skillSetHash string, evidencePlanComplete bool, validationKey string) (bool, string) {
	result, ok := client.result(namespace, taskName)
	if !ok {
		return false, ""
	}
	a, err := parseAnalysis(result)
	if err != nil {
		return false, rejectionReason("analysis Task produced an invalid result: ", err)
	}
	telemetry, err := client.analysisTelemetry(context.Background(), namespace, taskName)
	if err != nil {
		return false, rejectionReason("analysis Task telemetry unavailable: ", err)
	}
	if err := validateAnalysisAcceptance(a, telemetry, taskName, apiMode, minToolCalls, minGCSBytes, skillSetHash, evidencePlanComplete, validationKey); err != nil {
		return false, rejectionReason("analysis Task failed acceptance: ", err)
	}
	applyParsedAnalysis(tc, a, telemetry, model, contractHash, skillSetHash, taskName)
	return true, ""
}

func applyParsedAnalysis(tc *models.TestCase, a analysis, telemetry analysisTelemetry, model, contractHash, skillSetHash, taskName string) {
	now := time.Now().UTC().Format(time.RFC3339)
	gcsBytes := 0
	if a.GCSBytes != nil {
		gcsBytes = *a.GCSBytes
	}
	if telemetry.Model != "" {
		model = telemetry.Model
	}
	tc.AISummary = &models.AISummary{GeneratedAt: now, Summary: a.Summary, IsTransient: *a.IsTransient}
	tc.AIAnalysis = &models.AIAnalysis{
		GeneratedAt:            now,
		Model:                  model,
		RootCause:              a.RootCause,
		Severity:               a.Severity,
		SuggestedFix:           a.SuggestedFix,
		RelevantFiles:          a.RelevantFiles,
		Mode:                   "agentic",
		ContractHash:           contractHash,
		TaskName:               taskName,
		ToolCalls:              telemetry.ToolCalls,
		ToolFailures:           telemetry.ToolFailures,
		ModelRequests:          telemetry.ModelRequests,
		ModelFailures:          telemetry.ModelFailures,
		ContextBytes:           telemetry.ContextBytes,
		GCSBytes:               gcsBytes,
		ContextTruncations:     telemetry.ContextTruncations,
		TaskRetries:            telemetry.TaskRetries,
		TaskOutcome:            telemetry.TaskOutcome,
		StopReason:             telemetry.StopReason,
		ElapsedMs:              telemetry.ElapsedMs,
		InputTokens:            telemetry.InputTokens,
		OutputTokens:           telemetry.OutputTokens,
		BudgetExhausted:        telemetry.BudgetExhausted,
		TimelineVerified:       telemetry.TimelineVerified,
		ArtifactPathsValidated: telemetry.ValidationPassed,
		CritiquePassed:         true,
		CritiqueVersion:        orka.AcceptanceVersion,
		SkillSetHash:           skillSetHash,
	}
}

// setUnavailable stamps the engine's "unavailable" placeholder on a failing test
// with no result, mirroring internal/ai/service.go (AISummary with the prefix,
// no AIAnalysis). Returns true if it changed the test.
func setUnavailable(tc *models.TestCase, reason string) bool {
	if tc.AIAnalysis != nil {
		return false
	}
	summary := unavailablePrefix + reason
	if tc.AISummary != nil {
		if !strings.HasPrefix(tc.AISummary.Summary, unavailablePrefix) || tc.AISummary.Summary == summary {
			return false
		}
	}
	tc.AISummary = &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     summary,
		IsTransient: false,
	}
	return true
}

// markUnavailable derives the deadline/Task-phase reason (batch path) and marks
// tc unavailable.
func markUnavailable(tc *models.TestCase, kube *orka.KubeClient, namespace, taskName string) (bool, string) {
	reason := "analysis did not complete before the deadline"
	if kube != nil {
		phase, err := kube.TaskPhase(context.Background(), namespace, taskName)
		switch {
		case orka.IsNotFound(err) || (err == nil && phase == ""):
			reason = "analysis Task not found"
		case err != nil:
			// leave the deadline reason; the phase could not be read
		case phase == "Failed" || phase == "Cancelled":
			reason = "analysis Task " + strings.ToLower(phase)
		}
	}
	return setUnavailable(tc, reason), reason
}

// gcTools deletes the per-build Tool CRDs for every build whose Tasks are all
// terminal, so the base x builds Tool set does not accumulate. Tools for a build
// with a still-running Task are kept so the run can finish reading them.
func gcTools(kube *orka.KubeClient, namespace string, builds map[string]bool) {
	ctx := context.Background()
	deleted := 0
	for buildID := range builds {
		selector := orka.BuildLabel + "=" + buildID
		tasks, err := kube.ListByLabel(ctx, orka.TasksGVR, namespace, selector)
		if err != nil {
			log.Printf("⚠ GC list tasks for build %s: %v", buildID, err)
			continue
		}
		allTerminal := true
		for _, t := range tasks {
			phase, _, _ := unstructured.NestedString(t.Object, "status", "phase")
			if !orka.TerminalPhase(phase) {
				allTerminal = false
				break
			}
		}
		if !allTerminal {
			continue
		}
		tools, err := kube.ListByLabel(ctx, orka.ToolsGVR, namespace, selector)
		if err != nil {
			log.Printf("⚠ GC list tools for build %s: %v", buildID, err)
			continue
		}
		for _, tool := range tools {
			if err := kube.Delete(ctx, orka.ToolsGVR, namespace, tool.GetName()); err != nil {
				log.Printf("⚠ GC delete tool %s: %v", tool.GetName(), err)
				continue
			}
			deleted++
		}
	}
	if deleted > 0 {
		log.Printf("🧹 GC deleted %d per-build Tools for completed builds", deleted)
	}
}

// webhookPayload is the subset of Orka's Task.webhookURL POST body we use.
type webhookPayload struct {
	TaskName  string `json:"taskName"`
	Phase     string `json:"phase"`
	ResultRef *struct {
		Available bool `json:"available"`
	} `json:"resultRef,omitempty"`
}

// webhookServer patches a single result into the skeleton as each Task
// completes, driven by Orka's completion webhook instead of polling. Patches are
// serialized so concurrent deliveries never corrupt a jobs/*.json file.
type webhookServer struct {
	mu        sync.Mutex
	client    *orkaClient
	dataDir   string
	namespace string
	manifest  *orka.AnalysisManifest
	index     map[string]string
}

func (s *webhookServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var p webhookPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil || p.TaskName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if orka.TerminalPhase(p.Phase) {
		manifest, indexed, loaded := s.manifestForTask(p.TaskName)
		if !loaded || manifest == nil {
			http.Error(w, "analysis manifest unavailable", http.StatusServiceUnavailable)
			return
		}
		if !indexed {
			log.Printf("ⓘ ignoring superseded analysis Task webhook %s", p.TaskName)
			w.WriteHeader(http.StatusOK)
			return
		}
		patch := s.preparePatch(p, manifest)
		if patch.retry {
			http.Error(w, patch.reason, http.StatusServiceUnavailable)
			return
		}
		s.mu.Lock()
		s.patchTask(p, patch)
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

type preparedPatch struct {
	analysis     *analysis
	taskName     string
	telemetry    analysisTelemetry
	model        string
	contractHash string
	skillSetHash string
	reason       string
	retry        bool
}

func (s *webhookServer) manifestForTask(taskName string) (*orka.AnalysisManifest, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded := true
	if s.manifest == nil || s.index[taskName] == "" {
		loaded = s.rebuildIndex()
	}
	return s.manifest, s.index[taskName] != "", loaded
}

func (s *webhookServer) preparePatch(p webhookPayload, manifest *orka.AnalysisManifest) preparedPatch {
	if p.Phase != "Succeeded" || (p.ResultRef != nil && !p.ResultRef.Available) {
		return preparedPatch{reason: "analysis Task " + strings.ToLower(p.Phase)}
	}
	result, ok := s.client.result(s.namespace, p.TaskName)
	if !ok {
		return preparedPatch{reason: "analysis Task result is not readable yet", retry: true}
	}
	parsed, err := parseAnalysis(result)
	if err != nil {
		return preparedPatch{reason: rejectionReason("analysis Task produced an invalid result: ", err)}
	}
	telemetry, err := s.client.analysisTelemetry(context.Background(), s.namespace, p.TaskName)
	if err != nil {
		return preparedPatch{reason: rejectionReason("analysis Task telemetry unavailable: ", err), retry: true}
	}
	if err := validateAnalysisAcceptance(parsed, telemetry, p.TaskName, manifest.APIMode, manifest.MinToolCalls, manifest.MinGCSBytes, manifest.SkillSetHash, manifest.TaskEvidencePlanComplete(p.TaskName), manifest.ValidationKey); err != nil {
		retry := telemetry.EventCount == 0 || telemetry.TaskOutcome == ""
		return preparedPatch{reason: rejectionReason("analysis Task failed acceptance: ", err), retry: retry}
	}
	return preparedPatch{analysis: &parsed, telemetry: telemetry, model: manifest.Model, contractHash: manifest.ContractHash, skillSetHash: manifest.SkillSetHash}
}

func (s *webhookServer) rebuildIndex() bool {
	manifest, err := orka.LoadAnalysisManifest(s.dataDir)
	if err != nil {
		log.Printf("load analysis manifest for webhook index: %v", err)
		return false
	}
	s.manifest = manifest
	s.index = map[string]string{}
	jobFiles, _ := filepath.Glob(filepath.Join(s.dataDir, "jobs", "*.json"))
	for _, jf := range jobFiles {
		raw, err := os.ReadFile(jf)
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(raw, &detail) != nil {
			continue
		}
		if !s.manifest.Jobs[detail.JobID] {
			continue
		}
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			for ti := range run.TestCases {
				tc := &run.TestCases[ti]
				if tc.Status == "failed" {
					ref, err := s.manifest.TaskRef(detail.JobID, *run, ti, *tc)
					if err == nil {
						s.index[ref.Name] = jf
					}
				}
			}
		}
	}
	return true
}

func (s *webhookServer) patchTask(p webhookPayload, patch preparedPatch) {
	patch.taskName = p.TaskName
	jf := s.index[p.TaskName]
	if jf == "" {
		s.rebuildIndex()
		jf = s.index[p.TaskName]
	}
	if jf == "" {
		return
	}
	raw, err := os.ReadFile(jf)
	if err != nil {
		return
	}
	var detail models.JobDetail
	if json.Unmarshal(raw, &detail) != nil {
		return
	}
	for ri := range detail.Runs {
		run := &detail.Runs[ri]
		for ti := range run.TestCases {
			tc := &run.TestCases[ti]
			if tc.Status != "failed" {
				continue
			}
			ref, err := s.manifest.TaskRef(detail.JobID, *run, ti, *tc)
			if err != nil || ref.Name != p.TaskName {
				continue
			}
			if s.applyPrepared(tc, patch) {
				if err := statefile.WriteJSON(jf, detail); err != nil {
					log.Printf("write %s: %v", jf, err)
				} else {
					log.Printf("🔔 patched %s (phase=%s)", p.TaskName, p.Phase)
				}
			}
			return
		}
	}
}

func (s *webhookServer) applyPrepared(tc *models.TestCase, patch preparedPatch) bool {
	if analysisCurrent(tc.AIAnalysis, patch.contractHash, patch.taskName) {
		return false
	}
	if patch.analysis != nil {
		applyParsedAnalysis(tc, *patch.analysis, patch.telemetry, patch.model, patch.contractHash, patch.skillSetHash, patch.taskName)
		return true
	}
	tc.AISummary = nil
	tc.AIAnalysis = nil
	return setUnavailable(tc, patch.reason)
}

func analysisCurrent(a *models.AIAnalysis, contractHash, taskName string) bool {
	return a != nil && a.ContractHash == contractHash && a.TaskName == taskName
}

// analysis is the model's output JSON shape.
type analysis struct {
	Summary         string   `json:"summary"`
	RootCause       string   `json:"root_cause"`
	Severity        string   `json:"severity"`
	IsTransient     *bool    `json:"is_transient"`
	SuggestedFix    string   `json:"suggested_fix"`
	RelevantFiles   []string `json:"relevant_files"`
	ValidationToken string   `json:"validation_token"`
	GCSBytes        *int     `json:"gcs_bytes"`
}

// parseAnalysis extracts and validates the last analysis-shaped JSON object.
func parseAnalysis(text string) (analysis, error) {
	var best analysis
	found := false
	depth, start := 0, -1
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			if depth > 0 {
				inString = true
			}
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					var a analysis
					if json.Unmarshal([]byte(text[start:i+1]), &a) == nil && (a.RootCause != "" || a.Severity != "" || a.Summary != "") {
						best, found = a, true
					}
				}
			}
		}
	}
	if !found {
		return analysis{}, fmt.Errorf("no analysis JSON object found")
	}
	if err := validateAnalysisShape(best); err != nil {
		return analysis{}, err
	}
	return best, nil
}

func validateAnalysisShape(a analysis) error {
	if strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if strings.TrimSpace(a.RootCause) == "" {
		return fmt.Errorf("root_cause is required")
	}
	if a.IsTransient == nil {
		return fmt.Errorf("is_transient is required")
	}
	switch strings.ToLower(strings.TrimSpace(a.Severity)) {
	case "critical", "high", "medium", "low":
	default:
		return fmt.Errorf("severity %q is invalid", a.Severity)
	}
	if strings.TrimSpace(a.SuggestedFix) == "" {
		return fmt.Errorf("suggested_fix is required")
	}
	if a.RelevantFiles == nil {
		return fmt.Errorf("relevant_files array is required")
	}
	if a.GCSBytes == nil || *a.GCSBytes < 0 {
		return fmt.Errorf("gcs_bytes is required and must be non-negative")
	}
	if strings.TrimSpace(a.ValidationToken) == "" {
		return fmt.Errorf("validation_token is required")
	}
	return nil
}

func validateAnalysisAcceptance(a analysis, telemetry analysisTelemetry, taskName, expectedAPIMode string, minToolCalls, minGCSBytes int, skillSetHash string, evidencePlanComplete bool, validationKey string) error {
	if a.GCSBytes == nil || *a.GCSBytes < 0 {
		return fmt.Errorf("gcs_bytes is required and must be non-negative")
	}
	if !orka.VerifyAnalysisValidationToken(validationKey, taskName, a.validationInput(), *a.GCSBytes, a.ValidationToken) {
		return fmt.Errorf("validation_token does not match the final analysis")
	}
	if telemetry.EventCount == 0 {
		return fmt.Errorf("execution event stream is empty")
	}
	if telemetry.TaskOutcome != "succeeded" {
		if telemetry.TaskOutcome == "" {
			return fmt.Errorf("execution event stream has no terminal Task outcome")
		}
		return fmt.Errorf("analysis Task outcome is %s", telemetry.TaskOutcome)
	}
	if err := orka.ValidateObservedAPIMode(expectedAPIMode, telemetry.APIMode); err != nil {
		return err
	}
	if telemetry.ToolCalls < minToolCalls {
		return fmt.Errorf("only %d tool call(s), need at least %d", telemetry.ToolCalls, minToolCalls)
	}
	if *a.GCSBytes < minGCSBytes {
		return fmt.Errorf("only %d GCS byte(s), need at least %d", *a.GCSBytes, minGCSBytes)
	}
	requireEvidenceLookup := skillSetHash != "" && !evidencePlanComplete
	for name, outcome := range telemetry.qualityToolOutcomes {
		if outcome == "failed" && requiredQualityTool(name, *a.IsTransient, requireEvidenceLookup) {
			return fmt.Errorf("required quality tool %s failed without a successful retry", name)
		}
	}
	if requireEvidenceLookup && telemetry.qualityToolOutcomes["required_evidence"] != "completed" {
		return fmt.Errorf("analysis did not consult required_evidence for an incomplete initial plan")
	}
	if !telemetry.ValidationPassed {
		return fmt.Errorf("analysis did not successfully complete submit_analysis")
	}
	if *a.IsTransient && !telemetry.TimelineVerified {
		return fmt.Errorf("transient verdict did not complete verify_timeline")
	}
	return nil
}

func requiredQualityTool(name string, transient, requireEvidenceLookup bool) bool {
	switch name {
	case "validate_analysis", "submit_analysis":
		return true
	case "verify_timeline":
		return transient
	case "required_evidence":
		return requireEvidenceLookup
	default:
		return false
	}
}

func (a analysis) validationInput() orka.AnalysisValidation {
	isTransient := false
	if a.IsTransient != nil {
		isTransient = *a.IsTransient
	}
	return orka.AnalysisValidation{
		Summary: a.Summary, RootCause: a.RootCause, Severity: a.Severity,
		IsTransient: isTransient, SuggestedFix: a.SuggestedFix,
		RelevantFiles: append([]string(nil), a.RelevantFiles...),
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func rejectionReason(prefix string, err error) string {
	return prefix + oneLine(redact.URLs(err.Error()))
}

type orkaClient struct {
	base  string
	token string
	http  *http.Client
}

// result fetches a Task's result text, or ok=false if not available.
func (c *orkaClient) result(namespace, taskName string) (string, bool) {
	endpoint := c.base + "/api/v1/tasks/" + url.PathEscape(taskName) + "/result"
	if namespace != "" {
		endpoint += "?namespace=" + url.QueryEscape(namespace)
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", false
	}
	var wrap struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &wrap) != nil || strings.TrimSpace(wrap.Result) == "" {
		return "", false
	}
	return wrap.Result, true
}

type patternTaskAnalyzer struct {
	kube         patternKubeClient
	client       *orkaClient
	namespace    string
	provider     string
	model        string
	apiMode      string
	version      string
	projectScope string
	timeout      string
	retries      int
	poll         time.Duration
	execution    map[string]any
}

type patternKubeClient interface {
	orka.TaskExecutionClient
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	TaskPhase(context.Context, string, string) (string, error)
}

func (a *patternTaskAnalyzer) AnalyzePattern(ctx context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	input := ai.BuildPatternInput(subject, failures)
	if len(input.Failures) < 2 {
		return nil, nil
	}
	fingerprint := strings.Join([]string{
		a.projectScope,
		a.provider,
		a.model,
		a.apiMode,
		a.timeout,
		strconv.Itoa(a.retries),
		input.SystemPrompt,
		input.UserPrompt,
	}, "\x00")
	name := orka.PatternTaskName(jobID, fingerprint, a.version)
	task := orka.BuildAITask(orka.AITaskSpec{
		Name: name, Namespace: a.namespace, Provider: a.provider, Model: a.model, APIMode: a.apiMode,
		Timeout: a.timeout, MaxRetries: a.retries,
		SystemPrompt: input.SystemPrompt, Prompt: input.UserPrompt,
		Labels: map[string]string{
			orka.ManagedByLabel: orka.ManagedByValue,
			orka.ProjectLabel:   a.projectScope,
			orka.TaskTypeLabel:  "pattern",
		},
		Execution: a.execution,
	})
	poll := a.poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	skipApply, err := orka.PrepareTaskExecution(ctx, a.kube, a.namespace, name, a.execution, poll)
	if err != nil {
		return nil, fmt.Errorf("prepare pattern Task %s: %w", name, err)
	}
	if !skipApply {
		if err := a.kube.Apply(ctx, orka.TasksGVR, a.namespace, task); err != nil {
			return nil, fmt.Errorf("apply pattern Task %s: %w", name, err)
		}
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastTelemetryErr error
	for {
		if result, ok := a.client.result(a.namespace, name); ok {
			telemetry, telemetryErr := a.client.analysisTelemetry(ctx, a.namespace, name)
			if telemetryErr != nil {
				lastTelemetryErr = fmt.Errorf("pattern Task %s telemetry: %w", name, telemetryErr)
			} else if err := orka.ValidateObservedAPIMode(a.apiMode, telemetry.APIMode); err != nil {
				lastTelemetryErr = fmt.Errorf("pattern Task %s API mode: %w", name, err)
			} else {
				return ai.ParsePatternResult(subject, input.Failures, result)
			}
		}
		phase, err := a.kube.TaskPhase(ctx, a.namespace, name)
		if err == nil {
			switch phase {
			case "Failed", "Cancelled":
				return nil, fmt.Errorf("pattern Task %s %s", name, strings.ToLower(phase))
			case "Succeeded":
				if lastTelemetryErr != nil && !errors.Is(lastTelemetryErr, errTaskEventsNotReadableYet) {
					return nil, lastTelemetryErr
				}
			}
		}
		select {
		case <-ctx.Done():
			if lastTelemetryErr != nil {
				return nil, fmt.Errorf("%w: %w", lastTelemetryErr, ctx.Err())
			}
			return nil, fmt.Errorf("pattern Task %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}
