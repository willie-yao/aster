package prcomment

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/aster/backend/internal/statefile"
)

// StateFilename is the tracking file written into the data directory.
const StateFilename = "pr_comment_state.json"

// RetentionWindow bounds how long a comment record is kept. Records also keep
// the pull request's triage page alive, so retaining them forever would grow
// both the state file and the published data directory without limit.
// Expiring one cannot cause a second comment: every write is confirmed against
// the pull request itself first.
const RetentionWindow = 90 * 24 * time.Hour

// MaxPostAttempts is how many times posting may fail for one pull request
// before it is abandoned. A pull request that can never be commented on (locked
// conversation, permissions revoked) would otherwise occupy a slot under the
// per-pass cap on every pass and starve newer ones.
const MaxPostAttempts = 3

// attemptFailure counts failed post attempts for one pull request. The
// timestamp exists so these can be pruned: a pull request that only ever fails
// is never recorded or settled, so nothing else would ever expire it.
type attemptFailure struct {
	Count  int       `json:"count"`
	LastAt time.Time `json:"last_at"`
}

// Commented records one posted comment. It is a fast path that saves an API
// call, not the authoritative dedup signal: this file does not survive a
// data-directory reset, so every write is confirmed against GitHub first.
type Commented struct {
	Number      int       `json:"number"`
	CommentedAt time.Time `json:"commented_at"`
}

// State is the on-disk tracking state, scoped to the repository it belongs to
// so retargeting never reuses another repository's records.
type State struct {
	// Repo is the "owner/name" these records belong to. State for a different
	// repository is discarded on load.
	Repo string `json:"repo,omitempty"`
	// ActivatedAt is when commenting was first enabled, kept for operator
	// audit. Eligibility is decided by ActivatedAbove, not by this timestamp.
	ActivatedAt time.Time `json:"activated_at"`
	// ActivatedAbove is the highest pull request number that existed when
	// commenting was enabled. Only higher-numbered pull requests are eligible,
	// so turning the feature on cannot comment on work already in flight.
	//
	// Pull request numbers are assigned by GitHub and increase monotonically,
	// which makes this watermark independent of the engine's clock. A timestamp
	// cutoff is not: a runner clock lagging behind GitHub would make pull
	// requests opened before activation look newer than it.
	//
	// If this file is lost the watermark is set again from GitHub, moving it
	// forward, so a reset cannot cause a second comment on anything that
	// already exists.
	ActivatedAbove int `json:"activated_above"`
	// Tracked maps pull request number to its recorded comment.
	Tracked map[int]Commented `json:"tracked"`
	// Failures counts consecutive failed post attempts per pull request.
	Failures map[int]attemptFailure `json:"failures,omitempty"`
	// Pending records pull requests a write was attempted for but not
	// confirmed. It exists only to retain their triage pages: GitHub may have
	// accepted a comment whose confirmation was lost, and if that pull request
	// then closes it never becomes a candidate again, so nothing else would
	// recover the record before its page is pruned. It is deliberately not a
	// dedup signal, because an attempt that failed must still be retried.
	Pending map[int]time.Time `json:"pending,omitempty"`
	// Settled marks pull requests no pass should consider again because the
	// engine opened them itself. Without it they would consume a slot under the
	// per-pass cap on every pass and starve newer pull requests.
	Settled map[int]time.Time `json:"settled,omitempty"`
}

