package prcomment

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// Poster is the GitHub surface the runner needs. It is an interface so tests
// can prove no write is attempted, which is the property that matters most
// here.
type Poster interface {
	// HighestPullRequestNumber returns the largest number the repository has
	// assigned, open or closed, draft or not.
	HighestPullRequestNumber(ctx context.Context, owner, repo string) (int, error)
	// ListPullRequestsCommentedBy returns open pull requests login has already
	// commented on. It is read once, at activation, to recover records lost
	// with the data directory.
	ListPullRequestsCommentedBy(ctx context.Context, owner, repo, login string) (map[int]bool, error)
	// GetPullRequest reads one pull request's current state, used to confirm
	// eligibility immediately before writing to it.
	GetPullRequest(ctx context.Context, owner, repo string, number int) (ghpr.PullRequest, error)
	// HasCommentBy reports whether login has already commented on one pull
	// request, read directly from its comment timeline.
	HasCommentBy(ctx context.Context, owner, repo string, number int, login string) (bool, error)
	// CommentPullRequest posts one timeline comment.
	CommentPullRequest(ctx context.Context, owner, repo string, number int, body string) error
}

// Options configure one commenting pass.
type Options struct {
	// Owner and Repo name the repository whose pull requests are commented on.
	Owner string
	Repo  string
	// SiteURL is the dashboard root the comment links into.
	SiteURL string
	// BotLogin is the resolved posting identity, "{slug}[bot]".
	BotLogin string
	// DataDir is where the tracking state file lives.
	DataDir string
	// MaxPerPass caps comments in this pass. Non-positive uses the default.
	MaxPerPass int
	// DryRun logs the exact body instead of posting it.
	DryRun bool
	// EnginePullRequestMarker is the body marker Aster embeds in pull requests
	// it opens itself. Those are opened under a different credential than the
	// commenting App, so they cannot be recognized by author. Empty skips the
	// check.
	EnginePullRequestMarker string
	// CandidatesTruncated reports that the triage listing hit its cap, so the
	// dashboard did not publish a page for every open pull request. Commenting
	// is skipped entirely in that case: a comment promises a triage page, and
	// pages outside the published set are pruned.
	CandidatesTruncated bool
	// now is a seam for tests.
	now func() time.Time
}

func (o Options) clock() time.Time {
	if o.now != nil {
		return o.now().UTC()
	}
	return time.Now().UTC()
}

func (o Options) fullRepo() string { return o.Owner + "/" + o.Repo }

// Stats summarize one pass for logging.
type Stats struct {
	// Posted counts comments actually written to GitHub. It is always zero in
	// a dry run.
	Posted int
	// Planned counts comments a dry run would have posted. It is always zero in
	// a live run.
	Planned int
	Skipped int
	Failed  int
	DryRun  bool
	// Activated is true on the pass that recorded the watermark. That pass
	// posts nothing at all.
	Activated bool
}

