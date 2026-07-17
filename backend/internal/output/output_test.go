package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
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
		Name:    name,
		JobID:   name,
		JobType: models.JobTypePeriodic,
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

	path := filepath.Join(dir, "jobs", "periodic-cluster-api-provider-azure-e2e-main.json")
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

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"periodic-cluster-api-provider-azure-e2e-main", "periodic-cluster-api-provider-azure-e2e-main"},
		{"job/with:special chars!", "job-with-special-chars-"},
		{"has spaces here", "has-spaces-here"},
		{"keep_underscores_too", "keep_underscores_too"},
		{"dots.are.replaced", "dots-are-replaced"},
	}
	for _, tt := range tests {
		got := SanitizeFilename(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
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

	if err := WriteAll(dir, sampleConfig(), dash, details, flakiness, models.SearchIndex{GeneratedAt: "2025-01-15T12:00:00Z"}); err != nil {
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
	if gotManifest.ID != "capz" || gotManifest.Branding.Title != "CAPZ Prow Dashboard" {
		t.Errorf("manifest round-trip mismatch: %+v", gotManifest)
	}

	// dashboard.json exists
	if _, err := os.Stat(filepath.Join(dir, "dashboard.json")); err != nil {
		t.Error("dashboard.json missing")
	}
	// job files exist
	for _, d := range details {
		p := filepath.Join(dir, "jobs", SanitizeFilename(d.JobID)+".json")
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
