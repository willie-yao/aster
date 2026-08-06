package promptauthor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestLiveSandboxedPromptAuthor(t *testing.T) {
	if os.Getenv("LIVE_SANDBOX_OPENCODE") != "1" {
		t.Skip("set LIVE_SANDBOX_OPENCODE=1 and SRT_BIN for the disposable Copilot smoke test")
	}
	repo := initLivePromptRepo(t)
	result, err := NewOpenCodeRuntime().Generate(context.Background(), Spec{
		Repo:        agentruntime.RepoRef{Owner: "sandbox", Name: "prompt-author", Ref: "main", CloneURL: repo},
		Instruction: "Author a project-specific diagnostic prompt for this disposable Go repository. Keep unknown CI details explicit in Unresolved details.",
		NativeModel: "github-copilot/claude-sonnet-4.6", UseAmbientAuth: true,
		MaxTurns: 12, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Body == "" {
		t.Fatal("prompt author returned an empty body")
	}
	t.Logf("sandboxed prompt bytes=%d duration=%s", len(result.Body), result.Duration.Round(time.Millisecond))
}

func initLivePromptRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"README.md":          "# Widget controller\n\nA small Go controller used for sandbox validation.\n",
		"go.mod":             "module example.com/widget\n\ngo 1.25\n",
		"cmd/widget/main.go": "package main\n\nfunc main() {}\n",
	}
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runLiveGit(t, dir, "init", "-b", "main")
	runLiveGit(t, dir, "add", ".")
	runLiveGit(t, dir, "-c", "user.name=Sandbox Test", "-c", "user.email=sandbox@example.com", "-c", "commit.gpgsign=false", "commit", "--no-gpg-sign", "-m", "init")
	return dir
}

func runLiveGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, output)
	}
}
