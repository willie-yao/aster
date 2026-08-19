package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func detailFor(number int) models.PullRequestDetail {
	return models.PullRequestDetail{
		PullRequestSummary: models.PullRequestSummary{Number: number, Repo: "example/project"},
	}
}

func TestWritePullRequestsWritesIndexAndDetails(t *testing.T) {
	dir := t.TempDir()
	index := models.PullRequestIndex{Repo: "example/project", PullRequests: []models.PullRequestSummary{{Number: 7}}}
	if err := WritePullRequests(dir, index, []models.PullRequestDetail{detailFor(7)}, models.SharedFailureIndex{}); err != nil {
		t.Fatalf("WritePullRequests: %v", err)
	}

	var got models.PullRequestIndex
	readJSON(t, filepath.Join(dir, PullRequestIndexFilename), &got)
	if got.Repo != "example/project" || len(got.PullRequests) != 1 {
		t.Fatalf("index = %+v", got)
	}
	var detail models.PullRequestDetail
	readJSON(t, filepath.Join(dir, "pull-requests", "7.json"), &detail)
	if detail.Number != 7 {
		t.Fatalf("detail = %+v", detail)
	}
	// Empty slices serialize as [] so the frontend never sees null.
	if detail.Checks == nil {
		t.Error("checks should serialize as an empty array")
	}
}

func TestWritePullRequestsPrunesClosedPullRequests(t *testing.T) {
	dir := t.TempDir()
	both := []models.PullRequestDetail{detailFor(7), detailFor(8)}
	if err := WritePullRequests(dir, models.PullRequestIndex{}, both, models.SharedFailureIndex{}); err != nil {
		t.Fatalf("WritePullRequests: %v", err)
	}
	if err := WritePullRequests(dir, models.PullRequestIndex{}, []models.PullRequestDetail{detailFor(7)}, models.SharedFailureIndex{}); err != nil {
		t.Fatalf("WritePullRequests: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "pull-requests", "7.json")); err != nil {
		t.Errorf("still-open pull request detail was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pull-requests", "8.json")); !os.IsNotExist(err) {
		t.Errorf("closed pull request detail survived: %v", err)
	}
}

func TestWritePullRequestIndexNormalizesNilList(t *testing.T) {
	dir := t.TempDir()
	if err := WritePullRequestIndex(dir, models.PullRequestIndex{}); err != nil {
		t.Fatalf("WritePullRequestIndex: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, PullRequestIndexFilename))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["pull_requests"]) != "[]" {
		t.Errorf("pull_requests = %s, want []", raw["pull_requests"])
	}
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func TestWritePullRequestsWritesSharedFailures(t *testing.T) {
	dir := t.TempDir()
	shared := models.SharedFailureIndex{
		Repo: "example/project",
		Failures: []models.SharedFailure{{
			ID: "abc123", BaseRef: "main", JobName: "pull-project-e2e", TestName: "[It] creates a cluster",
			PullRequests: []models.SharedFailureMember{{Number: 7}, {Number: 8}},
			Escalatable:  true,
		}},
	}
	if err := WritePullRequests(dir, models.PullRequestIndex{}, nil, shared); err != nil {
		t.Fatalf("WritePullRequests: %v", err)
	}

	var got models.SharedFailureIndex
	readJSON(t, filepath.Join(dir, SharedFailureIndexFilename), &got)
	if len(got.Failures) != 1 || got.Failures[0].ID != "abc123" {
		t.Fatalf("shared failures = %+v", got.Failures)
	}
	if len(got.Failures[0].PullRequests) != 2 {
		t.Errorf("members = %+v, want both pull requests", got.Failures[0].PullRequests)
	}
}

func TestWriteSharedFailuresNormalizesNilList(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSharedFailures(dir, models.SharedFailureIndex{}); err != nil {
		t.Fatalf("WriteSharedFailures: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, SharedFailureIndexFilename))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["failures"]) != "[]" {
		t.Errorf("failures = %s, want []", raw["failures"])
	}
}
