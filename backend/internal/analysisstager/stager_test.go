package analysisstager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func TestExecuteStagesVerifiedWorkspace(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixture(t)
	if err := Execute(context.Background(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.VerifySourceWorkspace(context.Background(), filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir), request.Source.Revision); err != nil {
		t.Fatal(err)
	}
	if count := strings.TrimSpace(runStagerGit(t, filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir), "rev-list", "HEAD", "--count")); count != "1" {
		t.Fatalf("staged source history count=%s", count)
	}
	if err := agentanalysis.VerifyArtifactFiles(filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir), request.Artifacts); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(workspaceRoot, agentanalysis.WorkspaceResultDir))
	if err != nil || len(entries) != 0 {
		t.Fatalf("result entries=%v err=%v", entries, err)
	}
}

func TestExecuteStagesIgnorePolicyInputToPreservePolicyOutput(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixtureWithSourceSetupAndPolicies(t, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("#!/bin/sh\necho fixture\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}, agentanalysis.WorkspaceSourceModeIgnoreExecutable, agentanalysis.WorkspaceSourceModePreserve)
	inputSource := filepath.Join(inputRoot, request.ManifestHash, agentanalysis.WorkspaceSourceDir)
	if mode := strings.Fields(runStagerGit(t, inputSource, "ls-files", "--stage", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("input index mode=%s", mode)
	}
	if info, err := os.Stat(filepath.Join(inputSource, "script.sh")); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("input worktree mode=%v err=%v", info, err)
	}
	if err := Execute(t.Context(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err != nil {
		t.Fatal(err)
	}
	outputSource := filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir)
	if err := agentanalysis.VerifyPreparedSourceWorkspace(t.Context(), outputSource, request.Source.Revision, agentanalysis.WorkspaceSourceModePreserve); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(outputSource, "script.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("output worktree mode=%v err=%v", info, err)
	}
}

func TestExecuteStagesTrackedInternalSourceSymlink(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixtureWithSourceSetup(t, func(root string) {
		if err := os.Symlink(filepath.Join("pkg", "controller.go"), filepath.Join(root, "controller-link.go")); err != nil {
			t.Fatal(err)
		}
	})
	if err := Execute(t.Context(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir, "controller-link.go"))
	if err != nil || target != filepath.Join("pkg", "controller.go") {
		t.Fatalf("staged symlink target=%q err=%v", target, err)
	}
}

func TestExecuteRejectsDirectorySymlinkCycle(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixtureWithSourceSetup(t, func(root string) {
		if err := os.Symlink(".", filepath.Join(root, "loop")); err != nil {
			t.Fatal(err)
		}
	})
	if err := Execute(t.Context(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err == nil || !strings.Contains(err.Error(), "directory symlinks contain a cycle") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsChangedArtifactInput(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixture(t)
	path := filepath.Join(inputRoot, request.ManifestHash, agentanalysis.WorkspaceArtifactsDir, "logs", "build.log")
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err == nil || !strings.Contains(err.Error(), "staged artifacts") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsSourceSymlink(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixture(t)
	sourceRoot := filepath.Join(inputRoot, request.ManifestHash, agentanalysis.WorkspaceSourceDir)
	if err := os.Symlink("pkg/controller.go", filepath.Join(sourceRoot, "linked.go")); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err == nil || !strings.Contains(err.Error(), "staged source") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsSourceGitMetadataSymlink(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixture(t)
	sourceRoot := filepath.Join(inputRoot, request.ManifestHash, agentanalysis.WorkspaceSourceDir)
	if err := os.Symlink(filepath.Join("..", "..", "pkg", "controller.go"), filepath.Join(sourceRoot, ".git", "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err == nil || !strings.Contains(err.Error(), "Git metadata contains a symlink") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRequiresEmptyWorkspace(t *testing.T) {
	inputRoot, workspaceRoot, request := stagerFixture(t)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), request, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot}); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("error=%v", err)
	}
}

func stagerFixture(t *testing.T) (string, string, agentanalysis.WorkspaceStageRequest) {
	t.Helper()
	return stagerFixtureWithSourceSetup(t, nil)
}

func stagerFixtureWithSourceSetup(t *testing.T, setup func(string)) (string, string, agentanalysis.WorkspaceStageRequest) {
	t.Helper()
	return stagerFixtureWithSourceSetupAndPolicies(t, setup, agentanalysis.WorkspaceSourceModePreserve, agentanalysis.WorkspaceSourceModePreserve)
}

func stagerFixtureWithSourceSetupAndPolicies(t *testing.T, setup func(string), inputPolicy, outputPolicy agentanalysis.WorkspaceSourceModePolicy) (string, string, agentanalysis.WorkspaceStageRequest) {
	t.Helper()
	inputRoot := t.TempDir()
	pending := filepath.Join(inputRoot, "pending")
	sourceRoot := filepath.Join(pending, agentanalysis.WorkspaceSourceDir)
	artifactRoot := filepath.Join(pending, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(filepath.Join(sourceRoot, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "pkg", "controller.go"), []byte("package controller\n\nfunc reconcile() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(sourceRoot)
	}
	runStagerGit(t, sourceRoot, "init", "-q")
	runStagerGit(t, sourceRoot, "config", "user.name", "Test")
	runStagerGit(t, sourceRoot, "config", "user.email", "test@example.com")
	runStagerGit(t, sourceRoot, "config", "commit.gpgsign", "false")
	runStagerGit(t, sourceRoot, "add", ".")
	runStagerGit(t, sourceRoot, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runStagerGit(t, sourceRoot, "rev-parse", "HEAD"))
	if inputPolicy == agentanalysis.WorkspaceSourceModeIgnoreExecutable {
		if err := os.Chmod(filepath.Join(sourceRoot, "script.sh"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := agentanalysis.SetPreparedSourceModePolicy(t.Context(), sourceRoot, inputPolicy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "logs", "build.log"), []byte("setup\nartifact-only-marker specific failure\ncleanup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	failure := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "specific failure"},
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(failure, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceStageRequestWithSourceModePolicies(manifest, inputPolicy, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, filepath.Join(inputRoot, manifest.Hash)); err != nil {
		t.Fatal(err)
	}
	return inputRoot, t.TempDir(), stage
}

func runStagerGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
