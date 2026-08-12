package agentanalysis

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyPreparedSourceWorkspaceCategories(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*testing.T, string)
		category string
	}{
		{name: "worktree content", mutate: func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, category: SourceWorktreeContentChanged},
		{name: "staged content", mutate: func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("staged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runWorkspaceGit(t, root, "add", "script.sh")
		}, category: SourceStagedContentChanged},
		{name: "index mode", mutate: func(t *testing.T, root string) {
			t.Helper()
			runWorkspaceGit(t, root, "update-index", "--chmod=-x", "script.sh")
		}, category: SourceIndexModeChanged},
		{name: "index flags", mutate: func(t *testing.T, root string) {
			t.Helper()
			runWorkspaceGit(t, root, "update-index", "--assume-unchanged", "script.sh")
		}, category: SourceIndexFlagsChanged},
		{name: "untracked", mutate: func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("extra\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, category: SourceUntrackedFiles},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, policy, _, revision := preparedModeFixture(t)
			test.mutate(t, root)
			err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy)
			if got := SourceIntegrityCategory(err); got != test.category {
				t.Fatalf("category=%q error=%v want=%q", got, err, test.category)
			}
		})
	}
}

func TestVerifySourceWorkspaceModeCategory(t *testing.T) {
	root, _, _, revision := preparedModeFixtureUnsealed(t)
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := VerifySourceWorkspace(t.Context(), root, revision)
	if got := SourceIntegrityCategory(err); got != SourceWorktreeModeChanged {
		t.Fatalf("category=%q error=%v", got, err)
	}
}

func TestVerifyPreparedSourceWorkspaceGitDiffError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shim requires a POSIX shell")
	}
	root, policy, _, revision := preparedModeFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	shim := filepath.Join(bin, "git")
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = diff ]; then exit 2; fi\ndone\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy)
	if got := SourceIntegrityCategory(err); got != SourceGitDiffError || err.Error() != SourceGitDiffError {
		t.Fatalf("category=%q error=%v", got, err)
	}
}

func TestInspectPreparedSourceIntegrity(t *testing.T) {
	root, policy, _, revision := preparedModeFixture(t)
	clean, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Head != revision || clean.Tree == "" || clean.CoreFileMode != "false" || clean.IndexModeExecutable < 1 || clean.IndexFlagsChanged != 0 || clean.PorcelainV2Entries != 0 || clean.GitExitCodes.Staged != 0 || clean.GitExitCodes.WorktreeContent != 0 || clean.GitExitCodes.WorktreeAll != 0 {
		t.Fatalf("clean snapshot=%+v", clean)
	}
	if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
	if err != nil {
		t.Fatal(err)
	}
	if dirty.WorktreeContentChanges != 1 || dirty.GitExitCodes.WorktreeContent != 1 || dirty.PorcelainV2Entries < 1 || dirty.PorcelainV2SHA256 == clean.PorcelainV2SHA256 {
		t.Fatalf("dirty snapshot=%+v", dirty)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestInspectPreparedSourceIntegrityDifferentials(t *testing.T) {
	t.Run("mode only", func(t *testing.T) {
		root, _, _, revision := preparedModeFixtureUnsealed(t)
		if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
			t.Fatal(err)
		}
		snapshot, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, WorkspaceSourceModePreserve)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.WorktreeContentChanges != 0 || snapshot.WorktreeModeChanges != 1 || snapshot.GitExitCodes.WorktreeContent != 0 || snapshot.GitExitCodes.WorktreeAll != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("staged", func(t *testing.T) {
		root, policy, _, revision := preparedModeFixture(t)
		if err := os.WriteFile(filepath.Join(root, "script.sh"), []byte("staged\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, root, "add", "script.sh")
		snapshot, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.StagedContentChanges != 1 || snapshot.GitExitCodes.Staged != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("staged mode", func(t *testing.T) {
		root, policy, _, revision := preparedModeFixture(t)
		runWorkspaceGit(t, root, "update-index", "--chmod=-x", "script.sh")
		snapshot, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.StagedContentChanges != 0 || snapshot.StagedModeChanges != 1 || snapshot.GitExitCodes.Staged != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("colon-prefixed path", func(t *testing.T) {
		root, policy, _, revision := preparedModeFixture(t)
		if err := os.WriteFile(filepath.Join(root, ":file"), []byte("staged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, root, "add", "--", "./:file")
		snapshot, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.StagedContentChanges != 1 || snapshot.StagedModeChanges != 1 || snapshot.GitExitCodes.Staged != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
		if err := VerifyPreparedSourceWorkspace(t.Context(), root, revision, policy); SourceIntegrityCategory(err) != SourceStagedContentChanged {
			t.Fatalf("category=%q error=%v", SourceIntegrityCategory(err), err)
		}
	})
	t.Run("untracked", func(t *testing.T) {
		root, policy, _, revision := preparedModeFixture(t)
		if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("extra\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := InspectPreparedSourceIntegrity(t.Context(), root, revision, policy)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.UntrackedFiles != 1 || snapshot.PorcelainV2Entries != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
}
