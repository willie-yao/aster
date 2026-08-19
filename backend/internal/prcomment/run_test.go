package prcomment

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ghpr"
)

// fakePoster records every call so tests can assert that no write happened,
// which is the property most of this package exists to guarantee.
type fakePoster struct {
	posted map[int]string
	// timeline is the authoritative per-pull-request comment view read
	// immediately before each write.
	timeline map[int]bool
	// state, draft, and bodies are the pull request's current server state.
	state  map[int]string
	draft  map[int]bool
	bodies map[int]string
	// failOn returns an error from the post itself.
	failOn map[int]bool

	commented     map[int]bool
	listErr       error
	highest       int
	highestErr    error
	getErr        error
	timelineErr   error
	timelineCalls int
}

func newFakePoster() *fakePoster {
	return &fakePoster{
		posted:    map[int]string{},
		timeline:  map[int]bool{},
		state:     map[int]string{},
		draft:     map[int]bool{},
		bodies:    map[int]string{},
		failOn:    map[int]bool{},
		commented: map[int]bool{},
	}
}

func (f *fakePoster) HighestPullRequestNumber(_ context.Context, _, _ string) (int, error) {
	if f.highestErr != nil {
		return 0, f.highestErr
	}
	return f.highest, nil
}

func (f *fakePoster) ListPullRequestsCommentedBy(_ context.Context, _, _, _ string) (map[int]bool, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := map[int]bool{}
	for k, v := range f.commented {
		out[k] = v
	}
	return out, nil
}

func (f *fakePoster) GetPullRequest(_ context.Context, _, _ string, number int) (ghpr.PullRequest, error) {
	if f.getErr != nil {
		return ghpr.PullRequest{}, f.getErr
	}
	state := f.state[number]
	if state == "" {
		state = "open"
	}
	return ghpr.PullRequest{
		Number: number, State: state, Draft: f.draft[number], Body: f.bodies[number],
	}, nil
}

func (f *fakePoster) HasCommentBy(_ context.Context, _, _ string, number int, _ string) (bool, error) {
	f.timelineCalls++
	if f.timelineErr != nil {
		return false, f.timelineErr
	}
	return f.timeline[number], nil
}

func (f *fakePoster) CommentPullRequest(_ context.Context, _, _ string, number int, body string) error {
	if f.failOn[number] {
		return fmt.Errorf("simulated failure on #%d", number)
	}
	f.posted[number] = body
	f.timeline[number] = true
	return nil
}

func testOptions(t *testing.T, dir string, now time.Time) Options {
	t.Helper()
	return Options{
		Owner:    "o",
		Repo:     "r",
		SiteURL:  "https://dash.test",
		BotLogin: "aster[bot]",
		DataDir:  dir,
		now:      func() time.Time { return now },
	}
}

// captureLog redirects the standard logger for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return &buf
}

// activate runs the write-free first pass so later passes can post.
func activate(t *testing.T, poster *fakePoster, dir string, now time.Time) {
	t.Helper()
	if _, err := Run(context.Background(), poster, nil, testOptions(t, dir, now)); err != nil {
		t.Fatalf("activation pass: %v", err)
	}
}

var start = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestRunActivationPassPostsNothing proves enabling the feature does not
// backfill: the first pass records the watermark and writes nothing.
func TestRunActivationPassPostsNothing(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 42

	// A pull request above the watermark would otherwise look eligible.
	stats, err := Run(context.Background(), poster, []Candidate{{Number: 50, Author: "a"}}, testOptions(t, dir, start))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stats.Activated {
		t.Error("the first pass did not report activation")
	}
	if stats.Posted != 0 || len(poster.posted) != 0 {
		t.Fatalf("the activation pass posted %v", poster.posted)
	}

	state := LoadState(filepath.Join(dir, StateFilename), "o/r")
	if state.ActivatedAbove != 42 {
		t.Fatalf("ActivatedAbove = %d, want 42 (GitHub's newest pull request)", state.ActivatedAbove)
	}
}

