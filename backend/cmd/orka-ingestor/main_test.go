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
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "test-model", "v1", 2)
	for _, buildID := range []string{"103", "102", "101"} {
		tc := models.TestCase{
			Name:            "should reconcile",
			Status:          "failed",
			FailureMessage:  "timed out waiting for controller update",
			FailureLocation: "test/e2e/controller.go:44",
		}
		run := models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
			TestCases: []models.TestCase{tc},
		}
		detail.Runs = append(detail.Runs, run)
		manifest.SetBuild(detail.JobID, buildID, "build-"+buildID, "tool-"+buildID, "logs/job/"+buildID+"/")
		ref, err := manifest.TaskRef(detail.JobID, run, 0, tc)
		if err != nil {
			t.Fatal(err)
		}
		results[ref.Name] = `{"summary":"stale controller configuration","root_cause":"the controller wrote stale configuration","severity":"High","is_transient":false,"suggested_fix":"serialize the update","relevant_files":["config/controller.yaml"]}`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
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
	patched, failed, missing := ingestPass(client, nil, namespace, dir, manifest, "test-model", false, builds)
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
		analysis := run.TestCases[0].AIAnalysis
		if analysis == nil {
			t.Fatalf("build %s has no ingested analysis", run.BuildID)
		}
		if analysis.ToolCalls != 3 || analysis.ElapsedMs != 10000 || analysis.InputTokens != 100 || analysis.OutputTokens != 20 {
			t.Fatalf("build %s telemetry = %+v", run.BuildID, analysis)
		}
		if !analysis.ArtifactPathsValidated {
			t.Fatalf("build %s did not record validate_analysis", run.BuildID)
		}
		if !analysis.CritiquePassed || analysis.CritiqueVersion != orkaAcceptanceVersion {
			t.Fatalf("build %s acceptance metadata = %+v", run.BuildID, analysis)
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
		kube:         kube,
		client:       &orkaClient{base: server.URL, http: server.Client()},
		namespace:    namespace,
		provider:     "models",
		model:        "strong-model",
		version:      "v1",
		projectScope: "project",
		timeout:      "5m",
		retries:      1,
		poll:         time.Millisecond,
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
	baseName := kube.applied["metadata"].(map[string]any)["name"].(string)
	variants := []struct {
		name    string
		timeout string
		retries int
	}{
		{name: "timeout", timeout: "6m", retries: analyzer.retries},
		{name: "retries", timeout: analyzer.timeout, retries: 2},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			variantKube := &fakePatternKube{}
			variantAnalyzer := *analyzer
			variantAnalyzer.kube = variantKube
			variantAnalyzer.timeout = variant.timeout
			variantAnalyzer.retries = variant.retries
			if _, err := variantAnalyzer.AnalyzePattern(context.Background(), "periodic-controller", "periodic-controller", failures); err != nil {
				t.Fatal(err)
			}
			name := variantKube.applied["metadata"].(map[string]any)["name"].(string)
			if name == baseName {
				t.Fatalf("%s change reused pattern Task name %q", variant.name, name)
			}
		})
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
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "m", "v1", 2)
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/")
	s := &webhookServer{dataDir: dir, namespace: "orka-system"}
	s.rebuildIndex()
	if len(s.index) != 0 {
		t.Fatalf("index before manifest = %+v, want empty", s.index)
	}
	if err := manifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	s.rebuildIndex()
	ref, err := manifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	name := ref.Name
	path := filepath.Join(dir, "jobs", models.JobDataFilename("job"))
	if got := s.index[name]; got != path {
		t.Fatalf("index[%q] = %q, want %q", name, got, path)
	}
	transient := false
	s.patchTask(webhookPayload{TaskName: name, Phase: "Succeeded"}, preparedPatch{
		analysis:     &analysis{Summary: "root", RootCause: "root", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"},
		telemetry:    analysisTelemetry{EventCount: 1, ToolCalls: 2},
		model:        manifest.Model,
		contractHash: manifest.ContractHash,
	})
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
	if got := detail.Runs[0].TestCases[0].AIAnalysis.ContractHash; got != manifest.ContractHash {
		t.Fatalf("contract hash = %q, want %q", got, manifest.ContractHash)
	}
}

