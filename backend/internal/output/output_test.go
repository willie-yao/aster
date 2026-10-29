package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

func sampleDashboard() models.Dashboard {
	return models.Dashboard{
		GeneratedAt: time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC),
		Jobs: []models.JobSummary{
			{
				ProwJob: models.ProwJob{
					Name:     "periodic-cluster-api-provider-azure-e2e-main",
					Category: "e2e",
					Branch:   "main",
				},
				OverallStatus:  "PASSING",
				PassRateRecent: new(0.95),
				RecentRuns: []models.RunSummary{
					{BuildID: "100", Passed: true, Timestamp: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)},
				},
			},
		},
	}
}

func sampleJobDetail(name string) models.JobDetail {
	return models.JobDetail{
		Name:           name,
		JobID:          name,
		JobType:        models.JobTypePeriodic,
		ConfigFile:     "config/jobs/example/periodics.yaml",
		ConfigRevision: strings.Repeat("a", 40),
		PassRateRecent: new(1.0),
		Runs: []models.BuildResult{
			{
				BuildInfo: models.BuildInfo{
					BuildID: "100",
					JobName: name,
					Started: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
					Passed:  true,
					Result:  "SUCCESS",
				},
				TestsTotal:  5,
				TestsPassed: 5,
			},
		},
	}
}

func TestWriteDashboard(t *testing.T) {
	dir := t.TempDir()
	dash := sampleDashboard()

	if err := WriteDashboard(dir, dash); err != nil {
		t.Fatalf("WriteDashboard: %v", err)
	}

	var got models.Dashboard
	readJSON(t, filepath.Join(dir, "dashboard.json"), &got)
	if got.GeneratedAt != dash.GeneratedAt {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, dash.GeneratedAt)
	}
	if len(got.Jobs) != len(dash.Jobs) {
		t.Errorf("len(Jobs) = %d, want %d", len(got.Jobs), len(dash.Jobs))
	}
}

func TestWriteDashboard_EmptyJobsWritesArray(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDashboard(dir, models.Dashboard{GeneratedAt: time.Now()}); err != nil {
		t.Fatalf("WriteDashboard: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "dashboard.json"))
	if err != nil {
		t.Fatalf("read dashboard.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal dashboard.json: %v", err)
	}
	if got := strings.TrimSpace(string(raw["jobs"])); got != "[]" {
		t.Fatalf("jobs = %s, want []", got)
	}
}

func TestWriteDashboard_CreatesParentDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")

	if err := WriteDashboard(dir, sampleDashboard()); err != nil {
		t.Fatalf("WriteDashboard with nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboard.json")); err != nil {
		t.Fatalf("dashboard.json not created in nested dir: %v", err)
	}
}

func TestWriteDashboard_NormalizesMode(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDashboard(dir, sampleDashboard()); err != nil {
		t.Fatalf("WriteDashboard: %v", err)
	}
	// On a POSIX filesystem the best-effort chmod runs and normalizes the mode
	// to 0644. On filesystems that reject chmod (SMB/azurefile) the write must
	// still succeed; that path is tolerated by ignoring the chmod error.
	fi, err := os.Stat(filepath.Join(dir, "dashboard.json"))
	if err != nil {
		t.Fatalf("stat dashboard.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("dashboard.json mode = %o, want 644", got)
	}
}

func TestWriteJobDetail(t *testing.T) {
	dir := t.TempDir()
	detail := sampleJobDetail("periodic-cluster-api-provider-azure-e2e-main")

	if err := WriteJobDetail(dir, detail); err != nil {
		t.Fatalf("WriteJobDetail: %v", err)
	}

	var got models.JobDetail
	readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), &got)
	if got.Name != detail.Name {
		t.Errorf("Name = %q, want %q", got.Name, detail.Name)
	}
	if len(got.Runs) != len(detail.Runs) {
		t.Errorf("len(Runs) = %d, want %d", len(got.Runs), len(detail.Runs))
	}
}

