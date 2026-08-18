package agentanalysis

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type workspacePrepareBrowser struct {
	files map[string][]byte
}

func (b workspacePrepareBrowser) BuildRoot() string { return "fixture" }
func (b workspacePrepareBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return nil, nil
}
func (b workspacePrepareBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	paths := make([]string, 0, len(b.files))
	for path := range b.files {
		paths = append(paths, path)
	}
	return paths, false, nil
}
func (b workspacePrepareBrowser) Read(_ context.Context, path string, offset, length int) ([]byte, int64, error) {
	data := b.files[path]
	if offset >= len(data) {
		return nil, int64(len(data)), nil
	}
	end := min(offset+length, len(data))
	return append([]byte(nil), data[offset:end]...), int64(len(data)), nil
}
func (b workspacePrepareBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return nil, nil
}
func (b workspacePrepareBrowser) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return nil, nil
}

func TestPrepareWorkspaceInputSealsSkillsAndCleansUp(t *testing.T) {
	sourceRepo, revision := workspacePrepareSourceRepository(t)
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "skills", "failure.yaml"), []byte(`id: fixture.failure
triggers: ["deterministic failure"]
procedure: Read the failure log and preserve uncertainty.
required_evidence:
  - id: failure-log
    any_of: ["failure[.]log$"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	set, _, err := skills.LoadForTools(projectDir, []string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	publicDir := filepath.Join(t.TempDir(), "public")
	inputRoot := filepath.Join(t.TempDir(), "private-input")
	request := ai.FailureAnalysisRequest{
		JobID: "periodic::fixture", BuildPrefix: "logs/fixture/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "fixture", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "fixture", Status: "failed", FailureMessage: "deterministic failure"},
	}
	prepared, err := PrepareWorkspaceInput(t.Context(), request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}, WorkspacePreparationOptions{
		PublicOutputDir: publicDir, InputRoot: inputRoot, ConsumerPrompt: "Inspect this project.", SkillSet: set,
		Browser: workspacePrepareBrowser{files: map[string][]byte{"logs/failure.log": []byte("deterministic failure\n")}},
		PrepareSource: func(ctx context.Context, destination string, source sourceinvestigation.Repository) (WorkspaceSourceModePolicy, error) {
			if output, err := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", sourceRepo, destination).CombinedOutput(); err != nil {
				t.Fatalf("clone source: %v: %s", err, output)
			}
			return ConfigurePreparedSourceModePolicy(ctx, destination, source.Revision)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	foundConsumer := false
	for _, planned := range prepared.Manifest.SkillPlan {
		foundConsumer = foundConsumer || planned.ID == "fixture.failure"
	}
	if prepared.Manifest.SkillSetHash != set.Hash() || !foundConsumer || prepared.Manifest.EffectivePromptSHA256 == "" {
		t.Fatalf("manifest identity = %+v", prepared.Manifest)
	}
	resolvedInput, err := filepath.EvalSymlinks(inputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(prepared.Root) != prepared.Manifest.Hash || !strings.HasPrefix(prepared.Root, filepath.Clean(resolvedInput)+string(filepath.Separator)) {
		t.Fatalf("prepared root = %s", prepared.Root)
	}
	if err := VerifyPreparedSourceWorkspace(t.Context(), prepared.SourceRoot, revision, prepared.SourceModePolicy); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactWorkspace(prepared.ArtifactRoot, prepared.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := CleanupWorkspaceInput(publicDir, inputRoot, prepared.Manifest.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Root); !os.IsNotExist(err) {
		t.Fatalf("prepared input remains after cleanup: %v", err)
	}
}

func TestPrepareWorkspaceInputRejectsPublicRoot(t *testing.T) {
	publicDir := t.TempDir()
	_, err := PrepareWorkspaceInput(t.Context(), ai.FailureAnalysisRequest{}, sourceinvestigation.Repository{}, WorkspacePreparationOptions{
		PublicOutputDir: publicDir, InputRoot: filepath.Join(publicDir, "private"),
	})
	if err == nil || !strings.Contains(err.Error(), "inside public output") {
		t.Fatalf("error = %v", err)
	}
}

func workspacePrepareSourceRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "controller.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "commit.gpgsign", "false"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"},
		{"add", "controller.go"}, {"commit", "-qm", "fixture"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return root, strings.TrimSpace(string(output))
}