// TestRunWatermarkComesFromGitHubNotTheListing matters because the triage
// listing is capped and excludes drafts. A draft or rarely-updated pull request
// above a listing-derived watermark would be backfilled once it was updated.
func TestRunWatermarkComesFromGitHubNotTheListing(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 500

	published := []Candidate{{Number: 450, Author: "a"}}
	activateWith := func(candidates []Candidate) {
		if _, err := Run(context.Background(), poster, candidates, testOptions(t, dir, start)); err != nil {
			t.Fatalf("activation pass: %v", err)
		}
	}
	activateWith(published)

	if got := LoadState(filepath.Join(dir, StateFilename), "o/r").ActivatedAbove; got != 500 {
		t.Fatalf("ActivatedAbove = %d, want 500 from GitHub rather than 450 from the listing", got)
	}

	// The draft is marked ready and now appears in triage output.
	ready := []Candidate{{Number: 450, Author: "a"}, {Number: 500, Author: "b"}}
	if _, err := Run(context.Background(), poster, ready, testOptions(t, dir, start.Add(time.Hour))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(poster.posted) != 0 {
		t.Fatalf("backfilled %v, which existed at activation", poster.posted)
	}
}

// TestRunNeverBackfillsOnALaterPass is the clock-skew guard. A pull request
// that existed at activation must stay ineligible forever, not merely during
// the activation pass, however the two clocks compare.
func TestRunNeverBackfillsOnALaterPass(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 10
	activate(t, poster, dir, start)

	existing := []Candidate{{Number: 10, Author: "was open at activation"}}
	for pass := range 5 {
		opts := testOptions(t, dir, start.Add(time.Duration(pass+1)*time.Hour))
		if _, err := Run(context.Background(), poster, existing, opts); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if len(poster.posted) != 0 {
		t.Fatalf("backfilled %v after activation", poster.posted)
	}
}

// TestRunPostsOnlyPullRequestsAboveTheWatermark covers the steady state.
func TestRunPostsOnlyPullRequestsAboveTheWatermark(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 10
	activate(t, poster, dir, start)

	pulls := []Candidate{{Number: 10, Author: "old"}, {Number: 11, Author: "new"}}
	stats, err := Run(context.Background(), poster, pulls, testOptions(t, dir, start.Add(time.Hour)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("posted %d, want 1", stats.Posted)
	}
	if _, ok := poster.posted[11]; !ok {
		t.Fatalf("posted on %v, want only #11", poster.posted)
	}
	if !strings.Contains(poster.posted[11], "https://dash.test/pull-requests/11") {
		t.Fatalf("comment body is missing the triage link:\n%s", poster.posted[11])
	}
}

// TestRunIsIdempotentAcrossPasses is the core dedup guarantee.
func TestRunIsIdempotentAcrossPasses(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	pulls := []Candidate{{Number: 7, Author: "a"}}
	for pass := range 5 {
		opts := testOptions(t, dir, start.Add(time.Duration(pass+1)*time.Hour))
		if _, err := Run(context.Background(), poster, pulls, opts); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if len(poster.posted) != 1 {
		t.Fatalf("posted %d comments across 5 passes, want 1", len(poster.posted))
	}
}

// TestRunIsIdempotentAfterDataDirectoryReset is the case the issue calls out.
// The Pages path keeps the data directory only in an Actions cache that
// expires, so losing local state is routine and must not double-post.
func TestRunIsIdempotentAfterDataDirectoryReset(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	pulls := []Candidate{{Number: 7, Author: "a"}}
	if _, err := Run(context.Background(), poster, pulls, testOptions(t, dir, start.Add(time.Hour))); err != nil {
		t.Fatalf("posting pass: %v", err)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("posted %d, want 1 before the reset", len(poster.posted))
	}

	// Wipe the data directory exactly as an expired Actions cache would. The
	// watermark is re-established from GitHub, which now knows about #7.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	poster.highest = 7

	for pass := range 3 {
		opts := testOptions(t, dir, start.Add(time.Duration(pass+2)*time.Hour))
		if _, err := Run(context.Background(), poster, pulls, opts); err != nil {
			t.Fatalf("post-reset pass %d: %v", pass, err)
		}
	}
	if len(poster.posted) != 1 {
		t.Fatalf("posted again after a data-directory reset: %d total", len(poster.posted))
	}
}

// TestRunDoesNotDuplicateWhenLocalStateIsLost proves the pull request's own
// timeline, not local state, is what ultimately prevents a second comment. It
// covers a crash between posting and saving, where the record never landed.
func TestRunDoesNotDuplicateWhenLocalStateIsLost(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	// A previous pass posted on #11 but died before recording it.
	poster.timeline[11] = true

	stats, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, testOptions(t, dir, start.Add(time.Hour)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 0 || len(poster.posted) != 0 {
		t.Fatalf("posted a duplicate over an unrecorded comment: %v", poster.posted)
	}
	// The outcome is recorded so the next pass does not pay for the check.
	if !LoadState(filepath.Join(dir, StateFilename), "o/r").Recorded(11) {
		t.Error("the discovered existing comment was not recorded")
	}
}

// TestRunConfirmsEligibilityImmediatelyBeforePosting covers the window between
// the snapshot selection ran against and the write itself.
func TestRunConfirmsEligibilityImmediatelyBeforePosting(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakePoster)
	}{
		{name: "closed after selection", setup: func(f *fakePoster) { f.state[11] = "closed" }},
		{name: "merged after selection", setup: func(f *fakePoster) { f.state[11] = "merged" }},
		{name: "converted to draft", setup: func(f *fakePoster) { f.draft[11] = true }},
		{
			name:  "an engine fix pull request",
			setup: func(f *fakePoster) { f.bodies[11] = "text\n<!-- aster-fix-marker:deadbeef -->\n" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			poster := newFakePoster()
			activate(t, poster, dir, start)
			tc.setup(poster)

			opts := testOptions(t, dir, start.Add(time.Hour))
			opts.EnginePullRequestMarker = "aster-fix-marker"
			if _, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, opts); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(poster.posted) != 0 {
				t.Fatalf("posted %v despite the pull request being ineligible at write time", poster.posted)
			}
		})
	}
}

// TestRunCommentsDespiteProseMentioningTheMarker proves self-detection matches
// the engine's hidden marker comment, not a bare mention in contributor prose.
func TestRunCommentsDespiteProseMentioningTheMarker(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)
	poster.bodies[11] = "This reverts a change from aster-fix-marker runs."

	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.EnginePullRequestMarker = "aster-fix-marker"
	if _, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := poster.posted[11]; !ok {
		t.Fatalf("prose mentioning the marker suppressed the comment: %v", poster.posted)
	}
}

// TestRunSkipsPostWhenConfirmationFails proves an unverifiable pull request is
// left alone rather than posted to on stale information.
func TestRunSkipsPostWhenConfirmationFails(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakePoster)
	}{
		{name: "pull request unreadable", setup: func(f *fakePoster) { f.getErr = fmt.Errorf("unreadable") }},
		{name: "timeline unreadable", setup: func(f *fakePoster) { f.timelineErr = fmt.Errorf("unavailable") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			poster := newFakePoster()
			activate(t, poster, dir, start)
			tc.setup(poster)

			stats, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, testOptions(t, dir, start.Add(time.Hour)))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(poster.posted) != 0 {
				t.Fatalf("posted %v without confirming eligibility", poster.posted)
			}
			if stats.Failed != 1 {
				t.Fatalf("stats = %+v, want 1 failed", stats)
			}
		})
	}
}