func TestWriteJobReliability(t *testing.T) {
	cases := []struct {
		name   string
		rate   *float64
		status string
		wire   string
	}{
		{"no completed outcomes", nil, "UNKNOWN", "null"},
		{"completed failures", new(0.0), "FAILING", "0"},
		{"completed passes", new(1.0), "PASSING", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dashboard := sampleDashboard()
			dashboard.Jobs[0].PassRateRecent = tc.rate
			dashboard.Jobs[0].OverallStatus = tc.status
			if err := WriteDashboard(dir, dashboard); err != nil {
				t.Fatal(err)
			}
			var published struct {
				Jobs []map[string]json.RawMessage `json:"jobs"`
			}
			readJSON(t, filepath.Join(dir, "dashboard.json"), &published)
			if got := string(published.Jobs[0]["pass_rate_recent"]); got != tc.wire {
				t.Errorf("summary pass rate = %s, want %s", got, tc.wire)
			}
			if got := string(published.Jobs[0]["overall_status"]); got != `"`+tc.status+`"` {
				t.Errorf("summary reliability = %s, want %s", got, tc.status)
			}
			detail := sampleJobDetail("job")
			detail.PassRateRecent = tc.rate
			if err := WriteJobDetail(dir, detail); err != nil {
				t.Fatal(err)
			}
			var publishedDetail map[string]json.RawMessage
			readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), &publishedDetail)
			if got := string(publishedDetail["pass_rate_recent"]); got != tc.wire {
				t.Errorf("detail pass rate = %s, want %s", got, tc.wire)
			}
		})
	}
}

func TestWriteJobDetailBackfillsPatternIdentity(t *testing.T) {
	dir := t.TempDir()
	detail := sampleJobDetail("periodic-pattern")
	detail.PatternAnalyses = []models.PatternAnalysis{{
		ID: "stable-pattern", ContentHash: "", JobID: detail.JobID, Subject: detail.Name,
		BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "shared cause",
		SharedBuilds: []string{"1", "2"}, SuggestedFix: "fix it", Summary: "summary",
	}}
	if err := WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
	var written models.JobDetail
	readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), &written)
	pattern := written.PatternAnalyses[0]
	if pattern.ID != "stable-pattern" || pattern.ContentHash != models.PatternHash(pattern) {
		t.Fatalf("written pattern = %+v", pattern)
	}
	if detail.PatternAnalyses[0].ContentHash != "" {
		t.Fatal("WriteJobDetail mutated its input")
	}
}

func TestWriteFlakinessReportBackfillsPatternIdentity(t *testing.T) {
	dir := t.TempDir()
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{{
		ID: "stable-pattern", ContentHash: "stale", JobID: "job", Subject: "job", BuildsAnalyzed: 3, Systemic: true,
		Confidence: "high", SharedRootCause: "shared cause", Summary: "summary",
		CausalGroups: []models.PatternCausalGroup{{
			ID: "stale-group", ContentHash: "stale", Builds: []string{"1", "2"}, RootCause: "shared cause", Confidence: "high",
		}},
	}}}
	if err := WriteFlakinessReport(dir, report); err != nil {
		t.Fatal(err)
	}
	var written models.FlakinessReport
	readJSON(t, filepath.Join(dir, "flakiness.json"), &written)
	pattern := written.RecurringPatterns[0]
	group := pattern.CausalGroups[0]
	if pattern.ID != "stable-pattern" || pattern.ContentHash != models.PatternHash(pattern) ||
		group.ID != models.PatternCausalGroupID(pattern.ID, group) || group.ContentHash != models.PatternCausalGroupHash(group) {
		t.Fatalf("written pattern = %+v", pattern)
	}
}

