package onboard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReusableDeployContract(t *testing.T) {
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
		"ai-cache-generation:",
		"AI_CACHE_GENERATION: ${{ inputs.ai-cache-generation }}",
		"WORKFLOW_REPOSITORY: ${{ job.workflow_repository }}",
		"WORKFLOW_REF: ${{ job.workflow_ref }}",
		"WORKFLOW_SHA: ${{ job.workflow_sha }}",
		"repository: ${{ steps.engine-identity.outputs.repository }}",
		"ref: ${{ steps.engine-identity.outputs.sha }}",
		"ACTUAL_SHA=\"$(git rev-parse HEAD)\"",
		"engine/frontend/public/data/provenance.json",
		"reusable_workflow_sha: process.env.WORKFLOW_SHA",
		"engine_commit: process.env.ENGINE_COMMIT",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("reusable deploy workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"github.job_workflow_sha",
		"repository: willie-yao/prow-ai-dashboard\n          # Build the engine code",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("reusable deploy workflow contains stale contract %q", forbidden)
		}
	}
}
