package onboard

import (
	"reflect"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

func TestBuildPromptJobSummariesUsesSweepAndDiscoveryMetadata(t *testing.T) {
	source := Repo{FullName: "example/project"}
	jobs := []models.ProwJob{
		{Name: "presubmit-e2e", JobType: models.JobTypePresubmit, Repo: "example/project", ConfigFile: "config/presubmits.yaml"},
		{Name: "periodic-e2e", JobType: models.JobTypePeriodic, ConfigFile: "config/periodics.yaml", Branch: "main"},
	}
	definitions := []jobconfig.JobDefinition{
		{
			Name: "periodic-e2e", JobType: models.JobTypePeriodic, ConfigFile: "config/periodics.yaml",
			Branches: []string{"release-.*"},
			Refs: []jobconfig.RepoRef{
				{Org: "example", Repo: "project", BaseRef: "main"},
				{Org: "example", Repo: "dependency", BaseRef: "stable"},
			},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-b, dashboard-a"},
		},
		{
			Name: "presubmit-e2e", JobType: models.JobTypePresubmit, Repo: "example/project", ConfigFile: "config/presubmits.yaml",
			Branches:    []string{"main"},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
	}

	got := buildPromptJobSummaries(jobs, definitions, source, "selected-dashboard")
	if len(got) != 2 || got[0].Name != "periodic-e2e" || got[1].Name != "presubmit-e2e" {
		t.Fatalf("summaries are not sorted: %+v", got)
	}
	if got[0].Repo != source.FullName {
		t.Fatalf("periodic repo = %q", got[0].Repo)
	}
	if !reflect.DeepEqual(got[0].Branches, []string{"example/dependency@stable", "example/project@main", "main", "release-.*"}) {
		t.Fatalf("periodic branches = %v", got[0].Branches)
	}
	if !reflect.DeepEqual(got[0].Dashboards, []string{"dashboard-a", "dashboard-b"}) {
		t.Fatalf("periodic dashboards = %v", got[0].Dashboards)
	}
	if !reflect.DeepEqual(got[1].Branches, []string{"main"}) || !reflect.DeepEqual(got[1].Dashboards, []string{"dashboard-a"}) {
		t.Fatalf("presubmit summary = %+v", got[1])
	}
}

func TestBuildPromptJobSummariesFallsBackToFinalSweepMetadata(t *testing.T) {
	got := buildPromptJobSummaries([]models.ProwJob{{
		Name: "periodic", JobType: models.JobTypePeriodic, ConfigFile: "periodics.yaml", Branch: "main",
	}}, nil, Repo{FullName: "example/project"}, "dashboard-a")
	want := []promptJobSummary{{
		Name: "periodic", Type: models.JobTypePeriodic, ConfigFile: "periodics.yaml", Repo: "example/project",
		Branches: []string{"main"}, Dashboards: []string{"dashboard-a"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries = %+v, want %+v", got, want)
	}
}
