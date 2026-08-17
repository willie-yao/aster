// Package prescalation runs on-demand AI analysis for a single pull request
// test failure that the deterministic pass could not explain.
//
// Escalation is admin-initiated, one at a time, and bounded. It exists only for
// the residual set: failures the base branch, other pull requests, and
// flakiness history all failed to account for. The analysis it runs is the
// ordinary agentic failure analysis under a separate module, so it is gated by
// the same critique and judge rules as every other analysis.
package prescalation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

// Escalation states.
const (
	StateQueued     = "queued"
	StateRunning    = "running"
	StateComplete   = "complete"
	StateFailed     = "failed"
	StateNotStarted = "not_started"
)

var (
	// ErrInvalid marks a malformed request.
	ErrInvalid = errors.New("escalation request is invalid")
	// ErrNotEligible marks a failure the deterministic pass already explained.
	ErrNotEligible = errors.New("failure is already explained without analysis")
	// ErrUnavailable marks a dependency that could not serve the request.
	ErrUnavailable = errors.New("escalation is unavailable")
	// ErrIdempotencyConflict marks a reused request ID for a different subject.
	ErrIdempotencyConflict = errors.New("request id was already used for another failure")
	// ErrBusy marks a rejected start because another escalation is running.
	ErrBusy = errors.New("another escalation is already running")
)

// Ref identifies one failing test on one pull request check.
type Ref struct {
	PullNumber int    `json:"pull_number"`
	JobID      string `json:"job_id"`
	BuildID    string `json:"build_id"`
	TestName   string `json:"test_name"`
}

func (r Ref) normalized() (Ref, error) {
	r.JobID = strings.TrimSpace(r.JobID)
	r.BuildID = strings.TrimSpace(r.BuildID)
	r.TestName = strings.TrimSpace(r.TestName)
	if r.PullNumber <= 0 || r.JobID == "" || r.BuildID == "" || r.TestName == "" {
		return Ref{}, ErrInvalid
	}
	if len(r.JobID) > 512 || len(r.BuildID) > 128 || len(r.TestName) > 1024 {
		return Ref{}, ErrInvalid
	}
	return r, nil
}

// identity is the stable key for one subject.
func (r Ref) identity() string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s", r.PullNumber, r.JobID, r.BuildID, r.TestName)
}

