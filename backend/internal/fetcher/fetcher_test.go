package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
)

func TestLoadAnalysisTraceStoreRestoresRetainedLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, output.AITraceFilename)
	retained := ai.AnalysisTrace{
		JobID: "job", BuildID: "1", TestName: "test", Outcome: "error",
		StartedAt: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z",
	}
	if err := statefile.WriteJSON(path, ai.AnalysisTraceFile{Version: 1, Traces: []ai.AnalysisTrace{retained}}); err != nil {
		t.Fatal(err)
	}
	if got := len(loadAnalysisTraceStore(path).Snapshot().Traces); got != 1 {
		t.Fatalf("restored %d traces, want 1", got)
	}
	if got := len(loadAnalysisTraceStore(filepath.Join(dir, "missing.json")).Snapshot().Traces); got != 0 {
		t.Fatalf("missing snapshot restored %d traces, want 0", got)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := len(loadAnalysisTraceStore(path).Snapshot().Traces); got != 0 {
		t.Fatalf("corrupt snapshot restored %d traces, want 0", got)
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
		jd("recovered", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 10, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleRecovered}}),
		jd("observing", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 10, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleObserving}}),
		jd("verified-fixed", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 10, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleVerifiedFixed}}),
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

func TestNormalizeBuildResultAddsBuildFailureCase(t *testing.T) {
	result := models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID: "1", Result: "FAILURE", DurationSeconds: 248,
			JUnitComplete: true,
		},
	}

	normalizeBuildResult(&result)
	if len(result.TestCases) != 1 {
		t.Fatalf("test cases = %+v, want one build failure", result.TestCases)
	}
	got := result.TestCases[0]
	if got.Source != models.TestCaseSourceBuild || got.Status != "failed" || got.Name != "Prow job execution" {
		t.Fatalf("build failure = %+v", got)
	}
	if got.DurationSeconds != 248 || result.TestsTotal != 0 || result.TestsFailed != 0 {
		t.Fatalf("duration/counts = %v/%d/%d", got.DurationSeconds, result.TestsTotal, result.TestsFailed)
	}

	normalizeBuildResult(&result)
	if len(result.TestCases) != 1 {
		t.Fatalf("normalization duplicated build failure: %+v", result.TestCases)
	}
}

func TestNormalizeBuildResultBuildFailureEligibility(t *testing.T) {
	cases := []struct {
		name   string
		result models.BuildResult
		want   bool
	}{
		{
			name: "passed build",
			result: models.BuildResult{BuildInfo: models.BuildInfo{
				Passed: true, Result: "SUCCESS", JUnitComplete: true,
			}},
		},
		{
			name: "pending build",
			result: models.BuildResult{BuildInfo: models.BuildInfo{
				Result: "PENDING", JUnitComplete: true,
			}},
		},
		{
			name: "incomplete discovery",
			result: models.BuildResult{BuildInfo: models.BuildInfo{
				Result: "FAILURE", JUnitComplete: false,
			}},
		},
		{
			name: "truncated discovery",
			result: models.BuildResult{BuildInfo: models.BuildInfo{
				Result: "FAILURE", JUnitComplete: false, JUnitTruncated: true,
			}},
		},
		{
			name: "existing failed junit",
			result: models.BuildResult{
				BuildInfo: models.BuildInfo{Result: "FAILURE", JUnitComplete: true},
				TestCases: []models.TestCase{{Name: "test", Status: "failed"}},
			},
		},
		{
			name: "failed build with passing junit only",
			result: models.BuildResult{
				BuildInfo: models.BuildInfo{Result: "FAILURE", JUnitComplete: true},
				TestCases: []models.TestCase{{Name: "test", Status: "passed"}},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalizeBuildResult(&tc.result)
			got := false
			for _, testCase := range tc.result.TestCases {
				got = got || testCase.Source == models.TestCaseSourceBuild
			}
			if got != tc.want {
				t.Fatalf("build failure present = %t, want %t: %+v", got, tc.want, tc.result.TestCases)
			}
		})
	}
}

func TestNormalizeBuildResultExcludesBuildSubjectFromJUnitCounts(t *testing.T) {
	result := models.BuildResult{
		BuildInfo: models.BuildInfo{Result: "FAILURE", JUnitComplete: true},
		TestCases: []models.TestCase{
			{Name: "passed", Status: "passed"},
			{Name: "skipped", Status: "skipped"},
		},
	}

	normalizeBuildResult(&result)
	if len(result.TestCases) != 3 || result.TestCases[2].Source != models.TestCaseSourceBuild {
		t.Fatalf("test cases = %+v", result.TestCases)
	}
	if result.TestsTotal != 2 || result.TestsPassed != 1 || result.TestsFailed != 0 || result.TestsSkipped != 1 {
		t.Fatalf("JUnit counts = total:%d passed:%d failed:%d skipped:%d", result.TestsTotal, result.TestsPassed, result.TestsFailed, result.TestsSkipped)
	}
}

func TestSetJobCatalogRecordsResolvedTestInfraRevision(t *testing.T) {
	revision := strings.Repeat("a", 40)
	t.Run("testgrid", func(t *testing.T) {
		p := &pipeline{cfg: &project.Config{TestGrid: project.TestGrid{Dashboard: "dashboard"}}}
		catalog := &jobconfig.Catalog{Revision: revision}
		p.setJobCatalog(catalog)
		if p.jobCatalog != catalog || p.cfg.Discovery.ResolvedTestInfraRevision != revision {
			t.Fatalf("pipeline = %+v", p)
		}
	})
	t.Run("bucket", func(t *testing.T) {
		p := &pipeline{cfg: &project.Config{Discovery: project.Discovery{Source: project.DiscoveryBucket}}}
		catalog := &jobconfig.Catalog{Revision: "bucket"}
		p.setJobCatalog(catalog)
		if p.jobCatalog != catalog || p.cfg.Discovery.ResolvedTestInfraRevision != "" {
			t.Fatalf("pipeline = %+v", p)
		}
	})
}
