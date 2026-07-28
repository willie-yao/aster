package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type doctorMapFS map[string]string

func (f doctorMapFS) ReadFile(path string) ([]byte, error) {
	value, ok := f[filepath.Clean(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(value), nil
}

type doctorFakeSweeper struct {
	jobs  []models.ProwJob
	err   error
	calls int
}

func (f *doctorFakeSweeper) Discover(context.Context, *project.Config, bool) ([]models.ProwJob, error) {
	f.calls++
	return append([]models.ProwJob(nil), f.jobs...), f.err
}

const doctorProjectYAML = `id: project
name: Project
testgrid:
  dashboard: dashboard
storage:
  provider: gcs
  bucket: bucket
branding:
  title: Project
  base_path: /dashboard
  site_url: https://example.test/dashboard
  source_repo:
    owner: example
    name: project
`

const doctorPagesWorkflow = `jobs:
  deploy:
    uses: example/workflow@main
    with:
      ai-api: ${{ vars.AI_API }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-model: ${{ vars.AI_MODEL }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
`

func doctorFiles(extra map[string]string) doctorMapFS {
	files := doctorMapFS{
		"/consumer/project.yaml":      doctorProjectYAML,
		"/consumer/prompts/system.md": "# Prompt\n",
	}
	for path, value := range extra {
		files[filepath.Clean(path)] = value
	}
	return files
}

func TestDoctor_ValidPagesScaffold(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow}),
		sweeper: sweeper,
	})
	if report.HasFailures() {
		t.Fatalf("unexpected failures: %+v", report.Checks)
	}
	if sweeper.calls != 1 {
		t.Fatalf("discovery calls = %d", sweeper.calls)
	}
	if !hasDoctorCheck(report, "Pages AI values", DoctorWarn) || !hasDoctorCheck(report, "Prow discovery", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesMissingProviderMappings(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs: {}\n"}),
		sweeper: sweeper,
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_KubernetesPlaceholdersAreActionable(t *testing.T) {
	values := `persistence:
  storageClass: "<your-rwx-storage-class>"
ai:
  enabled: true
  api: chat_completions
  endpoint: "http://<your-model-svc>/v1/chat/completions"
  model: "<your-model-id>"
`
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: sweeper,
	})
	if !hasDoctorCheck(report, "Kubernetes storage", DoctorFail) || !hasDoctorCheck(report, "Kubernetes AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
	for _, check := range report.Checks {
		if check.Status == DoctorFail && check.Action == "" {
			t.Fatalf("failure has no next action: %+v", check)
		}
	}
}

func TestDoctor_KubernetesDisabledAI(t *testing.T) {
	values := `persistence:
  storageClass: azurefile-csi
ai:
  enabled: false
`
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: sweeper,
	})
	if report.HasFailures() || !hasDoctorCheck(report, "Kubernetes AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_InvalidProjectStopsBeforeDiscovery(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job"}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorMapFS{"/consumer/project.yaml": "unknown: true\n"},
		sweeper: sweeper,
	})
	if !report.HasFailures() || sweeper.calls != 0 || !hasDoctorCheck(report, "project.yaml", DoctorFail) {
		t.Fatalf("report=%+v calls=%d", report, sweeper.calls)
	}
}

func TestDoctor_MissingPromptAndZeroJobs(t *testing.T) {
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "    ai: false\n"})
	delete(files, "/consumer/prompts/system.md")
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files: files, sweeper: &doctorFakeSweeper{},
	})
	if !hasDoctorCheck(report, "prompts/system.md", DoctorFail) || !hasDoctorCheck(report, "Prow discovery", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_DiscoveryErrorIsActionable(t *testing.T) {
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "    ai: false\n"}),
		sweeper: &doctorFakeSweeper{err: errors.New("catalog unavailable")},
	})
	for _, check := range report.Checks {
		if check.Name == "Prow discovery" {
			if check.Status != DoctorFail || check.Action == "" || !strings.Contains(check.Detail, "catalog unavailable") {
				t.Fatalf("check = %+v", check)
			}
			return
		}
	}
	t.Fatal("missing Prow discovery check")
}

func hasDoctorCheck(report DoctorReport, name string, status DoctorStatus) bool {
	for _, check := range report.Checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}

type doctorFailingWriter struct{}

func (doctorFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestWriteDoctorReport_PropagatesOutputError(t *testing.T) {
	report := DoctorReport{Checks: []DoctorCheck{{Name: "check", Status: DoctorPass, Detail: "ok"}}}
	if err := WriteDoctorReport(doctorFailingWriter{}, report); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteDoctorReport_SanitizesTerminalControls(t *testing.T) {
	var out strings.Builder
	report := DoctorReport{Checks: []DoctorCheck{{Name: "check\nforged", Status: DoctorFail, Detail: "bad\x1b[31m", Action: "fix\rnow"}}}
	if err := WriteDoctorReport(&out, report); err != nil {
		t.Fatalf("WriteDoctorReport: %v", err)
	}
	if strings.ContainsAny(out.String(), "\n\r\x1b") && strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("terminal controls were not sanitized: %q", out.String())
	}
	if !strings.Contains(out.String(), "check?forged") || !strings.Contains(out.String(), "fix?now") {
		t.Fatalf("sanitized fields missing: %q", out.String())
	}
}
