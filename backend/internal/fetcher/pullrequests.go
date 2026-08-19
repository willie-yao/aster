package fetcher

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/githubapp"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prattribution"
	"github.com/willie-yao/aster/backend/internal/prcomment"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prtriage"
)

// writePullRequestOutput is a seam for tests.
var writePullRequestOutput = output.WritePullRequests

// pullRequestsEnabled reports whether the consumer opted into the pull request
// triage view.
func (p *pipeline) pullRequestsEnabled() bool {
	return p.cfg != nil && p.cfg.PullRequests != nil && p.cfg.PullRequests.Enabled
}

// warnPullRequestTokenMissing logs once at startup when triage is enabled with
// no read token. Anonymous GitHub reads are capped at 60 requests per hour,
// which a single pass over a busy repository exhausts, and the resulting 403s
// only surface as a view that stops updating.
func (p *pipeline) warnPullRequestTokenMissing() {
	if !p.pullRequestsEnabled() || githubReadToken() != "" {
		return
	}
	log.Println("⚠ Pull request triage is enabled but neither GITHUB_READ_TOKEN nor GITHUB_TOKEN is set. " +
		"GitHub reads are anonymous and capped at 60 requests per hour, which one triage pass can exhaust.")
}

// warnCommentCredentialsMissing logs once at startup when commenting is enabled
// without GitHub App credentials. Commenting posts as an App so contributors see
// a bot account, and without one the pass can do nothing, so this is named up
// front rather than at the end of the first refresh.
func (p *pipeline) warnCommentCredentialsMissing() {
	if !p.cfg.CommentEnabled() {
		return
	}
	if _, ok := githubapp.CredentialsFromEnv(); ok {
		return
	}
	log.Printf("⚠ pull_requests.comment.enabled is set but %s and %s are unset, so no comments can be posted. "+
		"Commenting authenticates as a GitHub App so the comment comes from a bot account.",
		githubapp.EnvAppID, githubapp.EnvPrivateKey)
}

// attributionBaseline builds the base-branch evidence each failure is compared
// against. It reads the base-only flakiness report rather than the published
// one, so publishing presubmits cannot change a verdict. A nil result means the
// dashboard pass produced nothing, which attribution reports as inconclusive.
func (r *refreshResult) attributionBaseline() prattribution.Baseline {
	if r == nil {
		return prattribution.Baseline{}
	}
	return prattribution.BuildBaseline(r.details, r.baseFlakiness)
}

// triageOutcome is one triage pass's published result, carried to commenting so
// it acts on exactly what was written.
type triageOutcome struct {
	details []models.PullRequestDetail
	// truncated reports that more open pull requests exist than were published,
	// so the dashboard does not have a page for every one of them.
	truncated bool
}