// TestRunDryRunWritesNothing proves the default configuration cannot post.
func TestRunDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	dry := testOptions(t, dir, start)
	dry.DryRun = true
	if _, err := Run(context.Background(), poster, nil, dry); err != nil {
		t.Fatalf("activation pass: %v", err)
	}

	buf := captureLog(t)
	later := testOptions(t, dir, start.Add(time.Hour))
	later.DryRun = true
	stats, err := Run(context.Background(), poster, []Candidate{{Number: 7, Author: "contributor"}}, later)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(poster.posted) != 0 {
		t.Fatalf("dry run posted %v", poster.posted)
	}
	// Reads are expected: confirmation is read-only and runs in dry run too, so
	// a preview matches a live pass. Only writes must not happen.
	if stats.Posted != 0 || stats.Planned != 1 {
		t.Fatalf("stats = %+v, want 0 posted and 1 planned", stats)
	}
	// The log must carry the exact body an operator is being asked to approve.
	if !strings.Contains(buf.String(), Body("contributor", "https://dash.test", 7)) {
		t.Fatalf("dry run did not log the exact body:\n%s", buf.String())
	}
}

// TestRunDryRunDoesNotSuppressALaterLiveRun proves previewing does not record
// comments that were never posted, which would silently skip them forever.
func TestRunDryRunDoesNotSuppressALaterLiveRun(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	dry := testOptions(t, dir, start)
	dry.DryRun = true
	if _, err := Run(context.Background(), poster, nil, dry); err != nil {
		t.Fatalf("activation pass: %v", err)
	}

	pulls := []Candidate{{Number: 7, Author: "a"}}
	preview := testOptions(t, dir, start.Add(time.Hour))
	preview.DryRun = true
	if _, err := Run(context.Background(), poster, pulls, preview); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	stats, err := Run(context.Background(), poster, pulls, testOptions(t, dir, start.Add(2*time.Hour)))
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("posted %d after a dry run, want 1", stats.Posted)
	}
}

