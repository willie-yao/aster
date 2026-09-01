// Package prescalation runs on-demand AI analysis for a single pull request
// test failure that the deterministic pass could not explain.
//
// Escalation is admin-initiated, one at a time, and bounded. Admission is
// reserved before any work happens, so the number of requests that reach the
// artifact bucket and GitHub is capped by the queue bound rather than by how
// many admins clicked. It exists only for the residual set: failures the base
// branch, other pull requests, and flakiness history all failed to account for.
// The analysis it runs is the ordinary agentic failure analysis under a
// separate module, so it is gated by the same deterministic critique rules as every
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

// maxGetRevalidations bounds how many times one status read will follow a
// record replaced while its evidence was being looked up. Replacement requires
// a concurrent start, so this is never reached in practice; it exists so the
// read cannot spin.
const maxGetRevalidations = 3

// Ref identifies one failing test on one pull request check.
type Ref struct {
	PullNumber int    `json:"pull_number"`
	JobID      string `json:"job_id"`
	BuildID    string `json:"build_id"`
	TestName   string `json:"test_name"`
}

// subject constrains a reference that can key one escalation record. The
// service is generic over it so a pull request failure and a failure shared
// across several pull requests share one admission, single-flight, retry, and
// persistence implementation instead of two copies of it.
type subject[T any] interface {
	normalized() (T, error)
	identity() string
}

// evidenced is implemented by resolved work whose analysis describes a build
// the service chose rather than one the reference names. A pull request
// reference names its own build, so its record can never describe anything
// else. A shared failure keeps one identity while the build under it moves on,
// so its finished result has to be revalidated against the build a new request
// would read, or the same test failing again months later for a different
// reason would be answered with the old analysis forever.
type evidenced interface {
	evidence() EscalationEvidence
}

