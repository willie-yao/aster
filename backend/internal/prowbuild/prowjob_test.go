package prowbuild

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestFetchProwJobMetadata_Presubmit(t *testing.T) {
	path := "pr-logs/pull/example_project/42/pull-e2e/100/prowjob.json"
	b := &fakeBackend{objects: map[string]string{path: `{
  "metadata":{"name":"pj-id"},
  "spec":{"type":"presubmit","job":"pull-e2e","context":"pull-e2e","rerun_command":"/test pull-e2e","cluster":"build","report":true,
    "refs":{"org":"example","repo":"project","base_ref":"main","base_sha":"base","pulls":[{"number":42,"author":"dev","sha":"head","head_ref":"fix","link":"https://example/pr/42"}]}},
  "status":{"state":"success","description":"Job succeeded.","url":"https://prow/build","build_id":"100","startTime":"2026-07-20T01:00:00Z","completionTime":"2026-07-20T02:00:00Z"}
}`}}
	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "example/project"}, JobName: "pull-e2e", BuildID: "100", PullNumber: "42"}
	got, err := FetchProwJobMetadata(context.Background(), b, loc)
	if err != nil {
		t.Fatalf("FetchProwJobMetadata: %v", err)
	}
	if got.Type != models.JobTypePresubmit || got.RerunCommand != "/test pull-e2e" || got.State != "success" {
		t.Errorf("metadata = %+v", got)
	}
	if got.Refs == nil || got.Refs.FullRepo() != "example/project" || got.Refs.BaseSHA != "base" || got.Refs.Pulls[0].SHA != "head" {
		t.Errorf("refs = %+v", got.Refs)
	}
}

func TestFetchProwJobMetadata_RejectsWrongPull(t *testing.T) {
	path := "pr-logs/pull/example_project/42/pull-e2e/100/prowjob.json"
	b := &fakeBackend{objects: map[string]string{path: `{"spec":{"type":"presubmit","job":"pull-e2e","refs":{"org":"example","repo":"project","pulls":[{"number":7}]}}}`}}
	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "example/project"}, JobName: "pull-e2e", BuildID: "100", PullNumber: "42"}
	if _, err := FetchProwJobMetadata(context.Background(), b, loc); err == nil {
		t.Fatal("expected pull mismatch error")
	}
}

func TestFetchProwJobMetadataRejectsWrongRepo(t *testing.T) {
	path := "pr-logs/pull/example_project/42/pull-e2e/100/prowjob.json"
	b := &fakeBackend{objects: map[string]string{path: `{"spec":{"type":"presubmit","job":"pull-e2e","refs":{"org":"other","repo":"project","pulls":[{"number":42,"sha":"head"}]}}}`}}
	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "example/project"}, JobName: "pull-e2e", BuildID: "100", PullNumber: "42"}
	if _, err := FetchProwJobMetadata(context.Background(), b, loc); err == nil || !strings.Contains(err.Error(), "repo is") {
		t.Fatalf("error = %v", err)
	}
}

func TestFetchProwJobMetadataRejectsMissingPullSHA(t *testing.T) {
	path := "pr-logs/pull/example_project/42/pull-e2e/100/prowjob.json"
	b := &fakeBackend{objects: map[string]string{path: `{"spec":{"type":"presubmit","job":"pull-e2e","refs":{"org":"example","repo":"project","pulls":[{"number":42}]}}}`}}
	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "example/project"}, JobName: "pull-e2e", BuildID: "100", PullNumber: "42"}
	if _, err := FetchProwJobMetadata(context.Background(), b, loc); err == nil || !strings.Contains(err.Error(), "head SHA") {
		t.Fatalf("error = %v", err)
	}
}