// TestRunFailedPostDoesNotAbortThePass proves one bad post neither stops the
// others nor loses the records already made.
func TestRunFailedPostDoesNotAbortThePass(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.failOn[2] = true
	activate(t, poster, dir, start)

	pulls := []Candidate{
		{Number: 1, Author: "a"}, {Number: 2, Author: "b"}, {Number: 3, Author: "c"},
	}
	stats, err := Run(context.Background(), poster, pulls, testOptions(t, dir, start.Add(time.Hour)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 2 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 2 posted and 1 failed", stats)
	}

	// The successful posts must be durable, so a retry does not repost them.
	state := LoadState(filepath.Join(dir, StateFilename), "o/r")
	if !state.Recorded(1) || !state.Recorded(3) {
		t.Fatalf("successful posts were not recorded: %+v", state.Tracked)
	}
	if state.Recorded(2) {
		t.Fatal("a failed post was recorded as successful")
	}
}

// TestRunAbandonsPullRequestsThatKeepFailing proves a permanently unpostable
// pull request cannot occupy a cap slot forever and starve newer ones.
func TestRunAbandonsPullRequestsThatKeepFailing(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.failOn[1] = true
	activate(t, poster, dir, start)

	pulls := []Candidate{{Number: 1, Author: "a"}}
	for pass := range MaxPostAttempts {
		opts := testOptions(t, dir, start.Add(time.Duration(pass+1)*time.Hour))
		stats, err := Run(context.Background(), poster, pulls, opts)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if stats.Failed != 1 {
			t.Fatalf("pass %d failed %d, want 1", pass, stats.Failed)
		}
	}

	// The cap slot must now be free for a newer pull request.
	pulls = append(pulls, Candidate{Number: 2, Author: "b"})
	opts := testOptions(t, dir, start.Add(100*time.Hour))
	opts.MaxPerPass = 1
	stats, err := Run(context.Background(), poster, pulls, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("posted %d, want 1: the abandoned pull request still holds the cap slot", stats.Posted)
	}
	if _, ok := poster.posted[2]; !ok {
		t.Fatalf("posted %v, want the newer pull request", poster.posted)
	}
}

// TestRunRefusesWhenTheListingIsTruncated proves no comment is posted when the
// dashboard cannot publish a page for every open pull request, because pages
// outside the published set are pruned and the link would break.
func TestRunRefusesWhenTheListingIsTruncated(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.CandidatesTruncated = true
	_, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, opts)
	if err == nil {
		t.Fatal("expected an error when the listing was truncated")
	}
	if !strings.Contains(err.Error(), "pull_requests.max") {
		t.Fatalf("error = %v, want it to name the setting to raise", err)
	}
	if len(poster.posted) != 0 {
		t.Fatalf("posted %v despite a truncated listing", poster.posted)
	}
}

// TestRunRecordsTheWatermarkEvenWhenTruncated proves enabling commenting on a
// repository over the cap still pins the watermark now. Deferring it until the
// cap was raised would activate at that later point and permanently skip every
// pull request opened in between.
func TestRunRecordsTheWatermarkEvenWhenTruncated(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 10
	opts := testOptions(t, dir, start)
	opts.CandidatesTruncated = true

	stats, err := Run(context.Background(), poster, nil, opts)
	if err != nil {
		t.Fatalf("activation pass: %v", err)
	}
	if !stats.Activated {
		t.Error("did not activate while truncated")
	}
	if got := LoadState(filepath.Join(dir, StateFilename), "o/r").ActivatedAbove; got != 10 {
		t.Fatalf("ActivatedAbove = %d, want 10 recorded despite truncation", got)
	}
	if len(poster.posted) != 0 {
		t.Fatalf("posted %v on the activation pass", poster.posted)
	}
}

