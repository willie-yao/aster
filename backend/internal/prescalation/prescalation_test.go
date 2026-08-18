package prescalation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func testRef(test string) Ref {
	return Ref{PullNumber: 6209, JobID: "org/repo/pull-e2e", BuildID: "100", TestName: test}
}

type fakeResolver struct {
	err error
	// gate, when non-nil, holds every resolution open so a test can observe how
	// many requests reached the expensive work at once.
	gate chan struct{}
	mu   sync.Mutex
	n    int
}

func (f *fakeResolver) Resolve(_ context.Context, ref Ref) (Resolved, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	if f.err != nil {
		return Resolved{}, f.err
	}
	return Resolved{Ref: ref}, nil
}

func (f *fakeResolver) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

type fakeRunner struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	inFlight int32
	maxSeen  int32
	err      error
	view     View
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (f *fakeRunner) Run(ctx context.Context, resolved Resolved) (View, error) {
	current := atomic.AddInt32(&f.inFlight, 1)
	for {
		seen := atomic.LoadInt32(&f.maxSeen)
		if current <= seen || atomic.CompareAndSwapInt32(&f.maxSeen, seen, current) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)
	f.started <- struct{}{}
	select {
	case <-f.release:
	case <-ctx.Done():
		return View{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return View{}, f.err
	}
	view := f.view
	if view.State == "" {
		view.State = StateComplete
		view.RootCause = "root cause for " + resolved.Ref.TestName
	}
	return view, nil
}

func newService(t *testing.T, resolver Resolver, runner Runner, opts Options) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service, err := New(ctx, resolver, runner, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

// drain blocks until every admitted escalation has released its slot.
func drain(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func waitForState(t *testing.T, service *Service, ref Ref, want string) View {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		view, err := service.Get(ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if view.State == want {
			return view
		}
		select {
		case <-deadline:
			t.Fatalf("state = %q, want %q", view.State, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestStartRunsOneEscalationToCompletion(t *testing.T) {
	runner := newFakeRunner()
	service := newService(t, &fakeResolver{}, runner, Options{})

	view, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.State != StateQueued {
		t.Fatalf("initial state = %q, want queued", view.State)
	}
	<-runner.started
	close(runner.release)

	done := waitForState(t, service, testRef("TestA"), StateComplete)
	if done.RootCause == "" || done.CompletedAt.IsZero() {
		t.Fatalf("completed view = %+v", done)
	}
}

// Escalation is expensive, so exactly one analysis may be in flight no matter
// how many subjects are started.
func TestOnlyOneEscalationRunsAtATime(t *testing.T) {
	runner := newFakeRunner()
	service := newService(t, &fakeResolver{}, runner, Options{MaxQueued: 4})

	for i := 0; i < 4; i++ {
		ref := testRef(fmt.Sprintf("Test%d", i))
		if _, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	<-runner.started
	// Give any wrongly-admitted peers a chance to enter Run.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&runner.maxSeen); got != 1 {
		t.Fatalf("concurrent runs = %d, want 1", got)
	}
	close(runner.release)
	for i := 0; i < 4; i++ {
		waitForState(t, service, testRef(fmt.Sprintf("Test%d", i)), StateComplete)
	}
}

// The single slot bounds model traffic, but resolution reads the artifact
// bucket and GitHub before any model call. Admission must be reserved first, or
// every concurrent request pays that cost and only then queues.
func TestConcurrentStartsBoundResolverCalls(t *testing.T) {
	const (
		bound   = 2
		callers = 8
	)
	resolver := &fakeResolver{gate: make(chan struct{})}
	runner := newFakeRunner()
	service := newService(t, resolver, runner, Options{MaxQueued: bound})
	// Idempotent so a failed assertion cannot strand the parked callers.
	openGate := sync.OnceFunc(func() { close(resolver.gate) })
	defer openGate()
	releaseRuns := sync.OnceFunc(func() { close(runner.release) })
	defer releaseRuns()

	var busy int32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := testRef(fmt.Sprintf("Test%d", i))
			_, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i))
			switch {
			case errors.Is(err, ErrBusy):
				atomic.AddInt32(&busy, 1)
			case err != nil:
				t.Errorf("Start: %v", err)
			}
		}(i)
	}
	// Every caller must reach a decision before the gate opens, or an early
	// release would let a rejected peer back in and understate the fan-out.
	deadline := time.After(3 * time.Second)
	for {
		if resolver.calls()+int(atomic.LoadInt32(&busy)) >= callers {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d of %d callers decided", resolver.calls()+int(atomic.LoadInt32(&busy)), callers)
		case <-time.After(time.Millisecond):
		}
	}
	// Every admitted caller is parked inside Resolve, so the resolver count is
	// the number of requests that reached the expensive work.
	if got := resolver.calls(); got != bound {
		t.Errorf("resolver calls = %d, want at most the queue bound %d", got, bound)
	}
	if got := atomic.LoadInt32(&busy); got != callers-bound {
		t.Errorf("rejected starts = %d, want %d", got, callers-bound)
	}
	openGate()
	wg.Wait()

	if got := resolver.calls(); got != bound {
		t.Errorf("resolver calls after the gate opened = %d, want %d", got, bound)
	}
	releaseRuns()
}

// A rejected start did no work, so it must leave no trace a later request would
// mistake for an escalation that already happened.
func TestABusyStartIsNotRecorded(t *testing.T) {
	runner := newFakeRunner()
	service := newService(t, &fakeResolver{}, runner, Options{MaxQueued: 1})

	if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runner.started

	if _, err := service.Start(context.Background(), testRef("TestB"), "octocat", "req-2"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	view, err := service.Get(testRef("TestB"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateNotStarted {
		t.Fatalf("state = %q, want not_started", view.State)
	}
	close(runner.release)
}

// Queue time counts against the escalation's own budget. A request that never
// reaches the slot must finish rather than hold a goroutine and an unprunable
// running record for the life of the process.
func TestAQueuedEscalationTimesOutWithoutRunning(t *testing.T) {
	store := &memoryStore{}
	runner := newFakeRunner()
	service := newService(t, &fakeResolver{}, runner, Options{
		Timeout: 60 * time.Millisecond, MaxQueued: 2, Store: store,
	})

	// Occupy the single slot so the escalation can never be admitted to it.
	held, blocked := make(chan struct{}), make(chan struct{})
	go func() {
		service.active <- struct{}{}
		close(held)
		<-blocked
	}()
	<-held
	defer close(blocked)

	ref := testRef("TestA")
	if _, err := service.Start(context.Background(), ref, "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	view := waitForState(t, service, ref, StateFailed)
	if view.Error != "escalation timed out" {
		t.Errorf("error = %q, want the timeout message", view.Error)
	}
	if got := atomic.LoadInt32(&runner.maxSeen); got != 0 {
		t.Errorf("runs = %d, want the queued escalation never to reach the runner", got)
	}
	// The state is visible before persistence completes, so drain first.
	drain(t, service)
	// Only finished records are snapshotted and prunable, so persistence
	// proves the record is no longer marked running.
	restored, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := restored[ref.identity()]; !ok {
		t.Error("the timed-out escalation should be a finished, prunable record")
	}
}

// blockingResolver stands in for the bucket and GitHub reads: it does nothing
// until its context ends.
type blockingResolver struct{ entered chan struct{} }

func newBlockingResolver() *blockingResolver {
	return &blockingResolver{entered: make(chan struct{})}
}

func (b *blockingResolver) Resolve(ctx context.Context, _ Ref) (Resolved, error) {
	close(b.entered)
	<-ctx.Done()
	return Resolved{}, ctx.Err()
}

// swallowingResolver reports success from a cancelled context, as the real
// resolver can: a missing finished.json reads as a pending build, and a failed
// changed-file listing is discarded because change context is optional.
type swallowingResolver struct{ entered chan struct{} }

func (s *swallowingResolver) Resolve(ctx context.Context, ref Ref) (Resolved, error) {
	close(s.entered)
	<-ctx.Done()
	return Resolved{Ref: ref}, nil
}

// Resolution reads the artifact bucket and GitHub, so it is part of the
// accepted lifetime rather than an unbounded prelude to it. Anything that ends
// that lifetime must reach a request that is still resolving, or the admission
// token it holds outlives its budget and stalls a shutdown drain.
func TestResolutionObservesTheAcceptedLifetime(t *testing.T) {
	t.Run("the escalation deadline bounds resolution", func(t *testing.T) {
		service := newService(t, newBlockingResolver(), newFakeRunner(),
			Options{Timeout: 50 * time.Millisecond})
		_, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the expired budget as the cause", err)
		}
		drain(t, service)
	})

	t.Run("shutdown cancels resolution", func(t *testing.T) {
		resolver := newBlockingResolver()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service, err := New(ctx, resolver, newFakeRunner(), Options{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		go func() {
			<-resolver.entered
			cancel()
		}()
		if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the shutdown as the cause", err)
		}
		drain(t, service)
	})

	// The caller's own deadline must survive as its own cause: the handler maps
	// a timeout and a cancellation to different statuses.
	t.Run("a departed caller cancels resolution", func(t *testing.T) {
		resolver := newBlockingResolver()
		service := newService(t, resolver, newFakeRunner(), Options{})
		callerCtx, cancelCaller := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancelCaller()
		if _, err := service.Start(callerCtx, testRef("TestA"), "octocat", "req-1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the caller's deadline as the cause", err)
		}
		drain(t, service)
	})

	// A resolver that discards a cancellation must not be able to launch an
	// analysis the requester is no longer waiting for.
	t.Run("a swallowed cancellation still stops the escalation", func(t *testing.T) {
		runner := newFakeRunner()
		service := newService(t, &swallowingResolver{entered: make(chan struct{})}, runner,
			Options{Timeout: 50 * time.Millisecond})
		ref := testRef("TestA")
		if _, err := service.Start(context.Background(), ref, "octocat", "req-1"); err == nil {
			t.Fatal("want an error rather than a record built from a cut-off resolution")
		}
		drain(t, service)
		if view, _ := service.Get(ref); view.State != StateNotStarted {
			t.Errorf("state = %q, want not_started", view.State)
		}
		if got := atomic.LoadInt32(&runner.maxSeen); got != 0 {
			t.Errorf("runs = %d, want the cut-off resolution never to reach the runner", got)
		}
	})
}

func TestStartIsIdempotentForTheSameSubject(t *testing.T) {
	runner := newFakeRunner()
	resolver := &fakeResolver{}
	service := newService(t, resolver, runner, Options{})

	for i := 0; i < 3; i++ {
		if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	<-runner.started
	close(runner.release)
	waitForState(t, service, testRef("TestA"), StateComplete)

	if resolver.calls() != 1 {
		t.Errorf("resolver calls = %d, want a single resolution", resolver.calls())
	}
}

// Reusing a request ID for a different failure is a client bug, not a second
// escalation, so it is rejected rather than silently starting new work.
func TestReusedRequestIDForAnotherSubjectIsRejected(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{})

	if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err := service.Start(context.Background(), testRef("TestB"), "octocat", "req-1")
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestStartRejectsMalformedRefsAndIdentity(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{})

	cases := []struct {
		name  string
		ref   Ref
		owner string
		req   string
	}{
		{name: "no pull number", ref: Ref{JobID: "j", BuildID: "b", TestName: "t"}, owner: "o", req: "r"},
		{name: "no job", ref: Ref{PullNumber: 1, BuildID: "b", TestName: "t"}, owner: "o", req: "r"},
		{name: "no build", ref: Ref{PullNumber: 1, JobID: "j", TestName: "t"}, owner: "o", req: "r"},
		{name: "no test", ref: Ref{PullNumber: 1, JobID: "j", BuildID: "b"}, owner: "o", req: "r"},
		{name: "no owner", ref: testRef("TestA"), req: "r"},
		{name: "no request id", ref: testRef("TestA"), owner: "o"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Start(context.Background(), tc.ref, tc.owner, tc.req); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestResolverRejectionIsNotRecordedAsAnEscalation(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{err: ErrNotEligible}, runner, Options{})

	if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("err = %v, want ErrNotEligible", err)
	}
	view, err := service.Get(testRef("TestA"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateNotStarted {
		t.Fatalf("state = %q, want not_started", view.State)
	}
}

func TestRunnerFailureSurfacesASafeMessage(t *testing.T) {
	runner := newFakeRunner()
	runner.err = errors.New("model endpoint https://secret.internal/v1 returned 500 for token sk-abc123")
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{})

	if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	view := waitForState(t, service, testRef("TestA"), StateFailed)
	for _, leak := range []string{"secret.internal", "sk-abc123", "500"} {
		if contains(view.Error, leak) {
			t.Errorf("error message leaks %q: %q", leak, view.Error)
		}
	}
}

func TestGetReportsNotStartedForAnUntouchedSubject(t *testing.T) {
	service := newService(t, &fakeResolver{}, newFakeRunner(), Options{})

	view, err := service.Get(testRef("TestA"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateNotStarted {
		t.Fatalf("state = %q", view.State)
	}
}

type memoryStore struct {
	mu      sync.Mutex
	results map[string]View
	loadErr error
}

func (m *memoryStore) Load() (map[string]View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	out := map[string]View{}
	for k, v := range m.results {
		out[k] = v
	}
	return out, nil
}

func (m *memoryStore) Save(results map[string]View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = map[string]View{}
	for k, v := range results {
		m.results[k] = v
	}
	return nil
}

func TestCompletedResultsSurviveRestart(t *testing.T) {
	store := &memoryStore{}
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{Store: store})

	if _, err := service.Start(context.Background(), testRef("TestA"), "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, service, testRef("TestA"), StateComplete)
	// The state is visible before persistence completes, so drain first.
	drain(t, service)

	restarted := newService(t, &fakeResolver{}, newFakeRunner(), Options{Store: store})
	view, err := restarted.Get(testRef("TestA"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.State != StateComplete || view.RootCause == "" {
		t.Fatalf("restored view = %+v, want the completed result", view)
	}
}

// A process that stopped mid-escalation left work that never finished, so it
// must not come back looking perpetually in progress.
func TestInFlightEscalationsAreNotRestoredAsRunning(t *testing.T) {
	store := &memoryStore{results: map[string]View{
		testRef("TestA").identity(): {Ref: testRef("TestA"), State: StateRunning},
		testRef("TestB").identity(): {Ref: testRef("TestB"), State: StateQueued},
		testRef("TestC").identity(): {Ref: testRef("TestC"), State: StateComplete, RootCause: "done"},
	}}
	service := newService(t, &fakeResolver{}, newFakeRunner(), Options{Store: store})

	for _, name := range []string{"TestA", "TestB"} {
		view, _ := service.Get(testRef(name))
		if view.State != StateNotStarted {
			t.Errorf("%s state = %q, want not_started", name, view.State)
		}
	}
	if view, _ := service.Get(testRef("TestC")); view.State != StateComplete {
		t.Errorf("completed result should survive, got %q", view.State)
	}
}

func TestRetentionIsBounded(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{MaxRecords: 2, MaxQueued: 1})

	for i := 0; i < 5; i++ {
		ref := testRef(fmt.Sprintf("Test%d", i))
		if _, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForState(t, service, ref, StateComplete)
	}
	service.mu.Lock()
	retained := len(service.records)
	service.mu.Unlock()
	if retained > 2 {
		t.Fatalf("retained records = %d, want the bound respected", retained)
	}
}

// Records cannot be pruned while they run, so retention tighter than the queue
// would evict results the instant they land. A full queue's analyses were all
// paid for, so every one of them must stay readable and persisted.
func TestAFullQueuesResultsAreAllRetained(t *testing.T) {
	store := &memoryStore{}
	runner := newFakeRunner()
	service := newService(t, &fakeResolver{}, runner, Options{
		MaxRecords: 2, MaxQueued: 4, Store: store,
	})

	// Holding the runner keeps all four records running, and therefore
	// unprunable, until the queue is full.
	for i := 0; i < 4; i++ {
		ref := testRef(fmt.Sprintf("Test%d", i))
		if _, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	service.mu.Lock()
	queued := len(service.records)
	service.mu.Unlock()
	if queued != 4 {
		t.Fatalf("records while running = %d, want the full queue retained", queued)
	}

	close(runner.release)
	drain(t, service)

	restored, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 4; i++ {
		ref := testRef(fmt.Sprintf("Test%d", i))
		view, err := service.Get(ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if view.State != StateComplete {
			t.Errorf("Test%d state = %q, want the result to survive", i, view.State)
		}
		if _, ok := restored[ref.identity()]; !ok {
			t.Errorf("Test%d was never persisted", i)
		}
	}
}

func TestEligible(t *testing.T) {
	cases := []struct {
		verdict models.AttributionVerdict
		want    bool
	}{
		{verdict: models.AttributionUnexplained, want: true},
		{verdict: models.AttributionTouchesChangedCode, want: true},
		{verdict: models.AttributionInconclusive, want: true},
		{verdict: models.AttributionPreExisting},
		{verdict: models.AttributionWidespread},
		{verdict: models.AttributionKnownFlake},
	}
	for _, tc := range cases {
		t.Run(string(tc.verdict), func(t *testing.T) {
			got := Eligible(&models.FailureAttribution{Verdict: tc.verdict})
			if got != tc.want {
				t.Fatalf("Eligible(%s) = %t, want %t", tc.verdict, got, tc.want)
			}
		})
	}
	// A failure with no verdict has not been ruled out, so it stays eligible.
	if !Eligible(nil) {
		t.Error("an unattributed failure should be eligible")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// persistCountingStore records the most recent snapshot written.
type persistCountingStore struct {
	mu   sync.Mutex
	last map[string]View
}

func (c *persistCountingStore) Load() (map[string]View, error) { return map[string]View{}, nil }

func (c *persistCountingStore) Save(results map[string]View) error {
	// Real IO takes time, which is what opens the interleaving window.
	time.Sleep(2 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = map[string]View{}
	for k, v := range results {
		c.last[k] = v
	}
	return nil
}

func (c *persistCountingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.last)
}

type instantRunner struct{}

func (instantRunner) Run(context.Context, Resolved) (View, error) {
	return View{State: StateComplete, RootCause: "ok"}, nil
}

// Cancelling the service wakes every queued escalation at once, and the
// cancellation path finishes without holding the active slot. Persisting must
// stay ordered so an older snapshot cannot overwrite a newer one.
func TestConcurrentFinishesDoNotLosePersistedResults(t *testing.T) {
	store := &persistCountingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	service, err := New(ctx, &fakeResolver{}, instantRunner{}, Options{Store: store, MaxQueued: 12})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Occupy the single slot so the rest queue behind it.
	held, blocked := make(chan struct{}), make(chan struct{})
	go func() {
		service.active <- struct{}{}
		close(held)
		<-blocked
	}()
	<-held
	defer close(blocked)

	const queued = 12
	for i := 0; i < queued; i++ {
		ref := Ref{PullNumber: 1, JobID: "j", BuildID: "b", TestName: fmt.Sprintf("Test%d", i)}
		if _, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	cancel()
	drain(t, service)

	service.mu.Lock()
	inMemory := len(service.records)
	service.mu.Unlock()
	if got := store.count(); got != inMemory {
		t.Fatalf("persisted %d results but %d are in memory; a snapshot was overwritten", got, inMemory)
	}
}

type failOnceRunner struct {
	mu    sync.Mutex
	calls int
}

func (f *failOnceRunner) Run(context.Context, Resolved) (View, error) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first {
		return View{}, errors.New("provider returned 503")
	}
	return View{State: StateComplete, RootCause: "recovered"}, nil
}

func (f *failOnceRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A failure is a transient outcome, not a durable fact. A provider blip, a
// timeout, or a shutdown that interrupted queued work must not pin a subject as
// permanently un-analyzable, especially since the result is persisted.
func TestAFailedEscalationCanBeRetried(t *testing.T) {
	runner := &failOnceRunner{}
	service := newService(t, &fakeResolver{}, runner, Options{})
	ref := testRef("TestA")

	if _, err := service.Start(context.Background(), ref, "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, service, ref, StateFailed)

	// The maintainer clicks Investigate again; the UI mints a fresh key.
	if _, err := service.Start(context.Background(), ref, "octocat", "req-2"); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	view := waitForState(t, service, ref, StateComplete)
	if view.RootCause != "recovered" {
		t.Fatalf("view = %+v, want the retried result", view)
	}
	if runner.count() != 2 {
		t.Errorf("runner calls = %d, want the retry to reach the runner", runner.count())
	}
}

// Replaying one request must not start new work, even after it failed. That is
// what distinguishes a retry from a duplicate delivery.
func TestReplayingTheSameKeyReturnsTheFailureWithoutRerunning(t *testing.T) {
	runner := &failOnceRunner{}
	service := newService(t, &fakeResolver{}, runner, Options{})
	ref := testRef("TestA")

	if _, err := service.Start(context.Background(), ref, "octocat", "req-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, service, ref, StateFailed)

	view, err := service.Start(context.Background(), ref, "octocat", "req-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if view.State != StateFailed {
		t.Fatalf("replay state = %q, want the original failure", view.State)
	}
	if runner.count() != 1 {
		t.Errorf("runner calls = %d, want no rerun on replay", runner.count())
	}
}

// The replay index outlives the records it points at and its keys are client
// controlled, so it needs its own bound.
func TestIdempotencyIndexIsBounded(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	service := newService(t, &fakeResolver{}, runner, Options{MaxRecords: 2, MaxQueued: 1})
	ref := testRef("TestA")

	for i := 0; i < 200; i++ {
		if _, err := service.Start(context.Background(), ref, "octocat", fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForState(t, service, ref, StateComplete)
	}
	service.mu.Lock()
	keys := len(service.idempotency)
	service.mu.Unlock()
	if keys > 2*maxIdempotencyKeysPerRecord {
		t.Fatalf("idempotency entries = %d, want the bound respected", keys)
	}
}
