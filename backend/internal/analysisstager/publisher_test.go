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
	"time"

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
	publishOptions := PublishOptions{
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
	}
	result, err := Publish(t.Context(), publish, publishOptions)
	if err != nil || result.Status != "published" || result.ManifestHash != manifest.Hash {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	root := filepath.Join(inputRoot, manifest.Hash)
	if err := agentanalysis.VerifyPreparedSourceWorkspace(t.Context(), filepath.Join(root, agentanalysis.WorkspaceSourcesDir, "primary"), revision, result.SourceModePolicy); err != nil {
		t.Fatal(err)
	}
	if err := agentanalysis.VerifyArtifactFiles(filepath.Join(root, agentanalysis.WorkspaceArtifactsDir), files); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, agentanalysis.WorkspaceSourcesDir, "primary", ".git", "config")
	sealedTime := time.Unix(946684800, 0)
	if err := os.Chtimes(configPath, sealedTime, sealedTime); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(t.Context(), publish, publishOptions); err != nil {
		t.Fatalf("reuse published snapshot: %v", err)
	}
	if info, err := os.Stat(configPath); err != nil || !info.ModTime().Equal(sealedTime) {
		t.Fatalf("published source was mutated on reuse: info=%v err=%v", info, err)
	}
	wrong, _ := agentanalysis.NewWorkspaceCleanupRequest(manifest.Hash, "analysis-lease-other")
	if _, err := Cleanup(t.Context(), wrong, inputRoot); err == nil {
		t.Fatal("wrong lease cleanup succeeded")
	}
	cleanup, _ := agentanalysis.NewWorkspaceCleanupRequest(manifest.Hash, "analysis-lease-1")
	readLock, err := lockSnapshotReadOnly(inputRoot, manifest.Hash)
	if err != nil {
		t.Fatal(err)
	}
	type cleanupOutcome struct {
		result CleanupResult
		err    error
	}
	cleanupDone := make(chan cleanupOutcome, 1)
	go func() {
		cleaned, cleanupErr := Cleanup(t.Context(), cleanup, inputRoot)
		cleanupDone <- cleanupOutcome{result: cleaned, err: cleanupErr}
	}()
	select {
	case outcome := <-cleanupDone:
		t.Fatalf("cleanup did not wait for staging lock: %+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	unlockSnapshot(readLock)
	outcome := <-cleanupDone
	cleaned, err := outcome.result, outcome.err
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

func TestPublishRemoteSnapshotStagesMultipleRevisions(t *testing.T) {
	repository, firstRevision, secondRevision := publisherSourceRepositoryWithTwoRevisions(t)
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
	manifest, err := agentanalysis.NewWorkspaceManifestWithSources(ai.FailureAnalysisRequest{
		JobID: "periodic::fixture", BuildPrefix: "logs/fixture/1/", Build: models.BuildInfo{BuildID: "1", JobName: "fixture", RepoRefs: map[string]string{"example/repo": secondRevision}},
		TestCase: models.TestCase{Name: "fixture", Status: "failed", FailureMessage: "deterministic failure"},
	}, []agentanalysis.WorkspaceSourceRef{
		{ID: "server", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: firstRevision}},
		{ID: "client", Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: secondRevision}},
	}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceRemoteStageRequest(manifest, server.URL+"/build", agentanalysis.WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	publish, err := agentanalysis.NewWorkspacePublishRequest(stage, files, "analysis-lease-multi")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(t.Context(), publish, PublishOptions{
		InputRoot: t.TempDir(), Client: server.Client(),
		ValidateSource: func(context.Context, *http.Client, sourceinvestigation.Repository) error { return nil },
		PrepareSource: func(ctx context.Context, destination, _, _, expected string) error {
			if output, err := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", repository, destination).CombinedOutput(); err != nil {
				t.Fatalf("clone: %v: %s", err, output)
			}
			if output, err := exec.CommandContext(ctx, "git", "-C", destination, "checkout", "--quiet", "--detach", expected).CombinedOutput(); err != nil {
				t.Fatalf("checkout: %v: %s", err, output)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SourceModePolicies) != 2 || result.SourceModePolicies[0].SourceID != "client" || result.SourceModePolicies[1].SourceID != "server" {
		t.Fatalf("result=%+v", result)
	}
}

func publisherSourceRepositoryWithTwoRevisions(t *testing.T) (string, string, string) {
	t.Helper()
	root, first := publisherSourceRepository(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\n\nconst Version = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "main.go"}, {"commit", "-qm", "second"}} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	second := strings.TrimSpace(publisherGit(t, root, "rev-parse", "HEAD"))
	return root, first, second
}