// TestRunStopsWhenTheWatermarkCannotBeEstablished proves activation fails
// closed. A zero watermark would make every existing pull request eligible.
func TestRunStopsWhenTheWatermarkCannotBeEstablished(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highestErr = fmt.Errorf("rate limited")

	if _, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, testOptions(t, dir, start)); err == nil {
		t.Fatal("expected an error when the watermark cannot be read")
	}
	if LoadState(filepath.Join(dir, StateFilename), "o/r").Activated() {
		t.Fatal("activated with an unestablished watermark")
	}
}

func TestRunRequiresIdentityAndRepo(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "no owner", mutate: func(o *Options) { o.Owner = "" }},
		{name: "no repo", mutate: func(o *Options) { o.Repo = "" }},
		{name: "no bot login", mutate: func(o *Options) { o.BotLogin = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poster := newFakePoster()
			opts := testOptions(t, dir, start)
			tc.mutate(&opts)
			if _, err := Run(context.Background(), poster, nil, opts); err == nil {
				t.Fatal("expected an error")
			}
			if poster.timelineCalls != 0 {
				t.Fatal("a misconfigured pass still called GitHub")
			}
		})
	}
}

func TestRunRequiresPoster(t *testing.T) {
	if _, err := Run(context.Background(), nil, nil, testOptions(t, t.TempDir(), start)); err == nil {
		t.Fatal("expected an error for a nil poster")
	}
}

// TestLoadStateDiscardsAnotherRepositorysState proves retargeting cannot reuse
// records that describe different pull requests.
func TestLoadStateDiscardsAnotherRepositorysState(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFilename)
	original := &State{
		Repo: "other/repo", ActivatedAt: start, ActivatedAbove: 5,
		Tracked: map[int]Commented{7: {Number: 7}}, Failures: map[int]attemptFailure{},
	}
	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := LoadState(path, "o/r")
	if reloaded.Activated() || reloaded.ActivatedAbove != 0 {
		t.Error("another repository's watermark was reused")
	}
	if reloaded.Recorded(7) {
		t.Error("another repository's records were reused")
	}
	if reloaded.Repo != "o/r" {
		t.Errorf("Repo = %q, want %q", reloaded.Repo, "o/r")
	}
}

func TestLoadStateSurvivesUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFilename)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureLog(t)

	state := LoadState(path, "o/r")
	if state == nil || state.Tracked == nil || state.Failures == nil {
		t.Fatal("LoadState did not return usable state")
	}
	if state.Activated() {
		t.Error("unparsable state produced a watermark")
	}
}

func TestStateRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFilename)

	state := LoadState(path, "o/r")
	state.ActivatedAt = start
	state.ActivatedAbove = 42
	state.Record(7, start)
	state.RecordFailure(9, start)
	if err := state.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := LoadState(path, "o/r")
	if reloaded.ActivatedAbove != 42 || !reloaded.ActivatedAt.Equal(start) {
		t.Errorf("watermark did not survive: %+v", reloaded)
	}
	if !reloaded.Recorded(7) {
		t.Error("record did not survive a round trip")
	}
	if reloaded.Failures[9].Count != 1 {
		t.Error("failure counter did not survive a round trip")
	}
}

// TestRecordClearsFailures proves a pull request that eventually succeeds is
// not left carrying a stale failure counter.
func TestRecordClearsFailures(t *testing.T) {
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	state.RecordFailure(7, start)
	state.RecordFailure(7, start)
	state.Record(7, start)

	if state.Exhausted(7) || state.Failures[7].Count != 0 {
		t.Fatalf("failure counter survived a successful post: %v", state.Failures)
	}
}

func TestExhaustedAtMaxAttempts(t *testing.T) {
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	for i := range MaxPostAttempts {
		if state.Exhausted(7) {
			t.Fatalf("exhausted after %d attempts, want %d", i, MaxPostAttempts)
		}
		state.RecordFailure(7, start)
	}
	if !state.Exhausted(7) {
		t.Fatalf("not exhausted after %d attempts", MaxPostAttempts)
	}
}

