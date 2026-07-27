package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func TestClearAnalysisTrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, output.AITraceFilename)
	if err := os.WriteFile(path, []byte(`{"traces":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearAnalysisTrace(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("trace file still exists: %v", err)
	}
	if err := clearAnalysisTrace(dir); err != nil {
		t.Fatalf("missing trace file: %v", err)
	}
}

func TestAIEndpoint_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{Endpoint: "https://yaml.example/v1/chat"}}
	if got := aiEndpoint(cfg); got != "https://yaml.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want yaml value", got)
	}
}

func TestAIEndpoint_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value", got)
	}
}

func TestAIEndpoint_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "" {
		t.Errorf("aiEndpoint: got %q, want empty", got)
	}
}

func TestAIEndpoint_NilAIBlock(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value when AI block is nil", got)
	}
}

func TestAIModel_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{Model: "yaml-model"}}
	if got := aiModel(cfg); got != "yaml-model" {
		t.Errorf("aiModel: got %q, want yaml value", got)
	}
}

func TestAIModel_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value", got)
	}
}

func TestAIModel_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_MODEL", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "" {
		t.Errorf("aiModel: got %q, want empty", got)
	}
}

func TestAIModel_NilAIBlock(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value when AI block is nil", got)
	}
}

func TestFetchBuildResultParsesRootJUnitWhenTreeTruncated(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		fullPath := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("logs/job/1/started.json", `{"timestamp":1000}`)
	write("logs/job/1/finished.json", `{"timestamp":1060,"passed":false,"result":"FAILURE"}`)
	write("logs/job/1/artifacts/junit.e2e_suite.1.xml", `<testsuite name="suite"><testcase name="case" classname="suite" status="passed"/></testsuite>`)
	for i := 0; i < 2001; i++ {
		write(fmt.Sprintf("logs/job/1/artifacts/clusters/%04d/log.txt", i), "x")
	}

	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetchBuildResult(context.Background(), backend,
		&models.ProwJob{Name: "job", JobType: models.JobTypePeriodic}, prowbuild.Build{ID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.JUnitComplete || !result.JUnitTruncated {
		t.Fatalf("complete=%v truncated=%v, want false true", result.JUnitComplete, result.JUnitTruncated)
	}
	if len(result.JUnitURLs) != 1 || result.TestsTotal != 1 || result.TestsPassed != 1 || len(result.TestCases) != 1 {
		t.Fatalf("result = %+v, want one parsed root JUnit test", result)
	}
}

func TestLoadCachedJobDetailsRequiresUsableJUnitDiscovery(t *testing.T) {
	dir := t.TempDir()
	jobsDir := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	detail := models.JobDetail{
		JobID: "job",
		Runs: []models.BuildResult{
			{BuildInfo: models.BuildInfo{BuildID: "complete", Result: "SUCCESS", JUnitComplete: true}},
			{BuildInfo: models.BuildInfo{BuildID: "truncated-with-url", Result: "SUCCESS", JUnitTruncated: true, JUnitURLs: []string{"https://web/junit.xml"}}},
			{BuildInfo: models.BuildInfo{BuildID: "truncated-empty", Result: "SUCCESS", JUnitTruncated: true}},
			{BuildInfo: models.BuildInfo{BuildID: "zero-tests", Result: "SUCCESS", JUnitTruncated: true, JUnitURLs: []string{"https://web/junit-empty.xml"}}, TestCases: []models.TestCase{}},
			{BuildInfo: models.BuildInfo{BuildID: "pending", Result: "PENDING", JUnitComplete: true}},
			{BuildInfo: models.BuildInfo{BuildID: "read-failure", Result: "FAILURE", JUnitURLs: []string{"https://web/junit-read.xml"}}},
			{BuildInfo: models.BuildInfo{BuildID: "parse-failure", Result: "FAILURE", JUnitURLs: []string{"https://web/junit-parse.xml"}}},
		},
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, models.JobDataFilename(detail.JobID)), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cached := loadCachedJobDetails(dir)[detail.JobID]
	want := []string{"complete", "truncated-with-url", "zero-tests"}
	if len(cached) != len(want) {
		t.Fatalf("cached = %+v, want build IDs %v", cached, want)
	}
	for _, buildID := range want {
		if cached[buildID].BuildID != buildID {
			t.Errorf("build %q was not cached", buildID)
		}
	}
	for _, buildID := range []string{"truncated-empty", "pending", "read-failure", "parse-failure"} {
		if _, ok := cached[buildID]; ok {
			t.Errorf("build %q should be refetched", buildID)
		}
	}
}

func TestCollectRecurringPatterns_FiltersAndRanks(t *testing.T) {
	jd := func(subject string, pa *models.PatternAnalysis) models.JobDetail {
		d := models.JobDetail{JobID: subject, Name: subject}
		if pa != nil {
			pa.Subject = subject
			d.PatternAnalyses = []models.PatternAnalysis{*pa}
		}
		return d
	}
	details := []models.JobDetail{
		jd("low-systemic", &models.PatternAnalysis{Systemic: true, Confidence: "low", BuildsAnalyzed: 9}),
		jd("not-systemic", &models.PatternAnalysis{Systemic: false, Confidence: "high", BuildsAnalyzed: 8}),
		jd("high-3builds", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 3}),
		jd("high-6builds", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 6}),
		jd("no-pattern", nil),
	}

	got := collectRecurringPatterns(details)

	// Only systemic verdicts are kept.
	if len(got) != 3 {
		t.Fatalf("got %d patterns, want 3 (systemic only)", len(got))
	}
	// Ranked by confidence desc, then builds desc: high/6, high/3, low/9.
	wantOrder := []string{"high-6builds", "high-3builds", "low-systemic"}
	for i, want := range wantOrder {
		if got[i].Subject != want {
			t.Errorf("rank %d: got %q, want %q", i, got[i].Subject, want)
		}
	}
}

func TestRunWatch_RejectsNonPositiveIntervals(t *testing.T) {
	ctx := context.Background()
	opts := Options{}
	if err := RunWatch(ctx, opts, 0, time.Hour); err == nil {
		t.Error("expected error for zero watch interval")
	}
	if err := RunWatch(ctx, opts, time.Minute, 0); err == nil {
		t.Error("expected error for zero reconcile interval")
	}
}

func TestFailureLocationFile(t *testing.T) {
	cases := map[string]string{
		"test/e2e/foo_test.go:123":                                "test/e2e/foo_test.go",
		"test/e2e/foo_test.go:123:45":                             "test/e2e/foo_test.go",
		"sigs.k8s.io/cluster-api/test@v1.13.3/framework/x.go:190": "sigs.k8s.io/cluster-api/test@v1.13.3/framework/x.go",
		"":              "",
		"   ":           "",
		"plain/path.go": "plain/path.go",
	}
	for in, want := range cases {
		if got := failureLocationFile(in); got != want {
			t.Errorf("failureLocationFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGatherPatternFailures_SeedsFailingTestLocation(t *testing.T) {
	d := &models.JobDetail{
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "100", Passed: false, Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name:            "[It] upgrade test",
				Status:          "failed",
				FailureLocation: "test/e2e/azure_apiversion_upgrade_test.go:88",
				AIAnalysis:      &models.AIAnalysis{Severity: "high", RelevantFiles: []string{"test/e2e/config/azure-dev.yaml"}},
			}},
		}},
	}
	got := gatherPatternFailures(d)
	if len(got) != 1 {
		t.Fatalf("expected 1 pattern failure, got %d", len(got))
	}
	// The failing test's file is carried in LocationFile (kept out of the
	// correlation prompt), not folded into the prompt-facing RelevantFiles.
	if got[0].LocationFile != "test/e2e/azure_apiversion_upgrade_test.go" {
		t.Errorf("LocationFile = %q, want the failing-test file", got[0].LocationFile)
	}
	if len(got[0].RelevantFiles) != 1 || got[0].RelevantFiles[0] != "test/e2e/config/azure-dev.yaml" {
		t.Errorf("RelevantFiles should stay the AI's list only, got %v", got[0].RelevantFiles)
	}
}
