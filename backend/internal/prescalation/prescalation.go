// Package prescalation runs on-demand AI analysis for a single pull request
// test failure that the deterministic pass could not explain.
//
// Escalation is admin-initiated, one at a time, and bounded. Admission is
// reserved before any work happens, so the number of requests that reach the
// artifact bucket and GitHub is capped by the queue bound rather than by how
// many admins clicked. It exists only for the residual set: failures the base
// branch, other pull requests, and flakiness history all failed to account for.
// The analysis it runs is the ordinary agentic failure analysis under a
// separate module, so it is gated by the same critique and judge rules as every
// other analysis.
package prescalation

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	// ErrBusy marks a rejected start because the escalation queue is full.
	ErrBusy = errors.New("too many escalations are already in progress")
)

// maxIdempotencyKeysPerRecord bounds replay keys retained per retained record.
const maxIdempotencyKeysPerRecord = 8

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
	// Timeout bounds one escalation's whole accepted lifetime: resolution,
	// queue time, and the analysis itself.
	Timeout time.Duration
	// MaxQueued bounds accepted-but-unfinished escalations, including the one
	// running. A start past the bound is rejected with ErrBusy instead of
	// queueing, which caps the artifact and GitHub work escalation can trigger.
	MaxQueued int
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
	if o.MaxQueued <= 0 {
		o.MaxQueued = 4
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
//
// Records are keyed by failure, not by requester, so every admin sees the same
// escalation for the same failure. That is deliberate: escalation is the only
// part of the feature that spends tokens, and per-admin isolation would let
// several maintainers each pay for the same analysis. The owner is retained
// only to scope replay keys.
type Service struct {
	ctx      context.Context
	resolver Resolver
	runner   Runner
	opts     Options
	// active is a single global slot, so escalation can never fan out into
	// concurrent model traffic no matter how many admins click.
	active chan struct{}
	// admitted bounds accepted work. A token is taken before resolution, so
	// the bucket reads and GitHub calls one escalation needs are capped the
	// same way its model traffic is.
	admitted chan struct{}

	mu          sync.Mutex
	records     map[string]*record
	idempotency map[string]string
	// saveMu serializes persistence so concurrent finishes cannot write
	// snapshots out of order.
	saveMu sync.Mutex
	wg     sync.WaitGroup
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
		admitted:    make(chan struct{}, opts.MaxQueued),
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
	replay := false
	if previous, ok := s.idempotency[idempotencyKey]; ok {
		if previous != identity {
			s.mu.Unlock()
			return View{}, ErrIdempotencyConflict
		}
		// The same key for the same subject is a replay of one request, so it
		// returns the subject's current state rather than starting new work.
		replay = true
	}
	if existing := s.records[identity]; existing != nil && (replay || !retryable(existing)) {
		s.idempotency[idempotencyKey] = identity
		s.pruneIdempotencyLocked()
		view := existing.view
		s.mu.Unlock()
		return view, nil
	}
	s.mu.Unlock()

	// Admission is reserved before resolution, not after it. Resolution reads
	// the artifact bucket and GitHub, so admitting afterwards would let every
	// concurrent request pay that cost and only then queue.
	select {
	case s.admitted <- struct{}{}:
	default:
		return View{}, ErrBusy
	}
	// The wait group covers the whole accepted lifetime, resolution included,
	// so a shutdown drain does not miss work that has not reached its goroutine
	// yet. It is released exactly once, by whoever ends up owning the token.
	s.wg.Add(1)
	// One deadline spans resolution, queue time, and the analysis, so the whole
	// accepted lifetime is bounded rather than just the part after the slot is
	// won. Rooting it in the service context means a shutdown also cancels work
	// that has not started running yet.
	lifetime, endLifetime := context.WithTimeout(s.ctx, s.opts.Timeout)
	release := func() {
		endLifetime()
		<-s.admitted
		s.wg.Done()
	}

	// Resolution happens outside the lock because it reads published state and
	// GitHub. It also rejects failures the deterministic pass already explained.
	// It observes the caller's context too, so a client that goes away stops the
	// bucket and GitHub work it asked for.
	resolveCtx, cancelResolve := context.WithCancel(lifetime)
	stopPropagation := context.AfterFunc(ctx, cancelResolve)
	resolved, err := s.resolver.Resolve(resolveCtx, ref)
	stopPropagation()
	cancelResolve()
	// A resolver can report success from partial reads: a missing finished.json
	// reads as a pending build, and the changed-file lister is optional. So the
	// budget and the caller are checked directly rather than trusted to surface
	// as an error, or a cut-off resolution would launch an analysis. They are
	// checked first because an expired budget is the operative fact, and
	// because they carry the cause the operator needs.
	switch {
	case lifetime.Err() != nil:
		release()
		return View{}, lifetime.Err()
	case ctx.Err() != nil:
		release()
		return View{}, ctx.Err()
	case err != nil:
		release()
		return View{}, err
	}

	s.mu.Lock()
	if existing := s.records[identity]; existing != nil && !retryable(existing) {
		s.idempotency[idempotencyKey] = identity
		s.pruneIdempotencyLocked()
		view := existing.view
		s.mu.Unlock()
		release()
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
	s.pruneIdempotencyLocked()
	view := rec.view
	s.mu.Unlock()

	go s.run(lifetime, rec, resolved, release)
	return view, nil
}

// retryable reports whether a finished record may be started again. A failure
// is a transient outcome, not a durable fact: a provider error, a timeout, or a
// shutdown that interrupted queued work must not pin a subject forever.
func retryable(rec *record) bool {
	return !rec.running && rec.view.State == StateFailed
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

// Wait blocks until every admitted escalation has released its slot, or until
// ctx is done. Admission covers resolution too, so a shutdown drain does not
// race a request that has not started running yet, and every escalation that
// reached a record has persisted its outcome by the time Wait returns.
func (s *Service) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) run(ctx context.Context, rec *record, resolved Resolved, release func()) {
	// The admission token, its wait-group entry, and the accepted lifetime are
	// held until the escalation has persisted its outcome, so a queue slot
	// frees only when the work it stands for is done and a drain cannot end
	// early.
	defer release()

	// Waiting for the single slot keeps queued work visible rather than
	// rejecting it, while still admitting exactly one analysis at a time. A
	// request that never reaches the slot within its budget finishes as failed,
	// which leaves it retryable and prunable.
	select {
	case s.active <- struct{}{}:
	case <-ctx.Done():
		s.finish(rec, View{Ref: rec.view.Ref, State: StateFailed, Error: safeError(ctx.Err())})
		return
	}
	defer func() { <-s.active }()

	s.mu.Lock()
	rec.view.State = StateRunning
	rec.updatedAt = s.opts.Now().UTC()
	s.mu.Unlock()

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
	s.mu.Unlock()

	s.persist()
}

// persist writes the current results. Snapshot and write are serialized
// together: the cancellation path in run finishes without holding the active
// slot, so several goroutines can persist at once, and taking the snapshot
// outside this lock would let an older snapshot overwrite a newer one.
func (s *Service) persist() {
	store := s.opts.Store
	if store == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	snapshot := s.snapshotLocked()
	s.mu.Unlock()
	// A persistence failure must not lose the in-memory result, but it must be
	// visible: the results silently stop surviving restarts otherwise.
	if err := store.Save(snapshot); err != nil {
		log.Printf("⚠ Persisting pull request escalation results failed: %v", err)
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

// pruneIdempotencyLocked bounds the replay index. Its entries outlive the
// records they point at, and a client controls the key, so repeated requests
// for one retained subject would otherwise grow the map without limit.
func (s *Service) pruneIdempotencyLocked() {
	limit := s.opts.MaxRecords * maxIdempotencyKeysPerRecord
	if len(s.idempotency) <= limit {
		return
	}
	// Entries whose record is already gone can never serve a replay.
	for key, identity := range s.idempotency {
		if _, ok := s.records[identity]; !ok {
			delete(s.idempotency, key)
		}
	}
	// Dropping a live entry only costs replay protection for that one key; the
	// record itself still answers the request.
	for key := range s.idempotency {
		if len(s.idempotency) <= limit {
			break
		}
		delete(s.idempotency, key)
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