// Run posts the comment on newly observed pull requests.
//
// candidates must be the pull requests whose triage pages this pass just
// published, so a comment can only link to a page that exists.
//
// Duplicate posting is prevented in three layers, each covering the one before:
// the activation watermark makes everything that existed at enable time
// permanently ineligible, local state skips what this deployment already
// posted, and every write is preceded by reading the pull request itself, which
// is authoritative even when local state was lost.
func Run(ctx context.Context, poster Poster, candidates []Candidate, opts Options) (Stats, error) {
	if poster == nil {
		return Stats{}, fmt.Errorf("prcomment: a poster is required")
	}
	if opts.Owner == "" || opts.Repo == "" {
		return Stats{}, fmt.Errorf("prcomment: owner and repo are required")
	}
	if opts.BotLogin == "" {
		return Stats{}, fmt.Errorf("prcomment: the posting identity is required")
	}

	statePath := filepath.Join(opts.DataDir, StateFilename)
	stats := Stats{DryRun: opts.DryRun}

	var runErr error
	lockErr := statefile.WithLock(statePath, func() error {
		// Loaded, decided, and saved under one lock so a concurrent pass cannot
		// post between this one's read and its save.
		state := LoadState(statePath, opts.fullRepo())
		state.Prune(opts.clock())

		// Activation is a write-free pass. Recording the watermark and stopping
		// means no pull request that exists now can ever be commented on.
		if !state.Activated() {
			// The bound is read from GitHub rather than taken from candidates,
			// which are capped and exclude drafts. A draft or rarely-updated
			// pull request above a candidate-derived watermark would be
			// backfilled the moment it was updated.
			highest, err := poster.HighestPullRequestNumber(ctx, opts.Owner, opts.Repo)
			if err != nil {
				runErr = fmt.Errorf("prcomment: %w", err)
				return nil
			}
			// Activation also runs after a data-directory reset, where earlier
			// comments are still live on GitHub. Adopting them restores the
			// retention that keeps their triage pages from being pruned when
			// those pull requests close.
			existing, err := poster.ListPullRequestsCommentedBy(ctx, opts.Owner, opts.Repo, opts.BotLogin)
			if err != nil {
				runErr = fmt.Errorf("prcomment: %w", err)
				return nil
			}
			state.Adopt(existing, opts.clock())
			state.ActivatedAt = opts.clock()
			state.ActivatedAbove = highest
			stats.Activated = true
			log.Printf("💬 Pull request comments activated for %s above pull request #%d, adopting %d existing comment(s); only higher-numbered pull requests are eligible, so this pass posts nothing",
				opts.fullRepo(), state.ActivatedAbove, len(existing))
			return state.Save(statePath)
		}

		// A truncated triage listing means the dashboard did not publish a page
		// for every open pull request. A comment promises a page, so none is
		// posted until the operator raises the cap. Checked after activation so
		// the watermark is still recorded: otherwise raising the cap later
		// would activate then, permanently skipping everything opened between.
		if opts.CandidatesTruncated {
			runErr = fmt.Errorf("prcomment: the pull request listing was truncated, so a triage page is not published for every open pull request; raise pull_requests.max above the number of open pull requests before commenting")
			return nil
		}

		selection := Select(SelectInput{
			Candidates:     candidates,
			ActivatedAbove: state.ActivatedAbove,
			Recorded:       state.Recorded,
			Settled:        state.IsSettled,
			Exhausted:      state.Exhausted,
			BotLogin:       opts.BotLogin,
		})
		stats.Skipped = len(selection.Skipped)

		// The cap bounds writes, so it is counted against comments actually
		// made rather than candidates examined. Confirmation is bounded
		// separately so a pass with many ineligible candidates cannot spend an
		// unbounded amount of GitHub budget.
		limit := opts.MaxPerPass
		if limit <= 0 {
			limit = DefaultMaxPerPass
		}
		confirmations, writes := 0, 0
		for i, pull := range selection.Selected {
			// writes counts attempts, not successes. A failed post may still
			// have reached GitHub, so the cap has to bound what was sent.
			if writes+stats.Planned >= limit || confirmations >= limit*confirmationsPerWrite {
				for _, remaining := range selection.Selected[i:] {
					log.Printf("    ⏭ pr #%d: %s", remaining.Number, SkipOverCap)
					stats.Skipped++
				}
				break
			}
			confirmations++
			// Confirmation is read-only, so a dry run performs it too and a
			// preview reports exactly what a live pass would do.
			eligible, reason, err := confirmEligible(ctx, poster, pull.Number, opts)
			if err != nil {
				log.Printf("    ⚠ pr #%d: confirming before commenting: %v", pull.Number, err)
				stats.Failed++
				continue
			}
			if !eligible {
				switch reason {
				case SkipAlreadyCommented:
					// Record it so the next pass does not spend the check again.
					state.Record(pull.Number, opts.clock())
				case SkipSelfAuthored:
					// Rule it out for good; it would otherwise hold a cap slot
					// on every pass.
					state.Settle(pull.Number, opts.clock())
				}
				log.Printf("    ⏭ pr #%d: %s", pull.Number, reason)
				stats.Skipped++
				continue
			}

			body := Body(pull.Author, opts.SiteURL, pull.Number)
			if opts.DryRun {
				log.Printf("💬 [dry run] would comment on %s#%d as %s:\n%s",
					opts.fullRepo(), pull.Number, opts.BotLogin, body)
				stats.Planned++
				continue
			}

			// Retention is persisted before the write. GitHub may accept a
			// comment whose response is lost, and if that pull request then
			// closes it never becomes a candidate again, so nothing later would
			// keep the page the comment links to.
			state.RecordIntent(pull.Number, opts.clock())
			if err := state.Save(statePath); err != nil {
				runErr = fmt.Errorf("prcomment: %w", err)
				return nil
			}

			writes++
			if err := poster.CommentPullRequest(ctx, opts.Owner, opts.Repo, pull.Number, body); err != nil {
				// One unpostable pull request must not abandon the rest or
				// discard the records already made this pass.
				log.Printf("    ⚠ pr #%d: posting comment: %v", pull.Number, err)
				state.RecordFailure(pull.Number, opts.clock())
				stats.Failed++
				continue
			}
			state.Record(pull.Number, opts.clock())
			stats.Posted++
			log.Printf("💬 Commented on %s#%d as %s", opts.fullRepo(), pull.Number, opts.BotLogin)
		}
		return state.Save(statePath)
	})
	if lockErr != nil {
		return stats, fmt.Errorf("prcomment: %w", lockErr)
	}
	return stats, runErr
}

// confirmEligible re-reads one pull request immediately before writing to it.
// Selection ran against a snapshot and local state that a reset can wipe, so
// this is the authoritative check and the last one before an unattended write.
func confirmEligible(ctx context.Context, poster Poster, number int, opts Options) (bool, SkipReason, error) {
	current, err := poster.GetPullRequest(ctx, opts.Owner, opts.Repo, number)
	if err != nil {
		return false, "", err
	}
	if !strings.EqualFold(current.State, "open") {
		return false, SkipNotOpen, nil
	}
	if current.Draft {
		return false, SkipDraft, nil
	}
	// Aster opens its own fix pull requests into this repository under a
	// different credential, so they cannot be recognized by author. The full
	// hidden comment form is matched rather than the bare token, so ordinary
	// prose mentioning the engine does not read as one.
	if marker := opts.EnginePullRequestMarker; marker != "" && strings.Contains(current.Body, "<!-- "+marker+":") {
		return false, SkipSelfAuthored, nil
	}
	posted, err := poster.HasCommentBy(ctx, opts.Owner, opts.Repo, number, opts.BotLogin)
	if err != nil {
		return false, "", err
	}
	if posted {
		return false, SkipAlreadyCommented, nil
	}
	return true, "", nil
}
