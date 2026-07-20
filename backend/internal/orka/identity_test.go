package orka

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAnalysisContractHashTracksSemanticInputs(t *testing.T) {
	base := AnalysisContract{
		Provider: "models", Model: "model-a", Version: "v1", Timeout: "10m", Retries: 1, MinToolCalls: 2,
		AcceptanceVersion: AcceptanceVersion, SystemPrompt: "system",
		Tools: []ToolContract{{Name: "read", Definition: map[string]any{"description": "read files"}}},
	}
	first, err := AnalysisContractHash(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AnalysisContractHash(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("contract hashes differ: %q != %q", first, second)
	}
	changes := []AnalysisContract{base, base, base, base, base, base, base, base, base, base, base, base}
	changes[0].Model = "model-b"
	changes[1].Version = "v2"
	changes[2].Timeout = "20m"
	changes[3].SystemPrompt = "changed"
	changes[4].Tools = []ToolContract{{Name: "read", Definition: map[string]any{"description": "changed"}}}
	changes[5].MinToolCalls = 3
	changes[6].AcceptanceVersion++
	changes[7].SkillSetHash = "changed"
	changes[8].ToolAuthSecret = "changed"
	changes[9].ToolAuthKey = "changed"
	changes[10].ValidationKeyHash = "changed"
	changes[11].MinGCSBytes = 100
	for i, changed := range changes {
		got, err := AnalysisContractHash(changed)
		if err != nil {
			t.Fatal(err)
		}
		if got == first {
			t.Errorf("change %d did not change contract hash", i)
		}
	}
}

func TestAnalysisTaskNameSeparatesScopeContractAndTestIndex(t *testing.T) {
	base := AnalysisTaskName("project-a", "build-a", "contract-a", 0, "prompt")
	if base == "" || !strings.HasPrefix(base, "az-analysis-") {
		t.Fatalf("task name = %q", base)
	}
	variants := []string{
		AnalysisTaskName("project-b", "build-a", "contract-a", 0, "prompt"),
		AnalysisTaskName("project-a", "build-b", "contract-a", 0, "prompt"),
		AnalysisTaskName("project-a", "build-a", "contract-b", 0, "prompt"),
		AnalysisTaskName("project-a", "build-a", "contract-a", 1, "prompt"),
		AnalysisTaskName("project-a", "build-a", "contract-a", 0, "changed"),
	}
	for i, got := range variants {
		if got == base {
			t.Errorf("variant %d collided with %q", i, base)
		}
	}
}

func TestBuildScopeSeparatesProjectsAndJobs(t *testing.T) {
	projectA := ProjectScopeID("a", "gcs", "bucket", "", "", "")
	projectB := ProjectScopeID("b", "gcs", "bucket", "", "", "")
	base := BuildScopeID(projectA, "job-a", "1", "logs/job-a/1/")
	for i, got := range []string{
		BuildScopeID(projectB, "job-a", "1", "logs/job-a/1/"),
		BuildScopeID(projectA, "job-b", "1", "logs/job-b/1/"),
		BuildScopeID(projectA, "job-a", "1", "logs/job-a/other/"),
	} {
		if got == base {
			t.Errorf("build-scope variant %d collided with %q", i, base)
		}
	}
}

func TestToolScopeTracksContract(t *testing.T) {
	base := ToolScopeID("build", "contract-a")
	if got := ToolScopeID("build", "contract-b"); got == base {
		t.Fatalf("different Tool contracts produced the same scope %q", got)
	}
}

func TestFailurePromptIncludesShardAndLocation(t *testing.T) {
	prompt := FailurePrompt("project", "job", "logs/job/1/", models.TestCase{
		Name: "test", FailureLocation: "test/e2e/foo.go:42", JUnitFile: "junit-03.xml", FailureMessage: "boom",
	})
	for _, want := range []string{"Job: job", "Build: logs/job/1/", "JUnit file: junit-03.xml", "test/e2e/foo.go:42", "boom"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
}
