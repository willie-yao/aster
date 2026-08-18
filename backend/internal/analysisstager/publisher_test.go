package analysisstager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestPublishAndCleanupRemoteSnapshot(t *testing.T) {
	sourceRepo, revision := publisherSourceRepository(t)
	artifact := []byte("deterministic failure\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/build/logs/failure.log" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(artifact)
	}))
	defer server.Close()
	artifactRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "logs", "failure.log"), artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(ai.FailureAnalysisRequest{
		JobID: "periodic::fixture", BuildPrefix: "logs/fixture/1/", Build: models.BuildInfo{BuildID: "1", JobName: "fixture", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "fixture", Status: "failed", FailureMessage: "deterministic failure"},
	}, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceRemoteStageRequest(manifest, server.URL+"/build", agentanalysis.WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	publish, err := agentanalysis.NewWorkspacePublishRequest(stage, files, "analysis-lease-1")
	if err != nil {
		t.Fatal(err)
	}
	inputRoot := t.TempDir()
	result, err := Publish(t.Context(), publish, PublishOptions{
		InputRoot: inputRoot, Client: server.Client(),
		ValidateSource: func(context.Context, *http.Client, sourceinvestigation.Repository) error { return nil },
		PrepareSource: func(ctx context.Context, destination, _, _, expected string) error {
			output, err := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", sourceRepo, destination).CombinedOutput()
			if err != nil {
				t.Fatalf("clone: %v: %s", err, output)
			}
			if got := strings.TrimSpace(publisherGit(t, destination, "rev-parse", "HEAD")); got != expected {
				t.Fatalf("HEAD=%s want=%s", got, expected)
			}
			return nil
		},
	})
	if err != nil || result.Status != "published" || result.ManifestHash != manifest.Hash {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	root := filepath.Join(inputRoot, manifest.Hash)
	if err := agentanalysis.VerifyPreparedSourceWorkspace(t.Context(), filepath.Join(root, agentanalysis.WorkspaceSourceDir), revision, result.SourceModePolicy); err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.VerifyArtifactFiles(filepath.Join(root, agentanalysis.WorkspaceArtifactsDir), files); err != nil {
		t.Fatal(err)
	}
	wrong, _ := agentanalysis.NewWorkspaceCleanupRequest(manifest.Hash, "analysis-lease-other")
	if _, err := Cleanup(t.Context(), wrong, inputRoot); err == nil {
		t.Fatal("wrong lease cleanup succeeded")
	}
	cleanup, _ := agentanalysis.NewWorkspaceCleanupRequest(manifest.Hash, "analysis-lease-1")
	cleaned, err := Cleanup(t.Context(), cleanup, inputRoot)
	if err != nil || cleaned.Status != "deleted" {
		t.Fatalf("cleanup=%+v err=%v", cleaned, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("snapshot remains: %v", err)
	}
}

func publisherSourceRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "commit.gpgsign", "false"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"add", "main.go"}, {"commit", "-qm", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return root, strings.TrimSpace(publisherGit(t, root, "rev-parse", "HEAD"))
}

func publisherGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}
