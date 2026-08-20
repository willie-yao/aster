package agentanalysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestWorkspaceMultiSourceEvidenceKeepsSourceIdentity(t *testing.T) {
	primaryRoot, artifactRoot, failure, _ := workspaceTestInputs(t)
	dependencyRoot := t.TempDir()
	if err := copyWorkspaceTestTree(primaryRoot, dependencyRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependencyRoot, "pkg", "controller.go"), []byte("package controller\n\nfunc reconcileDependency() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, dependencyRoot, "add", "pkg/controller.go")
	runWorkspaceGit(t, dependencyRoot, "commit", "-qm", "dependency revision")
	primaryRevision := strings.TrimSpace(runWorkspaceGit(t, primaryRoot, "rev-parse", "HEAD"))
	dependencyRevision := strings.TrimSpace(runWorkspaceGit(t, dependencyRoot, "rev-parse", "HEAD"))

	workspaceRoot := t.TempDir()
	sourcesRoot := filepath.Join(workspaceRoot, WorkspaceSourcesDir)
	if err := os.Mkdir(sourcesRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for id, root := range map[string]string{"dependency": dependencyRoot, "primary": primaryRoot} {
		if err := copyWorkspaceTestTree(root, filepath.Join(sourcesRoot, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	sources := []WorkspaceSourceRef{
		{ID: "dependency", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: dependencyRevision}},
		{ID: "primary", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: primaryRevision}},
	}
	manifest, err := NewWorkspaceManifestWithSources(failure, sources, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	handles, diagnostics, err := BuildWorkspaceEvidenceHandles(workspaceRoot, []WorkspaceEvidenceRange{
		{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
		{Root: WorkspaceSourceDir, SourceID: "dependency", Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
		{Root: WorkspaceSourceDir, SourceID: "primary", Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.AcceptedSourceHandleCount != 2 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	instruction, err := WorkspaceFinalizationInstruction(handles)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"sources/dependency/pkg/controller.go line 3", "sources/primary/pkg/controller.go line 3"} {
		if !strings.Contains(instruction, path) {
			t.Fatalf("instruction missing %q: %s", path, instruction)
		}
	}
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	dependencyID := workspaceHandleIDForSource(t, handles, "dependency", "pkg/controller.go", 3)
	primaryID := workspaceHandleIDForSource(t, handles, "primary", "pkg/controller.go", 3)
	var structured map[string]any
	if err := json.Unmarshal([]byte(workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{artifactID}, []any{dependencyID, primaryID})), &structured); err != nil {
		t.Fatal(err)
	}
	structured["relevant_file_ids"] = []string{primaryID}
	raw, _ := json.Marshal(structured)
	analysis, validation, err := ParseWorkspaceAnalysis(string(raw), handles, manifest, artifactRoot, sourcesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || !slices.Equal(analysis.RelevantFiles, []string{"pkg/controller.go"}) || len(analysis.SourceCitations) != 2 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
	if analysis.SourceCitations[0].SourceID != "dependency" || analysis.SourceCitations[0].Quote != "func reconcileDependency() {}" || analysis.SourceCitations[1].SourceID != "primary" || analysis.SourceCitations[1].Quote != "func reconcile() {}" {
		t.Fatalf("citations=%+v", analysis.SourceCitations)
	}

	structured["relevant_file_ids"] = []string{dependencyID}
	dependencyRelevant, _ := json.Marshal(structured)
	_, dependencyValidation, err := ParseWorkspaceAnalysis(string(dependencyRelevant), handles, manifest, artifactRoot, sourcesRoot)
	if err != nil || dependencyValidation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Contains(dependencyValidation.Codes, WorkspaceInvalidRelevantFile) {
		t.Fatalf("dependency relevant file validation=%+v err=%v", dependencyValidation, err)
	}

	tampered := analysis
	tampered.SourceCitations = slices.Clone(analysis.SourceCitations)
	tampered.SourceCitations[0].SourceID = "primary"
	if _, _, err := ValidateWorkspaceAnalysis(tampered, manifest, artifactRoot, sourcesRoot); err == nil {
		t.Fatal("citation was verified against the wrong source root")
	}
}

func workspaceHandleIDForSource(t *testing.T, handles []WorkspaceEvidenceHandle, sourceID, path string, line int) string {
	t.Helper()
	for _, handle := range handles {
		if handle.Root == WorkspaceSourceDir && handle.SourceID == sourceID && handle.Path == path && handle.LineStart == line && handle.LineEnd == line {
			return handle.ID
		}
	}
	t.Fatalf("source handle unavailable: source=%s path=%s line=%d handles=%+v", sourceID, path, line, handles)
	return ""
}
