package orka

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAnalysisManifestRoundTripAndTaskIdentity(t *testing.T) {
	manifest := NewAnalysisManifest("project", "Project", "contract", "models", "model", "v1")
	manifest.SetBuild("job", "1", "build-scope", "tool-scope", "logs/job/1/")
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
	if first.ToolScope != "tool-scope" || first.Prompt == "" {
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
	if got != first || !loaded.Jobs["job"] {
		t.Fatalf("loaded task ref = %+v, jobs = %+v", got, loaded.Jobs)
	}
}