// LoadState reads tracking state for repo. It always returns ready, non-nil
// state with non-nil maps: a missing file, an unparsable file, or state scoped
// to a different repository all yield fresh state.
func LoadState(path, repo string) *State {
	fresh := &State{
		Repo: repo, Tracked: map[int]Commented{},
		Failures: map[int]attemptFailure{}, Settled: map[int]time.Time{},
		Pending: map[int]time.Time{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fresh // no state yet
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Warning: failed to parse pull request comment state: %v", err)
		return fresh
	}
	if s.Repo != "" && s.Repo != repo {
		log.Printf("pull request comments: target repo changed (%s -> %s); starting state fresh", s.Repo, repo)
		return fresh
	}
	if s.Tracked == nil {
		s.Tracked = map[int]Commented{}
	}
	if s.Failures == nil {
		s.Failures = map[int]attemptFailure{}
	}
	if s.Settled == nil {
		s.Settled = map[int]time.Time{}
	}
	if s.Pending == nil {
		s.Pending = map[int]time.Time{}
	}
	s.Repo = repo
	return &s
}

// Save writes the state atomically.
func (s *State) Save(path string) error {
	if err := statefile.WriteJSON(path, s); err != nil {
		return fmt.Errorf("writing pull request comment state: %w", err)
	}
	return nil
}

// Activated reports whether the watermark has been recorded.
func (s *State) Activated() bool { return s != nil && !s.ActivatedAt.IsZero() }

// Record marks a pull request as commented on.
func (s *State) Record(number int, at time.Time) {
	if s == nil {
		return
	}
	s.Tracked[number] = Commented{Number: number, CommentedAt: at.UTC()}
	delete(s.Failures, number)
	delete(s.Pending, number)
}

// RecordIntent notes that a comment is about to be posted. It is saved before
// the write, so a comment GitHub accepts but never confirms still retains the
// page it links to.
func (s *State) RecordIntent(number int, at time.Time) {
	if s == nil {
		return
	}
	s.Pending[number] = at.UTC()
}

// Recorded reports whether state already tracks a comment for number.
func (s *State) Recorded(number int) bool {
	if s == nil {
		return false
	}
	_, ok := s.Tracked[number]
	return ok
}

// RecordFailure counts one failed post attempt.
func (s *State) RecordFailure(number int, at time.Time) {
	if s == nil {
		return
	}
	entry := s.Failures[number]
	entry.Count++
	entry.LastAt = at.UTC()
	s.Failures[number] = entry
}

// Settle marks a pull request as one the engine opened, so later passes skip it
// before it can consume a slot under the per-pass cap.
func (s *State) Settle(number int, at time.Time) {
	if s == nil {
		return
	}
	s.Settled[number] = at.UTC()
}

// IsSettled reports whether a pull request was already ruled out for good.
func (s *State) IsSettled(number int) bool {
	if s == nil {
		return false
	}
	_, ok := s.Settled[number]
	return ok
}

// Adopt records comments GitHub reports that this state has no record of, which
// happens after the data directory is reset. It restores the retention that
// keeps those pull requests' triage pages alive once they close.
func (s *State) Adopt(numbers map[int]bool, at time.Time) {
	if s == nil {
		return
	}
	for number := range numbers {
		if !s.Recorded(number) {
			s.Tracked[number] = Commented{Number: number, CommentedAt: at.UTC()}
		}
	}
}

// CommentedNumbers returns the pull requests a comment was posted on. Their
// triage pages must survive pruning: the comments are public and permanent, so
// a page removed when the pull request closes leaves a broken link behind.
func (s *State) CommentedNumbers() map[int]bool {
	if s == nil {
		return nil
	}
	out := make(map[int]bool, len(s.Tracked)+len(s.Pending))
	for number := range s.Tracked {
		out[number] = true
	}
	// An attempted write counts too: the comment may exist even though its
	// confirmation was lost.
	for number := range s.Pending {
		out[number] = true
	}
	return out
}

// CommentedNumbersAt reads the commented set for repo straight from disk. The
// output writer needs it before the commenting pass runs.
func CommentedNumbersAt(dataDir, repo string) map[int]bool {
	return LoadState(filepath.Join(dataDir, StateFilename), repo).CommentedNumbers()
}

// Prune drops records older than RetentionWindow, bounding both this file and
// the triage pages it retains.
func (s *State) Prune(now time.Time) {
	if s == nil {
		return
	}
	for number, entry := range s.Tracked {
		if now.Sub(entry.CommentedAt) > RetentionWindow {
			delete(s.Tracked, number)
			delete(s.Failures, number)
		}
	}
	for number, at := range s.Settled {
		if now.Sub(at) > RetentionWindow {
			delete(s.Settled, number)
		}
	}
	for number, entry := range s.Failures {
		if now.Sub(entry.LastAt) > RetentionWindow {
			delete(s.Failures, number)
		}
	}
	for number, at := range s.Pending {
		if now.Sub(at) > RetentionWindow {
			delete(s.Pending, number)
		}
	}
}

// Exhausted reports whether posting has failed too often to keep retrying.
func (s *State) Exhausted(number int) bool {
	if s == nil {
		return false
	}
	return s.Failures[number].Count >= MaxPostAttempts
}