// TestWriteFlakinessReportPublishesLowPassRateSection pins the published shape
// of the optional pass-rate section: it is always present so consumers can tell
// an empty rule from an old engine, and its entries flatten TestFlakiness
// alongside the window the rate was measured over.
func TestWriteFlakinessReportPublishesLowPassRateSection(t *testing.T) {
	t.Run("empty when the rule is off", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteFlakinessReport(dir, models.FlakinessReport{}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		got, ok := raw["low_pass_rate"]
		if !ok {
			t.Fatal("low_pass_rate missing from flakiness.json")
		}
		if string(got) != "[]" {
			t.Errorf("low_pass_rate = %s, want []", got)
		}
	})

	t.Run("entries flatten the embedded test", func(t *testing.T) {
		dir := t.TempDir()
		report := models.FlakinessReport{GeneratedAt: "2026-08-17T10:00:00Z", LowPassRate: []models.LowPassRateEntry{{
			TestFlakiness: models.TestFlakiness{
				TestName: "TestA", JobName: "job", JobID: "job",
				TotalRuns: 10, Failures: 1, Passes: 9, FailRate: 0.1,
				Classification: models.ClassificationOneOff,
			},
			WindowRuns: 6,
			PassRate:   0.5,
		}}}
		if err := WriteFlakinessReport(dir, report); err != nil {
			t.Fatal(err)
		}
		var written struct {
			LowPassRate []map[string]any `json:"low_pass_rate"`
		}
		readJSON(t, filepath.Join(dir, "flakiness.json"), &written)
		if len(written.LowPassRate) != 1 {
			t.Fatalf("low_pass_rate = %d entries, want 1", len(written.LowPassRate))
		}
		entry := written.LowPassRate[0]
		for key, want := range map[string]any{
			"test_name":      "TestA",
			"classification": string(models.ClassificationOneOff),
			"fail_rate":      0.1,
			"window_runs":    float64(6),
			"pass_rate":      0.5,
		} {
			if entry[key] != want {
				t.Errorf("entry[%q] = %v, want %v", key, entry[key], want)
			}
		}
		var fixture models.FlakinessReport
		readJSON(t, "../../../frontend/tests/fixtures/low-pass-rate.json", &fixture)
		var published models.FlakinessReport
		readJSON(t, filepath.Join(dir, "flakiness.json"), &published)
		if !reflect.DeepEqual(published, fixture) {
			t.Fatalf("published report differs from frontend fixture:\ngot  %+v\nwant %+v", published, fixture)
		}
	})
}

func TestWriteAllKeepsPatternIdentityConsistent(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{
		ID: "stable-pattern", JobID: "job-alpha", Subject: "job-alpha",
		BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "shared cause",
		SharedBuilds: []string{"1", "2"}, SuggestedFix: "fix it", Summary: "summary",
	}
	detail := sampleJobDetail(pattern.JobID)
	detail.PatternAnalyses = []models.PatternAnalysis{pattern}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}}
	if err := WriteAll(dir, sampleConfig(), sampleDashboard(), []models.JobDetail{detail}, report, models.SearchIndex{}); err != nil {
		t.Fatal(err)
	}
	var writtenDetail models.JobDetail
	var writtenReport models.FlakinessReport
	readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), &writtenDetail)
	readJSON(t, filepath.Join(dir, "flakiness.json"), &writtenReport)
	if writtenDetail.ConfigFile != detail.ConfigFile || writtenDetail.ConfigRevision != detail.ConfigRevision {
		t.Fatalf("written Prow config source = %q@%q", writtenDetail.ConfigFile, writtenDetail.ConfigRevision)
	}
	jobPattern := writtenDetail.PatternAnalyses[0]
	reportPattern := writtenReport.RecurringPatterns[0]
	if jobPattern.ID != reportPattern.ID || jobPattern.ContentHash == "" || jobPattern.ContentHash != reportPattern.ContentHash {
		t.Fatalf("job pattern=%+v report pattern=%+v", jobPattern, reportPattern)
	}
}

func TestJobDataFilenameIsInjective(t *testing.T) {
	a := models.JobDataFilename("foo/bar-baz/qux")
	b := models.JobDataFilename("foo-bar/baz/qux")
	if a == b {
		t.Fatalf("distinct job IDs collided at %q", a)
	}
	for _, name := range []string{a, b} {
		if strings.ContainsAny(name, "/+=") || !strings.HasSuffix(name, ".json") {
			t.Errorf("unsafe job filename %q", name)
		}
	}
}

func sampleConfig() *project.Config {
	return &project.Config{
		ID:        "capz",
		Name:      "Cluster API Provider Azure",
		ShortName: "CAPZ",
		Discovery: project.Discovery{TestGridDashboard: "sig-cluster-lifecycle-cluster-api-provider-azure"},
		Storage:   project.Storage{Provider: "gcs", Bucket: "kubernetes-ci-logs"},
		Branding: project.Branding{
			Title:    "CAPZ Prow Dashboard",
			BasePath: "/capz-prow-dashboard",
			SiteURL:  "https://example.test/capz-prow-dashboard",
			SourceRepo: project.SourceRepo{
				Owner: "kubernetes-sigs",
				Name:  "cluster-api-provider-azure",
			},
		},
	}
}

