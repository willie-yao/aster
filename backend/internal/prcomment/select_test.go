package prcomment

import (
	"testing"
)

// activation is the watermark used across these tests: pull requests numbered
// at or below it already existed when commenting was enabled.
const activation = 100

func pull(number int, author string) Candidate {
	return Candidate{Number: number, Author: author}
}

func numbers(pulls []Candidate) []int {
	out := make([]int, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, p.Number)
	}
	return out
}

func equal(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// skipReasonFor returns the recorded reason for one pull request number.
func skipReasonFor(sel Selection, number int) SkipReason {
	for _, s := range sel.Skipped {
		if s.Number == number {
			return s.Reason
		}
	}
	return ""
}

// TestSelectSkipsPullRequestsAtOrBelowWatermark is the guard against the
// backfill blast: enabling the feature must not comment on pull requests that
// were already open.
func TestSelectSkipsPullRequestsAtOrBelowWatermark(t *testing.T) {
	got := Select(SelectInput{
		Candidates: []Candidate{
			pull(activation-1, "older"),
			pull(activation, "exactly-at-watermark"),
			pull(activation+1, "newer"),
		},
		ActivatedAbove: activation,
		BotLogin:       "aster[bot]",
	})

	if want := []int{activation + 1}; !equal(numbers(got.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(got.Selected), want)
	}
	for _, number := range []int{activation - 1, activation} {
		if reason := skipReasonFor(got, number); reason != SkipBeforeActivation {
			t.Errorf("pr #%d skipped for %q, want %q", number, reason, SkipBeforeActivation)
		}
	}
}

// TestSelectWatermarkIgnoresClocks is why eligibility is decided by pull
// request number rather than creation time. A runner clock lagging behind
// GitHub makes pull requests that predate activation look newer than it, which
// a timestamp cutoff would treat as eligible on the very next pass.
func TestSelectWatermarkIgnoresClocks(t *testing.T) {
	got := Select(SelectInput{
		Candidates:     []Candidate{pull(activation-1, "opened before activation")},
		ActivatedAbove: activation,
		BotLogin:       "aster[bot]",
	})
	if len(got.Selected) != 0 {
		t.Fatalf("selected %v, want none regardless of any clock", numbers(got.Selected))
	}
}

func TestSelectSkipsAlreadyCommented(t *testing.T) {
	got := Select(SelectInput{
		Candidates:     []Candidate{pull(101, "a"), pull(102, "b")},
		ActivatedAbove: activation,
		Recorded:       func(number int) bool { return number == 101 },
		BotLogin:       "aster[bot]",
	})

	if want := []int{102}; !equal(numbers(got.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(got.Selected), want)
	}
	if reason := skipReasonFor(got, 101); reason != SkipAlreadyCommented {
		t.Errorf("pr #101 skipped for %q, want %q", reason, SkipAlreadyCommented)
	}
}

// TestSelectHonorsLocalStateAsWell proves local state supplements the GitHub
// query rather than being ignored when the query returns nothing.
func TestSelectHonorsLocalStateAsWell(t *testing.T) {
	got := Select(SelectInput{
		Candidates:     []Candidate{pull(101, "a"), pull(102, "b")},
		ActivatedAbove: activation,
		Recorded:       func(number int) bool { return number == 101 },
		BotLogin:       "aster[bot]",
	})
	if want := []int{102}; !equal(numbers(got.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(got.Selected), want)
	}
}

// TestSelectSkipsItsOwnPullRequests covers the author match. Fix pull requests
// opened under a different credential are caught later, by the body-marker
// check in confirmEligible.
func TestSelectSkipsItsOwnPullRequests(t *testing.T) {
	got := Select(SelectInput{
		Candidates: []Candidate{
			pull(101, "aster[bot]"),
			pull(102, "ASTER[BOT]"),
			pull(103, "human"),
		},
		ActivatedAbove: activation,
		BotLogin:       "aster[bot]",
	})

	if want := []int{103}; !equal(numbers(got.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(got.Selected), want)
	}
	for _, number := range []int{101, 102} {
		if reason := skipReasonFor(got, number); reason != SkipSelfAuthored {
			t.Errorf("pr #%d skipped for %q, want %q", number, reason, SkipSelfAuthored)
		}
	}
}

func TestSelectSkipsDrafts(t *testing.T) {
	draft := pull(101, "a")
	draft.Draft = true
	got := Select(SelectInput{
		Candidates:     []Candidate{draft, pull(102, "b")},
		ActivatedAbove: activation,
		BotLogin:       "aster[bot]",
	})

	if want := []int{102}; !equal(numbers(got.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(got.Selected), want)
	}
	if reason := skipReasonFor(got, 101); reason != SkipDraft {
		t.Errorf("pr #101 skipped for %q, want %q", reason, SkipDraft)
	}
}
func TestSelectIsDeterministic(t *testing.T) {
	pulls := []Candidate{pull(130, "a"), pull(110, "b"), pull(120, "c")}
	first := Select(SelectInput{Candidates: pulls, ActivatedAbove: activation, BotLogin: "x"})

	reversed := []Candidate{pulls[2], pulls[1], pulls[0]}
	second := Select(SelectInput{Candidates: reversed, ActivatedAbove: activation, BotLogin: "x"})

	if !equal(numbers(first.Selected), numbers(second.Selected)) {
		t.Fatalf("selection depends on input order: %v then %v",
			numbers(first.Selected), numbers(second.Selected))
	}
	if want := []int{110, 120, 130}; !equal(numbers(first.Selected), want) {
		t.Fatalf("selected = %v, want %v", numbers(first.Selected), want)
	}
}