func TestIngestRefreshesMismatchedContractHash(t *testing.T) {
	const namespace = "orka-system"
	manifest := orkaapi.NewAnalysisManifest("project", "test", "new-contract", "models", "model", "v1", 2)
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary:  &models.AISummary{Summary: "old"},
		AIAnalysis: &models.AIAnalysis{RootCause: "old root", Mode: "agentic", ContractHash: "old-contract"},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/")
	ref, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ref.Name) {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"summary":"new root","root_cause":"new root","severity":"High","is_transient":false,"suggested_fix":"fix it"}`})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	patched, failed, missing := ingestPass(
		&orkaClient{base: server.URL, http: server.Client()}, nil, namespace, dir, manifest, "model", false, map[string]bool{},
	)
	if patched != 1 || failed != 1 || missing != 0 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
	data, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename("job")))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	got := detail.Runs[0].TestCases[0].AIAnalysis
	if got == nil || got.RootCause != "new root" || got.ContractHash != manifest.ContractHash {
		t.Fatalf("refreshed analysis = %+v", got)
	}
}

func TestIngestKeepsMatchingContractHash(t *testing.T) {
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", "v1", 2)
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary:  &models.AISummary{Summary: "current"},
		AIAnalysis: &models.AIAnalysis{RootCause: "current root", Mode: "agentic", ContractHash: manifest.ContractHash},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/")

	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	client := &orkaClient{base: "http://127.0.0.1:1", http: &http.Client{}}
	patched, failed, missing := ingestPass(client, nil, "orka-system", dir, manifest, "model", false, map[string]bool{})
	if patched != 0 || failed != 1 || missing != 0 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
}

func writeAcceptedEvents(w http.ResponseWriter, transient bool) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	events := []map[string]any{
		{"seq": 1, "type": "TaskStarted", "createdAt": base},
		{"seq": 2, "type": "ToolCallStarted", "toolName": "read-artifact", "toolCallID": "call-1", "createdAt": base.Add(time.Second)},
		{"seq": 3, "type": "ToolCallCompleted", "toolName": "read-artifact", "toolCallID": "call-1", "createdAt": base.Add(2 * time.Second)},
		{"seq": 4, "type": "ToolCallStarted", "toolName": "grep-artifact", "toolCallID": "call-2", "createdAt": base.Add(3 * time.Second)},
		{"seq": 5, "type": "ToolCallCompleted", "toolName": "grep-artifact", "toolCallID": "call-2", "createdAt": base.Add(4 * time.Second)},
		{"seq": 6, "type": "ToolCallStarted", "toolName": "validate-analysis-bscope", "toolCallID": "call-3", "createdAt": base.Add(5 * time.Second)},
		{"seq": 7, "type": "ToolCallCompleted", "toolName": "validate-analysis-bscope", "toolCallID": "call-3", "createdAt": base.Add(6 * time.Second)},
		{"seq": 8, "type": "ModelRequestCompleted", "provider": "openai", "model": "actual-model", "stopReason": "stop", "inputTokens": 100, "outputTokens": 20, "createdAt": base.Add(7 * time.Second)},
	}
	if transient {
		events = append(events,
			map[string]any{"seq": 9, "type": "ToolCallStarted", "toolName": "verify-timeline-bscope", "toolCallID": "call-4", "createdAt": base.Add(8 * time.Second)},
			map[string]any{"seq": 10, "type": "ToolCallCompleted", "toolName": "verify-timeline-bscope", "toolCallID": "call-4", "createdAt": base.Add(9 * time.Second)},
		)
	}
	last := int64(len(events) + 1)
	events = append(events, map[string]any{"seq": last, "type": "TaskSucceeded", "createdAt": base.Add(10 * time.Second)})
	_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": last, "events": events})
}
