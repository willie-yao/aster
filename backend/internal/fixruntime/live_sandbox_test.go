package fixruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestLiveSandboxedFixRuntime(t *testing.T) {
	if os.Getenv("LIVE_SANDBOX_OPENCODE") != "1" {
		t.Skip("set LIVE_SANDBOX_OPENCODE=1 and SRT_BIN for the disposable Copilot smoke test")
	}
	domains := []string{"models.dev:443", "api.githubcopilot.com:443", "github.com:443"}
	configured := &project.FixAgentRuntime{Type: "opencode", NetworkDomains: domains}
	runtime, err := New(configured)
	if err != nil {
		t.Fatal(err)
	}
	repo := initLiveFixRepo(t)
	result, err := runtime.Generate(context.Background(), agentruntime.GenerateSpec{
		Repo:        agentruntime.RepoRef{Owner: "sandbox", Name: "fix", Ref: "main", CloneURL: repo},
		Instruction: "Change value.txt to contain exactly fixed followed by a newline. Use Bash to run ./check.sh after editing. Do not change any other file.",
		NativeModel: "github-copilot/claude-sonnet-4.6", UseAmbientAuth: true,
		AllowBash: true, NetworkDomains: configured.NetworkDomains, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files["value.txt"] != "fixed\n" {
		t.Fatalf("changed files = %v", result.Files)
	}
}

func initLiveFixRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "value.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\ntest \"$(cat value.txt)\" = fixed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runFixGit(t, dir, "init", "-b", "main")
	runFixGit(t, dir, "add", ".")
	runFixGit(t, dir, "-c", "user.name=Sandbox Test", "-c", "user.email=sandbox@example.com", "-c", "commit.gpgsign=false", "commit", "--no-gpg-sign", "-m", "init")
	return dir
}

func runFixGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, output)
	}
}
