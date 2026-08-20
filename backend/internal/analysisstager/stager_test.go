package analysisstager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestExecuteStagesVerifiedWorkspace(t *testing.T) {
	fixture := stagerFixture(t)
	if err := Execute(context.Background(), fixture.stage, fixture.execution, fixture.options()); err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.VerifySourceWorkspace(context.Background(), stagerSourceRoot(fixture.workspaceRoot, "primary"), fixture.stage.Source.Revision); err != nil {
		t.Fatal(err)
	}
	if count := strings.TrimSpace(runStagerGit(t, stagerSourceRoot(fixture.workspaceRoot, "primary"), "rev-list", "HEAD", "--count")); count != "1" {
		t.Fatalf("staged source history count=%s", count)
	}
	artifacts, err := agentanalysis.ReadWorkspaceArtifactManifest(filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash), fixture.stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.VerifyArtifactFiles(filepath.Join(fixture.workspaceRoot, agentanalysis.WorkspaceArtifactsDir), artifacts); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.workspaceRoot, agentanalysis.WorkspaceResultDir))
	if err != nil || len(entries) != 0 {
		t.Fatalf("result entries=%v err=%v", entries, err)
	}
	written, err := agentanalysis.ReadWorkspaceExecutionRequestFile(fixture.requestRoot)
	if err != nil || written.Hash != fixture.execution.Hash {
		t.Fatalf("written request=%+v err=%v", written, err)
	}
}

func TestExecuteRejectsExecutionStageMismatch(t *testing.T) {
	fixture := stagerFixture(t)
	mismatch, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceModePolicy(
		fixture.execution.Manifest, agentanalysis.WorkspaceSourceModeIgnoreExecutable, fixture.execution.ModelProvider,
		time.Duration(fixture.execution.TimeoutSeconds)*time.Second, fixture.execution.MaxSteps, fixture.execution.ModelContextTokens,
		fixture.execution.ModelOutputTokens, fixture.execution.OutputLimitBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), fixture.stage, mismatch, fixture.options()); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteStagesIgnorePolicyInputToPreservePolicyOutput(t *testing.T) {
	fixture := stagerFixtureWithSourceSetupAndPolicies(t, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("#!/bin/sh\necho fixture\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}, agentanalysis.WorkspaceSourceModeIgnoreExecutable, agentanalysis.WorkspaceSourceModePreserve)
	inputSource := stagerSourceRoot(filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash), "primary")
	if mode := strings.Fields(runStagerGit(t, inputSource, "ls-files", "--stage", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("input index mode=%s", mode)
	}
	if info, err := os.Stat(filepath.Join(inputSource, "script.sh")); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("input worktree mode=%v err=%v", info, err)
	}
	if err := Execute(t.Context(), fixture.stage, fixture.execution, fixture.options()); err != nil {
		t.Fatal(err)
	}
	outputSource := stagerSourceRoot(fixture.workspaceRoot, "primary")
	if err := agentanalysis.VerifyPreparedSourceWorkspace(t.Context(), outputSource, fixture.stage.Source.Revision, agentanalysis.WorkspaceSourceModePreserve); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(outputSource, "script.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("output worktree mode=%v err=%v", info, err)
	}
}

func TestExecuteStagesTrackedInternalSourceSymlink(t *testing.T) {
	fixture := stagerFixtureWithSourceSetup(t, func(root string) {
		if err := os.Symlink(filepath.Join("pkg", "controller.go"), filepath.Join(root, "controller-link.go")); err != nil {
			t.Fatal(err)
		}
	})
	if err := Execute(t.Context(), fixture.stage, fixture.execution, fixture.options()); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(stagerSourceRoot(fixture.workspaceRoot, "primary"), "controller-link.go"))
	if err != nil || target != filepath.Join("pkg", "controller.go") {
		t.Fatalf("staged symlink target=%q err=%v", target, err)
	}
}