// refreshPullRequests rebuilds the pull request view and writes its output. It
// reads presubmit definitions from the job catalog, so it must run after
// discovery has populated one. The refresh result supplies the base-branch
// evidence that attribution compares each failure against. It returns what it
// published so commenting can act on exactly that set.
func (p *pipeline) refreshPullRequests(ctx context.Context, res *refreshResult) (triageOutcome, error) {
	if p.jobCatalog == nil {
		return triageOutcome{}, fmt.Errorf("job catalog unavailable, cannot resolve presubmits")
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
	result, err := prtriage.Collect(ctx, p.backend, client, p.jobCatalog, opts)
	if err != nil {
		return triageOutcome{}, err
	}
	details := result.Details
	changes := p.pullRequestChanges(ctx, client, repo, details)
	prattribution.Annotate(details, res.attributionBaseline(),
		prattribution.Repository{Owner: repo.Owner, Name: repo.Name}, changes)
	index := result.Index
	// Clustering reads the verdicts Annotate just attached, so it runs after it.
	shared := models.SharedFailureIndex{
		Repo:     index.Repo,
		Failures: prattribution.Clusters(details),
	}

	index.GeneratedAt = time.Now().UTC()
	for i := range details {
		details[i].GeneratedAt = index.GeneratedAt
	}
	shared.GeneratedAt = index.GeneratedAt
	// Pages for pull requests the engine has publicly linked to must survive
	// pruning, which otherwise removes them as soon as the pull request closes.
	retain := prcomment.CommentedNumbersAt(p.opts.OutDir, repo.Owner+"/"+repo.Name)
	if err := writePullRequestOutput(p.opts.OutDir, index, details, shared, retain); err != nil {
		return triageOutcome{}, fmt.Errorf("writing pull request output: %w", err)
	}
	log.Printf("✅ Wrote %d open pull requests and %d shared failures to %s",
		len(details), len(shared.Failures), output.PullRequestIndexFilename)
	return triageOutcome{details: details, truncated: result.Truncated}, nil
}

// runPullRequestPass refreshes the pull request view without aborting the
// surrounding pass. The dashboard must still publish when GitHub is
// unreachable, so a failure keeps the previously written view.
func (p *pipeline) runPullRequestPass(ctx context.Context, res *refreshResult) {
	if !p.pullRequestsEnabled() {
		return
	}
	outcome, err := p.refreshPullRequests(ctx, res)
	if err != nil {
		// Commenting is deliberately skipped: without a successful publish the
		// triage pages a comment would link to are missing or stale.
		log.Printf("⚠ Pull request triage failed, keeping the previous view: %v", err)
		return
	}
	p.runPullRequestComments(ctx, outcome)
}

// runPullRequestComments posts the optional bot comment on newly observed pull
// requests. It acts on the pages this pass just published, so a comment can
// only ever link to a page that exists. A failure is logged rather than
// propagated: an unattended comment is the least important thing a pass does.
func (p *pipeline) runPullRequestComments(ctx context.Context, outcome triageOutcome) {
	if !p.cfg.CommentEnabled() {
		return
	}
	// Commenting is a GitHub write, so it obeys the same switch as issues,
	// fix PRs, and notifications.
	if p.opts.SkipSideEffects {
		log.Println("💬 Skipping pull request comments: side effects are disabled")
		return
	}
	if err := p.commentOnPullRequests(ctx, outcome); err != nil {
		log.Printf("⚠ Pull request commenting failed: %v", err)
	}
}

// commentCandidates converts the published details into commenting candidates.
// Building them from what was written is what guarantees every comment links to
// an existing page.
func commentCandidates(details []models.PullRequestDetail) []prcomment.Candidate {
	out := make([]prcomment.Candidate, 0, len(details))
	for _, detail := range details {
		out = append(out, prcomment.Candidate{
			Number: detail.Number,
			Author: detail.Author,
		})
	}
	return out
}

// commentOnPullRequests resolves the App identity and runs one commenting pass.
// Dry run takes exactly this path and stops only at the write, so what an
// operator previews is what a live run does.
func (p *pipeline) commentOnPullRequests(ctx context.Context, outcome triageOutcome) error {
	creds, ok := githubapp.CredentialsFromEnv()
	if !ok {
		return fmt.Errorf("pull_requests.comment.enabled is set but %s and %s are unset; "+
			"commenting posts as a GitHub App so contributors see a bot account rather than a person",
			githubapp.EnvAppID, githubapp.EnvPrivateKey)
	}
	app, err := githubapp.New(p.client, creds)
	if err != nil {
		return err
	}
	repo := p.cfg.Branding.SourceRepo
	login, err := app.Login(ctx)
	if err != nil {
		return err
	}
	token, err := app.InstallationToken(ctx, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	dryRun := p.cfg.CommentDryRun()
	log.Printf("💬 Pull request comments on %s/%s post as %s (dry run: %t)", repo.Owner, repo.Name, login, dryRun)

	stats, err := prcomment.Run(ctx, ghpr.NewClient(p.client, token), commentCandidates(outcome.details), prcomment.Options{
		Owner:                   repo.Owner,
		Repo:                    repo.Name,
		SiteURL:                 p.cfg.Branding.SiteURL,
		BotLogin:                login,
		DataDir:                 p.opts.OutDir,
		MaxPerPass:              p.cfg.PullRequests.Comment.MaxPerPass,
		DryRun:                  dryRun,
		EnginePullRequestMarker: fixpr.MarkerPrefix(),
		CandidatesTruncated:     outcome.truncated,
	})
	if err != nil {
		return err
	}
	log.Printf("💬 Pull request comments: %d posted, %d planned, %d skipped, %d failed",
		stats.Posted, stats.Planned, stats.Skipped, stats.Failed)
	return nil
}

// changedFileLister fetches one pull request's changed files. It is a seam for
// tests and is satisfied by *ghpr.Client.
type changedFileLister interface {
	ChangedFiles(ctx context.Context, owner, repo string, number int) (ghpr.ChangedFileSet, error)
}

// pullRequestChanges fetches changed files only for pull requests with a
// failing check whose build tested the current head. A stale build describes a
// different revision than the diff would, and attribution skips overlap for it,
// so fetching would spend GitHub quota for nothing. A per-pull failure is
// logged and skipped: without changed files attribution simply omits overlap.
func (p *pipeline) pullRequestChanges(ctx context.Context, lister changedFileLister, repo project.SourceRepo, details []models.PullRequestDetail) map[int]prattribution.PullChanges {
	var wanted []int
	for _, detail := range details {
		if comparableFailingCheck(detail) {
			wanted = append(wanted, detail.Number)
		}
	}
	if len(wanted) == 0 {
		return map[int]prattribution.PullChanges{}
	}

	changes := make(map[int]prattribution.PullChanges, len(wanted))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, changedFilesWorkers)
	for _, number := range wanted {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(number int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			set, err := lister.ChangedFiles(ctx, repo.Owner, repo.Name, number)
			if err != nil {
				log.Printf("    ⚠ pr #%d: listing changed files: %v", number, err)
				return
			}
			mu.Lock()
			changes[number] = prattribution.NewPullChanges(set.Paths(), set.FilesTruncated)
			mu.Unlock()
		}(number)
	}
	wg.Wait()
	return changes
}

// changedFilesWorkers bounds concurrent GitHub calls for changed files.
const changedFilesWorkers = 4

// comparableFailingCheck reports whether any failing check on the pull request
// tested the current head, which is the only case overlap can be computed for.
func comparableFailingCheck(detail models.PullRequestDetail) bool {
	for _, check := range detail.Checks {
		if !check.Passed && !check.Finished.IsZero() && !check.Stale && len(check.Failures) > 0 {
			return true
		}
	}
	return false
}