// View is the public state of one escalation.
type View struct {
	Ref   Ref    `json:"ref"`
	State string `json:"state"`
	// RootCause, Severity, and Citations mirror the published analysis fields.
	RootCause    string                    `json:"root_cause,omitempty"`
	Severity     string                    `json:"severity,omitempty"`
	SuggestedFix string                    `json:"suggested_fix,omitempty"`
	Citations    []models.EvidenceCitation `json:"citations,omitempty"`
	// Error is a safe message when State is failed.
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// Resolver turns a Ref into the inputs one analysis needs, and reports whether
// the failure is eligible for escalation.
type Resolver interface {
	Resolve(context.Context, Ref) (Resolved, error)
}

// Runner performs one analysis for a resolved subject.
type Runner interface {
	Run(context.Context, Resolved) (View, error)
}

// Options bound the service.
type Options struct {
	// Timeout bounds one escalation.
	Timeout time.Duration
	// MaxRecords bounds retained results before the oldest are pruned.
	MaxRecords int
	// Now is the clock, for tests.
	Now func() time.Time
	// Store persists completed results so they survive a restart. Optional.
	Store Store
}

func (o Options) normalized() Options {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxRecords <= 0 {
		o.MaxRecords = 128
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Store persists completed escalations across restarts.
type Store interface {
	Load() (map[string]View, error)
	Save(map[string]View) error
}

type record struct {
	view      View
	running   bool
	updatedAt time.Time
}

// Service runs one escalation at a time and remembers recent results.
type Service struct {
	ctx      context.Context
	resolver Resolver
	runner   Runner
	opts     Options
	// active is a single global slot, so escalation can never fan out into
	// concurrent model traffic no matter how many admins click.
	active chan struct{}

	mu          sync.Mutex
	records     map[string]*record
	idempotency map[string]string
	wg          sync.WaitGroup
}

// New constructs the service and restores any persisted results.
func New(ctx context.Context, resolver Resolver, runner Runner, opts Options) (*Service, error) {
	if ctx == nil || resolver == nil || runner == nil {
		return nil, fmt.Errorf("prescalation: ctx, resolver, and runner are required")
	}
	opts = opts.normalized()
	s := &Service{
		ctx: ctx, resolver: resolver, runner: runner, opts: opts,
		active:      make(chan struct{}, 1),
		records:     map[string]*record{},
		idempotency: map[string]string{},
	}
	if opts.Store != nil {
		restored, err := opts.Store.Load()
		if err != nil {
			return nil, fmt.Errorf("prescalation: restoring results: %w", err)
		}
		for identity, view := range restored {
			// An escalation that was running when the process stopped did not
			// finish, so it is restored as never started rather than as
			// perpetually running.
			if view.State == StateQueued || view.State == StateRunning {
				continue
			}
			s.records[identity] = &record{view: view, updatedAt: opts.Now().UTC()}
		}
	}
	return s, nil
}

// Start begins one escalation, or returns the existing state for a subject that
// is already running or already complete.
func (s *Service) Start(ctx context.Context, ref Ref, owner, requestID string) (View, error) {
	ref, err := ref.normalized()
	if err != nil {
		return View{}, err
	}
	owner, requestID = strings.TrimSpace(owner), strings.TrimSpace(requestID)
	if owner == "" || requestID == "" || len(owner) > 256 || len(requestID) > 256 {
		return View{}, ErrInvalid
	}
	identity := ref.identity()
	idempotencyKey := owner + "\x00" + requestID

	s.mu.Lock()
	if previous, ok := s.idempotency[idempotencyKey]; ok && previous != identity {
		s.mu.Unlock()
		return View{}, ErrIdempotencyConflict
	}
	if existing := s.records[identity]; existing != nil {
		s.idempotency[idempotencyKey] = identity
		view := existing.view
		s.mu.Unlock()
		return view, nil
	}
	s.mu.Unlock()

	// Resolution happens outside the lock because it reads published state and
	// GitHub. It also rejects failures the deterministic pass already explained.
	resolved, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return View{}, err
	}

	s.mu.Lock()
	if existing := s.records[identity]; existing != nil {
		s.idempotency[idempotencyKey] = identity
		view := existing.view
		s.mu.Unlock()
		return view, nil
	}
	now := s.opts.Now().UTC()
	rec := &record{
		view:      View{Ref: ref, State: StateQueued, StartedAt: now},
		running:   true,
		updatedAt: now,
	}
	s.records[identity] = rec
	s.idempotency[idempotencyKey] = identity
	s.pruneLocked()
	view := rec.view
	s.wg.Add(1)
	s.mu.Unlock()

	go s.run(rec, resolved)
	return view, nil
}

// Get returns the current state for a subject.
func (s *Service) Get(ref Ref) (View, error) {
	ref, err := ref.normalized()
	if err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.records[ref.identity()]; rec != nil {
		return rec.view, nil
	}
	return View{Ref: ref, State: StateNotStarted}, nil
}

// Wait blocks until in-flight escalations stop.
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) run(rec *record, resolved Resolved) {
	defer s.wg.Done()

	// Waiting for the single slot keeps queued work visible rather than
	// rejecting it, while still admitting exactly one analysis at a time.
	select {
	case s.active <- struct{}{}:
	case <-s.ctx.Done():
		s.finish(rec, View{Ref: rec.view.Ref, State: StateFailed, Error: "escalation was cancelled"})
		return
	}
	defer func() { <-s.active }()

	s.mu.Lock()
	rec.view.State = StateRunning
	rec.updatedAt = s.opts.Now().UTC()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Timeout)
	defer cancel()
	view, err := s.runner.Run(ctx, resolved)
	if err != nil {
		s.finish(rec, View{Ref: rec.view.Ref, State: StateFailed, Error: safeError(err)})
		return
	}
	view.Ref = rec.view.Ref
	if view.State == "" {
		view.State = StateComplete
	}
	s.finish(rec, view)
}

func (s *Service) finish(rec *record, view View) {
	now := s.opts.Now().UTC()
	s.mu.Lock()
	view.StartedAt = rec.view.StartedAt
	view.CompletedAt = now
	rec.view = view
	rec.running = false
	rec.updatedAt = now
	snapshot := s.snapshotLocked()
	store := s.opts.Store
	s.mu.Unlock()

	if store != nil {
		// A persistence failure must not lose the in-memory result.
		_ = store.Save(snapshot)
	}
}

// snapshotLocked copies completed results for persistence.
func (s *Service) snapshotLocked() map[string]View {
	out := make(map[string]View, len(s.records))
	for identity, rec := range s.records {
		if !rec.running {
			out[identity] = rec.view
		}
	}
	return out
}

// pruneLocked drops the oldest finished records past the retention bound.
func (s *Service) pruneLocked() {
	if len(s.records) <= s.opts.MaxRecords {
		return
	}
	var oldestKey string
	var oldest time.Time
	for identity, rec := range s.records {
		if rec.running {
			continue
		}
		if oldestKey == "" || rec.updatedAt.Before(oldest) {
			oldestKey, oldest = identity, rec.updatedAt
		}
	}
	if oldestKey != "" {
		delete(s.records, oldestKey)
		for key, identity := range s.idempotency {
			if identity == oldestKey {
				delete(s.idempotency, key)
			}
		}
	}
}

// safeError keeps provider and path detail out of an operator-visible message.
func safeError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "escalation timed out"
	case errors.Is(err, context.Canceled):
		return "escalation was cancelled"
	default:
		return "escalation could not complete"
	}
}