// TestRunSettlesEngineAuthoredPullRequests proves Aster's own fix pull requests
// are ruled out for good rather than re-examined every pass. They stay open for
// a long time, so without this they would hold slots under the per-pass cap and
// starve genuine contributions.
func TestRunSettlesEngineAuthoredPullRequests(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)
	poster.bodies[11] = "fix\n<!-- aster-fix-marker:deadbeef -->\n"

	opts := func(at time.Time) Options {
		o := testOptions(t, dir, at)
		o.EnginePullRequestMarker = "aster-fix-marker"
		o.MaxPerPass = 1
		return o
	}
	fixPR := Candidate{Number: 11, Author: "some-fix-bot"}
	if _, err := Run(context.Background(), poster, []Candidate{fixPR}, opts(start.Add(time.Hour))); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !LoadState(filepath.Join(dir, StateFilename), "o/r").IsSettled(11) {
		t.Fatal("the engine's own pull request was not settled")
	}

	// The single cap slot must now be available to a newer contribution.
	before := poster.timelineCalls
	pulls := []Candidate{fixPR, {Number: 12, Author: "human"}}
	stats, err := Run(context.Background(), poster, pulls, opts(start.Add(2*time.Hour)))
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("posted %d, want 1: the engine's pull request still holds the cap slot", stats.Posted)
	}
	if _, ok := poster.posted[12]; !ok {
		t.Fatalf("posted %v, want the contributor pull request", poster.posted)
	}
	if poster.timelineCalls-before > 1 {
		t.Errorf("re-examined the settled pull request: %d confirmations", poster.timelineCalls-before)
	}
}

