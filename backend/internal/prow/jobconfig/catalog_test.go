package jobconfig

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestParseCatalog(t *testing.T) {
	data := []byte(`periodics:
- name: periodic-example
  cluster: build
  cron: "0 1 * * *"
  extra_refs:
  - org: example
    repo: project
    base_ref: main
    path_alias: example.io/project
presubmits:
  example/project:
  - name: pull-example-e2e
    context: e2e
    cluster: build
    always_run: false
    optional: true
    run_before_merge: true
    branches: ["^main$"]
    skip_branches: ["^release-"]
    run_if_changed: "test/e2e"
    trigger: "(?m)^/test e2e$"
    rerun_command: "/test e2e"
`)
	jobs, err := ParseCatalog(data, "jobs.yaml")
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	periodic := jobs[0]
	if periodic.JobType != models.JobTypePeriodic || periodic.Cron != "0 1 * * *" || !periodic.TestsRepo("example/project") {
		t.Errorf("periodic = %+v", periodic)
	}
	if len(periodic.Refs) != 1 || periodic.Refs[0].PathAlias != "example.io/project" {
		t.Errorf("periodic refs = %+v", periodic.Refs)
	}
	presubmit := jobs[1]
	if presubmit.JobType != models.JobTypePresubmit || presubmit.Repo != "example/project" {
		t.Errorf("presubmit = %+v", presubmit)
	}
	if presubmit.AlwaysRun == nil || *presubmit.AlwaysRun || !presubmit.Optional || !presubmit.RunBeforeMerge {
		t.Errorf("presubmit flags = %+v", presubmit)
	}
	if got := presubmit.EffectiveRerunCommand(); got != "/test e2e" {
		t.Errorf("rerun command = %q", got)
	}
}

func TestEffectiveRerunCommand_Default(t *testing.T) {
	job := JobDefinition{Name: "pull-example", JobType: models.JobTypePresubmit}
	if got := job.EffectiveRerunCommand(); got != "/test pull-example" {
		t.Fatalf("EffectiveRerunCommand = %q", got)
	}
	if got := (JobDefinition{Name: "periodic-example", JobType: models.JobTypePeriodic}).EffectiveRerunCommand(); got != "" {
		t.Fatalf("periodic rerun command = %q", got)
	}
}

func TestCatalogFromJobs(t *testing.T) {
	catalog := CatalogFromJobs([]models.ProwJob{{
		Name: "pull-e2e", JobType: models.JobTypePresubmit, Repo: "example/project",
	}}, "bucket")
	job, ok := catalog.Jobs["example/project/pull-e2e"]
	if !ok || job.EffectiveRerunCommand() != "/test pull-e2e" {
		t.Fatalf("catalog = %+v", catalog)
	}
}
