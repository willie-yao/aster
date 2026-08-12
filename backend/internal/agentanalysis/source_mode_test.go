package agentanalysis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type preparedSourceSnapshot struct {
	head          string
	tree          string
	index         string
	controllerSum string
	scriptSum     string
	symlinkTarget string
	manifestHash  string
}

func TestConfigurePreparedSourceModePolicySealsModeOnlyProjection(t *testing.T) {
	root, _, manifest, revision := preparedModeFixtureUnsealed(t)
	before := snapshotPreparedSource(t, root, manifest.Hash)

	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySourceWorkspace(t.Context(), root, revision); err == nil || !strings.Contains(err.Error(), "tracked files changed") {
		t.Fatalf("mode-only drift error=%v", err)
	}
	policy, err := configurePreparedSourceModePolicy(t.Context(), root, revision, func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if policy != WorkspaceSourceModeIgnoreExecutable {
		t.Fatalf("policy=%q", policy)
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy); err != nil {
		t.Fatal(err)
	}
	after := snapshotPreparedSource(t, root, manifest.Hash)
	if before != after {
		t.Fatalf("prepared source identity changed:\nbefore=%+v\nafter=%+v", before, after)
	}
	if mode := strings.Fields(runWorkspaceGit(t, root, "ls-files", "--stage", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("index mode=%s", mode)
	}
	if mode := strings.Fields(runWorkspaceGit(t, root, "ls-tree", "HEAD", "script.sh"))[0]; mode != "100755" {
		t.Fatalf("tree mode=%s", mode)
	}
	info, err := os.Stat(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("worktree executable bits=%#o", info.Mode().Perm())
	}
}

func TestConfigurePreparedSourceModePolicyPreservesCapableFilesystem(t *testing.T) {
	root, _, _, revision := preparedModeFixtureUnsealed(t)
	if err := SetPreparedSourceModePolicy(t.Context(), root, WorkspaceSourceModeIgnoreExecutable); err != nil {
		t.Fatal(err)
	}
	policy, err := ConfigurePreparedSourceModePolicy(t.Context(), root, revision)
	if err != nil {
		t.Fatal(err)
	}
	if policy != WorkspaceSourceModePreserve {
		t.Fatalf("policy=%q", policy)
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurePreparedSourceModePolicyRejectsModeDriftOnCapableFilesystem(t *testing.T) {
	root, _, _, revision := preparedModeFixtureUnsealed(t)
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigurePreparedSourceModePolicy(t.Context(), root, revision); err == nil || !strings.Contains(err.Error(), "mode-preserving filesystem") {
		t.Fatalf("error=%v", err)
	}
}

func TestPreparedSourceModePolicyStillRejectsMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "content",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("#!/bin/sh\necho changed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "index mode",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				runWorkspaceGit(t, root, "update-index", "--chmod=-x", "script.sh")
			},
		},
		{
			name: "staged content",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("#!/bin/sh\necho staged\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runWorkspaceGit(t, root, "add", "script.sh")
				original := runWorkspaceGit(t, root, "show", "HEAD:script.sh")
				if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "untracked file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("extra\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "ignored file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("extra\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink target",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Remove(filepath.Join(root, "controller-link.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("script.sh", filepath.Join(root, "controller-link.go")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "Git metadata link",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("config", filepath.Join(root, ".git", "unsafe-link")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, policy, _, revision := preparedModeFixture(t)
			tc.mutate(t, root)
			if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy); err == nil {
				t.Fatal("mutated prepared source was accepted")
			}
		})
	}
}

func TestPreparedSourceModePolicyMismatchIsRejected(t *testing.T) {
	root, policy, _, revision := preparedModeFixture(t)
	if policy != WorkspaceSourceModeIgnoreExecutable {
		t.Fatalf("policy=%q", policy)
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, WorkspaceSourceModePreserve); err == nil || !strings.Contains(err.Error(), "mode policy changed") {
		t.Fatalf("error=%v", err)
	}
	if err := SetPreparedSourceModePolicy(t.Context(), root, WorkspaceSourceModePreserve); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, WorkspaceSourceModeIgnoreExecutable); err == nil || !strings.Contains(err.Error(), "mode policy changed") {
		t.Fatalf("error=%v", err)
	}
}

func TestPreparedSourceModePolicyChangesOnlyPreparedIdentities(t *testing.T) {
	root, _, manifest, revision := preparedModeFixture(t)
	gateway := testGatewayProvider("https://model-gateway.platform.svc.cluster.local:8443/v1", "test-model")
	preserveStage, err := NewWorkspaceStageRequestWithSourceModePolicies(manifest, WorkspaceSourceModePreserve, WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	ignoreStage, err := NewWorkspaceStageRequestWithSourceModePolicies(manifest, WorkspaceSourceModeIgnoreExecutable, WorkspaceSourceModeIgnoreExecutable)
	if err != nil {
		t.Fatal(err)
	}
	mixedStage, err := NewWorkspaceStageRequestWithSourceModePolicies(manifest, WorkspaceSourceModeIgnoreExecutable, WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	preserveRequest, err := NewWorkspaceExecutionRequestWithSourceModePolicy(manifest, WorkspaceSourceModePreserve, gateway, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	ignoreRequest, err := NewWorkspaceExecutionRequestWithSourceModePolicy(manifest, WorkspaceSourceModeIgnoreExecutable, gateway, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if preserveStage.Hash == ignoreStage.Hash || preserveStage.Hash == mixedStage.Hash || ignoreStage.Hash == mixedStage.Hash || preserveRequest.Hash == ignoreRequest.Hash {
		t.Fatalf("stage hashes %q %q %q request hashes %q %q", preserveStage.Hash, ignoreStage.Hash, mixedStage.Hash, preserveRequest.Hash, ignoreRequest.Hash)
	}
	if preserveStage.ManifestHash != ignoreStage.ManifestHash || preserveRequest.Manifest.Hash != ignoreRequest.Manifest.Hash || manifest.Hash != preserveStage.ManifestHash {
		t.Fatal("manifest identity changed with source mode policy")
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, WorkspaceSourceModeIgnoreExecutable); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPreparedSourceWorkspaceAcceptsReadOnlyMount(t *testing.T) {
	root, policy, _, revision := preparedModeFixture(t)
	restore := makePreparedTreeReadOnly(t, root)
	defer restore()
	if err := VerifyPreparedSourceWorkspace(context.Background(), root, revision, policy); err != nil {
		t.Fatal(err)
	}
}

func preparedModeFixture(t *testing.T) (string, WorkspaceSourceModePolicy, WorkspaceManifest, string) {
	t.Helper()
	root, _, manifest, revision := preparedModeFixtureUnsealed(t)
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := configurePreparedSourceModePolicy(t.Context(), root, revision, func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	return root, policy, manifest, revision
}

func preparedModeFixtureUnsealed(t *testing.T) (string, string, WorkspaceManifest, string) {
	t.Helper()
	root, artifactRoot, request, source := workspaceTestInputs(t)
	if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("#!/bin/sh\necho fixture\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("pkg", "controller.go"), filepath.Join(root, "controller-link.go")); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, root, "add", "script.sh", "controller-link.go")
	runWorkspaceGit(t, root, "commit", "-qm", "add executable and symlink")
	revision := strings.TrimSpace(runWorkspaceGit(t, root, "rev-parse", "HEAD"))
	source.Revision = revision
	request.Build.RepoRefs["example/repo"] = revision
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	return root, artifactRoot, manifest, revision
}

func snapshotPreparedSource(t *testing.T, root, manifestHash string) preparedSourceSnapshot {
	t.Helper()
	controller, err := os.ReadFile(filepath.Join(root, "pkg", "controller.go"))
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "controller-link.go"))
	if err != nil {
		t.Fatal(err)
	}
	return preparedSourceSnapshot{
		head:          strings.TrimSpace(runWorkspaceGit(t, root, "rev-parse", "HEAD")),
		tree:          strings.TrimSpace(runWorkspaceGit(t, root, "rev-parse", "HEAD^{tree}")),
		index:         runWorkspaceGit(t, root, "ls-files", "--stage", "-z"),
		controllerSum: fmt.Sprintf("%x", sha256.Sum256(controller)),
		scriptSum:     fmt.Sprintf("%x", sha256.Sum256(script)),
		symlinkTarget: target,
		manifestHash:  manifestHash,
	}
}

func makePreparedTreeReadOnly(t *testing.T, root string) func() {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return os.Chmod(path, 0o600)
		})
	}
}
