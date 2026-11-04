package fetcher

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/githubapp"
	"github.com/willie-yao/aster/backend/internal/project"
)

type readAuthTransport func(*http.Request) (*http.Response, error)

func (f readAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func readAuthPipeline(t *testing.T) (*pipeline, error) {
	t.Helper()
	projectDir := t.TempDir()
	config := fmt.Sprintf(`id: test
name: Test
discovery:
  source: bucket
storage:
  provider: local
  base: %s
branding:
  title: Test
  base_path: /
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: repo
`, t.TempDir())
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return setupPipeline(Options{ProjectDir: projectDir, OutDir: t.TempDir()})
}

func TestSetupPipelineCapturesReadAuthForWatchLifetime(t *testing.T) {
	for _, tc := range []struct{ name, read, generic, want string }{
		{name: "read token", read: "read", generic: "generic", want: "read"},
		{name: "generic token", generic: "generic", want: "generic"},
		{name: "anonymous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ASTER_APP_ID", "")
			t.Setenv("ASTER_APP_PRIVATE_KEY", "")
			t.Setenv("GITHUB_READ_TOKEN", tc.read)
			t.Setenv("GITHUB_TOKEN", tc.generic)
			p, err := readAuthPipeline(t)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GITHUB_READ_TOKEN", "replacement")
			calls := 0
			client := &http.Client{Transport: readAuthTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				want := ""
				if tc.want != "" {
					want = "Bearer " + tc.want
				}
				if r.Header.Get("Authorization") != want {
					t.Error("pipeline did not retain its startup read credential")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			for range 2 {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/example/repo", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := p.githubReadAuth.Do(client, req, "example", "repo", "metadata")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			if calls != 2 {
				t.Fatalf("calls = %d, want 2", calls)
			}
		})
	}
}

func TestSetupPipelineRejectsIncompleteReadApp(t *testing.T) {
	t.Setenv("ASTER_APP_ID", "1")
	t.Setenv("ASTER_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_READ_TOKEN", "static")
	if _, err := readAuthPipeline(t); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("setupPipeline = %v", err)
	}
}

func TestSetupPipelineConfiguresAppWithoutAI(t *testing.T) {
	setCommentAppCredentials(t)
	t.Setenv("GITHUB_READ_TOKEN", "static")
	p, err := readAuthPipeline(t)
	if err != nil {
		t.Fatal(err)
	}
	if !p.githubReadAuth.Configured() || p.enableAI {
		t.Fatal("worker read App authentication depends on AI being enabled")
	}
}

func TestScheduledAnalysisReusesPipelineReadAuth(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "32768")
	p := &pipeline{
		opts:           Options{OutDir: t.TempDir()},
		aiToken:        "model-token",
		githubReadAuth: githubapp.StaticReadAuth("github-token"),
		aiProject: &analysisruntime.Project{
			Config: &project.Config{AI: &project.AI{}},
			Provider: project.AIProvider{
				API: project.AIAPIChatCompletions, Endpoint: "https://model.invalid/v1/chat/completions", Model: "fixture",
			},
		},
	}
	first, err := p.ensureAnalysisRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.ensureAnalysisRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.GitHubReadAuth != p.githubReadAuth {
		t.Fatal("scheduled analysis did not reuse the pipeline's read authentication")
	}
}
