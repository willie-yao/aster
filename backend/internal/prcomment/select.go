package prcomment

import (
	"sort"
	"strings"
)

// SkipReason explains why a pull request was not selected. It is reported so an
// operator reading a dry run can tell "nothing to do" apart from "the watermark
// or the cap suppressed everything".
type SkipReason string

const (
	// SkipBeforeActivation means the pull request already existed when
	// commenting was enabled. Without this, enabling the feature would comment
	// on every currently-open pull request at once.
	SkipBeforeActivation SkipReason = "opened before commenting was enabled"
	// SkipAlreadyCommented means GitHub or local state already shows a comment.
	SkipAlreadyCommented SkipReason = "already commented"
	// SkipSelfAuthored means the bot opened the pull request. Aster opens its
	// own fix pull requests into the source repository, and commenting on them
	// would link them back to the triage page for no one's benefit.
	SkipSelfAuthored SkipReason = "opened by the commenting bot"
	// SkipDraft means the pull request is not ready for review. Listing already
	// excludes drafts, so this is a backstop for callers passing their own list.
	SkipDraft SkipReason = "draft"
	// SkipOverCap means the per-pass cap was reached.
	SkipOverCap SkipReason = "per-pass cap reached"
	// SkipNotOpen means the pull request closed or merged after selection.
	SkipNotOpen SkipReason = "no longer open"
	// SkipFailedTooOften means posting has failed repeatedly for this pull
	// request. Retrying forever would let one unpostable pull request hold a
	// cap slot on every pass and starve newer ones.
	SkipFailedTooOften SkipReason = "posting failed too many times"
)

// Skipped is one pull request that was considered and passed over.
type Skipped struct {
	Number int
	Reason SkipReason
}

// Candidate is one pull request commenting may act on. It is deliberately
// decoupled from any GitHub client type: candidates are built from the triage
// details that were just published, so a comment can only ever link to a page
// that exists.
type Candidate struct {
	Number int
	Author string
	Draft  bool
}

// SelectInput is everything Select needs to decide. Keeping it data-only makes
// every rule testable without a network.
type SelectInput struct {
	// Candidates are the pull requests published by this pass.
	Candidates []Candidate
	// ActivatedAbove is the highest pull request number that existed when
	// commenting was enabled. Only higher numbers are eligible. Pull request
	// numbers come from GitHub and increase monotonically, so this holds
	// regardless of the engine's clock.
	ActivatedAbove int
	// Recorded reports whether local state already tracks a comment. It is a
	// fast path only: every write is confirmed against GitHub regardless.
	Recorded func(number int) bool
	// Settled reports whether a pull request was already ruled out for good,
	// which keeps it from consuming a slot under the cap on every pass.
	Settled func(number int) bool
	// Exhausted reports whether a pull request has failed too many times to
	// keep retrying. Without it a permanently unpostable pull request would
	// occupy a cap slot on every pass and starve newer ones.
	Exhausted func(number int) bool
	// BotLogin is the commenting identity, used to recognize its own pull
	// requests.
	BotLogin string
}

// Selection is the outcome of one selection pass.
type Selection struct {
	// Selected are the pull requests to comment on, in ascending number order
	// so a pass is deterministic and the oldest eligible one is never starved
	// by the cap.
	Selected []Candidate
	// Skipped explains every pull request that was passed over.
	Skipped []Skipped
}

// Select applies the eligibility rules that can be decided without contacting
// GitHub: the activation watermark, local records, settled and exhausted pull
// requests, the self-authored author match, and the draft backstop.
//
// It does not apply the per-pass cap. That cap bounds writes, and whether a
// candidate becomes a write is only known after confirming it against GitHub,
// so the runner applies it there. Capping here would let a candidate that turns
// out to be ineligible consume a slot a real contribution needed.
func Select(in SelectInput) Selection {
	out := Selection{}
	for _, pull := range in.Candidates {
		if reason, skip := skipReason(in, pull); skip {
			out.Skipped = append(out.Skipped, Skipped{Number: pull.Number, Reason: reason})
			continue
		}
		out.Selected = append(out.Selected, pull)
	}
	// Ascending order keeps a pass deterministic and stops the cap from
	// starving the oldest eligible pull request.
	sort.Slice(out.Selected, func(i, j int) bool { return out.Selected[i].Number < out.Selected[j].Number })
	return out
}

// skipReason returns the first rule that disqualifies pull, if any.
func skipReason(in SelectInput, pull Candidate) (SkipReason, bool) {
	if pull.Draft {
		return SkipDraft, true
	}
	if pull.Number <= in.ActivatedAbove {
		return SkipBeforeActivation, true
	}
	if in.Recorded != nil && in.Recorded(pull.Number) {
		return SkipAlreadyCommented, true
	}
	if login := strings.TrimSpace(in.BotLogin); login != "" && strings.EqualFold(pull.Author, login) {
		return SkipSelfAuthored, true
	}
	if in.Settled != nil && in.Settled(pull.Number) {
		return SkipSelfAuthored, true
	}
	if in.Exhausted != nil && in.Exhausted(pull.Number) {
		return SkipFailedTooOften, true
	}
	return "", false
}
