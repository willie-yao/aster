// Command orka-ingestor patches Orka analysis results back into the fetcher's
// dashboard skeleton. For every failing test in jobs/*.json it re-derives the
// producer's content-addressed Task name, fetches that Task's result from the
// Orka API, parses the analysis JSON, and writes ai_summary/ai_analysis onto the
// test case. The frontend then renders a dashboard produced entirely by Orka.
//
// Idempotent: re-running patches whatever results are now available and leaves
// the rest untouched, so it can run repeatedly as Tasks complete.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir to patch in place (holds jobs/*.json)")
	apiBase := flag.String("api", "http://localhost:8080", "Orka API base URL")
	token := flag.String("token", "", "bearer token for the Orka API (or set -token-file)")
	tokenFile := flag.String("token-file", "", "file holding the bearer token")
	version := flag.String("version", "v1", "content-address version suffix (must match the producer run)")
	model := flag.String("model", "claude-sonnet-4.5", "model label recorded on each analysis")
	wait := flag.Duration("wait", 0, "keep polling until every failing test is patched or this deadline elapses (0 = single pass)")
	poll := flag.Duration("poll", 15*time.Second, "interval between passes when -wait is set")
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
		srv := &webhookServer{client: client, dataDir: *dataDir, version: *version, model: *model}
		http.HandleFunc("/webhook", srv.handle)
		http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(skeletonStatus(*dataDir, *version))
		})
		st := skeletonStatus(*dataDir, *version)
		log.Printf("🔔 webhook receiver on %s (data=%s version=%s); %d failing: %d analyzed, %d unavailable, %d pending",
			*addr, *dataDir, *version, st.Failing, st.Analyzed, st.Unavailable, st.Pending)
		log.Fatal(http.ListenAndServe(*addr, nil))
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
		changed := false
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
				if applyResult(tc, client, name, model) {
					patched++
					changed = true
					continue
				}
				missing++
				if final && markUnavailable(tc, kube, namespace, name) {
					changed = true
				}
			}
		}
		if changed {
			out, err := json.MarshalIndent(detail, "", "  ")
			if err != nil {
				log.Printf("marshal %s: %v", jf, err)
				continue
			}
			if err := os.WriteFile(jf, out, 0o644); err != nil {
				log.Printf("write %s: %v", jf, err)
			}
		}
	}
	return patched, failedTests, missing
}

// applyResult fetches taskName's result, parses the analysis, and patches it
// onto tc. Returns true if it patched (result available and parseable).
func applyResult(tc *models.TestCase, client *orkaClient, taskName, model string) bool {
	result, ok := client.result(taskName)
	if !ok {
		return false
	}
	a, ok := parseAnalysis(result)
	if !ok {
		return false
	}
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
	return true
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
	mu      sync.Mutex
	client  *orkaClient
	dataDir string
	version string
	model   string
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
	// Act only on terminal phases; ignore intermediate notifications.
	if orka.TerminalPhase(p.Phase) {
		s.mu.Lock()
		s.patchTask(p)
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

// patchTask finds the failing test whose content-addressed Task name matches the
// webhook and patches its result or an unavailable placeholder.
func (s *webhookServer) patchTask(p webhookPayload) {
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
				if tc.Status != "failed" {
					continue
				}
				name := orka.TaskName(run.BuildID, orka.FailureHash(tc.Name, tc.FailureMessage), s.version)
				if name != p.TaskName {
					continue
				}
				if s.applyOrMark(tc, p, name) {
					if out, err := json.MarshalIndent(detail, "", "  "); err != nil {
						log.Printf("marshal %s: %v", jf, err)
					} else if err := os.WriteFile(jf, out, 0o644); err != nil {
						log.Printf("write %s: %v", jf, err)
					} else {
						log.Printf("🔔 patched %s (phase=%s)", p.TaskName, p.Phase)
					}
				}
				return // task name is unique to one test
			}
		}
	}
}

// applyOrMark patches tc from the webhook's result, or marks it unavailable with
// a phase reason. Returns true if it changed tc.
func (s *webhookServer) applyOrMark(tc *models.TestCase, p webhookPayload, name string) bool {
	if tc.AIAnalysis != nil {
		return false // already patched
	}
	if p.Phase == "Succeeded" && (p.ResultRef == nil || p.ResultRef.Available) {
		if applyResult(tc, s.client, name, s.model) {
			return true
		}
		return setUnavailable(tc, "analysis Task produced no readable result")
	}
	return setUnavailable(tc, "analysis Task "+strings.ToLower(p.Phase))
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
func (c *orkaClient) result(taskName string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api/v1/tasks/"+taskName+"/result", nil)
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
	if json.Unmarshal(body, &wrap) != nil || wrap.Result == "" {
		return "", false
	}
	return wrap.Result, true
}
