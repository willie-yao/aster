package orka

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAnalysisManifestRoundTripAndTaskIdentity(t *testing.T) {
	manifest := NewAnalysisManifest("project", "Project", "contract", "models", "model", APIModeAuto, "v1", 2)
	manifest.SkillSetHash = "skills-hash"
	manifest.ValidationKey = "validation-key"
	manifest.MinGCSBytes = 123
	manifest.SetConsecutiveFailures("job", "test", 7)
	manifest.SetBuild("job", "1", "build-scope", "tool-scope", "logs/job/1/", "")
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1"}}
	tc := models.TestCase{Name: "test", JUnitFile: "junit.xml", FailureMessage: "boom"}
	first, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manifest.TaskRef("job", run, 1, tc)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name == second.Name {
		t.Fatalf("duplicate test indices produced the same Task %q", first.Name)
	}
	if first.ToolScope != "tool-scope" || !strings.Contains(first.Prompt, "Consecutive failures on this test: at least 3") {
		t.Fatalf("task ref = %+v", first)
	}

	dir := t.TempDir()
	if err := manifest.Write(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAnalysisManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	if got != first || loaded.ConsecutiveFailures["job::test"] != persistentFailurePromptFloor || !loaded.Jobs["job"] || loaded.MinToolCalls != 2 || loaded.MinGCSBytes != 123 || loaded.APIMode != APIModeAuto || loaded.SkillSetHash != "skills-hash" || loaded.ValidationKey != "validation-key" {
		t.Fatalf("loaded task ref = %+v, jobs = %+v, min_tool_calls = %d", got, loaded.Jobs, loaded.MinToolCalls)
	}
}

func TestAnalysisManifestRecurrenceIdentityStabilizesAtPersistenceFloor(t *testing.T) {
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1"}}
	tc := models.TestCase{Name: "test", FailureMessage: "failure"}
	manifest := NewAnalysisManifest("project", "Project", "contract", "models", "model", APIModeAuto, "v1", 2)
	manifest.SetBuild("job", "1", "build-scope", "tool-scope", "logs/job/1/", "")
	withoutRecurrence, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SetConsecutiveFailures("job", "test", persistentFailurePromptFloor)
	atFloor, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SetConsecutiveFailures("job", "test", 9)
	aboveFloor, err := manifest.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	if withoutRecurrence.Name == atFloor.Name {
		t.Fatalf("persistence evidence did not change Task identity %q", withoutRecurrence.Name)
	}
	if atFloor != aboveFloor {
		t.Fatalf("persistent recurrence changed Task identity: at floor=%+v above floor=%+v", atFloor, aboveFloor)
	}
}

func TestAnalysisManifestPromptSeedChangesTaskIdentity(t *testing.T) {
	run := models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "1"}}
	tc := models.TestCase{Name: "test", FailureBody: "failure"}
	first := NewAnalysisManifest("project", "Project", "contract", "models", "model", APIModeAuto, "v1", 2)
	first.ValidationKey = "key"
	first.SetBuild("job", "1", "build-scope", "tool-scope", "logs/job/1/", "tree-a")
	firstRef, err := first.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	second := NewAnalysisManifest("project", "Project", "contract", "models", "model", APIModeAuto, "v1", 2)
	second.ValidationKey = "key"
	second.SetBuild("job", "1", "build-scope", "tool-scope", "logs/job/1/", "tree-b")
	secondRef, err := second.TaskRef("job", run, 0, tc)
	if err != nil {
		t.Fatal(err)
	}
	if firstRef.Name == secondRef.Name {
		t.Fatalf("different artifact seeds produced the same Task %q", firstRef.Name)
	}
	if firstRef.Prompt != secondRef.Prompt {
		t.Fatalf("artifact seed changed the base failure prompt")
	}
}
