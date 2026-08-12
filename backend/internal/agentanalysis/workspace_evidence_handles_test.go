package agentanalysis

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildWorkspaceEvidenceHandlesUsesObservedLinesOnly(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 3},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
	)
	want := []WorkspaceEvidenceHandle{
		{ID: "artifact-001", Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
		{ID: "artifact-002", Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 3, LineEnd: 3},
		{ID: "source-001", Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
	}
	if !slices.Equal(handles, want) {
		t.Fatalf("handles=%+v want=%+v", handles, want)
	}
	instruction, err := WorkspaceFinalizationInstruction(handles)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"artifact-001", "artifacts/logs/build.log line 2", "source-001", "source/pkg/controller.go line 3"} {
		if !strings.Contains(instruction, value) {
			t.Fatalf("instruction missing %q: %s", value, instruction)
		}
	}
	if strings.Contains(instruction, "artifact-only-marker") || strings.Contains(instruction, "func reconcile") {
		t.Fatalf("instruction retained evidence content: %s", instruction)
	}
}

func TestBuildWorkspaceEvidenceHandlesPrioritizesFocusedRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	var content strings.Builder
	for line := 1; line <= 100; line++ {
		fmt.Fprintf(&content, "line-%03d\n", line)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "long.log"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "long.log", LineStart: 1, LineEnd: 100},
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "long.log", LineStart: 90, LineEnd: 90},
	)
	if len(handles) != maxWorkspaceEvidencePerRoot {
		t.Fatalf("handle count=%d", len(handles))
	}
	workspaceHandleID(t, handles, WorkspaceArtifactsDir, "long.log", 90)
}

func TestBuildWorkspaceEvidenceHandlesRejectsUnsafeRanges(t *testing.T) {
	sourceRoot, artifactRoot, _, _ := workspaceTestInputs(t)
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	for _, ranges := range [][]WorkspaceEvidenceRange{
		{{Root: WorkspaceArtifactsDir, Path: "../escape.log", LineStart: 1, LineEnd: 1}},
		{{Root: WorkspaceSourceDir, Path: ".git/config", LineStart: 1, LineEnd: 1}},
		{{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 0, LineEnd: 1}},
		{{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 1, LineEnd: 99}},
	} {
		if _, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges); err == nil {
			t.Fatalf("ranges were accepted: %+v", ranges)
		}
	}
}

func workspaceDefaultHandles(t *testing.T, sourceRoot, artifactRoot string) []WorkspaceEvidenceHandle {
	t.Helper()
	return workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 3},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 1, LineEnd: 3},
	)
}

func workspaceTestHandles(t *testing.T, sourceRoot, artifactRoot string, ranges ...WorkspaceEvidenceRange) []WorkspaceEvidenceHandle {
	t.Helper()
	workspaceRoot := t.TempDir()
	if err := os.Symlink(sourceRoot, filepath.Join(workspaceRoot, WorkspaceSourceDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifactRoot, filepath.Join(workspaceRoot, WorkspaceArtifactsDir)); err != nil {
		t.Fatal(err)
	}
	handles, err := BuildWorkspaceEvidenceHandles(workspaceRoot, ranges)
	if err != nil {
		t.Fatal(err)
	}
	return handles
}

func workspaceHandleID(t *testing.T, handles []WorkspaceEvidenceHandle, root, path string, line int) string {
	t.Helper()
	for _, handle := range handles {
		if handle.Root == root && handle.Path == path && handle.LineStart == line && handle.LineEnd == line {
			return handle.ID
		}
	}
	t.Fatalf("handle unavailable: root=%s path=%s line=%d handles=%+v", root, path, line, handles)
	return ""
}

func workspaceCitationSelection(id string) string { return id }
