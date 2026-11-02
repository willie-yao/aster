package models

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAIAnalysisPublicJSONOmitsModelAndRetainsConsumedFields(t *testing.T) {
	analysis := AIAnalysis{
		GeneratedAt: "2026-09-01T00:00:00Z", Model: "private-model",
		RootCause: "controller failed", Severity: "High", SuggestedFix: "fix the controller",
		RelevantFiles: []string{"controllers/example.go"}, Mode: "agentic",
	}
	data, err := json.Marshal(analysis)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "private-model") || strings.Contains(text, `"model"`) {
		t.Fatalf("public analysis leaked model: %s", text)
	}
	for _, want := range []string{
		`"generated_at":"2026-09-01T00:00:00Z"`, `"root_cause":"controller failed"`,
		`"severity":"High"`, `"suggested_fix":"fix the controller"`, `"relevant_files":["controllers/example.go"]`, `"mode":"agentic"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("public analysis missing %s: %s", want, text)
		}
	}
}

func TestPullRequestFinishedWire(t *testing.T) {
	started := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	finished := started.Add(30 * time.Minute)
	payload := struct {
		Checks  []PullRequestCheck    `json:"checks"`
		Members []SharedFailureMember `json:"members"`
	}{
		Checks: []PullRequestCheck{
			{JobName: "pull-e2e", JobID: "example/project/pull-e2e", BuildID: "101", Started: started.Add(2 * time.Hour)},
			{JobName: "pull-e2e", JobID: "example/project/pull-e2e", BuildID: "100", Started: started.Add(time.Hour), Finished: finished.Add(time.Hour)},
			{JobName: "pull-e2e", JobID: "example/project/pull-e2e", BuildID: "99", Passed: true, Started: started, Finished: finished},
		},
		Members: []SharedFailureMember{
			{Number: 1, BuildID: "101", Started: started.Add(2 * time.Hour)},
			{Number: 2, BuildID: "100", Started: started.Add(time.Hour), Finished: finished.Add(time.Hour)},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../../frontend/tests/fixtures/pull-request-checks.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, fixture); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, compact.Bytes()) {
		t.Fatalf("pull request wire shape differs from frontend fixture:\ngot  %s\nwant %s", data, compact.Bytes())
	}
}

func TestPullRequestObservationWire(t *testing.T) {
	payload := struct {
		Indexes []PullRequestIndex `json:"indexes"`
	}{}
	for _, truncated := range []bool{false, true} {
		payload.Indexes = append(payload.Indexes, PullRequestIndex{
			GeneratedAt: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC),
			Repo:        "example/project", Truncated: truncated,
			PullRequests: []PullRequestSummary{
				{Number: 7, Repo: "example/project", CIState: PullRequestCIUnknown},
				{Number: 8, Repo: "example/project", CIState: PullRequestCIUnknown, ChecksUnobserved: 1},
				{Number: 9, Repo: "example/project", CIState: PullRequestCIUnknown, ChecksObserved: 1, ChecksUnobserved: 1},
			},
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../../frontend/tests/fixtures/pull-request-observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, fixture); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, compact.Bytes()) {
		t.Fatalf("pull request observations differ from frontend fixture:\ngot  %s\nwant %s", data, compact.Bytes())
	}
}
