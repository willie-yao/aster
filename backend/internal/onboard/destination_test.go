package onboard

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInspectFileDestinationExistingDirectoryWithoutConflicts(t *testing.T) {
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"project.yaml": "new", "prompts/system.md": "prompt"}
	actions, stale, err := inspectFileDestination(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	want := []DestinationFilePlan{
		{Path: "project.yaml", Action: destinationActionCreate},
		{Path: "prompts/system.md", Action: destinationActionCreate},
	}
	if !reflect.DeepEqual(actions, want) || len(stale) != 0 {
		t.Fatalf("actions=%+v stale=%v", actions, stale)
	}
	content, err := os.ReadFile(unrelated)
	if err != nil || string(content) != "keep" {
		t.Fatalf("unrelated file changed: %q %v", content, err)
	}
}

func TestWriteFilesRequiresExplicitUpdate(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(filename, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeFiles(dir, map[string]string{"project.yaml": "new"}, false, nil)
	var conflict *destinationConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v", err)
	}
	content, readErr := os.ReadFile(filename)
	if readErr != nil || string(content) != "old" {
		t.Fatalf("existing file changed: %q %v", content, readErr)
	}
}

func TestWriteFilesUpdatePreservesUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"project.yaml": "new", "prompts/system.md": "prompt"}
	if err := writeFiles(dir, files, true, nil); err != nil {
		t.Fatal(err)
	}
	for filename, want := range map[string]string{"project.yaml": "new", "prompts/system.md": "prompt", "notes.txt": "keep"} {
		content, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil || string(content) != want {
			t.Fatalf("%s = %q, %v; want %q", filename, content, err, want)
		}
	}
}

func TestInspectFileDestinationWarnsAboutStaleDeploymentFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "deploy", "values.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, staleFiles, err := inspectFileDestination(dir, map[string]string{
		"project.yaml":                 "new",
		"prompts/system.md":            "prompt",
		".github/workflows/deploy.yml": "workflow",
		"CHECKLIST.md":                 "checklist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staleFiles, []string{"deploy/values.yaml"}) {
		t.Fatalf("stale files = %v", staleFiles)
	}
	if err := writeFiles(dir, map[string]string{
		"project.yaml":                 "new",
		"prompts/system.md":            "prompt",
		".github/workflows/deploy.yml": "workflow",
		"CHECKLIST.md":                 "checklist",
	}, false, nil); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(stale)
	if err != nil || string(content) != "old" {
		t.Fatalf("stale file changed: %q %v", content, err)
	}
}

func TestInspectFileDestinationRejectsPartialPathConflict(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".github"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := inspectFileDestination(dir, map[string]string{".github/workflows/deploy.yml": "workflow"})
	if err == nil || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectFileDestinationRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "..", "outside")
	_, _, err := inspectFileDestination(dir, map[string]string{"../outside": "unsafe"})
	if err == nil || !strings.Contains(err.Error(), "safe repo-relative path") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created: %v", statErr)
	}
}

func TestValidateDashboardConsumerDirRejectsCredentialWithoutLeaking(t *testing.T) {
	opts := Options{OutDir: "../fixture-token-dashboard", GitHubToken: "fixture-token"}
	err := validateDashboardConsumerDir(opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") || strings.Contains(err.Error(), opts.GitHubToken) {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectFileDestinationRejectsSymlinkedGeneratedParent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".github")); err != nil {
		t.Fatal(err)
	}
	_, _, err := inspectFileDestination(dir, map[string]string{".github/workflows/deploy.yml": "workflow"})
	if err == nil || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "workflows", "deploy.yml")); !os.IsNotExist(statErr) {
		t.Fatalf("outside file was created: %v", statErr)
	}
}

func TestWriteFilesRejectsUnreviewedReplacement(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(filename, []byte("appeared later"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeFiles(dir, map[string]string{"project.yaml": "new"}, true, []DestinationFilePlan{{Path: "project.yaml", Action: destinationActionCreate}})
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("error = %v", err)
	}
	content, readErr := os.ReadFile(filename)
	if readErr != nil || string(content) != "appeared later" {
		t.Fatalf("unreviewed replacement occurred: %q %v", content, readErr)
	}
}

func TestWriteFilesUsesTheInspectedNormalizedDirectory(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(filename, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := "  " + dir + "  "
	expected := []DestinationFilePlan{{Path: "project.yaml", Action: destinationActionReplace}}
	if err := writeFiles(outDir, map[string]string{"project.yaml": "new"}, true, expected); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filename)
	if err != nil || string(content) != "new" {
		t.Fatalf("normalized destination content = %q, %v", content, err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("untrimmed destination was used: %v", err)
	}
}

func TestWriteReviewedFileRejectsReplacementThatDisappeared(t *testing.T) {
	dir := t.TempDir()
	err := writeReviewedFile(dir, DestinationFilePlan{Path: "project.yaml", Action: destinationActionReplace}, []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "now create instead of replace") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "project.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("file was created without review: %v", statErr)
	}
}

func TestWriteReviewedFileDoesNotFollowSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".github")); err != nil {
		t.Fatal(err)
	}
	err := writeReviewedFile(dir, DestinationFilePlan{Path: ".github/workflows/deploy.yml", Action: destinationActionCreate}, []byte("workflow"))
	if err == nil || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "workflows")); !os.IsNotExist(statErr) {
		t.Fatalf("directory was created outside the destination: %v", statErr)
	}
}

func TestWriteReviewedFileCreatesParentsWithoutBulkMkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dashboard")
	action := DestinationFilePlan{Path: ".github/workflows/deploy.yml", Action: destinationActionCreate}
	if err := writeReviewedFile(dir, action, []byte("workflow")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if err != nil || string(content) != "workflow" {
		t.Fatalf("content = %q, %v", content, err)
	}
}