func TestExecuteRejectsDirectorySymlinkCycle(t *testing.T) {
	fixture := stagerFixtureWithSourceSetup(t, func(root string) {
		if err := os.Symlink(".", filepath.Join(root, "loop")); err != nil {
			t.Fatal(err)
		}
	})
	if err := Execute(t.Context(), fixture.stage, fixture.execution, fixture.options()); err == nil || !strings.Contains(err.Error(), "directory symlinks contain a cycle") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsChangedArtifactInput(t *testing.T) {
	fixture := stagerFixture(t)
	path := filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash, agentanalysis.WorkspaceArtifactsDir, "logs", "build.log")
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), fixture.stage, fixture.execution, fixture.options()); err == nil || !strings.Contains(err.Error(), "staged artifacts") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsSourceSymlink(t *testing.T) {
	fixture := stagerFixture(t)
	sourceRoot := stagerSourceRoot(filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash), "primary")
	if err := os.Symlink("pkg/controller.go", filepath.Join(sourceRoot, "linked.go")); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), fixture.stage, fixture.execution, fixture.options()); err == nil || !strings.Contains(err.Error(), "staged source") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRejectsSourceGitMetadataSymlink(t *testing.T) {
	fixture := stagerFixture(t)
	sourceRoot := stagerSourceRoot(filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash), "primary")
	if err := os.Symlink(filepath.Join("..", "..", "pkg", "controller.go"), filepath.Join(sourceRoot, ".git", "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), fixture.stage, fixture.execution, fixture.options()); err == nil || !strings.Contains(err.Error(), "Git metadata contains a symlink") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteRequiresEmptyWorkspace(t *testing.T) {
	fixture := stagerFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.workspaceRoot, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), fixture.stage, fixture.execution, fixture.options()); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishPreparedSnapshotRejectsContaminatedReuse(t *testing.T) {
	fixture := stagerFixture(t)
	snapshot := filepath.Join(fixture.inputRoot, fixture.stage.ManifestHash)
	inputRoot := t.TempDir()
	if _, err := PublishPreparedSnapshot(
		t.Context(), inputRoot, fixture.execution.Manifest,
		stagerSourceRoot(snapshot, "primary"),
		filepath.Join(snapshot, agentanalysis.WorkspaceArtifactsDir),
		fixture.stage.InputSourceModePolicy,
	); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(inputRoot, fixture.stage.ManifestHash, agentanalysis.WorkspaceArtifactsDir, "unexpected.log")
	if err := os.WriteFile(extra, []byte("unexpected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishPreparedSnapshot(
		t.Context(), inputRoot, fixture.execution.Manifest,
		stagerSourceRoot(snapshot, "primary"),
		filepath.Join(snapshot, agentanalysis.WorkspaceArtifactsDir),
		fixture.stage.InputSourceModePolicy,
	); err == nil || !strings.Contains(err.Error(), "sealed manifest") {
		t.Fatalf("error=%v", err)
	}
}

type stagerTestFixture struct {
	inputRoot     string
	workspaceRoot string
	requestRoot   string
	stage         agentanalysis.WorkspaceStageRequest
	execution     agentanalysis.WorkspaceExecutionRequest
}

func (f stagerTestFixture) options() Options {
	return Options{InputRoot: f.inputRoot, WorkspaceRoot: f.workspaceRoot, RequestRoot: f.requestRoot}
}

func stagerFixture(t *testing.T) stagerTestFixture {
	t.Helper()
	return stagerFixtureWithSourceSetup(t, nil)
}

func stagerFixtureWithSourceSetup(t *testing.T, setup func(string)) stagerTestFixture {
	t.Helper()
	return stagerFixtureWithSourceSetupAndPolicies(t, setup, agentanalysis.WorkspaceSourceModePreserve, agentanalysis.WorkspaceSourceModePreserve)
}

func stagerFixtureWithSourceSetupAndPolicies(t *testing.T, setup func(string), inputPolicy, outputPolicy agentanalysis.WorkspaceSourceModePolicy) stagerTestFixture {
	t.Helper()
	inputRoot := t.TempDir()
	pending := filepath.Join(inputRoot, "pending")
	sourceRoot := stagerSourceRoot(pending, "primary")
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
	if err := agentanalysis.WriteWorkspaceArtifactManifest(pending, files); err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceStageRequestWithSourceModePolicies(manifest, inputPolicy, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	provider := modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway, API: "chat_completions",
		Endpoint: "https://gateway.example.svc.cluster.local/v1/chat/completions", Model: "fixture",
		Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
	execution, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceModePolicy(manifest, outputPolicy, provider, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, filepath.Join(inputRoot, manifest.Hash)); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSnapshot(inputRoot, manifest.Hash)
	if err != nil {
		t.Fatal(err)
	}
	unlockSnapshot(lock)
	return stagerTestFixture{inputRoot: inputRoot, workspaceRoot: t.TempDir(), requestRoot: t.TempDir(), stage: stage, execution: execution}
}

func stagerSourceRoot(root, sourceID string) string {
	return filepath.Join(root, agentanalysis.WorkspaceSourcesDir, sourceID)
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

func TestExecuteStagesMultipleSourcesAtDistinctRevisions(t *testing.T) {
	inputRoot := t.TempDir()
	pending := filepath.Join(inputRoot, "pending")
	artifactRoot := filepath.Join(pending, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "logs", "build.log"), []byte("deterministic failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientRevision := createStagerSource(t, stagerSourceRoot(pending, "client"), "client")
	serverRevision := createStagerSource(t, stagerSourceRoot(pending, "server"), "server")
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	failure := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": clientRevision}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "deterministic failure"},
	}
	manifest, err := agentanalysis.NewWorkspaceManifestWithSources(failure, []agentanalysis.WorkspaceSourceRef{
		{ID: "server", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: serverRevision}},
		{ID: "client", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: clientRevision}},
	}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.WriteWorkspaceArtifactManifest(pending, files); err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	provider := modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway, API: "chat_completions",
		Endpoint: "https://gateway.example.svc.cluster.local/v1/chat/completions", Model: "fixture",
		Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
	execution, err := agentanalysis.NewWorkspaceExecutionRequest(manifest, provider, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(inputRoot, manifest.Hash)
	if err := os.Rename(pending, final); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSnapshot(inputRoot, manifest.Hash)
	if err != nil {
		t.Fatal(err)
	}
	unlockSnapshot(lock)
	workspaceRoot := t.TempDir()
	requestRoot := t.TempDir()
	if err := Execute(t.Context(), stage, execution, Options{InputRoot: inputRoot, WorkspaceRoot: workspaceRoot, RequestRoot: requestRoot}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct {
		revision string
		marker   string
	}{"client": {clientRevision, "client"}, "server": {serverRevision, "server"}} {
		root := stagerSourceRoot(workspaceRoot, id)
		if err := agentanalysis.VerifyPreparedSourceWorkspace(t.Context(), root, want.revision, agentanalysis.WorkspaceSourceModePreserve); err != nil {
			t.Fatalf("verify %s: %v", id, err)
		}
		data, err := os.ReadFile(filepath.Join(root, "identity.txt"))
		if err != nil || strings.TrimSpace(string(data)) != want.marker {
			t.Fatalf("source %s content=%q err=%v", id, data, err)
		}
	}
}

func createStagerSource(t *testing.T, root, marker string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "identity.txt"), []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}, {"config", "commit.gpgsign", "false"}, {"add", "."}, {"commit", "-qm", marker}} {
		runStagerGit(t, root, args...)
	}
	revision := strings.TrimSpace(runStagerGit(t, root, "rev-parse", "HEAD"))
	if err := agentanalysis.SetPreparedSourceModePolicy(t.Context(), root, agentanalysis.WorkspaceSourceModePreserve); err != nil {
		t.Fatal(err)
	}
	return revision
}
