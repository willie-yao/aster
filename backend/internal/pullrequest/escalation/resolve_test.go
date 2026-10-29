package escalation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// resolverFixture lays out the published pull request file and the build
// artifacts one escalation resolves against.
type resolverFixture struct {
	dataDir  string
	buildDir string
}

func newResolverFixture(t *testing.T) resolverFixture {
	t.Helper()
	root := t.TempDir()
	fixture := resolverFixture{
		dataDir:  filepath.Join(root, "data"),
		buildDir: filepath.Join(root, "bucket", "pr-logs", "pull", "org_repo", "6209", "pull-e2e", "100"),
	}
	if err := os.MkdirAll(fixture.buildDir, 0o755); err != nil {
		t.Fatalf("build dir: %v", err)
	}
	pulls := filepath.Join(fixture.dataDir, "pull-requests")
	if err := os.MkdirAll(pulls, 0o755); err != nil {
		t.Fatalf("data dir: %v", err)
	}
	detail := models.PullRequestDetail{
		PullRequestSummary: models.PullRequestSummary{
			Number: 6209, HeadSHA: "abc123", BaseRef: "main",
		},
		Checks: []models.PullRequestCheck{{
			JobID: "org/repo/pull-e2e", JobName: "pull-e2e", BuildID: "100",
			Failures: []models.PullRequestFailure{{
				TestCase: models.TestCase{Name: "TestA"},
			}},
		}},
	}
	writeJSON(t, filepath.Join(pulls, models.PullRequestDataFilename(6209)), detail)
	writeJSON(t, filepath.Join(fixture.buildDir, "started.json"), map[string]any{
		"timestamp": 1700000000, "repo-commit": "abc123",
	})
	return fixture
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f resolverFixture) resolver(t *testing.T) *DataResolver {
	t.Helper()
	backend, err := storage.NewLocalBackend(filepath.Join(filepath.Dir(f.dataDir), "bucket"), "")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return &DataResolver{
		DataDir: f.dataDir, Backend: backend,
		Repo: "org/repo", Owner: "org", Name: "repo",
	}
}

type finishedErrorBackend struct {
	storage.Backend
}

func (b finishedErrorBackend) Open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if strings.HasSuffix(name, "/finished.json") {
		return nil, 0, context.DeadlineExceeded
	}
	return b.Backend.Open(ctx, name)
}

func TestResolveRefusesUnreadableFinishedMetadata(t *testing.T) {
	fixture := newResolverFixture(t)
	resolver := fixture.resolver(t)
	resolver.Backend = finishedErrorBackend{Backend: resolver.Backend}

	resolved, err := resolver.Resolve(t.Context(), testRef("TestA"))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "build metadata unavailable") {
		t.Fatalf("err = %v, want unavailable build metadata", err)
	}
	if resolved.Request.Build.Result != "" {
		t.Fatalf("unavailable build = %+v", resolved.Request.Build)
	}
}

func TestResolveRefusesABuildWithNoFinishedMetadata(t *testing.T) {
	fixture := newResolverFixture(t)
	resolver := fixture.resolver(t)

	_, err := resolver.Resolve(context.Background(), testRef("TestA"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	// The same subject resolves once the build's outcome is readable.
	writeJSON(t, filepath.Join(fixture.buildDir, "finished.json"), map[string]any{
		"timestamp": 1700000900, "passed": false, "result": "FAILURE", "revision": "abc123",
	})
	resolved, err := resolver.Resolve(context.Background(), testRef("TestA"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Request.Build.Result != "FAILURE" {
		t.Fatalf("build result = %q, want the finished outcome", resolved.Request.Build.Result)
	}
}
