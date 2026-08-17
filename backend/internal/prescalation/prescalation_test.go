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
	mu  sync.Mutex
	n   int
}

func (f *fakeResolver) Resolve(_ context.Context, ref Ref) (Resolved, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
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
	service := newService(t, &fakeResolver{}, runner, Options{})

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
	service := newService(t, &fakeResolver{}, runner, Options{MaxRecords: 2})

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
	if retained > 3 {
		t.Fatalf("retained records = %d, want the bound respected", retained)
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
