package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	orkaapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type staticPatternAnalyzer struct{}

type fakePatternKube struct {
	applied map[string]any
}

func (f *fakePatternKube) Apply(_ context.Context, _ schema.GroupVersionResource, _ string, obj map[string]any) error {
	f.applied = obj
	return nil
}

func (*fakePatternKube) TaskPhase(context.Context, string, string) (string, error) {
	return "Succeeded", nil
}

func (staticPatternAnalyzer) AnalyzePattern(_ context.Context, _, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	builds := make([]string, 0, len(failures))
	for _, failure := range failures {
		builds = append(builds, failure.BuildID)
	}
	return &models.PatternAnalysis{
		Subject:         subject,
		GeneratedAt:     "2026-07-17T00:00:00Z",
		BuildsAnalyzed:  len(failures),
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "the controller repeatedly writes stale configuration",
		SharedBuilds:    builds,
		SuggestedFix:    "serialize controller updates in config/controller.yaml",
		Summary:         "The same controller update failed in three recent builds.",
		RelevantFiles:   []string{"config/controller.yaml"},
	}, nil
}

func TestIngestThenFinalizePatterns(t *testing.T) {
	const namespace = "orka-system"
	results := map[string]string{}
	detail := models.JobDetail{Name: "periodic-controller", JobID: "periodic-controller"}
	for _, buildID := range []string{"103", "102", "101"} {
		tc := models.TestCase{
			Name:            "should reconcile",
			Status:          "failed",
			FailureMessage:  "timed out waiting for controller update",
			FailureLocation: "test/e2e/controller.go:44",
		}
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
			TestCases: []models.TestCase{tc},
		})
		name := orkaapi.TaskName(buildID, orkaapi.FailureHash(tc.Name, tc.FailureMessage), "v1")
		results[name] = `{"root_cause":"the controller wrote stale configuration","severity":"High","is_transient":false,"suggested_fix":"serialize the update","relevant_files":["config/controller.yaml"]}`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/"), "/result")
		result, ok := results[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteDashboard(dir, models.Dashboard{Jobs: []models.JobSummary{{
		ProwJob: models.ProwJob{Name: detail.Name, JobID: detail.JobID},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteFlakinessReport(dir, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}
	client := &orkaClient{base: server.URL, http: server.Client()}
	builds := map[string]bool{}
	patched, failed, missing := ingestPass(client, nil, namespace, dir, "v1", "test-model", false, builds)
	if patched != 3 || failed != 3 || missing != 0 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}

	stats, err := orkaapi.FinalizePatterns(context.Background(), dir, staticPatternAnalyzer{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.PatternAnalyses != 1 || stats.RecurringPatterns != 1 {
		t.Fatalf("finalize stats = %+v", stats)
	}

	data, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename("periodic-controller")))
	if err != nil {
		t.Fatal(err)
	}
	var got models.JobDetail
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.PatternAnalyses) != 1 || got.PatternAnalyses[0].ID == "" {
		t.Fatalf("job patterns = %+v, want one identified pattern", got.PatternAnalyses)
	}
	for _, run := range got.Runs {
		if run.TestCases[0].AIAnalysis == nil {
			t.Fatalf("build %s has no ingested analysis", run.BuildID)
		}
	}
}

func TestPatternTaskAnalyzerAppliesTaskAndParsesResult(t *testing.T) {
	const namespace = "orka-system"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != namespace {
			t.Fatalf("missing namespace query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{
            "systemic": true,
            "confidence": "high",
            "shared_root_cause": "stale controller writes",
            "shared_builds": ["103", "102", "101"],
            "suggested_fix": "serialize controller updates",
            "summary": "The same controller path failed in all three builds."
        }`})
	}))
	defer server.Close()

	kube := &fakePatternKube{}
	analyzer := &patternTaskAnalyzer{
		kube:      kube,
		client:    &orkaClient{base: server.URL, http: server.Client()},
		namespace: namespace,
		provider:  "models",
		model:     "strong-model",
		version:   "v1",
		timeout:   "5m",
		retries:   1,
		poll:      time.Millisecond,
	}
	failures := []ai.PatternFailure{
		{BuildID: "103", RootCause: "stale controller write", Severity: "High"},
		{BuildID: "102", RootCause: "stale controller write", Severity: "High"},
		{BuildID: "101", RootCause: "stale controller write", Severity: "High"},
	}
	got, err := analyzer.AnalyzePattern(context.Background(), "periodic-controller", "periodic-controller", failures)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Systemic || got.BuildsAnalyzed != 3 {
		t.Fatalf("pattern = %+v", got)
	}
	if kube.applied == nil {
		t.Fatal("pattern Task was not applied")
	}
	spec := kube.applied["spec"].(map[string]any)
	aiSpec := spec["ai"].(map[string]any)
	if aiSpec["providerRef"].(map[string]any)["name"] != "models" || aiSpec["model"] != "strong-model" {
		t.Fatalf("applied AI spec = %+v", aiSpec)
	}
}

func TestWebhookIndexTargetsOneJobFile(t *testing.T) {
	dir := t.TempDir()
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	s := &webhookServer{dataDir: dir, namespace: "orka-system", version: "v1", model: "m"}
	s.rebuildIndex()
	name := orkaapi.TaskName("1", orkaapi.FailureHash("test", "boom"), "v1")
	path := filepath.Join(dir, "jobs", models.JobDataFilename("job"))
	if got := s.index[name]; got != path {
		t.Fatalf("index[%q] = %q, want %q", name, got, path)
	}
	s.patchTask(webhookPayload{TaskName: name, Phase: "Succeeded"}, preparedPatch{analysis: &analysis{RootCause: "root", Severity: "High"}})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Runs[0].TestCases[0].AIAnalysis == nil {
		t.Fatal("analysis was not patched")
	}
}