func TestWriteAll(t *testing.T) {
	dir := t.TempDir()
	dash := sampleDashboard()
	details := []models.JobDetail{
		sampleJobDetail("job-alpha"),
		sampleJobDetail("job-beta"),
	}
	stale := filepath.Join(dir, "jobs", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "dashboard.json"),
		filepath.Join(dir, "jobs", models.JobDataFilename(details[0].JobID)),
		stale,
	} {
		if err := os.WriteFile(path, []byte(`{"sentinel":"old"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	flakiness := models.FlakinessReport{
		GeneratedAt: "2025-01-15T12:00:00Z",
	}

	cfg := sampleConfig()
	cfg.Discovery.TestInfraRevision = strings.Repeat("a", 40)
	cfg.Discovery.ResolvedTestInfraRevision = strings.Repeat("a", 40)
	if err := WriteAll(dir, cfg, dash, details, flakiness, models.SearchIndex{GeneratedAt: "2025-01-15T12:00:00Z"}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	// manifest.json exists and round-trips the config
	var gotManifest project.Config
	readJSON(t, filepath.Join(dir, "manifest.json"), &gotManifest)
	if gotManifest.ID != "capz" || gotManifest.Branding.Title != "CAPZ Prow Dashboard" || gotManifest.Discovery.TestInfraRevision != strings.Repeat("a", 40) || gotManifest.Discovery.ResolvedTestInfraRevision != strings.Repeat("a", 40) {
		t.Errorf("manifest round-trip mismatch: %+v", gotManifest)
	}

	var gotDashboard models.Dashboard
	readJSON(t, filepath.Join(dir, "dashboard.json"), &gotDashboard)
	if !gotDashboard.GeneratedAt.Equal(dash.GeneratedAt) || len(gotDashboard.Jobs) != len(dash.Jobs) {
		t.Errorf("dashboard mismatch: %+v", gotDashboard)
	}
	for _, d := range details {
		var got models.JobDetail
		readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(d.JobID)), &got)
		if got.JobID != d.JobID || len(got.Runs) != len(d.Runs) {
			t.Errorf("job detail mismatch: %+v", got)
		}
	}
	// flakiness.json exists
	if _, err := os.Stat(filepath.Join(dir, "flakiness.json")); err != nil {
		t.Error("flakiness.json missing")
	}
	// search-index.json exists
	if _, err := os.Stat(filepath.Join(dir, "search-index.json")); err != nil {
		t.Error("search-index.json missing")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale job file still exists: %v", err)
	}
}

func TestWriteAllSupportingFailuresPreserveDashboardAndStaleJobs(t *testing.T) {
	details := []models.JobDetail{sampleJobDetail("job-alpha"), sampleJobDetail("job-beta")}
	alphaPath := filepath.Join("jobs", models.JobDataFilename(details[0].JobID))
	betaPath := filepath.Join("jobs", models.JobDataFilename(details[1].JobID))
	supporting := []struct {
		path      string
		wantError string
	}{
		{"manifest.json", "write manifest"},
		{alphaPath, "write job detail job-alpha"},
		{betaPath, "write job detail job-beta"},
		{"flakiness.json", "write flakiness report"},
		{"search-index.json", "write search index"},
	}
	tests := []struct {
		name    string
		blocked []string
	}{
		{"manifest", []string{"manifest.json"}},
		{"first job", []string{alphaPath}},
		{"second job", []string{betaPath}},
		{"flakiness", []string{"flakiness.json"}},
		{"search index", []string{"search-index.json"}},
		{"multiple supporting writes", []string{"manifest.json", alphaPath, "flakiness.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "jobs"), 0o755); err != nil {
				t.Fatal(err)
			}
			const old = "{\"sentinel\":\"old\"}\n"
			for _, name := range []string{"dashboard.json", filepath.Join("jobs", "stale.json")} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(old), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			blocked := make(map[string]bool, len(tt.blocked))
			for _, name := range tt.blocked {
				blocked[name] = true
				if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range supporting {
				if !blocked[file.path] {
					if err := os.WriteFile(filepath.Join(dir, file.path), []byte(old), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			err := WriteAll(dir, sampleConfig(), sampleDashboard(), details, models.FlakinessReport{}, models.SearchIndex{})
			if err == nil {
				t.Fatal("WriteAll returned nil error")
			}
			for _, file := range supporting {
				if strings.Contains(err.Error(), file.wantError) != blocked[file.path] {
					t.Errorf("error for %s: blocked=%v, got %v", file.path, blocked[file.path], err)
				}
				if blocked[file.path] {
					continue
				}
				data, readErr := os.ReadFile(filepath.Join(dir, file.path))
				if readErr != nil || string(data) == old || !json.Valid(data) {
					t.Errorf("supporting file %s did not advance: data=%q, error=%v", file.path, data, readErr)
				}
			}
			for _, name := range []string{"dashboard.json", filepath.Join("jobs", "stale.json")} {
				data, readErr := os.ReadFile(filepath.Join(dir, name))
				if readErr != nil || string(data) != old {
					t.Errorf("%s changed after supporting failure: data=%q, error=%v", name, data, readErr)
				}
			}
			for _, skipped := range []string{"write dashboard", "prune job details"} {
				if strings.Contains(err.Error(), skipped) {
					t.Errorf("unexpected phase error %q: %v", skipped, err)
				}
			}
		})
	}
}

func TestWriteAllAggregatesIndependentProjectionFailures(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteAll(dir, sampleConfig(), sampleDashboard(), []models.JobDetail{
		sampleJobDetail("job-alpha"), sampleJobDetail("job-beta"),
	}, models.FlakinessReport{}, models.SearchIndex{})
	if err == nil {
		t.Fatal("WriteAll returned nil error")
	}
	for _, want := range []string{
		"write manifest", "write job detail job-alpha", "write job detail job-beta",
		"write flakiness report", "write search index",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("aggregated error missing %q: %v", want, err)
		}
	}
	for _, skipped := range []string{"write dashboard", "prune job details"} {
		if strings.Contains(err.Error(), skipped) {
			t.Errorf("unexpected phase error %q: %v", skipped, err)
		}
	}
}

func TestWriteAllDashboardFailurePreservesStaleJobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dashboard.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "jobs", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	detail := sampleJobDetail("job-alpha")
	err := WriteAll(dir, sampleConfig(), sampleDashboard(), []models.JobDetail{detail}, models.FlakinessReport{}, models.SearchIndex{})
	if err == nil || !strings.HasPrefix(err.Error(), "write dashboard:") {
		t.Fatalf("WriteAll error = %v, want dashboard error", err)
	}
	for _, name := range []string{"manifest.json", filepath.Join("jobs", models.JobDataFilename(detail.JobID)), "flakiness.json", "search-index.json"} {
		var got map[string]json.RawMessage
		readJSON(t, filepath.Join(dir, name), &got)
	}
	if data, err := os.ReadFile(stale); err != nil || string(data) != `{}` {
		t.Fatalf("stale job file changed after dashboard failure: data=%q, error=%v", data, err)
	}
}

func TestWriteAllPruneFailureLeavesPublishedDashboard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jobs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	dash := sampleDashboard()
	err := WriteAll(dir, sampleConfig(), dash, nil, models.FlakinessReport{}, models.SearchIndex{})
	if err == nil || !strings.HasPrefix(err.Error(), "prune job details:") {
		t.Fatalf("WriteAll error = %v, want pruning error", err)
	}
	var got models.Dashboard
	readJSON(t, filepath.Join(dir, "dashboard.json"), &got)
	if !got.GeneratedAt.Equal(dash.GeneratedAt) || len(got.Jobs) != len(dash.Jobs) {
		t.Fatalf("dashboard not published before pruning failed: %+v", got)
	}
}

func TestWriteManifestOmitsPrivateProjectConfiguration(t *testing.T) {
	dir := t.TempDir()
	cfg := sampleConfig()
	cfg.AI = &project.AI{
		ServiceTier: "flex",
		Headers:     map[string]string{"X-Private-Routing": "private-route"},
	}
	cfg.Notifications = &project.Notifications{Email: &project.EmailNotifications{
		Enabled: true,
		From:    "private-sender@example.com",
		To:      []string{"private-team@example.com"},
		SMTP: project.EmailSMTP{
			Host:     "smtp.internal.example",
			Username: "private-user",
		},
	}}

	if err := WriteManifest(dir, cfg); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}

	for _, secret := range []string{"private-route", "X-Private-Routing", "private-sender@example.com", "private-team@example.com", "smtp.internal.example", "private-user"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("manifest.json leaks email notification config %q: %s", secret, string(data))
		}
	}
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := sampleConfig()

	if err := WriteManifest(dir, cfg); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	var got project.Config
	readJSON(t, filepath.Join(dir, "manifest.json"), &got)
	if got.ID != cfg.ID || got.Name != cfg.Name || got.Branding.SiteURL != cfg.Branding.SiteURL {
		t.Errorf("manifest mismatch: got %+v want %+v", got, cfg)
	}
}

func TestWriteManifestPublishesOnlyAggregateSkillMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := &project.Config{
		ID: "test", Name: "Test", Discovery: project.Discovery{TestGridDashboard: "test"},
		Storage:  project.Storage{Provider: "gcs", Bucket: "bucket"},
		Branding: project.Branding{Title: "Test", BasePath: "/", SiteURL: "https://example.invalid", SourceRepo: project.SourceRepo{Owner: "branding", Name: "repo"}},
		AI: &project.AI{
			SourceRepo: &project.SourceRepo{Owner: "analysis", Name: "source"},
			SkillBundle: &project.SkillBundleManifest{
				Profiles: []string{"prow", "kubernetes"}, EngineCount: 6, ConsumerCount: 11,
				ConsumerBundlePresent: true, Hash: "1234abcd",
			},
		},
	}
	if err := WriteManifest(dir, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"source_repo"`, `"owner": "analysis"`, `"engine_count": 6`, `"consumer_count": 11`, `"hash": "1234abcd"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("manifest missing %s: %s", want, text)
		}
	}
	for _, forbidden := range []string{"consumer.recipe", "procedure", "triggers", "required_evidence", strings.Repeat("a", 64)} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("manifest contains private skill metadata %q: %s", forbidden, text)
		}
	}
}

func TestPublicPatternOutputBackfillsCausalGroupIdentity(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{
		ID: "stable-pattern", ContentHash: "stale", JobID: "job-causal", Subject: "job-causal", BuildsAnalyzed: 3,
		Recurrence: models.PatternRecurrenceMixedCauses, Systemic: true, Confidence: "medium",
		CausalGroups: []models.PatternCausalGroup{
			{Builds: []string{"2", "1"}, RootCause: "missing call", Confidence: "high"},
			{Builds: []string{"3"}, RootCause: "one-off environment failure", Confidence: "medium"},
		},
		Summary: "mixed causes",
	}
	detail := sampleJobDetail(pattern.JobID)
	detail.PatternAnalyses = []models.PatternAnalysis{pattern}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}}
	if err := WriteAll(dir, sampleConfig(), sampleDashboard(), []models.JobDetail{detail}, report, models.SearchIndex{}); err != nil {
		t.Fatal(err)
	}

	var writtenDetail models.JobDetail
	var writtenReport models.FlakinessReport
	readJSON(t, filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)), &writtenDetail)
	readJSON(t, filepath.Join(dir, "flakiness.json"), &writtenReport)
	jobGroup := writtenDetail.PatternAnalyses[0].CausalGroups[0]
	reportGroup := writtenReport.RecurringPatterns[0].CausalGroups[0]
	if jobGroup.ID == "" || jobGroup.ContentHash == "" || jobGroup.ID != reportGroup.ID || jobGroup.ContentHash != reportGroup.ContentHash {
		t.Fatalf("job group=%+v report group=%+v", jobGroup, reportGroup)
	}
	if pattern.CausalGroups[0].ID != "" || pattern.CausalGroups[0].Builds[0] != "2" {
		t.Fatalf("input pattern mutated: %+v", pattern)
	}
}

// TestWriteManifest_OmitsPullRequestCommentConfig keeps operational commenting
// settings off the public site. The frontend has no use for them, and the
// manifest is world-readable on the Pages path.
func TestWriteManifest_OmitsPullRequestCommentConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := sampleConfig()
	dryRun := false
	cfg.PullRequests = &project.PullRequests{
		Enabled: true,
		Comment: &project.PullRequestComment{Enabled: true, DryRun: &dryRun, MaxPerPass: 25},
	}
	if err := WriteManifest(dir, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(data)
	for _, leaked := range []string{"comment", "dry_run", "max_per_pass"} {
		if strings.Contains(manifest, leaked) {
			t.Errorf("manifest publishes %q:\n%s", leaked, manifest)
		}
	}
	// The triage toggle itself must still publish: the nav tab depends on it.
	if !strings.Contains(manifest, "pull_requests") {
		t.Errorf("manifest dropped pull_requests, which the frontend needs:\n%s", manifest)
	}
}
