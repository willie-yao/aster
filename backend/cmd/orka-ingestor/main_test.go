package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const testValidationKey = "test-validation-key"

func withValidation(a analysis) analysis { return withValidationForTask(a, "task") }

func withValidationForTask(a analysis, taskName string) analysis {
	if a.GCSBytes == nil {
		zero := 0
		a.GCSBytes = &zero
	}
	if a.RelevantFiles == nil {
		a.RelevantFiles = []string{}
	}
	a.ValidationToken = orkaapi.AnalysisValidationToken(testValidationKey, taskName, a.validationInput(), *a.GCSBytes)
	return a
}

func validatedAnalysisJSON(t *testing.T, a analysis, taskNames ...string) string {
	t.Helper()
	taskName := "task"
	if len(taskNames) > 0 {
		taskName = taskNames[0]
	}
	a = withValidationForTask(a, taskName)
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type staticPatternAnalyzer struct{}

type fakePatternKube struct {
	applied map[string]any
	state   orkaapi.TaskState
	deleted bool
}

func (f *fakePatternKube) Apply(_ context.Context, _ schema.GroupVersionResource, _ string, obj map[string]any) error {
	f.applied = obj
	return nil
}

func (f *fakePatternKube) TaskState(context.Context, string, string) (orkaapi.TaskState, error) {
	return f.state, nil
}

func (f *fakePatternKube) DeleteTask(context.Context, string, string, string) error {
	f.deleted = true
	f.state = orkaapi.TaskState{}
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
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "test-model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
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
		manifest.SetBuild(detail.JobID, buildID, "build-"+buildID, "tool-"+buildID, "logs/job/"+buildID+"/", "")
		ref, err := manifest.TaskRef(detail.JobID, run, 0, tc)
		if err != nil {
			t.Fatal(err)
		}
		nonTransient := false
		results[ref.Name] = validatedAnalysisJSON(t, analysis{
			Summary: "stale controller configuration", RootCause: "the controller wrote stale configuration",
			Severity: "High", IsTransient: &nonTransient, SuggestedFix: "serialize the update",
			RelevantFiles: []string{"config/controller.yaml"},
		}, ref.Name)
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
	tracePath := filepath.Join(dir, output.AITraceFilename)
	traceStore, err := ai.LoadTraceStore(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	traces := traceStore.Snapshot().Traces
	if len(traces) != 3 {
		t.Fatalf("Orka traces = %d, want 3", len(traces))
	}
	for _, trace := range traces {
		if trace.Backend != "orka" || trace.TaskName == "" || trace.ContractHash != manifest.ContractHash || trace.APIMode != "responses" {
			t.Fatalf("Orka trace metadata = %+v", trace)
		}
	}
	if err := os.Remove(tracePath); err != nil {
		t.Fatal(err)
	}
	patched, failed, missing = ingestPass(client, nil, namespace, dir, manifest, "test-model", false, builds)
	if patched != 0 || failed != 3 || missing != 0 {
		t.Fatalf("trace restore ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
	traceStore, err = ai.LoadTraceStore(tracePath)
	if err != nil || len(traceStore.Snapshot().Traces) != 3 {
		t.Fatalf("restored traces = %+v, err=%v", traceStore.Snapshot().Traces, err)
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
		if !analysis.CritiquePassed || analysis.CritiqueVersion != orkaapi.AcceptanceVersion {
			t.Fatalf("build %s acceptance metadata = %+v", run.BuildID, analysis)
		}
	}
}

func TestIngestCountsTraceWriteFailureAsMissing(t *testing.T) {
	const namespace = "orka-system"
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	ref, err := manifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	nonTransient := false
	result := validatedAnalysisJSON(t, analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"}, ref.Name)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	oldSave := saveTraceSnapshot
	saveTraceSnapshot = func(*ai.TraceStore, string) error { return errors.New("disk full") }
	t.Cleanup(func() { saveTraceSnapshot = oldSave })
	patched, failed, missing := ingestPass(&orkaClient{base: server.URL, http: server.Client()}, nil, namespace, dir, manifest, "model", false, map[string]bool{})
	if patched != 1 || failed != 1 || missing != 1 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
}

func TestIngestPreservesUnreadableTraceSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, output.AITraceFilename)
	want := []byte(`{"version":999,"traces":[]}`)
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	patched, failed, missing := ingestPass(nil, nil, "orka-system", dir, manifest, "model", false, map[string]bool{})
	if patched != 0 || failed != 0 || missing != 1 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unreadable trace snapshot was overwritten: %s", got)
	}
}

func TestIngestLogsSortedRejectionSummary(t *testing.T) {
	const namespace = "orka-system"
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "test-model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	detail := models.JobDetail{Name: "periodic-controller", JobID: "periodic-controller"}
	results := map[string]string{}
	invalidResults := []string{
		"not JSON",
		"not JSON",
		`{"summary":"present"}`,
		"",
	}
	for i, result := range invalidResults {
		buildID := fmt.Sprintf("%d", i+1)
		tc := models.TestCase{Name: "should reconcile", Status: "failed", FailureMessage: "boom"}
		run := models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE"},
			TestCases: []models.TestCase{tc},
		}
		detail.Runs = append(detail.Runs, run)
		manifest.SetBuild(detail.JobID, buildID, "build-"+buildID, "tool-"+buildID, "logs/job/"+buildID+"/", "")
		ref, err := manifest.TaskRef(detail.JobID, run, 0, tc)
		if err != nil {
			t.Fatal(err)
		}
		results[ref.Name] = result
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	var logs bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	patched, failed, missing := ingestPass(
		&orkaClient{base: server.URL, http: server.Client()}, nil, namespace, dir, manifest, "test-model", true, map[string]bool{},
	)
	if patched != 0 || failed != 4 || missing != 4 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
	want := strings.Join([]string{
		"⚠ Orka rejection summary: 2 x analysis Task produced an invalid result: no analysis JSON object found",
		"⚠ Orka rejection summary: 1 x analysis Task produced an invalid result: root_cause is required",
		"⚠ Orka rejection summary: 1 x analysis did not complete before the deadline",
	}, "\n")
	if got := strings.TrimSpace(logs.String()); got != want {
		t.Fatalf("rejection summary:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyResultRedactsTelemetryURL(t *testing.T) {
	nonTransient := false
	result := validatedAnalysisJSON(t, analysis{
		Summary: "summary", RootCause: "cause", Severity: "High",
		IsTransient: &nonTransient, SuggestedFix: "fix",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			panic(http.ErrAbortHandler)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()

	tc := models.TestCase{Name: "test", Status: "failed"}
	accepted, rejection, _ := applyResult(
		&tc, &orkaClient{base: server.URL, http: server.Client()}, "orka-system", "task", "model", "contract", orkaapi.APIModeAuto, 0, 0, "", false, testValidationKey,
	)
	if accepted {
		t.Fatal("applyResult accepted analysis without telemetry")
	}
	if strings.Contains(rejection, server.URL) || !strings.Contains(rejection, "[redacted-url]") {
		t.Fatalf("rejection = %q, want redacted telemetry URL", rejection)
	}
	if !setUnavailable(&tc, rejection) {
		t.Fatal("setUnavailable did not publish the rejection")
	}
	if strings.Contains(tc.AISummary.Summary, server.URL) {
		t.Fatalf("published summary contains telemetry URL: %q", tc.AISummary.Summary)
	}
}

func TestPatternTaskAnalyzerAppliesTaskAndParsesResult(t *testing.T) {
	const namespace = "orka-system"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != namespace {
			t.Fatalf("missing namespace query: %s", r.URL.RawQuery)
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
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
		apiMode:      orkaapi.APIModeResponses,
		version:      "v1",
		projectScope: "project",
		timeout:      "5m",
		retries:      1,
		poll:         time.Millisecond,
		execution:    map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}},
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
	execution := spec["execution"].(map[string]any)
	if execution["nodeSelector"].(map[string]any)["agentpool"] != "cpu" {
		t.Fatalf("applied execution = %+v", execution)
	}
	metadata := kube.applied["metadata"].(map[string]any)
	baseName := metadata["name"].(string)
	labels := metadata["labels"].(map[string]any)
	if labels[orkaapi.ProjectLabel] != analyzer.projectScope || labels[orkaapi.TaskTypeLabel] != "pattern" {
		t.Fatalf("pattern labels = %+v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations[orkaapi.APIModeAnnotation] != orkaapi.APIModeResponses {
		t.Fatalf("pattern annotations = %+v", annotations)
	}
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

	oldExecution := map[string]any{"nodeSelector": map[string]any{"agentpool": "old"}}
	newExecution := map[string]any{"nodeSelector": map[string]any{"agentpool": "new"}}

	failedKube := &fakePatternKube{state: orkaapi.TaskState{Exists: true, Phase: "Failed", Execution: oldExecution, ResourceVersion: "1", UID: "uid-1"}}
	failedAnalyzer := *analyzer
	failedAnalyzer.kube = failedKube
	failedAnalyzer.execution = newExecution
	if _, err := failedAnalyzer.AnalyzePattern(context.Background(), "periodic-controller", "periodic-controller", failures); err != nil {
		t.Fatal(err)
	}
	failedName := failedKube.applied["metadata"].(map[string]any)["name"].(string)
	if failedName != baseName || !failedKube.deleted {
		t.Fatalf("failed placement recovery name=%q deleted=%v, want name=%q deleted=true", failedName, failedKube.deleted, baseName)
	}

	succeededKube := &fakePatternKube{state: orkaapi.TaskState{Exists: true, Phase: "Succeeded", Execution: oldExecution}}
	succeededAnalyzer := *analyzer
	succeededAnalyzer.kube = succeededKube
	succeededAnalyzer.execution = newExecution
	if _, err := succeededAnalyzer.AnalyzePattern(context.Background(), "periodic-controller", "periodic-controller", failures); err != nil {
		t.Fatal(err)
	}
	if succeededKube.applied != nil || succeededKube.deleted {
		t.Fatalf("successful pattern Task applied=%v deleted=%v, want cached reuse", succeededKube.applied != nil, succeededKube.deleted)
	}
}

func TestPatternTaskAnalyzerSurfacesTerminalTelemetryFailure(t *testing.T) {
	const namespace = "orka-system"
	result := `{
		"systemic": true,
		"confidence": "high",
		"shared_root_cause": "stale controller writes",
		"shared_builds": ["103", "102"],
		"suggested_fix": "serialize controller updates",
		"summary": "The same controller path failed twice."
	}`
	tests := []struct {
		name    string
		events  func(http.ResponseWriter)
		wantErr string
	}{
		{
			name: "events API failure",
			events: func(w http.ResponseWriter) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			},
			wantErr: "HTTP 401",
		},
		{
			name: "missing API mode",
			events: func(w http.ResponseWriter) {
				base := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"latestSeq": 2,
					"events": []map[string]any{
						{"seq": 1, "type": "ModelRequestCompleted", "createdAt": base},
						{"seq": 2, "type": "TaskSucceeded", "createdAt": base.Add(time.Second)},
					},
				})
			},
			wantErr: "did not report an API mode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					tc.events(w)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
			}))
			defer server.Close()
			analyzer := &patternTaskAnalyzer{
				kube: &fakePatternKube{}, client: &orkaClient{base: server.URL, http: server.Client()},
				namespace: namespace, provider: "models", model: "model", apiMode: orkaapi.APIModeResponses,
				version: "v1", projectScope: "project", timeout: "5m", retries: 1, poll: time.Millisecond,
			}
			failures := []ai.PatternFailure{{BuildID: "103", RootCause: "cause"}, {BuildID: "102", RootCause: "cause"}}
			if _, err := analyzer.AnalyzePattern(context.Background(), "job", "job", failures); err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), "context deadline exceeded") {
				t.Fatalf("AnalyzePattern error = %v, want %q without a deadline error", err, tc.wantErr)
			}
		})
	}
}

