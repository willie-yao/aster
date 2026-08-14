package onboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

func TestRunArtifactSmokeReportsAvailabilityWithoutDiagnosis(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "logs", "periodic-project", "123")
	if err := os.MkdirAll(filepath.Join(buildDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"prowjob.json":           `{"metadata":{"name":"periodic-project-123"},"spec":{"job":"periodic-project","type":"periodic"},"status":{"state":"success","build_id":"123"}}`,
		"started.json":           `{"timestamp":1}`,
		"build-log.txt":          "build output",
		"artifacts/junit_01.xml": `<testsuite name="smoke"></testsuite>`,
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(buildDir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := &Plan{
		Project:   project.Config{Storage: project.Storage{Provider: "local", Base: root}},
		Discovery: DiscoveryPlan{Jobs: []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic, JobID: "periodic-project"}}},
	}
	report := runArtifactSmoke(context.Background(), plan, 1)
	if !report.ReadOnly || len(report.Jobs) != 1 || len(report.Jobs[0].Builds) != 1 {
		t.Fatalf("report = %+v", report)
	}
	availability := report.Jobs[0].Availability
	for name, count := range map[string]ArtifactCount{
		"prowjob":   availability.ProwJobJSON,
		"started":   availability.StartedJSON,
		"build-log": availability.BuildLog,
		"junit":     availability.JUnit,
		"artifacts": availability.Artifacts,
	} {
		if count.Checked != 1 || count.Available != 1 {
			t.Errorf("%s = %+v", name, count)
		}
	}
}

func TestRunArtifactSmokeWarnsWhenSampledBuildsHaveNoJUnit(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "logs", "periodic-project", "123")
	if err := os.MkdirAll(filepath.Join(buildDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"prowjob.json":      `{"metadata":{"name":"periodic-project-123"},"spec":{"job":"periodic-project","type":"periodic"},"status":{"state":"success","build_id":"123"}}`,
		"started.json":      `{"timestamp":1}`,
		"build-log.txt":     "build output",
		"artifacts/tap.log": "ok 1 example",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(buildDir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := &Plan{
		Project:   project.Config{Storage: project.Storage{Provider: "local", Base: root}},
		Discovery: DiscoveryPlan{Jobs: []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic, JobID: "periodic-project"}}},
	}
	report := runArtifactSmoke(context.Background(), plan, 1)
	if len(report.Jobs) != 1 || report.Jobs[0].Availability.JUnit.Checked != 1 || report.Jobs[0].Availability.JUnit.Available != 0 {
		t.Fatalf("report = %+v", report)
	}
	joined := strings.Join(report.Warnings, "\n")
	for _, want := range []string{"no JUnit XML in 1 sampled build", "no JUnit XML in any sampled build", "synthesized build-level failures"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %s", want, joined)
		}
	}
}
