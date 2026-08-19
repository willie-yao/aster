// Package prcomment posts one bot comment on each newly observed pull request,
// linking to that pull request's triage page on the dashboard.
//
// This is the engine's only unattended write that contacts a contributor's pull
// request. Scheduled issue recovery also writes unattended, but only to issues a
// maintainer already confirmed, so the safeguards here are the point of the
// package rather than incidental to it:
//
//   - opt-in, and dry run until an operator explicitly turns the write on
//   - an activation cutoff, so enabling it never backfills open pull requests
//   - dedup against GitHub itself, so losing local state cannot double-post
//   - a per-pass cap, so a bug cannot fan out across a repository
//   - a skip for the bot's own pull requests, so it never comments on itself
package prcomment

import (
	"fmt"
	"strings"
)

// DefaultMaxPerPass bounds comments posted in one pass. Steady state is a
// handful of newly opened pull requests, so this is generous for normal
// operation while still capping a runaway.
const DefaultMaxPerPass = 10

// confirmationsPerWrite bounds how many candidates one pass may confirm against
// GitHub per unit of its write budget. Ineligible candidates do not consume the
// write cap, so this keeps a pass with many of them from spending unbounded
// API budget before it finishes.
const confirmationsPerWrite = 4

// engineIssuesURL is where contributors are pointed to report bot misbehavior,
// mirroring how the Prow bot points at its own repository.
const engineIssuesURL = "https://github.com/willie-yao/aster/issues/new"

// Body renders the comment for one pull request. It is deterministic: the same
// inputs always produce the same text, so a dry run shows exactly what a live
// run would post.
func Body(author, siteURL string, number int) string {
	var b strings.Builder
	if greeting := strings.TrimSpace(author); greeting != "" {
		fmt.Fprintf(&b, "Hi @%s. Thanks for your PR.\n\n", greeting)
	}
	b.WriteString("I'm Aster. Once your presubmits have run, the failing tests for this pull\n")
	b.WriteString("request and the evidence behind them are here:\n\n")
	fmt.Fprintf(&b, "%s\n\n", TriagePageURL(siteURL, number))
	b.WriteString("<details>\n\n")
	b.WriteString("I don't run tests and I don't gate this pull request; I only summarize results\n")
	b.WriteString("Prow has already published. I post this comment once, when I first observe a\n")
	b.WriteString("pull request, and I never edit or repost it. If you have questions or\n")
	b.WriteString("suggestions related to my behavior, please file an issue against the\n")
	fmt.Fprintf(&b, "[willie-yao/aster](%s) repository.\n\n", engineIssuesURL)
	b.WriteString("</details>\n")
	return b.String()
}

// TriagePageURL builds the dashboard link for one pull request.
func TriagePageURL(siteURL string, number int) string {
	return fmt.Sprintf("%s/pull-requests/%d", strings.TrimRight(strings.TrimSpace(siteURL), "/"), number)
}