func TestPatternTaskAnalyzerPreservesEventLagAtDeadline(t *testing.T) {
	const namespace = "orka-system"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": 5, "events": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"systemic":true}`})
	}))
	defer server.Close()
	analyzer := &patternTaskAnalyzer{
		kube: &fakePatternKube{}, client: &orkaClient{base: server.URL, http: server.Client()},
		namespace: namespace, provider: "models", model: "model", apiMode: orkaapi.APIModeResponses,
		version: "v1", projectScope: "project", timeout: "5m", retries: 1, poll: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	failures := []ai.PatternFailure{{BuildID: "103", RootCause: "cause"}, {BuildID: "102", RootCause: "cause"}}
	_, err := analyzer.AnalyzePattern(ctx, "job", "job", failures)
	if err == nil || !strings.Contains(err.Error(), "not readable yet") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("AnalyzePattern error = %v, want event-lag cause and deadline", err)
	}
}

func TestWebhookManifestForTaskReloadsUnknownTask(t *testing.T) {
	dir := t.TempDir()
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	newManifest := func(seed string) *orkaapi.AnalysisManifest {
		manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "m", orkaapi.APIModeAuto, "v1", 2)
		manifest.ValidationKey = testValidationKey
		manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", seed)
		return manifest
	}
	oldManifest := newManifest("old-tree")
	if err := oldManifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	s := &webhookServer{dataDir: dir, namespace: "orka-system"}
	s.rebuildIndex()
	oldRef, err := oldManifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.index[oldRef.Name] == "" {
		t.Fatalf("old task %q was not indexed", oldRef.Name)
	}

	currentManifest := newManifest("new-tree")
	currentManifest.SetEvidencePlan("job", "1", 0, "plan", true)
	currentRef, err := currentManifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	currentManifest.SetTaskEvidencePlanComplete(currentRef.Name, true)
	if err := currentManifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	loaded, indexed, loadSucceeded := s.manifestForTask(currentRef.Name)
	if !loadSucceeded || !indexed || loaded == nil || !loaded.TaskEvidencePlanComplete(currentRef.Name) {
		t.Fatalf("reloaded manifest = %+v indexed=%t loaded=%t", loaded, indexed, loadSucceeded)
	}
	if s.index[oldRef.Name] != "" {
		t.Fatalf("stale task %q remained indexed", oldRef.Name)
	}
}

