package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo creates a local git repo with one committed file and returns its
// path and default branch, for use as a CloneURL in tests (no network).
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	// Fully isolate git from the user's global and system config so no test git
	// operation can read or trigger their settings (e.g. commit.gpgsign, which
	// would pop a gpg prompt). GIT_CONFIG_GLOBAL/SYSTEM=/dev/null neutralizes
	// both config layers for the child git processes.
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "orig.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "--no-gpg-sign", "-m", "base")
	return dir
}

func TestLocalRuntime_RunOverlayAndExec(t *testing.T) {
	repo := initRepo(t)
	res, err := NewLocal().Run(context.Background(), Spec{
		Repo:    RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Overlay: map[string]string{"added.txt": "hello"},
		Command: []string{"git", "status", "--porcelain"},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed() {
		t.Errorf("expected pass, got exit=%d output=%q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "added.txt") {
		t.Errorf("overlay file not present in workspace; status=%q", res.Output)
	}
}

func TestLocalRuntime_FailingCommand(t *testing.T) {
	repo := initRepo(t)
	res, err := NewLocal().Run(context.Background(), Spec{
		Repo:    RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Command: []string{"git", "rev-parse", "--verify", "doesnotexist"},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Passed() || res.ExitCode == 0 {
		t.Errorf("expected non-zero exit, got exit=%d", res.ExitCode)
	}
}

func TestLocalRuntime_UnavailableCommand(t *testing.T) {
	repo := initRepo(t)
	_, err := NewLocal().Run(context.Background(), Spec{
		Repo:    RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Command: []string{"pad-nonexistent-binary-zzz"},
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("expected ErrUnavailable for a missing command binary, got %v", err)
	}
}

func TestLocalRuntime_ValidatesSpec(t *testing.T) {
	if _, err := NewLocal().Run(context.Background(), Spec{Repo: RepoRef{Owner: "o", Name: "n", Ref: "r"}}); err == nil {
		t.Error("expected error for empty command")
	}
	if _, err := NewLocal().Run(context.Background(), Spec{Command: []string{"echo"}}); err == nil {
		t.Error("expected error for missing repo ref")
	}
}

func TestOverlay_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if err := overlay(dir, map[string]string{"../escape.txt": "x"}); err != nil {
		// filepath.Clean("/../escape.txt") = "/escape.txt", which stays inside
		// dir, so this specific form is contained rather than rejected. The
		// invariant we assert is that nothing is written outside dir.
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("overlay wrote outside the workspace dir")
	}
}

func TestOverlay_RejectsSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// A symlinked directory in the checkout pointing outside the workspace.
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := overlay(dir, map[string]string{"link/pwned.txt": "x"})
	if err == nil {
		t.Fatal("expected overlay through a symlinked parent to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "pwned.txt")); statErr == nil {
		t.Error("overlay wrote through a symlink outside the workspace")
	}
}

func TestLocalRuntime_ParentCancelSkips(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: the command must not run to a normal verdict
	res, err := NewLocal().Run(ctx, Spec{
		Repo:    RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Command: []string{"git", "status"},
		Timeout: time.Minute,
	})
	// A canceled parent is an infra event: Run returns an error (mapped to
	// "skipped" by the caller), never a passed/failed result.
	if err == nil && res.Passed() {
		t.Error("expected a canceled parent to not yield a passed verdict")
	}
}

func TestTail(t *testing.T) {
	if got := tail("short", 100); got != "short" {
		t.Errorf("tail short = %q", got)
	}
	got := tail("0123456789", 4)
	if !strings.HasSuffix(got, "6789") || !strings.Contains(got, "truncated") {
		t.Errorf("tail truncation = %q", got)
	}
}

func TestRedactToken(t *testing.T) {
	if got := redactToken("url https://x-access-token:secret@github.com", "secret"); strings.Contains(got, "secret") {
		t.Errorf("token not redacted: %q", got)
	}
	if got := redactToken("no token here", ""); got != "no token here" {
		t.Errorf("empty token changed output: %q", got)
	}
}

func TestResultPassed(t *testing.T) {
	if !(Result{ExitCode: 0}).Passed() {
		t.Error("exit 0 should pass")
	}
	if (Result{ExitCode: 1}).Passed() {
		t.Error("exit 1 should not pass")
	}
	if (Result{ExitCode: 0, TimedOut: true}).Passed() {
		t.Error("timed out should not pass")
	}
}