// TestRunDryRunReportsWhatALiveRunWouldDo proves a preview is not inflated by
// pull requests a live pass would skip. Confirmation is read-only, so dry run
// performs it too.
func TestRunDryRunReportsWhatALiveRunWouldDo(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	dry := testOptions(t, dir, start)
	dry.DryRun = true
	if _, err := Run(context.Background(), poster, nil, dry); err != nil {
		t.Fatalf("activation pass: %v", err)
	}

	// #11 is an engine fix pull request, #12 already has a comment, #13 is real.
	poster.bodies[11] = "<!-- aster-fix-marker:deadbeef -->"
	poster.timeline[12] = true
	pulls := []Candidate{
		{Number: 11, Author: "a"}, {Number: 12, Author: "b"}, {Number: 13, Author: "c"},
	}
	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.DryRun = true
	opts.EnginePullRequestMarker = "aster-fix-marker"

	stats, err := Run(context.Background(), poster, pulls, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(poster.posted) != 0 {
		t.Fatalf("dry run posted %v", poster.posted)
	}
	if stats.Planned != 1 {
		t.Fatalf("planned %d, want 1: a preview must not count what a live pass would skip", stats.Planned)
	}
}

// TestStatePruneBoundsRetention proves records, and the pages they retain, do
// not accumulate forever.
func TestStatePruneBoundsRetention(t *testing.T) {
	now := start.Add(RetentionWindow * 2)
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	state.Record(1, now.Add(-RetentionWindow-time.Hour))
	state.RecordFailure(1, start)
	state.Record(2, now.Add(-time.Hour))
	state.Settle(3, now.Add(-RetentionWindow-time.Hour))
	state.Settle(4, now.Add(-time.Hour))

	state.Prune(now)

	if state.Recorded(1) || len(state.Failures) != 0 {
		t.Error("an expired record survived pruning")
	}
	if !state.Recorded(2) {
		t.Error("a recent record was pruned")
	}
	if state.IsSettled(3) {
		t.Error("an expired settlement survived pruning")
	}
	if !state.IsSettled(4) {
		t.Error("a recent settlement was pruned")
	}
	if retained := state.CommentedNumbers(); retained[1] || !retained[2] {
		t.Errorf("retention does not follow records: %v", retained)
	}
}

// TestPruneCannotCauseADuplicate proves an expired record is safe: the pull
// request itself, not local state, is the authoritative check.
func TestPruneCannotCauseADuplicate(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	pulls := []Candidate{{Number: 11, Author: "a"}}
	if _, err := Run(context.Background(), poster, pulls, testOptions(t, dir, start.Add(time.Hour))); err != nil {
		t.Fatalf("posting pass: %v", err)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("posted %d, want 1", len(poster.posted))
	}

	later := start.Add(RetentionWindow).Add(48 * time.Hour)
	if _, err := Run(context.Background(), poster, pulls, testOptions(t, dir, later)); err != nil {
		t.Fatalf("post-expiry pass: %v", err)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("posted again after the record expired: %d total", len(poster.posted))
	}
}

// TestRunCapBoundsWritesNotExaminations proves an ineligible candidate does not
// consume the per-pass budget. The cap exists to bound writes, so a pull request
// GitHub rejects must not delay a real contribution to a later pass.
func TestRunCapBoundsWritesNotExaminations(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	// #11 is an engine fix pull request and #12 already has a comment; only
	// #13 is postable. With a cap of one, it must still be posted this pass.
	poster.bodies[11] = "<!-- aster-fix-marker:deadbeef -->"
	poster.timeline[12] = true
	pulls := []Candidate{
		{Number: 11, Author: "a"}, {Number: 12, Author: "b"}, {Number: 13, Author: "c"},
	}
	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.MaxPerPass = 1
	opts.EnginePullRequestMarker = "aster-fix-marker"

	stats, err := Run(context.Background(), poster, pulls, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 1 {
		t.Fatalf("posted %d, want 1: ineligible candidates consumed the write budget", stats.Posted)
	}
	if _, ok := poster.posted[13]; !ok {
		t.Fatalf("posted %v, want the postable pull request", poster.posted)
	}
}

// TestRunCapStillBoundsWrites proves the write limit itself is hard.
func TestRunCapStillBoundsWrites(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	var pulls []Candidate
	for i := 1; i <= 50; i++ {
		pulls = append(pulls, Candidate{Number: i, Author: "a"})
	}
	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.MaxPerPass = 3

	stats, err := Run(context.Background(), poster, pulls, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Posted != 3 || len(poster.posted) != 3 {
		t.Fatalf("posted %d, want exactly the cap of 3", len(poster.posted))
	}
	// The lowest-numbered eligible pull requests are taken first.
	for _, number := range []int{1, 2, 3} {
		if _, ok := poster.posted[number]; !ok {
			t.Errorf("pull request %d was not posted; got %v", number, poster.posted)
		}
	}
}

// TestRunBoundsConfirmationsPerPass proves a pass with many ineligible
// candidates cannot spend unbounded GitHub budget looking for postable ones.
func TestRunBoundsConfirmationsPerPass(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	// Every candidate is already commented on, so none is postable.
	var pulls []Candidate
	for i := 1; i <= 100; i++ {
		pulls = append(pulls, Candidate{Number: i, Author: "a"})
		poster.timeline[i] = true
	}
	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.MaxPerPass = 2

	before := poster.timelineCalls
	if _, err := Run(context.Background(), poster, pulls, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spent := poster.timelineCalls - before; spent > opts.MaxPerPass*confirmationsPerWrite {
		t.Fatalf("made %d confirmations, want at most %d", spent, opts.MaxPerPass*confirmationsPerWrite)
	}
}

// TestStatePruneExpiresFailureOnlyRecords proves a pull request that only ever
// failed does not accumulate forever. It is never recorded or settled, so
// nothing else would expire it.
func TestStatePruneExpiresFailureOnlyRecords(t *testing.T) {
	now := start.Add(RetentionWindow * 2)
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	state.RecordFailure(1, now.Add(-RetentionWindow-time.Hour))
	state.RecordFailure(2, now.Add(-time.Hour))

	state.Prune(now)

	if _, ok := state.Failures[1]; ok {
		t.Error("an expired failure-only record survived pruning")
	}
	if _, ok := state.Failures[2]; !ok {
		t.Error("a recent failure record was pruned")
	}
}

// TestRunCapBoundsWriteAttempts proves the cap bounds what is sent to GitHub,
// not what succeeds. A failed post may still have reached GitHub, so counting
// only successes would let one pass exceed its public blast radius.
func TestRunCapBoundsWriteAttempts(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	var pulls []Candidate
	for i := 1; i <= 50; i++ {
		pulls = append(pulls, Candidate{Number: i, Author: "a"})
		poster.failOn[i] = true // every post errors
	}
	opts := testOptions(t, dir, start.Add(time.Hour))
	opts.MaxPerPass = 3

	stats, err := Run(context.Background(), poster, pulls, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Failed != 3 {
		t.Fatalf("attempted %d writes, want at most the cap of 3", stats.Failed)
	}
}

// TestRunAdoptsExistingCommentsOnActivation is the data-directory reset case.
// Local records are gone but the comments are still live on GitHub, and the
// pages they link to must keep being retained once those pull requests close.
func TestRunAdoptsExistingCommentsOnActivation(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.highest = 20
	// A previous deployment commented on #11 before its state was lost.
	poster.commented[11] = true

	activate(t, poster, dir, start)

	if !CommentedNumbersAt(dir, "o/r")[11] {
		t.Fatal("an existing comment was not adopted, so its triage page would be pruned on close")
	}
	if !LoadState(filepath.Join(dir, StateFilename), "o/r").Recorded(11) {
		t.Error("the adopted comment is not recorded")
	}
}

// TestRunFailsActivationWhenExistingCommentsCannotBeRead proves activation does
// not proceed on an unknown history, which would drop retention for live
// comments.
func TestRunFailsActivationWhenExistingCommentsCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	poster.listErr = fmt.Errorf("rate limited")

	if _, err := Run(context.Background(), poster, nil, testOptions(t, dir, start)); err == nil {
		t.Fatal("expected an error when existing comments cannot be read")
	}
	if LoadState(filepath.Join(dir, StateFilename), "o/r").Activated() {
		t.Fatal("activated without establishing the existing comment history")
	}
}

// TestRunRetentionSurvivesAnUnconfirmedWrite is the crash-after-accept case.
// GitHub may accept a comment whose confirmation is lost. If that pull request
// then closes it never becomes a candidate again, so nothing later could
// recover the record, and its page would be pruned out from under a public
// link. Retention is therefore written before the post.
func TestRunRetentionSurvivesAnUnconfirmedWrite(t *testing.T) {
	dir := t.TempDir()
	poster := newFakePoster()
	activate(t, poster, dir, start)

	// The post errors, standing in for a response lost after GitHub accepted it.
	poster.failOn[11] = true
	if _, err := Run(context.Background(), poster, []Candidate{{Number: 11, Author: "a"}}, testOptions(t, dir, start.Add(time.Hour))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !CommentedNumbersAt(dir, "o/r")[11] {
		t.Fatal("an attempted write did not retain the pull request's page")
	}

	// The pull request closes, so it is absent from every later candidate set.
	// Retention must persist without it ever being examined again.
	for pass := range 3 {
		opts := testOptions(t, dir, start.Add(time.Duration(pass+2)*time.Hour))
		if _, err := Run(context.Background(), poster, nil, opts); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if !CommentedNumbersAt(dir, "o/r")[11] {
		t.Fatal("retention was lost once the pull request stopped being a candidate")
	}

	// It must still be retried while it is a candidate: an attempt is not a
	// confirmation.
	if LoadState(filepath.Join(dir, StateFilename), "o/r").Recorded(11) {
		t.Fatal("an unconfirmed attempt was recorded as a posted comment")
	}
}

// TestRecordClearsPendingRetention proves a confirmed comment stops being
// tracked as merely attempted.
func TestRecordClearsPendingRetention(t *testing.T) {
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	state.RecordIntent(7, start)
	state.Record(7, start)

	if _, pending := state.Pending[7]; pending {
		t.Error("a confirmed comment is still marked as merely attempted")
	}
	if !state.CommentedNumbers()[7] {
		t.Error("a confirmed comment lost its retention")
	}
}

// TestPruneExpiresPendingRetention keeps unconfirmed attempts bounded.
func TestPruneExpiresPendingRetention(t *testing.T) {
	now := start.Add(RetentionWindow * 2)
	state := LoadState(filepath.Join(t.TempDir(), StateFilename), "o/r")
	state.RecordIntent(1, now.Add(-RetentionWindow-time.Hour))
	state.RecordIntent(2, now.Add(-time.Hour))

	state.Prune(now)

	if retained := state.CommentedNumbers(); retained[1] || !retained[2] {
		t.Fatalf("pending retention not bounded correctly: %v", retained)
	}
}
