package devmock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prescalation"
)

// escalations tracks on-demand analyses keyed by subject identity. Both
// escalation kinds share it because their only difference is the reference
// type: a start records a completion deadline and a read derives the state from
// the clock, so a poll never depends on a background goroutine.
type escalations[R comparable] struct {
	opts Options

	mu      sync.Mutex
	records map[R]*escalationRecord[R]
}

type escalationRecord[R any] struct {
	view       prescalation.View[R]
	completeAt time.Time
	requestIDs map[string]bool
}

func newEscalations[R comparable](opts Options) *escalations[R] {
	return &escalations[R]{opts: opts, records: map[R]*escalationRecord[R]{}}
}

// start admits one analysis, replaying an existing record when the same request
// id comes back so a retried POST does not restart the work.
func (e *escalations[R]) start(ref R, requestID string, resolve func(R) prescalation.View[R]) (prescalation.View[R], error) {
	if strings.TrimSpace(requestID) == "" {
		return prescalation.View[R]{}, prescalation.ErrInvalid
	}
	now := e.opts.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if record, ok := e.records[ref]; ok {
		record.requestIDs[requestID] = true
		return e.viewLocked(record, now, resolve), nil
	}
	record := &escalationRecord[R]{
		view: prescalation.View[R]{
			Ref: ref, State: prescalation.StateQueued, StartedAt: now,
		},
		completeAt: now.Add(e.opts.latency()),
		requestIDs: map[string]bool{requestID: true},
	}
	e.records[ref] = record
	return e.viewLocked(record, now, resolve), nil
}

// get reports the current state of one analysis. A subject nothing has been
// started for is not an error: it reports the not-started state the UI renders
// its start control from.
func (e *escalations[R]) get(ref R, resolve func(R) prescalation.View[R]) (prescalation.View[R], error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	record, ok := e.records[ref]
	if !ok {
		return prescalation.View[R]{Ref: ref, State: prescalation.StateNotStarted}, nil
	}
	return e.viewLocked(record, e.opts.now(), resolve), nil
}

// viewLocked advances the record to the state its deadline implies.
func (e *escalations[R]) viewLocked(record *escalationRecord[R], now time.Time, resolve func(R) prescalation.View[R]) prescalation.View[R] {
	if record.view.State == prescalation.StateComplete {
		return record.view
	}
	if now.Before(record.completeAt) {
		record.view.State = prescalation.StateRunning
		if now.Before(record.completeAt.Add(-e.opts.latency() / 2)) {
			record.view.State = prescalation.StateQueued
		}
		return record.view
	}
	completed := resolve(record.view.Ref)
	completed.Ref = record.view.Ref
	completed.State = prescalation.StateComplete
	completed.StartedAt = record.view.StartedAt
	completed.CompletedAt = now
	record.view = completed
	return record.view
}

// PullRequestEscalation fabricates on-demand analysis of one failing pull
// request check.
type PullRequestEscalation struct {
	opts   Options
	shared *escalations[prescalation.Ref]
}

func newPullRequestEscalation(opts Options) *PullRequestEscalation {
	return &PullRequestEscalation{opts: opts, shared: newEscalations[prescalation.Ref](opts)}
}

// Start admits one pull request escalation.
func (p *PullRequestEscalation) Start(_ context.Context, ref prescalation.Ref, _, requestID string) (prescalation.PullRequestView, error) {
	normalized, err := normalizePullRequestRef(ref)
	if err != nil {
		return prescalation.PullRequestView{}, err
	}
	return p.shared.start(normalized, requestID, p.resolve)
}

// Get reports one pull request escalation.
func (p *PullRequestEscalation) Get(ref prescalation.Ref) (prescalation.PullRequestView, error) {
	normalized, err := normalizePullRequestRef(ref)
	if err != nil {
		return prescalation.PullRequestView{}, err
	}
	return p.shared.get(normalized, p.resolve)
}

func (p *PullRequestEscalation) resolve(ref prescalation.Ref) prescalation.PullRequestView {
	analysis := lookupAnalysis(p.opts.DataDir, ref.JobID, ref.BuildID, ref.TestName)
	return prescalation.PullRequestView{
		RootCause: fmt.Sprintf(
			"Mock escalation for pull request %d. %s", ref.PullNumber, rootCauseOrPlaceholder(analysis)),
		Severity:     "medium",
		SuggestedFix: "Re-run the check, then compare the failing step against the pull request's changed files.",
		Citations:    escalationCitations(analysis),
	}
}

func normalizePullRequestRef(ref prescalation.Ref) (prescalation.Ref, error) {
	ref.JobID = strings.TrimSpace(ref.JobID)
	ref.BuildID = strings.TrimSpace(ref.BuildID)
	ref.TestName = strings.TrimSpace(ref.TestName)
	if ref.PullNumber <= 0 || ref.JobID == "" || ref.BuildID == "" || ref.TestName == "" {
		return prescalation.Ref{}, prescalation.ErrInvalid
	}
	return ref, nil
}

// SharedFailureEscalation fabricates on-demand analysis of a failure observed
// across several open pull requests.
type SharedFailureEscalation struct {
	opts   Options
	shared *escalations[prescalation.ClusterRef]
}

func newSharedFailureEscalation(opts Options) *SharedFailureEscalation {
	return &SharedFailureEscalation{opts: opts, shared: newEscalations[prescalation.ClusterRef](opts)}
}

// Start admits one shared failure escalation.
func (s *SharedFailureEscalation) Start(_ context.Context, ref prescalation.ClusterRef, _, requestID string) (prescalation.ClusterView, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if ref.ID == "" {
		return prescalation.ClusterView{}, prescalation.ErrInvalid
	}
	return s.shared.start(ref, requestID, s.resolve)
}

// Get reports one shared failure escalation.
func (s *SharedFailureEscalation) Get(ref prescalation.ClusterRef) (prescalation.ClusterView, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if ref.ID == "" {
		return prescalation.ClusterView{}, prescalation.ErrInvalid
	}
	return s.shared.get(ref, s.resolve)
}

func (s *SharedFailureEscalation) resolve(prescalation.ClusterRef) prescalation.ClusterView {
	return prescalation.ClusterView{
		RootCause: "Mock escalation for a failure shared across several pull requests. " +
			"No single pull request accounts for it, so it is attributed to the shared infrastructure.",
		Severity:     "high",
		SuggestedFix: "Check whether the shared dependency the failing step installs changed recently.",
	}
}

// escalationCitations names the evidence a completed escalation reports.
func escalationCitations(analysis publishedAnalysis) []models.EvidenceCitation {
	paths := citedPaths(analysis.FileLinks)
	if len(paths) == 0 {
		return nil
	}
	citations := make([]models.EvidenceCitation, 0, len(paths))
	for _, path := range paths {
		citations = append(citations, models.EvidenceCitation{
			Path: path, LineStart: 1, LineEnd: 20, Quote: "mock excerpt from " + path,
		})
	}
	return citations
}
