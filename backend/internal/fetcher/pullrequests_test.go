package fetcher

import (
	"context"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

func TestPullRequestsEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *project.Config
		want bool
	}{
		{name: "no config"},
		{name: "block absent", cfg: &project.Config{}},
		{name: "explicitly disabled", cfg: &project.Config{PullRequests: &project.PullRequests{}}},
		{name: "enabled", cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &pipeline{cfg: tc.cfg}
			if got := p.pullRequestsEnabled(); got != tc.want {
				t.Fatalf("pullRequestsEnabled = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestRefreshPullRequestsRequiresJobCatalog(t *testing.T) {
	p := &pipeline{cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}}
	if err := p.refreshPullRequests(context.Background()); err == nil {
		t.Fatal("want an error when no job catalog is available")
	}
}

// A pull request refresh failure must not abort the surrounding pass, so the
// dashboard still publishes when GitHub or the catalog is unavailable.
func TestRunPullRequestPassSwallowsFailures(t *testing.T) {
	p := &pipeline{cfg: &project.Config{PullRequests: &project.PullRequests{Enabled: true}}}
	p.runPullRequestPass(context.Background())
}

func TestRunPullRequestPassSkipsWhenDisabled(t *testing.T) {
	called := false
	original := writePullRequestOutput
	writePullRequestOutput = func(string, models.PullRequestIndex, []models.PullRequestDetail) error {
		called = true
		return nil
	}
	t.Cleanup(func() { writePullRequestOutput = original })

	(&pipeline{cfg: &project.Config{}}).runPullRequestPass(context.Background())
	if called {
		t.Fatal("disabled triage must not write output")
	}
}
