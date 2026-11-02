package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployWorkflowStagesPublicData(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "reusable-deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string
				Run  string
				Uses string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	positions := make(map[string]int)
	runs := make(map[string]string)
	for i, step := range workflow.Jobs["build-and-deploy"].Steps {
		if step.Name != "" {
			positions[step.Name] = i
			runs[step.Name] = step.Run
		}
		if strings.HasPrefix(step.Uses, "actions/upload-pages-artifact@") {
			positions["upload"] = i
		}
		if strings.Contains(step.Run, "cp -r") || step.Name == "Strip non-published data files" {
			t.Fatal("Pages must stage the public contract instead of copying and stripping private names")
		}
	}
	previous := -1
	for _, name := range []string{
		"Save data cache", "Stage public input data", "Write public build provenance",
		"Build frontend", "Stage public output data", "upload",
	} {
		position, ok := positions[name]
		if !ok || position <= previous {
			t.Fatalf("missing or out-of-order Pages step %q", name)
		}
		previous = position
	}
	for _, name := range []string{"Stage public input data", "Stage public output data"} {
		if !strings.Contains(runs[name], "engine/hack/stage-public-data.mjs") {
			t.Fatalf("%q does not use public data staging", name)
		}
	}
	if !strings.Contains(runs["Stage public input data"], "$PROJECT_DIR/data") {
		t.Fatal("pre-fetched consumer data must pass through public staging")
	}
	if !strings.Contains(runs["Stage public output data"], "engine/frontend/dist/data") {
		t.Fatal("final Pages output must be replaced by public staging")
	}
}
