package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
				PassRateRecent: 0.95,
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

	path := filepath.Join(dir, "dashboard.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard.json: %v", err)
	}

	var got models.Dashboard
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal dashboard.json: %v", err)
	}
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

	path := filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read job detail: %v", err)
	}

	var got models.JobDetail
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal job detail: %v", err)
	}
	if got.Name != detail.Name {
		t.Errorf("Name = %q, want %q", got.Name, detail.Name)
	}
	if len(got.Runs) != len(detail.Runs) {
		t.Errorf("len(Runs) = %d, want %d", len(got.Runs), len(detail.Runs))
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
	data, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)))
	if err != nil {
		t.Fatal(err)
	}
	var written models.JobDetail
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
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
		JobID: "job", Subject: "job", BuildsAnalyzed: 3, Systemic: true,
		Confidence: "high", SharedRootCause: "shared cause", Summary: "summary",
	}}}
	if err := WriteFlakinessReport(dir, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written models.FlakinessReport
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	pattern := written.RecurringPatterns[0]
	if pattern.ID == "" || pattern.ContentHash != models.PatternHash(pattern) {
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
		report := models.FlakinessReport{LowPassRate: []models.LowPassRateEntry{{
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
		data, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
		if err != nil {
			t.Fatal(err)
		}
		var written struct {
			LowPassRate []map[string]any `json:"low_pass_rate"`
		}
		if err := json.Unmarshal(data, &written); err != nil {
			t.Fatal(err)
		}
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
	jobData, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)))
	if err != nil {
		t.Fatal(err)
	}
	flakinessData, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var writtenDetail models.JobDetail
	var writtenReport models.FlakinessReport
	if err := json.Unmarshal(jobData, &writtenDetail); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(flakinessData, &writtenReport); err != nil {
		t.Fatal(err)
	}
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
		TestGrid:  project.TestGrid{Dashboard: "sig-cluster-lifecycle-cluster-api-provider-azure"},
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
	manifestData, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var gotManifest project.Config
	if err := json.Unmarshal(manifestData, &gotManifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if gotManifest.ID != "capz" || gotManifest.Branding.Title != "CAPZ Prow Dashboard" || gotManifest.Discovery.TestInfraRevision != strings.Repeat("a", 40) || gotManifest.Discovery.ResolvedTestInfraRevision != strings.Repeat("a", 40) {
		t.Errorf("manifest round-trip mismatch: %+v", gotManifest)
	}

	// dashboard.json exists
	if _, err := os.Stat(filepath.Join(dir, "dashboard.json")); err != nil {
		t.Error("dashboard.json missing")
	}
	// job files exist
	for _, d := range details {
		p := filepath.Join(dir, "jobs", models.JobDataFilename(d.JobID))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("job file %s missing", p)
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
}

func TestWriteAllPrunesStaleJobFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "jobs", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	detail := sampleJobDetail("job-alpha")
	if err := WriteAll(dir, sampleConfig(), sampleDashboard(), []models.JobDetail{detail}, models.FlakinessReport{}, models.SearchIndex{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale job file still exists: %v", err)
	}
}

func TestWriteAllRemovesRetiredPublicProjection(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "remediations.json")
	if err := os.WriteFile(stale, []byte(`{"remediations":{"pattern":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(dir, "remediation_state.json")
	if err := os.WriteFile(retained, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(dir, sampleConfig(), sampleDashboard(), nil, models.FlakinessReport{}, models.SearchIndex{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("retired public projection still exists: %v", err)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("retained private state was deleted: %v", err)
	}
	if !slices.Contains(NonPublishedFiles, "remediation_state.json") ||
		!slices.Contains(NonPublishedFiles, "remediation_prow_catalog.json") {
		t.Fatalf("legacy private files left the denylist: %v", NonPublishedFiles)
	}
}

func TestWriteManifest_OmitsAIEndpointAndModel(t *testing.T) {
	dir := t.TempDir()
	cfg := sampleConfig()
	cfg.AI = &project.AI{
		Endpoint: "https://internal.example/v1/chat/completions",
		Model:    "internal-only-model-name",
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

	// Raw-string assertions: the published JSON must not leak the model
	// identifier or endpoint URL, even when set on the in-memory config.
	if strings.Contains(string(data), "internal-only-model-name") {
		t.Errorf("manifest.json leaks AI model identifier: %s", string(data))
	}
	if strings.Contains(string(data), "internal.example") {
		t.Errorf("manifest.json leaks AI endpoint URL: %s", string(data))
	}
	for _, secret := range []string{"private-sender@example.com", "private-team@example.com", "smtp.internal.example", "private-user"} {
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

	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var got project.Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if got.ID != cfg.ID || got.Name != cfg.Name || got.Branding.SiteURL != cfg.Branding.SiteURL {
		t.Errorf("manifest mismatch: got %+v want %+v", got, cfg)
	}
}

func TestWriteManifestPublishesOnlyAggregateSkillMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := &project.Config{
		ID: "test", Name: "Test", Source: project.Source{}, TestGrid: project.TestGrid{Dashboard: "test"},
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

func TestPublicPatternOutputDefaultsRepeatedCausalGroupRemediation(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{
		JobID: "job-causal", Subject: "job-causal", BuildsAnalyzed: 3,
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

	jobData, err := os.ReadFile(filepath.Join(dir, "jobs", models.JobDataFilename(detail.JobID)))
	if err != nil {
		t.Fatal(err)
	}
	flakinessData, err := os.ReadFile(filepath.Join(dir, "flakiness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var writtenDetail models.JobDetail
	var writtenReport models.FlakinessReport
	if err := json.Unmarshal(jobData, &writtenDetail); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(flakinessData, &writtenReport); err != nil {
		t.Fatal(err)
	}
	jobPattern := writtenDetail.PatternAnalyses[0]
	reportPattern := writtenReport.RecurringPatterns[0]
	if len(jobPattern.RemediationInvestigations) != len(jobPattern.CausalGroups) ||
		len(reportPattern.RemediationInvestigations) != len(reportPattern.CausalGroups) {
		t.Fatalf("job summaries=%+v report summaries=%+v", jobPattern.RemediationInvestigations, reportPattern.RemediationInvestigations)
	}
	jobSummary := jobPattern.RemediationInvestigations[0]
	reportSummary := reportPattern.RemediationInvestigations[0]
	if jobSummary != reportSummary || jobSummary.State != models.PatternRemediationNotInvestigated {
		t.Fatalf("job summary=%+v report summary=%+v", jobSummary, reportSummary)
	}
	if jobSummary.CausalGroupID != jobPattern.CausalGroups[0].ID || jobSummary.CausalGroupHash != jobPattern.CausalGroups[0].ContentHash {
		t.Fatalf("summary=%+v group=%+v", jobSummary, jobPattern.CausalGroups[0])
	}
	if len(pattern.RemediationInvestigations) != 0 || pattern.CausalGroups[0].ID != "" || pattern.CausalGroups[0].Builds[0] != "2" {
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
