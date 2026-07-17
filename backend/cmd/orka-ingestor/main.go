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
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir to patch in place (holds jobs/*.json)")
	apiBase := flag.String("api", "http://localhost:8080", "Orka API base URL")
	token := flag.String("token", "", "bearer token for the Orka API (or set -token-file)")
	tokenFile := flag.String("token-file", "", "file holding the bearer token")
	version := flag.String("version", "v1", "content-address version suffix (must match the producer run)")
	provider := flag.String("provider", "copilot", "Orka Provider name for job-level pattern analysis")
	model := flag.String("model", "claude-sonnet-4.5", "model label recorded on each analysis")
	wait := flag.Duration("wait", 0, "keep polling until every failing test is patched or this deadline elapses (0 = single pass)")
	poll := flag.Duration("poll", 15*time.Second, "interval between passes when -wait is set")
	patternWait := flag.Duration("pattern-wait", 25*time.Minute, "total deadline for job-level pattern analysis (0 disables it)")
	patternPoll := flag.Duration("pattern-poll", 5*time.Second, "poll interval for job-level pattern Tasks")
	patternTimeout := flag.String("pattern-timeout", "10m", "per-Task timeout for job-level pattern analysis")
	patternRetries := flag.Int("pattern-retries", 1, "pattern Task retryPolicy maxRetries")
	namespace := flag.String("namespace", "orka-system", "namespace holding the Tasks and Tools")
	kubeContext := flag.String("context", "", "kubeconfig context for Task-status checks and GC (in-cluster config is used when empty)")
	gc := flag.Bool("gc", false, "after ingesting, delete per-build Tools whose Tasks are all terminal")
	serve := flag.Bool("serve", false, "run as a webhook receiver: patch each result as its Task completes (Task.webhookURL)")
	addr := flag.String("addr", ":8080", "listen address in -serve mode")
	flag.Parse()

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
		srv := &webhookServer{client: client, dataDir: *dataDir, namespace: *namespace, version: *version, model: *model}
		srv.rebuildIndex()
		http.HandleFunc("/webhook", srv.handle)
		http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(skeletonStatus(*dataDir, *version))
		})
		st := skeletonStatus(*dataDir, *version)
		log.Printf("🔔 webhook receiver on %s (data=%s version=%s); %d failing: %d analyzed, %d unavailable, %d pending",
			*addr, *dataDir, *version, st.Failing, st.Analyzed, st.Unavailable, st.Pending)
		server := &http.Server{Addr: *addr, Handler: http.DefaultServeMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		log.Fatal(server.ListenAndServe())
	}

	// A kube client enables phase-based unavailable reasons and Tool GC. It is
	// best-effort: without cluster access the ingestor still patches results and
	// marks anything unresolved at the deadline as unavailable.
	kube := newKubeClient(*kubeContext)

	deadline := time.Now().Add(*wait)
	builds := map[string]bool{}
	for {
		final := *wait > 0 && !time.Now().Before(deadline)
		patched, failedTests, missing := ingestPass(client, kube, *namespace, *dataDir, *version, *model, final, builds)
		log.Printf("patched %d/%d failing tests (%d results missing/unavailable)", patched, failedTests, missing)
		if *wait <= 0 || missing == 0 || final {
			break
		}
		time.Sleep(*poll)
	}

	if *patternWait > 0 {
		if kube == nil {
			log.Printf("⚠ pattern finalization skipped: no cluster access")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), *patternWait)
			analyzer := &patternTaskAnalyzer{
				kube: kube, client: client, namespace: *namespace,
				provider: *provider, model: *model, version: *version,
				timeout: *patternTimeout, retries: *patternRetries, poll: *patternPoll,
			}
			stats, err := orka.FinalizePatterns(ctx, *dataDir, analyzer)
			cancel()
			if err != nil {
				log.Printf("⚠ pattern finalization failed: %v", err)
			} else {
				log.Printf("🔗 finalized %d pattern analyses (%d systemic, %d failed) across %d jobs",
					stats.PatternAnalyses, stats.RecurringPatterns, stats.PatternFailures, stats.Jobs)
			}
		}
	}

	if *gc && kube != nil {
		gcTools(kube, *namespace, builds)
	}

	st := skeletonStatus(*dataDir, *version)
	log.Printf("📊 %d failing tests: %d analyzed, %d unavailable, %d pending",
		st.Failing, st.Analyzed, st.Unavailable, st.Pending)
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
func skeletonStatus(dataDir, version string) status {
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
// reason when a kube client is present). Distinct build IDs are recorded in
// builds. Returns how many failing tests it patched, saw, and left unresolved.
func ingestPass(client *orkaClient, kube *orka.KubeClient, namespace, dataDir, version, model string, final bool, builds map[string]bool) (patched, failedTests, missing int) {
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
		type pendingTest struct {
			tc   *models.TestCase
			name string
		}
		var pending []pendingTest
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			for ti := range run.TestCases {
				tc := &run.TestCases[ti]
				if tc.Status != "failed" {
					continue
				}
				failedTests++
				builds[run.BuildID] = true
				if tc.AIAnalysis != nil {
					continue // already patched by an earlier pass
				}
				name := orka.TaskName(run.BuildID, orka.FailureHash(tc.Name, tc.FailureMessage), version)
				pending = append(pending, pendingTest{tc: tc, name: name})
			}
		}
		var patchedCount, missingCount atomic.Int64
		var changed atomic.Bool
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, item := range pending {
			wg.Add(1)
			sem <- struct{}{}
			go func(item pendingTest) {
				defer wg.Done()
				defer func() { <-sem }()
				if applyResult(item.tc, client, namespace, item.name, model) {
					patchedCount.Add(1)
					changed.Store(true)
					return
				}
				missingCount.Add(1)
				if final && markUnavailable(item.tc, kube, namespace, item.name) {
					changed.Store(true)
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
	return patched, failedTests, missing
}

// applyResult fetches taskName's result, parses the analysis, and patches it
// onto tc. Returns true if it patched (result available and parseable).
func applyResult(tc *models.TestCase, client *orkaClient, namespace, taskName, model string) bool {
	result, ok := client.result(namespace, taskName)
	if !ok {
		return false
	}
	a, ok := parseAnalysis(result)
	if !ok {
		return false
	}
	applyParsedAnalysis(tc, a, model)
	return true
}

func applyParsedAnalysis(tc *models.TestCase, a analysis, model string) {
	now := time.Now().UTC().Format(time.RFC3339)
	tc.AISummary = &models.AISummary{GeneratedAt: now, Summary: a.RootCause, IsTransient: a.IsTransient}
	tc.AIAnalysis = &models.AIAnalysis{
		GeneratedAt:   now,
		Model:         model,
		RootCause:     a.RootCause,
		Severity:      a.Severity,
		SuggestedFix:  a.SuggestedFix,
		RelevantFiles: a.RelevantFiles,
		Mode:          "agentic",
	}
}

// setUnavailable stamps the engine's "unavailable" placeholder on a failing test
// with no result, mirroring internal/ai/service.go (AISummary with the prefix,
// no AIAnalysis). Returns true if it changed the test.
func setUnavailable(tc *models.TestCase, reason string) bool {
	if tc.AISummary != nil || tc.AIAnalysis != nil {
		return false
	}
	tc.AISummary = &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     unavailablePrefix + reason,
		IsTransient: false,
	}
	return true
}

// markUnavailable derives the deadline/Task-phase reason (batch path) and marks
// tc unavailable.
func markUnavailable(tc *models.TestCase, kube *orka.KubeClient, namespace, taskName string) bool {
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
	return setUnavailable(tc, reason)
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
	version   string
	model     string
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
		patch := s.preparePatch(p)
		s.mu.Lock()
		s.patchTask(p, patch)
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

type preparedPatch struct {
	analysis *analysis
	reason   string
}

func (s *webhookServer) preparePatch(p webhookPayload) preparedPatch {
	if p.Phase != "Succeeded" || (p.ResultRef != nil && !p.ResultRef.Available) {
		return preparedPatch{reason: "analysis Task " + strings.ToLower(p.Phase)}
	}
	result, ok := s.client.result(s.namespace, p.TaskName)
	if !ok {
		return preparedPatch{reason: "analysis Task produced no readable result"}
	}
	parsed, ok := parseAnalysis(result)
	if !ok {
		return preparedPatch{reason: "analysis Task produced no parseable result"}
	}
	return preparedPatch{analysis: &parsed}
}

func (s *webhookServer) rebuildIndex() {
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
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			for ti := range run.TestCases {
				tc := &run.TestCases[ti]
				if tc.Status == "failed" {
					name := orka.TaskName(run.BuildID, orka.FailureHash(tc.Name, tc.FailureMessage), s.version)
					s.index[name] = jf
				}
			}
		}
	}
}

func (s *webhookServer) patchTask(p webhookPayload, patch preparedPatch) {
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
			name := orka.TaskName(run.BuildID, orka.FailureHash(tc.Name, tc.FailureMessage), s.version)
			if name != p.TaskName {
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
	if tc.AIAnalysis != nil {
		return false
	}
	if patch.analysis != nil {
		applyParsedAnalysis(tc, *patch.analysis, s.model)
		return true
	}
	return setUnavailable(tc, patch.reason)
}

// analysis is the model's output JSON shape.
type analysis struct {
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	IsTransient   bool     `json:"is_transient"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// parseAnalysis extracts the last balanced JSON object containing an analysis
// from the model's (possibly prose-wrapped) result text.
func parseAnalysis(text string) (analysis, bool) {
	var best analysis
	found := false
	depth, start := 0, -1
	for i, ch := range text {
		switch ch {
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
					if json.Unmarshal([]byte(text[start:i+1]), &a) == nil && (a.RootCause != "" || a.Severity != "") {
						best, found = a, true
					}
				}
			}
		}
	}
	return best, found
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
	kube      patternKubeClient
	client    *orkaClient
	namespace string
	provider  string
	model     string
	version   string
	timeout   string
	retries   int
	poll      time.Duration
}

type patternKubeClient interface {
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	TaskPhase(context.Context, string, string) (string, error)
}

func (a *patternTaskAnalyzer) AnalyzePattern(ctx context.Context, jobID, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	input := ai.BuildPatternInput(subject, failures)
	if len(input.Failures) < 2 {
		return nil, nil
	}
	fingerprint := a.provider + "\x00" + a.model + "\x00" + input.SystemPrompt + "\x00" + input.UserPrompt
	name := orka.PatternTaskName(jobID, fingerprint, a.version)
	task := orka.BuildAITask(orka.AITaskSpec{
		Name: name, Namespace: a.namespace, Provider: a.provider, Model: a.model,
		Timeout: a.timeout, MaxRetries: a.retries,
		SystemPrompt: input.SystemPrompt, Prompt: input.UserPrompt,
		Labels: map[string]string{orka.ManagedByLabel: orka.ManagedByValue},
	})
	if err := a.kube.Apply(ctx, orka.TasksGVR, a.namespace, task); err != nil {
		return nil, fmt.Errorf("apply pattern Task %s: %w", name, err)
	}
	poll := a.poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if result, ok := a.client.result(a.namespace, name); ok {
			return ai.ParsePatternResult(subject, input.Failures, result)
		}
		phase, err := a.kube.TaskPhase(ctx, a.namespace, name)
		if err == nil && (phase == "Failed" || phase == "Cancelled") {
			return nil, fmt.Errorf("pattern Task %s %s", name, strings.ToLower(phase))
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pattern Task %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}