func TestWebhookAcknowledgesSupersededTask(t *testing.T) {
	dir := t.TempDir()
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "m", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	manifest.ValidationKey = testValidationKey
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "current-tree")
	if err := manifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	s := &webhookServer{dataDir: dir, namespace: "orka-system"}
	s.rebuildIndex()
	body, err := json.Marshal(webhookPayload{TaskName: "superseded-task", Phase: "Succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	s.handle(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
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
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "m", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
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
	if err := s.patchTask(webhookPayload{TaskName: name, Phase: "Succeeded"}, preparedPatch{
		analysis: &analysis{Summary: "root", RootCause: "root", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"},
		telemetry: summarizeEvents([]executionEvent{
			{Seq: 1, Type: "TaskStarted", CreatedAt: time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)},
			{Seq: 2, Type: "ModelRequestCompleted", InputTokens: 10, OutputTokens: 5, Content: json.RawMessage(`{"apiMode":"responses","responseID":"resp-webhook"}`), CreatedAt: time.Date(2026, 7, 22, 8, 0, 1, 0, time.UTC)},
			{Seq: 3, Type: "TaskSucceeded", CreatedAt: time.Date(2026, 7, 22, 8, 0, 2, 0, time.UTC)},
		}),
		model:        manifest.Model,
		contractHash: manifest.ContractHash,
	}); err != nil {
		t.Fatal(err)
	}
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
	gotAnalysis := detail.Runs[0].TestCases[0].AIAnalysis
	if gotAnalysis.ContractHash != manifest.ContractHash || gotAnalysis.TaskName != name {
		t.Fatalf("analysis identity = contract %q task %q, want contract %q task %q", gotAnalysis.ContractHash, gotAnalysis.TaskName, manifest.ContractHash, name)
	}
	traceStore, err := ai.LoadTraceStore(filepath.Join(dir, output.AITraceFilename))
	if err != nil {
		t.Fatal(err)
	}
	traces := traceStore.Snapshot().Traces
	if len(traces) != 1 || traces[0].TaskName != name || traces[0].Events[1].ResponseID != "resp-webhook" {
		t.Fatalf("webhook traces = %+v", traces)
	}
	if err := s.patchTask(webhookPayload{TaskName: name, Phase: "Succeeded"}, preparedPatch{
		analysis: &analysis{Summary: "root", RootCause: "root", Severity: "High", IsTransient: &transient, SuggestedFix: "fix"},
		telemetry: summarizeEvents([]executionEvent{
			{Seq: 1, Type: "TaskStarted"}, {Seq: 2, Type: "TaskSucceeded"},
		}),
		contractHash: manifest.ContractHash,
	}); err != nil {
		t.Fatal(err)
	}
	traceStore, err = ai.LoadTraceStore(filepath.Join(dir, output.AITraceFilename))
	traces = traceStore.Snapshot().Traces
	if err != nil || len(traces) != 1 || len(traces[0].Events) != 3 || traces[0].Events[1].ResponseID != "resp-webhook" {
		t.Fatalf("duplicate webhook traces = %+v, err=%v", traces, err)
	}
}

func TestWebhookPersistsFailedTaskTrace(t *testing.T) {
	dir := t.TempDir()
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "m", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	if err := manifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	s := &webhookServer{dataDir: dir, namespace: "orka-system"}
	s.rebuildIndex()
	if err := s.patchTask(webhookPayload{TaskName: ref.Name, Phase: "Failed"}, preparedPatch{
		reason: "analysis Task failed", contractHash: manifest.ContractHash,
		telemetry: summarizeEvents([]executionEvent{
			{Seq: 1, Type: "TaskStarted", CreatedAt: time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)},
			{Seq: 2, Type: "ModelRequestFailed", InputTokens: 10, OutputTokens: 2, CreatedAt: time.Date(2026, 7, 22, 8, 0, 1, 0, time.UTC)},
			{Seq: 3, Type: "TaskFailed", CreatedAt: time.Date(2026, 7, 22, 8, 0, 2, 0, time.UTC)},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	traceStore, err := ai.LoadTraceStore(filepath.Join(dir, output.AITraceFilename))
	if err != nil {
		t.Fatal(err)
	}
	traces := traceStore.Snapshot().Traces
	if len(traces) != 1 || traces[0].Outcome != "failed" || traces[0].ErrorCode != "task_failed" || traces[0].Events[1].ErrorCode != "model_request_failed" {
		t.Fatalf("failed webhook traces = %+v", traces)
	}
}

func TestWebhookTraceWriteFailureReturnsRetryableStatus(t *testing.T) {
	const namespace = "orka-system"
	dir := t.TempDir()
	detail := models.JobDetail{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "boom"}},
	}}}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	if err := manifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.TaskRef("job", detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	nonTransient := false
	result := validatedAnalysisJSON(t, analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"}, ref.Name)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()
	s := &webhookServer{
		client: &orkaClient{base: server.URL, http: server.Client()}, dataDir: dir, namespace: namespace,
		saveTrace: func(string, ai.AnalysisTrace) error { return errors.New("disk full") },
	}
	s.rebuildIndex()
	body, _ := json.Marshal(webhookPayload{TaskName: ref.Name, Phase: "Succeeded"})
	recorder := httptest.NewRecorder()
	s.handle(recorder, httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("first webhook status = %d, want 503", recorder.Code)
	}
	s.saveTrace = nil
	recorder = httptest.NewRecorder()
	s.handle(recorder, httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry webhook status = %d, want 200", recorder.Code)
	}
	traceStore, err := ai.LoadTraceStore(filepath.Join(dir, output.AITraceFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !traceStore.HasTerminalTask(namespace, ref.Name, manifest.ContractHash) {
		t.Fatalf("trace was not recovered: %+v", traceStore.Snapshot().Traces)
	}
}

func TestIngestRefreshesMismatchedContractHash(t *testing.T) {
	const namespace = "orka-system"
	manifest := orkaapi.NewAnalysisManifest("project", "test", "new-contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary:  &models.AISummary{Summary: "old"},
		AIAnalysis: &models.AIAnalysis{RootCause: "old root", Mode: "agentic", ContractHash: "old-contract"},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	ref, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	nonTransient := false
	newResult := validatedAnalysisJSON(t, analysis{Summary: "new root", RootCause: "new root", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix it"}, ref.Name)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ref.Name) {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": newResult})
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
	if got == nil || got.RootCause != "new root" || got.ContractHash != manifest.ContractHash || got.TaskName != ref.Name {
		t.Fatalf("refreshed analysis = %+v", got)
	}
}

func TestIngestRefreshesMismatchedTaskIdentity(t *testing.T) {
	const namespace = "orka-system"
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary:  &models.AISummary{Summary: "old"},
		AIAnalysis: &models.AIAnalysis{RootCause: "old root", Mode: "agentic", ContractHash: manifest.ContractHash, TaskName: "old-task"},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	ref, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	nonTransient := false
	newResult := validatedAnalysisJSON(t, analysis{Summary: "new root", RootCause: "new root", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix it"}, ref.Name)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeAcceptedEvents(w, false)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": newResult})
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
	if got == nil || got.RootCause != "new root" || got.TaskName != ref.Name {
		t.Fatalf("refreshed analysis = %+v", got)
	}
}

func TestIngestRetriesMissingTraceForMatchingTaskIdentity(t *testing.T) {
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary:  &models.AISummary{Summary: "current"},
		AIAnalysis: &models.AIAnalysis{RootCause: "current root", Mode: "agentic", ContractHash: manifest.ContractHash},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	ref, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	detail.Runs[0].TestCases[0].AIAnalysis.TaskName = ref.Name

	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	client := &orkaClient{base: "http://127.0.0.1:1", http: &http.Client{}}
	patched, failed, missing := ingestPass(client, nil, "orka-system", dir, manifest, "model", false, map[string]bool{})
	if patched != 0 || failed != 1 || missing != 1 {
		t.Fatalf("ingest = patched %d, failed %d, missing %d", patched, failed, missing)
	}
}

func TestIngestSkipsTraceBeforeRetentionBoundary(t *testing.T) {
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	tc := models.TestCase{
		Name: "test", Status: "failed", FailureMessage: "boom",
		AISummary: &models.AISummary{Summary: "current"},
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: "2026-07-22T08:00:00Z", RootCause: "current root", Mode: "agentic", ContractHash: manifest.ContractHash,
		},
	}
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1", Result: "FAILURE"}, TestCases: []models.TestCase{tc}}
	detail := models.JobDetail{Name: "job", JobID: "job", Runs: []models.BuildResult{run}}
	manifest.SetBuild("job", "1", "build-1", "tool-1", "logs/job/1/", "")
	ref, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	detail.Runs[0].TestCases[0].AIAnalysis.TaskName = ref.Name
	dir := t.TempDir()
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dir, output.AITraceFilename), ai.AnalysisTraceFile{
		Version: 1, DroppedTraces: 1, Traces: []ai.AnalysisTrace{{
			Backend: "orka", TaskNamespace: "orka-system", TaskName: "new-task", ContractHash: "contract",
			RecordedAt: "2026-07-22T09:00:00Z", Outcome: "succeeded",
		}},
	}); err != nil {
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
		{"seq": 3, "type": "ToolCallCompleted", "toolName": "read-artifact", "toolCallID": "call-1", "content": map[string]any{"resultLength": 40}, "createdAt": base.Add(2 * time.Second)},
		{"seq": 4, "type": "ToolCallStarted", "toolName": "grep-artifact", "toolCallID": "call-2", "createdAt": base.Add(3 * time.Second)},
		{"seq": 5, "type": "ToolCallCompleted", "toolName": "grep-artifact", "toolCallID": "call-2", "content": map[string]any{"resultLength": 60}, "createdAt": base.Add(4 * time.Second)},
		{"seq": 6, "type": "ToolCallStarted", "toolName": "validate-analysis-bscope", "toolCallID": "call-3", "createdAt": base.Add(5 * time.Second)},
		{"seq": 7, "type": "ToolCallCompleted", "toolName": "validate-analysis-bscope", "toolCallID": "call-3", "createdAt": base.Add(6 * time.Second)},
		{"seq": 8, "type": "ModelRequestCompleted", "provider": "openai", "model": "actual-model", "stopReason": "stop", "inputTokens": 100, "outputTokens": 20, "content": map[string]any{"apiMode": "responses", "responseID": "resp-test"}, "createdAt": base.Add(7 * time.Second)},
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

func TestWebhookMissingTerminalEventIsRetryable(t *testing.T) {
	const namespace = "orka-system"
	nonTransient := false
	result := validatedAnalysisJSON(t, analysis{Summary: "summary", RootCause: "cause", Severity: "High", IsTransient: &nonTransient, SuggestedFix: "fix"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
			events := []map[string]any{
				{"seq": 1, "type": "TaskStarted", "createdAt": base},
				{"seq": 2, "type": "ToolCallStarted", "toolName": "read-artifact", "toolCallID": "call-1", "createdAt": base},
				{"seq": 3, "type": "ToolCallStarted", "toolName": "validate-analysis", "toolCallID": "call-2", "createdAt": base},
				{"seq": 4, "type": "ToolCallCompleted", "toolName": "validate-analysis", "toolCallID": "call-2", "createdAt": base},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": 4, "events": events})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()
	manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
	manifest.ValidationKey = testValidationKey
	s := &webhookServer{client: &orkaClient{base: server.URL, http: server.Client()}, namespace: namespace}
	patch := s.preparePatch(webhookPayload{TaskName: "task", Phase: "Succeeded"}, manifest)
	if !patch.retry || !strings.Contains(patch.reason, "no terminal") {
		t.Fatalf("patch = %+v, want retryable terminal-event lag", patch)
	}
}

func TestWebhookRetriesWhenRejectedTaskTelemetryIsUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		phase   string
		result  string
		wantErr string
	}{
		{name: "failed Task", phase: "Failed", wantErr: "telemetry unavailable"},
		{name: "invalid result", phase: "Succeeded", result: "not JSON", wantErr: "telemetry unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					http.Error(w, "events not ready", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"result": tc.result})
			}))
			defer server.Close()
			manifest := orkaapi.NewAnalysisManifest("project", "test", "contract", "models", "model", orkaapi.APIModeAuto, "v1", 2)
			manifest.ValidationKey = testValidationKey
			s := &webhookServer{client: &orkaClient{base: server.URL, http: server.Client()}, namespace: "orka-system"}
			patch := s.preparePatch(webhookPayload{TaskName: "task", Phase: tc.phase}, manifest)
			if !patch.retry || !strings.Contains(patch.reason, tc.wantErr) {
				t.Fatalf("patch = %+v, want retryable telemetry failure", patch)
			}
		})
	}
}

func TestFinalizedSideEffectsCanBeDisabled(t *testing.T) {
	if got := finalizedSideEffects(true, "project", "data"); got != nil {
		t.Fatal("finalizedSideEffects returned a runner when disabled")
	}
	if got := finalizedSideEffects(false, "project", "data"); got == nil {
		t.Fatal("finalizedSideEffects returned nil when enabled")
	}
}

func TestFinalizeBatchPropagatesPostFinalizationFailure(t *testing.T) {
	want := errors.New("side effects unavailable")
	_, err := finalizeBatch(context.Background(), t.TempDir(), nil, func(context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestFinalizeBatchStopsBeforeSideEffectsOnFinalizationFailure(t *testing.T) {
	called := false
	_, err := finalizeBatch(context.Background(), t.TempDir(), staticPatternAnalyzer{}, func(context.Context) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "read dashboard") {
		t.Fatalf("error = %v, want dashboard finalization failure", err)
	}
	if called {
		t.Fatal("side effects ran after finalization failed")
	}
}

func TestParseAnalysisRequiresRelevantFilesArray(t *testing.T) {
	for _, value := range []string{"", `,"relevant_files":null`} {
		input := `{"summary":"summary","root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"` + value + `,"validation_token":"token"}`
		if _, err := parseAnalysis(input); err == nil || !strings.Contains(err.Error(), "relevant_files") {
			t.Fatalf("parse error = %v, want relevant_files rejection for %s", err, input)
		}
	}
}

func TestParseAnalysisIgnoresBracesInsideStrings(t *testing.T) {
	input := `prefix {"summary":"summary","root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"inspect unmatched { marker","relevant_files":[],"gcs_bytes":1,"validation_token":"token"} suffix`
	got, err := parseAnalysis(input)
	if err != nil {
		t.Fatalf("parseAnalysis() error = %v", err)
	}
	if got.SuggestedFix != "inspect unmatched { marker" {
		t.Fatalf("suggested_fix = %q", got.SuggestedFix)
	}
}

func TestSetUnavailableReplacesOnlyEnginePlaceholder(t *testing.T) {
	tc := &models.TestCase{AISummary: &models.AISummary{Summary: unavailablePrefix + "analysis did not complete before the deadline"}}
	if !setUnavailable(tc, "analysis Task failed acceptance: validation token mismatch") {
		t.Fatal("setUnavailable() did not replace the stale placeholder")
	}
	if got := tc.AISummary.Summary; got != unavailablePrefix+"analysis Task failed acceptance: validation token mismatch" {
		t.Fatalf("summary = %q", got)
	}
	if setUnavailable(tc, "analysis Task failed acceptance: validation token mismatch") {
		t.Fatal("setUnavailable() rewrote an identical placeholder")
	}

	real := &models.TestCase{AISummary: &models.AISummary{Summary: "real analysis"}}
	if setUnavailable(real, "new reason") {
		t.Fatal("setUnavailable() replaced a real analysis summary")
	}
}