// revalidates reports whether this kind's finished results can go stale.
func revalidates[W any]() bool {
	var zero W
	_, ok := any(zero).(evidenced)
	return ok
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

// EscalationEvidence names the build whose artifacts one analysis read. It is
// recorded for a subject whose evidence build is chosen rather than named by
// the request, so a stored result still says which build it describes after the
// choice would have landed on a different one.
type EscalationEvidence struct {
	// Repo and PullNumber accompany BuildID because a build is only addressed
	// by all three: a build id is unique within one repository's pull request,
	// not across a data directory that outlives a change of project.
	Repo       string `json:"repo,omitempty"`
	PullNumber int    `json:"pull_number,omitempty"`
	BuildID    string `json:"build_id,omitempty"`
}

// sameBuild reports whether two evidence records name the same build.
func (e EscalationEvidence) sameBuild(other EscalationEvidence) bool {
	return e.Repo == other.Repo && e.PullNumber == other.PullNumber && e.BuildID == other.BuildID
}

// View is the public state of one escalation.
type View[R any] struct {
	Ref   R      `json:"ref"`
	State string `json:"state"`
	// RootCause, Severity, and Citations mirror the published analysis fields.
	RootCause    string                    `json:"root_cause,omitempty"`
	Severity     string                    `json:"severity,omitempty"`
	SuggestedFix string                    `json:"suggested_fix,omitempty"`
	Citations    []models.EvidenceCitation `json:"citations,omitempty"`
	// Evidence names the build the analysis read, when the service chose it.
	Evidence *EscalationEvidence `json:"evidence,omitempty"`
	// Error is a safe message when State is failed.
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// PullRequestView is one pull request escalation's public state.
type PullRequestView = View[Ref]

// Resolver turns a Ref into the inputs one analysis needs, and reports whether
// the failure is eligible for escalation. The resolved work is opaque to the
// service, which only carries it from the resolver to the matching runner.
type Resolver[R, W any] interface {
	Resolve(context.Context, R) (W, error)
}

// Runner performs one analysis for a resolved subject.
type Runner[R, W any] interface {
	Run(context.Context, W) (View[R], error)
}

// Gate bounds concurrent analyses. Services can share one, so a server runs a
// single analysis at a time no matter how many escalation kinds it offers.
// Without sharing, each kind would carry its own slot and their model traffic
// would add up.
type Gate chan struct{}

// NewGate builds a gate admitting n analyses at once.
func NewGate(n int) Gate {
	if n <= 0 {
		n = 1
	}
	return make(Gate, n)
}

// Options bound the service.
type Options[R any] struct {
	// Timeout bounds one escalation's whole accepted lifetime: resolution,
	// queue time, and the analysis itself.
	Timeout time.Duration
	// MaxQueued bounds accepted-but-unfinished escalations, including the one
	// running. A start past the bound is rejected with ErrBusy instead of
	// queueing, which caps the artifact and GitHub work escalation can trigger.
	MaxQueued int
	// MaxRecords bounds retained results before the oldest are pruned. It is
	// raised to MaxQueued when configured lower, since a running record cannot
	// be pruned.
	MaxRecords int
	// Now is the clock, for tests.
	Now func() time.Time
	// Store persists completed results so they survive a restart. Optional.
	Store Store[R]
	// CurrentEvidence reports the build a new request for this subject would
	// read. A finished result is only reached through Start, which revalidates
	// it, so without this a status read would keep serving an analysis of
	// artifacts that no longer represent the failure and the caller would never
	// be offered a way to ask again. It must not perform expensive work: status
	// is polled. Nil disables the check.
	CurrentEvidence func(R) (EscalationEvidence, bool)
	// Gate bounds concurrent analyses. Nil gives this service its own single
	// slot; pass one shared gate to bound several services together.
	Gate Gate
}

func (o Options[R]) normalized() Options[R] {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxQueued <= 0 {
		o.MaxQueued = 4
	}
	if o.MaxRecords <= 0 {
		o.MaxRecords = 128
	}
	// A record cannot be pruned while it runs, so retention tighter than the
	// queue would evict results the moment they land, losing analyses that
	// were already paid for.
	if o.MaxRecords < o.MaxQueued {
		o.MaxRecords = o.MaxQueued
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Gate == nil {
		o.Gate = NewGate(1)
	}
	return o
}

// Store persists completed escalations across restarts.
type Store[R any] interface {
	Load() (map[string]View[R], error)
	Save(map[string]View[R]) error
}

type record[R any] struct {
	view      View[R]
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
type Service[R subject[R], W any] struct {
	ctx      context.Context
	resolver Resolver[R, W]
	runner   Runner[R, W]
	opts     Options[R]
	// active is the analysis slot, shared with any other service given the
	// same gate, so escalation can never fan out into concurrent model traffic
	// no matter how many admins click or how many escalation kinds exist.
	active Gate
	// admitted bounds accepted work. A token is taken before resolution, so
	// the bucket reads and GitHub calls one escalation needs are capped the
	// same way its model traffic is. It doubles as the drain: holding every
	// token means nothing is in flight and nothing more can start.
	admitted chan struct{}

	// revalidates is fixed by the work type: it reports whether a finished
	// result has to be checked against current evidence before it is reused.
	revalidates bool

	mu          sync.Mutex
	records     map[string]*record[R]
	idempotency map[string]string
	// saveMu serializes persistence so concurrent finishes cannot write
	// snapshots out of order.
	saveMu sync.Mutex
	// drainMu serializes Wait, which claims the whole queue.
	drainMu sync.Mutex
	drained bool
}

// New constructs the service and restores any persisted results.
func New[R subject[R], W any](ctx context.Context, resolver Resolver[R, W], runner Runner[R, W], opts Options[R]) (*Service[R, W], error) {
	if ctx == nil || resolver == nil || runner == nil {
		return nil, fmt.Errorf("prescalation: ctx, resolver, and runner are required")
	}
	// A kind whose finished results can go stale needs a way to notice from a
	// status read, or its results are only revalidated by a request no caller
	// has a reason to make, and it strands them exactly as if the check did not
	// exist. That is a wiring mistake, so it fails at construction.
	if revalidates[W]() && opts.CurrentEvidence == nil {
		return nil, fmt.Errorf("prescalation: this subject's evidence can move, so CurrentEvidence is required")
	}
	opts = opts.normalized()
	s := &Service[R, W]{
		ctx: ctx, resolver: resolver, runner: runner, opts: opts,
		revalidates: revalidates[W](),
		active:      opts.Gate,
		admitted:    make(chan struct{}, opts.MaxQueued),
		records:     map[string]*record[R]{},
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
			// Completion time orders the restored set, so pruning it drops the
			// genuinely oldest results rather than an arbitrary one. A record
			// from an older state file may carry neither timestamp; unknown age
			// sorts oldest so results with trustworthy times are kept first.
			updatedAt := view.CompletedAt
			if updatedAt.IsZero() {
				updatedAt = view.StartedAt
			}
			s.records[identity] = &record[R]{view: view, updatedAt: updatedAt}
		}
		// A store written under a larger bound holds more results than are
		// retained now, and nothing else would bring it back down until the
		// next escalation runs.
		s.mu.Lock()
		oversized := len(s.records) > opts.MaxRecords
		s.pruneLocked()
		s.mu.Unlock()
		if oversized {
			s.persist()
		}
	}
	return s, nil
}

// Start begins one escalation, or returns the existing state for a subject that
// is already running or already complete.
func (s *Service[R, W]) Start(ctx context.Context, ref R, owner, requestID string) (View[R], error) {
	ref, err := ref.normalized()
	if err != nil {
		return View[R]{}, err
	}
	owner, requestID = strings.TrimSpace(owner), strings.TrimSpace(requestID)
	if owner == "" || requestID == "" || len(owner) > 256 || len(requestID) > 256 {
		return View[R]{}, ErrInvalid
	}
	identity := ref.identity()
	idempotencyKey := owner + "\x00" + requestID

	s.mu.Lock()
	replay := false
	if previous, ok := s.idempotency[idempotencyKey]; ok {
		if previous != identity {
			s.mu.Unlock()
			return View[R]{}, ErrIdempotencyConflict
		}
		// The same key for the same subject is a replay of one request, so it
		// returns the subject's current state rather than starting new work.
		replay = true
	}
	// A replay and work already in flight are answered without resolving. A
	// finished result is only answered here when it cannot drift; otherwise the
	// request resolves first so the result can be checked against the build it
	// would read now.
	if existing := s.records[identity]; existing != nil &&
		(replay || existing.running || (!retryable(existing) && !s.revalidates)) {
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
		return View[R]{}, ErrBusy
	}
	// The token is released only once the work it stands for has finished, so
	// holding it is what makes a drain meaningful. It is released exactly once,
	// by whoever ends up owning it.
	// One deadline spans resolution, queue time, and the analysis, so the whole
	// accepted lifetime is bounded rather than just the part after the slot is
	// won. Rooting it in the service context means a shutdown also cancels work
	// that has not started running yet.
	lifetime, endLifetime := context.WithTimeout(s.ctx, s.opts.Timeout)
	release := func() {
		endLifetime()
		<-s.admitted
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
	// A resolver can report success from partial reads: the changed-file lister
	// is optional, so a cancelled listing is discarded. So the budget and the
	// caller are checked directly rather than trusted to surface as an error,
	// or a cut-off resolution would launch an analysis. They are checked first
	// because an expired budget is the operative fact, and because they carry
	// the cause the operator needs.
	switch {
	case lifetime.Err() != nil:
		release()
		return View[R]{}, lifetime.Err()
	case ctx.Err() != nil:
		release()
		return View[R]{}, ctx.Err()
	case err != nil:
		release()
		return View[R]{}, err
	}

	s.mu.Lock()
	if existing := s.records[identity]; existing != nil && !retryable(existing) && reusable(existing, resolved) {
		s.idempotency[idempotencyKey] = identity
		s.pruneIdempotencyLocked()
		view := existing.view
		s.mu.Unlock()
		release()
		return view, nil
	}
	now := s.opts.Now().UTC()
	rec := &record[R]{
		view:      View[R]{Ref: ref, State: StateQueued, StartedAt: now},
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

// reusable reports whether an existing record answers this request. Work still
// in flight always does. A finished result does only while it still describes
// the build a new request would read, so a shared failure is analyzed again
// once its evidence build moves on.
func reusable[R any, W any](rec *record[R], work W) bool {
	if rec.running {
		return true
	}
	ev, ok := any(work).(evidenced)
	if !ok {
		return true
	}
	return rec.view.Evidence != nil && rec.view.Evidence.sameBuild(ev.evidence())
}

// retryable reports whether a finished record may be started again. A failure
// is a transient outcome, not a durable fact: a provider error, a timeout, or a
// shutdown that interrupted queued work must not pin a subject forever.
func retryable[R any](rec *record[R]) bool {
	return !rec.running && rec.view.State == StateFailed
}

// Get returns the current state for a subject.
func (s *Service[R, W]) Get(ref R) (View[R], error) {
	ref, err := ref.normalized()
	if err != nil {
		return View[R]{}, err
	}
	identity := ref.identity()
	// The evidence lookup runs without the lock, so a start can replace the
	// record underneath it. Each pass validates the record it actually read: a
	// replacement is re-validated rather than trusted, because a completed
	// result is terminal for the caller and would never be checked again.
	for attempt := 0; attempt < maxGetRevalidations; attempt++ {
		s.mu.Lock()
		rec := s.records[identity]
		if rec == nil {
			s.mu.Unlock()
			return View[R]{Ref: ref, State: StateNotStarted}, nil
		}
		view, running := rec.view, rec.running
		s.mu.Unlock()

		// Work in flight is current by construction, and a result describing
		// the build a new request would read still answers.
		if running || !s.staleEvidence(ref, view) {
			return view, nil
		}

		s.mu.Lock()
		unchanged := s.records[identity] == rec
		s.mu.Unlock()
		// The stale result is still the stored one, so it is reported as never
		// started, which is what offers the caller a fresh analysis. The record
		// stays until that analysis replaces it.
		if unchanged {
			return View[R]{Ref: ref, State: StateNotStarted}, nil
		}
	}
	// Starts kept landing faster than status could be validated. Offering a
	// fresh analysis is the safe answer, because the alternative is serving a
	// result nothing has established is current.
	return View[R]{Ref: ref, State: StateNotStarted}, nil
}

// staleEvidence reports whether a finished result describes a build that is no
// longer the one a new request would read. Only a kind whose evidence build the
// service chooses can go stale, and only a completed result carries evidence to
// compare against.
func (s *Service[R, W]) staleEvidence(ref R, view View[R]) bool {
	if !s.revalidates || s.opts.CurrentEvidence == nil {
		return false
	}
	if view.State != StateComplete || view.Evidence == nil {
		return false
	}
	current, ok := s.opts.CurrentEvidence(ref)
	// Without a current build nothing establishes that the result is stale, so
	// it keeps answering rather than disappearing on a transient read failure.
	if !ok {
		return false
	}
	return !current.sameBuild(*view.Evidence)
}

// Wait blocks until every admitted escalation has released its slot, or until
// ctx is done. On success it keeps those slots, so a drained service accepts no
// further work. Admission covers resolution too, so a shutdown drain does not
// race a request that has not started running yet, and every escalation that
// reached a record has attempted to persist its outcome by the time Wait
// returns. It is safe to call more than once.
func (s *Service[R, W]) Wait(ctx context.Context) error {
	// One drain at a time: concurrent callers each claiming part of the queue
	// would stall each other short of the full set.
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.drained {
		return nil
	}
	// Claiming every token is the drain: a token is released only once the work
	// it stands for has finished, so holding them all means nothing is left.
	claimed := 0
	for claimed < s.opts.MaxQueued {
		select {
		case s.admitted <- struct{}{}:
			claimed++
		case <-ctx.Done():
			// A drain that gave up must hand back what it took, or the queue
			// would stay permanently smaller and a later attempt could never
			// finish.
			for ; claimed > 0; claimed-- {
				<-s.admitted
			}
			return ctx.Err()
		}
	}
	s.drained = true
	return nil
}

func (s *Service[R, W]) run(ctx context.Context, rec *record[R], resolved W, release func()) {
	// The admission token and the accepted lifetime are held until the
	// escalation has persisted its outcome, so a queue slot frees only when the
	// work it stands for is done and a drain cannot end early.
	defer release()

	// Waiting for the single slot keeps queued work visible rather than
	// rejecting it, while still admitting exactly one analysis at a time. A
	// request that never reaches the slot within its budget finishes as failed,
	// which leaves it retryable and prunable.
	select {
	case s.active <- struct{}{}:
	case <-ctx.Done():
		s.finish(rec, View[R]{Ref: rec.view.Ref, State: StateFailed, Error: safeError(ctx.Err())})
		return
	}
	defer func() { <-s.active }()

	s.mu.Lock()
	rec.view.State = StateRunning
	rec.updatedAt = s.opts.Now().UTC()
	s.mu.Unlock()

	view, err := s.runner.Run(ctx, resolved)
	if err != nil {
		s.finish(rec, View[R]{Ref: rec.view.Ref, State: StateFailed, Error: safeError(err)})
		return
	}
	view.Ref = rec.view.Ref
	if view.State == "" {
		view.State = StateComplete
	}
	s.finish(rec, view)
}

func (s *Service[R, W]) finish(rec *record[R], view View[R]) {
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
func (s *Service[R, W]) persist() {
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
func (s *Service[R, W]) snapshotLocked() map[string]View[R] {
	out := make(map[string]View[R], len(s.records))
	for identity, rec := range s.records {
		if !rec.running {
			out[identity] = rec.view
		}
	}
	return out
}

// pruneLocked drops the oldest finished records past the retention bound. It
// drops as many as it can: several escalations can finish between two starts,
// and a single eviction per start would leave the bound exceeded until enough
// later requests trickled it back down.
func (s *Service[R, W]) pruneLocked() {
	for len(s.records) > s.opts.MaxRecords {
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
		// Everything left is still running, so nothing more can be dropped.
		if oldestKey == "" {
			return
		}
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
func (s *Service[R, W]) pruneIdempotencyLocked() {
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
