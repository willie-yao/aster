package onboard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReusableDeploySerializesProjectRuns(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".github", "workflows", "reusable-deploy.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reusable deploy workflow: %v", err)
	}
	workflow := string(data)
	for _, want := range []string{
		"group: prow-ai-dashboard-${{ github.repository }}-${{ inputs.project_dir }}",
		"cancel-in-progress: false",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("reusable deploy workflow missing %q", want)
		}
	}
}
