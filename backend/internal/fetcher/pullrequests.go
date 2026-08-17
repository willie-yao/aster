package fetcher

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prattribution"
	"github.com/willie-yao/aster/backend/internal/prtriage"
)

// writePullRequestOutput is a seam for tests.
var writePullRequestOutput = output.WritePullRequests

// pullRequestsEnabled reports whether the consumer opted into the pull request
// triage view.
func (p *pipeline) pullRequestsEnabled() bool {
	return p.cfg != nil && p.cfg.PullRequests != nil && p.cfg.PullRequests.Enabled
}

// refreshPullRequests rebuilds the pull request view and writes its output. It
// reads presubmit definitions from the job catalog, so it must run after
// discovery has populated one. The refresh result supplies the base-branch
// evidence that attribution compares each failure against.
func (p *pipeline) refreshPullRequests(ctx context.Context, res *refreshResult) error {
	if p.jobCatalog == nil {
		return fmt.Errorf("job catalog unavailable, cannot resolve presubmits")
	}
	repo := p.cfg.Branding.SourceRepo
	opts := prtriage.Options{
		Owner:           repo.Owner,
		Repo:            repo.Name,
		MaxPullRequests: p.cfg.PullRequests.Max,
		BuildsPerJob:    p.cfg.PullRequests.BuildsPerJob,
		Workers:         p.opts.Workers,
	}
	client := ghpr.NewClient(p.client, githubReadToken())
	index, details, err := prtriage.Collect(ctx, p.backend, client, p.jobCatalog, opts)
	if err != nil {
		return err
	}
	var baseline prattribution.Baseline
	if res != nil {
		baseline = prattribution.BuildBaseline(res.details, res.flakiness)
	}
	prattribution.Annotate(details, baseline)

	index.GeneratedAt = time.Now().UTC()
	for i := range details {
		details[i].GeneratedAt = index.GeneratedAt
	}
	if err := writePullRequestOutput(p.opts.OutDir, index, details); err != nil {
		return fmt.Errorf("writing pull request output: %w", err)
	}
	log.Printf("✅ Wrote %d open pull requests to %s", len(details), output.PullRequestIndexFilename)
	return nil
}

// runPullRequestPass refreshes the pull request view without aborting the
// surrounding pass. The dashboard must still publish when GitHub is
// unreachable, so a failure keeps the previously written view.
func (p *pipeline) runPullRequestPass(ctx context.Context, res *refreshResult) {
	if !p.pullRequestsEnabled() {
		return
	}
	if err := p.refreshPullRequests(ctx, res); err != nil {
		log.Printf("⚠ Pull request triage failed, keeping the previous view: %v", err)
	}
}
