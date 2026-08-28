package analysischat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

var testRequestCounter atomic.Int64

func TestOptionsNormalizedTurnBudget(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		timeout time.Duration
		lease   time.Duration
	}{
		{name: "defaults", timeout: DefaultTurnTimeout, lease: DefaultTurnTimeout + 30*time.Second},
		{name: "short lease", opts: Options{TurnLeaseTTL: time.Minute}, timeout: DefaultTurnTimeout, lease: DefaultTurnTimeout + 30*time.Second},
		{name: "equal lease", opts: Options{TurnLeaseTTL: DefaultTurnTimeout}, timeout: DefaultTurnTimeout, lease: DefaultTurnTimeout + 30*time.Second},
		{name: "long lease", opts: Options{TurnLeaseTTL: 11 * time.Minute}, timeout: DefaultTurnTimeout, lease: 11 * time.Minute},
		{name: "custom timeout", opts: Options{TurnTimeout: time.Minute}, timeout: time.Minute, lease: 90 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := test.opts.normalized("/data")
			if opts.TurnTimeout != test.timeout || opts.TurnLeaseTTL != test.lease {
				t.Fatalf("turn budget = timeout %s lease %s", opts.TurnTimeout, opts.TurnLeaseTTL)
			}
		})
	}
}

func testRequestID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d", testRequestCounter.Add(1))
}

type fakeRunner struct {
	mu            sync.Mutex
	turns         []Turn
	reply         Reply
	err           error
	started       chan struct{}
	release       chan struct{}
	phases        []string
	ignoreContext bool
}

func (f *fakeRunner) Reply(ctx context.Context, turn Turn) (Reply, error) {
	f.mu.Lock()
	f.turns = append(f.turns, turn)
	started, release := f.started, f.release
	reply, err := f.reply, f.err
	f.mu.Unlock()
	for _, phase := range f.phases {
		turn.ReportProgress(phase)
	}
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		if f.ignoreContext {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			}
		}
	}
	return reply, err
}

func writeJobDetail(t *testing.T, dir string, detail models.JobDetail) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteJobDetail(dir, detail); err != nil {
		t.Fatal(err)
	}
}

func testDetail(testCases ...models.TestCase) models.JobDetail {
	return models.JobDetail{
		Name: "periodic-demo", JobID: "periodic-demo", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "123", JobName: "periodic-demo", WebURL: "https://example.test/build/123"},
			TestCases: testCases,
		}},
	}
}

func analyzedTest(name, junit, generated string) models.TestCase {
	return models.TestCase{
		Name: name, JUnitFile: junit, Status: "failed", FailureMessage: "timed out",
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: generated, RootCause: "the controller stopped", Severity: "High",
			SuggestedFix: "restart the controller", RelevantFiles: []string{"build-log.txt"},
			Disposition: models.AnalysisDispositionGrounded,
		},
	}
}

func requireAttempt(t *testing.T, view SessionView, requestID string) Attempt {
	t.Helper()
	for _, attempt := range view.Attempts {
		if attempt.RequestID == requestID {
			return attempt
		}
	}
	t.Fatalf("attempt %q missing from %+v", requestID, view.Attempts)
	return Attempt{}
}

func TestServiceCreateAndSend(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit_01.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{reply: Reply{
		Answer: "The timeout follows the controller exit.", Assessment: "supports",
		Citations:        []Citation{{Path: "build-log.txt", LineStart: 42, LineEnd: 42, Quote: "controller exited"}},
		EvidenceWarnings: []string{"citation 2 quote did not match"},
		ToolCalls:        2, GCSBytes: 1024, ElapsedMs: 50,
	}}
	now := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	service, err := NewService(t.Context(), dir, runner, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-23T12:00:00Z",
	}, "Alice", testRequestID(t))

	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Analysis.JUnitFile != "junit_01.xml" || len(created.Messages) != 0 || created.TurnsUsed != 0 || created.MaxTurns != 10 {
		t.Fatalf("created session = %+v", created)
	}

	got, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "  What proves this?  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "What proves this?" || got.TurnsUsed != 1 || got.MaxTurns != 10 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	attempt := requireAttempt(t, got, got.Messages[0].RequestID)
	if attempt.Outcome != requestSucceeded || attempt.Question != "What proves this?" || attempt.Turn != 1 {
		t.Fatalf("successful attempt = %+v", attempt)
	}
	assistant := got.Messages[1]
	if assistant.Assessment != "supports" || assistant.ToolCalls != 2 || len(assistant.Citations) != 1 ||
		!slices.Equal(assistant.EvidenceWarnings, []string{"citation 2 quote did not match"}) {
		t.Fatalf("assistant = %+v", assistant)
	}
	got.Messages[1].EvidenceWarnings[0] = "mutated response"
	restored, err := service.Get(created.ID, "alice")
	if err != nil || !slices.Equal(restored.Messages[1].EvidenceWarnings, []string{"citation 2 quote did not match"}) {
		t.Fatalf("restored evidence warnings = %v err=%v", restored.Messages[1].EvidenceWarnings, err)
	}

	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "What should I check next?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	secondTurn := runner.turns[1]
	runner.mu.Unlock()
	if turn.BuildPrefix != "logs/periodic-demo/123/" || turn.JobID != "periodic-demo" {
		t.Fatalf("turn identity = %+v", turn)
	}
	if turn.TestCase.AIAnalysis == nil || turn.TestCase.AIAnalysis.RootCause != "the controller stopped" {
		t.Fatalf("turn test case = %+v", turn.TestCase)
	}
	if len(secondTurn.History) != 2 || secondTurn.History[0].Role != "user" || secondTurn.History[1].Role != "assistant" {
		t.Fatalf("second turn history = %+v", secondTurn.History)
	}
	shared, err := service.Get(created.ID, "bob")
	if err != nil {
		t.Fatalf("shared Get error = %v", err)
	}
	if shared.CreatedBy != "alice" || shared.Messages[0].Actor != "alice" || shared.Attempts[0].Actor != "alice" {
		t.Fatalf("shared attribution = %+v", shared)
	}
}

func TestServiceFindSharedSessionAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	first, err := NewService(t.Context(), dir, &fakeRunner{}, Options{Now: now, MaxSessions: 1, MaxSessionsPerOwner: 1})
	if err != nil {
		t.Fatal(err)
	}
	older, err := first.Create(ref, "alice", "create-older")
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	newer, err := first.Create(ref, "alice", "create-newer")
	if err != nil {
		t.Fatal(err)
	}

	second, err := NewService(t.Context(), dir, &fakeRunner{}, Options{Now: now, MaxSessions: 1, MaxSessionsPerOwner: 1})
	if err != nil {
		t.Fatal(err)
	}
	found, err := second.Find(ref, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if older.ID != newer.ID || found.ID != older.ID {
		t.Fatalf("shared sessions = older %q newer %q found %q", older.ID, newer.ID, found.ID)
	}
	shared, err := second.Find(ref, "bob")
	if err != nil || shared.ID != older.ID || shared.CreatedBy != "alice" {
		t.Fatalf("cross-operator find = %+v err=%v", shared, err)
	}
	reused, err := second.Create(ref, "bob", "create-shared-at-capacity")
	if err != nil || reused.ID != older.ID {
		t.Fatalf("shared create at capacity = %+v err=%v", reused, err)
	}
}

func TestServiceFindReflectsActiveSharedSession(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	service, err := NewService(t.Context(), dir, runner, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice", "create-shared-active")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "bob", "turn-shared-active", "question", nil)
		done <- err
	}()
	<-runner.started
	found, err := service.Find(ref, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if found.Active == nil || found.Active.Actor != "bob" || found.Active.RequestID != "turn-shared-active" || found.Active.Question != "question" {
		t.Fatalf("shared active turn = %+v", found.Active)
	}
	replica, err := NewService(t.Context(), dir, &fakeRunner{}, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fromReplica, err := replica.Get(created.ID, "carol")
	if err != nil || fromReplica.Active == nil || fromReplica.Active.Actor != "bob" {
		t.Fatalf("replica active turn = %+v err=%v", fromReplica.Active, err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceFindRejectsChangedAnalysis(t *testing.T) {
	dir := t.TempDir()
	oldGenerated := "2026-07-23T12:00:00Z"
	newGenerated := "2026-07-26T12:00:00Z"
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", oldGenerated)))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	oldRef := AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", AnalysisGeneratedAt: oldGenerated,
	}
	created, err := service.Create(oldRef, "alice", "create-old-analysis")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", "turn-old-analysis", "What did the old analysis say?"); err != nil {
		t.Fatal(err)
	}
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", newGenerated)))
	if _, err := service.Find(oldRef, "alice"); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("stale analysis find error = %v", err)
	}
	newRef := oldRef
	newRef.AnalysisGeneratedAt = newGenerated
	if _, err := service.Find(newRef, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("changed analysis attached old session: %v", err)
	}
}

func TestServiceFindExpiresAndCreatesNewSession(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{SessionTTL: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	expired, err := service.Create(ref, "alice", "create-expired")
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	if _, err := service.Find(ref, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired find error = %v", err)
	}
	replacement, err := service.Create(ref, "alice", "create-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == expired.ID {
		t.Fatalf("replacement reused expired ID %q", replacement.ID)
	}
}

func TestServiceResolveRejectsAmbiguousAndChangedAnalysis(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(
		analyzedTest("TestCluster", "junit_01.xml", "2026-07-23T12:00:00Z"),
		analyzedTest("TestCluster", "junit_02.xml", "2026-07-23T12:00:00Z"),
	))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", testRequestID(t))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ambiguous Create error = %v", err)
	}
	_, err = service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit_01.xml",
		AnalysisGeneratedAt: "2026-07-23T11:00:00Z",
	}, "alice", testRequestID(t))

	if !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed Create error = %v", err)
	}
}

func TestServiceBoundsSessionsTurnsAndQuestions(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(
		analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z"),
		analyzedTest("TestOther", "junit.xml", "2026-07-23T12:00:00Z"),
	))
	runner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}
	service, err := NewService(t.Context(), dir, runner, Options{
		MaxSessions: 2, MaxSessionsPerOwner: 1, MaxTurns: 1, MaxQuestionBytes: 8,
	})

	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	otherRef := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestOther"}
	if _, err := service.Create(otherRef, "alice", testRequestID(t)); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("owner session limit error = %v", err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "123456789"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("question bound error = %v", err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "again"); !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("turn limit error = %v", err)
	}
	usage, err := service.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TurnsUsed != 1 || usage.MaxTurns != 1 {
		t.Fatalf("turn usage = %d/%d", usage.TurnsUsed, usage.MaxTurns)
	}
}

func TestServiceSerializesTurns(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "explains"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "first")
		done <- err
	}()
	<-runner.started
	if _, err := service.Send(context.Background(), created.ID, "bob", testRequestID(t), "second"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("concurrent Send error = %v", err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceResolvesPresubmitBuildPrefix(t *testing.T) {
	dir := t.TempDir()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z"))
	detail.Name = "pull-demo-e2e"
	detail.JobID = "example/project/pull-demo-e2e"
	detail.JobType = models.JobTypePresubmit
	detail.Repo = "example/project"
	detail.Runs[0].JobName = detail.Name
	detail.Runs[0].PullNumber = "42"
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: detail.JobID, BuildID: "123", TestName: "TestCluster"}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "explain"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.BuildPrefix != "pr-logs/pull/example_project/42/pull-demo-e2e/123/" {
		t.Fatalf("build prefix = %q", turn.BuildPrefix)
	}
}

func TestServiceRunnerErrorClearsBusy(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{err: errors.New("model unavailable")}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "first"); err == nil {
		t.Fatal("runner error was not returned")
	}
	runner.mu.Lock()
	runner.err = nil
	runner.reply = Reply{Answer: "recovered", Assessment: "explains"}
	runner.mu.Unlock()
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "retry"); err != nil {
		t.Fatalf("retry after runner error: %v", err)
	}
}

func TestServiceRejectsOversizedAnalysisReference(t *testing.T) {
	service, err := NewService(t.Context(), t.TempDir(), &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Create(AnalysisRef{JobID: strings.Repeat("x", maxJobIDBytes+1), BuildID: "1", TestName: "Test"}, "alice", testRequestID(t))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized reference error = %v", err)
	}
}

func TestServiceResolvesStrongJUnitIdentity(t *testing.T) {
	dir := t.TempDir()
	first := analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")
	first.SuiteName, first.ClassName = "suite", "first"
	second := analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")
	second.SuiteName, second.ClassName = "suite", "second"
	second.AIAnalysis.RootCause = "the second class failed"
	writeJobDetail(t, dir, testDetail(first, second))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
	}, "alice", testRequestID(t)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("weak identity Create error = %v", err)
	}
	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		SuiteName: "suite", ClassName: "second", JUnitFile: "junit.xml",
	}, "alice", testRequestID(t))

	if err != nil {
		t.Fatal(err)
	}
	if created.Analysis.SuiteName != "suite" || created.Analysis.ClassName != "second" {
		t.Fatalf("canonical analysis ref = %+v", created.Analysis)
	}
}

func TestServiceExpiryReleasesCapacity(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}, Options{
		SessionTTL: time.Minute, MaxSessions: 1, MaxSessionsPerOwner: 1, Now: now,
	})

	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	if _, err := service.Get(created.ID, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get at expiry error = %v", err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "expired"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Send at expiry error = %v", err)
	}
	if _, err := service.Create(ref, "alice", testRequestID(t)); err != nil {
		t.Fatalf("expired session did not release capacity: %v", err)
	}
}

func TestServiceBusySessionCompletesAcrossExpiry(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(
		analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z"),
		analyzedTest("TestOther", "junit.xml", "2026-07-23T12:00:00Z"),
	))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "explains"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	service, err := NewService(t.Context(), dir, runner, Options{
		SessionTTL: time.Minute, MaxSessions: 1, MaxSessionsPerOwner: 1, Now: now,
	})

	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "in flight")
		done <- err
	}()
	<-runner.started
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	if _, err := service.Get(created.ID, "alice"); err != nil {
		t.Fatalf("busy expired session should remain readable: %v", err)
	}
	reused, err := service.Create(ref, "bob", testRequestID(t))
	if err != nil || reused.ID != created.ID {
		t.Fatalf("busy shared session reuse = %+v err=%v", reused, err)
	}
	otherRef := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestOther"}
	if _, err := service.Create(otherRef, "alice", testRequestID(t)); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("busy expired session should retain capacity, got %v", err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight turn did not complete across expiry: %v", err)
	}
	if _, err := service.Get(created.ID, "alice"); err != nil {
		t.Fatalf("completed turn did not refresh session expiry: %v", err)
	}
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	if _, err := service.Get(created.ID, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("refreshed session was not evicted: %v", err)
	}
	if _, err := service.Create(ref, "alice", testRequestID(t)); err != nil {
		t.Fatalf("expired refreshed session did not release capacity: %v", err)
	}
}

func TestServiceDeleteRemovesSharedConversationAndReleasesCapacity(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}, Options{
		MaxSessions: 2, MaxSessionsPerOwner: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "question"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := service.store.context()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		state.Sessions[created.ID].FixSources = map[string]persistedTestFixSource{"preflight-only": {}}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := service.Delete(created.ID, "bob"); err != nil {
		t.Fatalf("shared delete error = %v", err)
	}
	if _, err := service.Get(created.ID, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get after delete error = %v", err)
	}
	if _, err := service.Find(ref, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Find after delete error = %v", err)
	}
	if err := service.Delete(created.ID, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("repeat Delete error = %v", err)
	}
	replacement, err := service.Create(ref, "alice", testRequestID(t))
	if err != nil {
		t.Fatalf("delete did not release owner capacity: %v", err)
	}
	if replacement.ID == created.ID || len(replacement.Messages) != 0 || replacement.TurnsUsed != 0 {
		t.Fatalf("replacement session = %+v", replacement)
	}
}

func TestServiceDeleteRejectsActiveSharedSessionAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "explains"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	first, err := NewService(t.Context(), dir, runner, Options{PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-cross-delete")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Send(context.Background(), created.ID, "alice", "turn-cross-delete", "in flight")
		done <- err
	}()
	<-runner.started
	if err := second.Delete(created.ID, "bob"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("delete active shared session error = %v", err)
	}
	observed, err := second.Get(created.ID, "bob")
	if err != nil || observed.Active == nil || observed.Active.Actor != "alice" {
		t.Fatalf("observed active session = %+v err=%v", observed.Active, err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.Delete(created.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(created.ID, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("deleted session remained visible: %v", err)
	}
}

func TestServiceResolvesTrimmedPublishedTestName(t *testing.T) {
	dir := t.TempDir()
	testCase := analyzedTest(" TestCluster ", "junit.xml", "2026-07-23T12:00:00Z")
	writeJobDetail(t, dir, testDetail(testCase))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
	}, "alice", testRequestID(t))

	if err != nil {
		t.Fatal(err)
	}
	if created.Analysis.TestName != "TestCluster" {
		t.Fatalf("canonical test name = %q", created.Analysis.TestName)
	}
}

func TestServiceRunnerFailuresReachTurnLimit(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{err: errors.New("model unavailable")}
	service, err := NewService(t.Context(), dir, runner, Options{MaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "retry"); err == nil || errors.Is(err, ErrTurnLimit) {
			t.Fatalf("attempt %d error = %v", i+1, err)
		}
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "retry again"); !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("third attempt error = %v", err)
	}
	runner.mu.Lock()
	attempts := len(runner.turns)
	runner.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("runner attempts = %d, want 2", attempts)
	}
	usage, err := service.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TurnsUsed != 2 || usage.MaxTurns != 2 {
		t.Fatalf("failure usage = %d/%d", usage.TurnsUsed, usage.MaxTurns)
	}
	if len(usage.Attempts) != 2 {
		t.Fatalf("exhausted attempts = %+v", usage.Attempts)
	}
	for _, attempt := range usage.Attempts {
		if attempt.Outcome != requestFailed || attempt.FailureKind != failureModel || attempt.Question != "retry" {
			t.Fatalf("exhausted attempt = %+v", attempt)
		}
	}
}

func TestServiceRejectsPublicStateDirectory(t *testing.T) {
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	if _, err := NewService(t.Context(), dataDir, &fakeRunner{}, Options{StateDir: filepath.Join(dataDir, "chat")}); err == nil || !strings.Contains(err.Error(), "dot-prefixed") {
		t.Fatalf("visible state directory error = %v", err)
	}
	if _, err := NewService(t.Context(), dataDir, &fakeRunner{}, Options{StateDir: filepath.Join(dataDir, ".private", "chat")}); err != nil {
		t.Fatalf("hidden state directory: %v", err)
	}
	hiddenTarget := filepath.Join(dataDir, ".hidden-target")
	if err := os.MkdirAll(hiddenTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	visibleLink := filepath.Join(dataDir, "chat-link")
	if err := os.Symlink(hiddenTarget, visibleLink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(t.Context(), dataDir, &fakeRunner{}, Options{StateDir: visibleLink}); err == nil || !strings.Contains(err.Error(), "dot-prefixed") {
		t.Fatalf("visible symlink state directory error = %v", err)
	}
	if _, err := NewService(t.Context(), dataDir, &fakeRunner{}, Options{StateDir: t.TempDir()}); err != nil {
		t.Fatalf("external state directory: %v", err)
	}
}

func TestServicePersistsSessionsAndIdempotentResults(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	firstRunner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "supports"}}
	first, err := NewService(t.Context(), dir, firstRunner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(ref, "alice", "create-persist")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), created.ID, "alice", "turn-persist", "question"); err != nil {
		t.Fatal(err)
	}

	secondRunner := &fakeRunner{reply: Reply{Answer: "duplicate", Assessment: "explains"}}
	second, err := NewService(t.Context(), dir, secondRunner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].RequestID != "turn-persist" || got.Messages[1].Content != "answer" {
		t.Fatalf("persisted messages = %+v", got.Messages)
	}
	persistedAttempt := requireAttempt(t, got, "turn-persist")
	if persistedAttempt.Outcome != requestSucceeded || persistedAttempt.Question != "question" || persistedAttempt.Turn != 1 {
		t.Fatalf("persisted attempt = %+v", persistedAttempt)
	}
	recreated, err := second.Create(ref, "alice", "create-persist")
	if err != nil {
		t.Fatal(err)
	}
	if recreated.ID != created.ID {
		t.Fatalf("idempotent create ID = %q, want %q", recreated.ID, created.ID)
	}
	if _, err := second.Create(AnalysisRef{JobID: "other", BuildID: "123", TestName: "TestCluster"}, "alice", "create-persist"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("create key conflict error = %v", err)
	}
	replayed, err := second.Send(context.Background(), created.ID, "alice", "turn-persist", "question")
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Messages) != 2 {
		t.Fatalf("replayed messages = %+v", replayed.Messages)
	}
	secondRunner.mu.Lock()
	calls := len(secondRunner.turns)
	secondRunner.mu.Unlock()
	if calls != 0 {
		t.Fatalf("replayed request ran model %d times", calls)
	}
	info, err := os.Stat(filepath.Join(dir, ".analysis-chat", stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}
}

func TestServiceSerializesTurnsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	first, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-shared")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Send(context.Background(), created.ID, "alice", "turn-shared", "question")
		done <- err
	}()
	<-runner.started
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-shared", "question"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("same request while active error = %v", err)
	}
	if _, err := second.Send(context.Background(), created.ID, "bob", "turn-other", "other question"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("different request while active error = %v", err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := second.Send(context.Background(), created.ID, "alice", "turn-shared", "question")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	runner.mu.Lock()
	calls := len(runner.turns)
	runner.mu.Unlock()
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestServicePersistsFailedRequestOutcome(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	failing := &fakeRunner{err: errors.New("model unavailable")}
	first, err := NewService(t.Context(), dir, failing, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(context.Background(), created.ID, "alice", "turn-failure", "question"); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("failed turn error = %v", err)
	}
	failed, err := first.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if failed.TurnsUsed != 1 || failed.MaxTurns != 10 {
		t.Fatalf("failed usage = %d/%d", failed.TurnsUsed, failed.MaxTurns)
	}

	succeeding := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "supports"}}
	second, err := NewService(t.Context(), dir, succeeding, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-failure", "question"); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("replayed failed request error = %v", err)
	}
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-failure", "different"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("message key conflict error = %v", err)
	}
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-retry", "question"); err != nil {
		t.Fatal(err)
	}
	succeeding.mu.Lock()
	calls := len(succeeding.turns)
	succeeding.mu.Unlock()
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1 explicit retry", calls)
	}
	retried, err := second.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if retried.TurnsUsed != 2 || retried.MaxTurns != 10 {
		t.Fatalf("retry usage = %d/%d", retried.TurnsUsed, retried.MaxTurns)
	}
	failedAttempt := requireAttempt(t, retried, "turn-failure")
	successAttempt := requireAttempt(t, retried, "turn-retry")
	if failedAttempt.Outcome != requestFailed || successAttempt.Outcome != requestSucceeded || failedAttempt.Turn != 1 || successAttempt.Turn != 2 {
		t.Fatalf("retry attempts = %+v", retried.Attempts)
	}
}

func TestServiceRestoresSafeFailureAttemptCategories(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
		kind string
	}{
		{name: "provider", err: fmt.Errorf("%w: token=provider-secret /private/provider/path", ErrProviderRequestFailed), want: ErrProviderRequestFailed, kind: failureProvider},
		{name: "validation", err: fmt.Errorf("%w: raw model prompt", ErrResponseValidationFailed), want: ErrResponseValidationFailed, kind: failureValidation},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
			service, err := NewService(t.Context(), dir, &fakeRunner{err: testCase.err}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-"+testCase.name)
			if err != nil {
				t.Fatal(err)
			}
			requestID := "turn-" + testCase.name
			if _, err := service.Send(t.Context(), created.ID, "alice", requestID, "What failed safely?"); !errors.Is(err, testCase.want) {
				t.Fatalf("send error = %v", err)
			}
			restored, err := service.Get(created.ID, "alice")
			if err != nil {
				t.Fatal(err)
			}
			attempt := requireAttempt(t, restored, requestID)
			if attempt.Outcome != requestFailed || attempt.FailureKind != testCase.kind || attempt.Question != "What failed safely?" {
				t.Fatalf("restored attempt = %+v", attempt)
			}
			encoded, err := json.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"provider-secret", "/private/provider/path", "raw model prompt", "private citation path"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("attempt leaked %q: %s", private, encoded)
				}
			}
		})
	}
}

func TestServiceRestoresTimedOutAttempt(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	service, err := NewService(t.Context(), dir, runner, Options{TurnTimeout: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", "turn-timeout", "Why did this time out?"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	restored, err := service.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	attempt := requireAttempt(t, restored, "turn-timeout")
	if attempt.Outcome != "timed_out" || attempt.FailureKind != "" || attempt.Question != "Why did this time out?" {
		t.Fatalf("timed out attempt = %+v", attempt)
	}
}

func TestRequestFailureCategoriesRoundTrip(t *testing.T) {
	cases := []struct {
		err  error
		kind string
	}{
		{ErrProviderRequestFailed, failureProvider},
		{ErrResponseValidationFailed, failureValidation},
	}
	for _, testCase := range cases {
		if got := requestFailureKind(fmt.Errorf("wrapped: %w", testCase.err)); got != testCase.kind {
			t.Errorf("requestFailureKind(%v) = %q, want %q", testCase.err, got, testCase.kind)
		}
		if got := persistedRequestError(testCase.kind, ""); !errors.Is(got, testCase.err) {
			t.Errorf("persistedRequestError(%q) = %v", testCase.kind, got)
		}
	}
}

func TestServiceRecoversExpiredTurnLease(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	opts := Options{Now: now, SessionTTL: time.Minute, TurnTimeout: 30 * time.Second, TurnLeaseTTL: time.Minute}
	first, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-lease")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Send(context.Background(), created.ID, "alice", "turn-stale", "question")
		done <- err
	}()
	<-runner.started
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-stale", "question"); !errors.Is(err, ErrRequestOutcomeUnknown) {
		t.Fatalf("expired lease replay error = %v", err)
	}
	close(runner.release)
	if err := <-done; !errors.Is(err, ErrRequestOutcomeUnknown) {
		t.Fatalf("expired lease completion error = %v", err)
	}
	unknown, err := second.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.TurnsUsed != 1 || unknown.MaxTurns != 10 {
		t.Fatalf("unknown usage = %d/%d", unknown.TurnsUsed, unknown.MaxTurns)
	}
	unknownAttempt := requireAttempt(t, unknown, "turn-stale")
	if unknownAttempt.Outcome != requestUnknown || unknownAttempt.Question != "question" {
		t.Fatalf("unknown attempt = %+v", unknownAttempt)
	}
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-after-stale", "question"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	calls := len(runner.turns)
	runner.mu.Unlock()
	if calls != 2 {
		t.Fatalf("runner calls = %d, want abandoned plus explicit retry", calls)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	for _, service := range []*Service{first, second} {
		if err := service.Wait(waitCtx); err != nil {
			t.Fatalf("waiting for recovered turns: %v", err)
		}
	}
}

func TestServiceExpiredCancelledTurnRestoresCancellation(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}), ignoreContext: true,
	}
	opts := Options{Now: now, SessionTTL: time.Minute, TurnTimeout: 30 * time.Second, TurnLeaseTTL: time.Minute, PollInterval: 10 * time.Millisecond}
	first, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-expired-cancel")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Stream(t.Context(), created.ID, "alice", "turn-expired-cancel", "cancel this question", nil)
		done <- err
	}()
	<-runner.started
	if err := second.Cancel(created.ID, "alice", "turn-expired-cancel"); err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	restored, err := second.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	attempt := requireAttempt(t, restored, "turn-expired-cancel")
	if attempt.Outcome != failureCancelled || attempt.Question != "cancel this question" || len(restored.Messages) != 0 {
		t.Fatalf("expired cancelled attempt = %+v messages=%+v", attempt, restored.Messages)
	}
	close(runner.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expired cancelled stream error = %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
	defer waitCancel()
	if err := first.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for expired cancelled turn: %v", err)
	}
}

func TestServiceStartupCleanupRemovesExpiredPersistence(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	firstCtx, cancel := context.WithCancel(t.Context())
	first, err := NewService(firstCtx, dir, &fakeRunner{}, Options{
		Now: now, SessionTTL: time.Minute, CleanupInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-startup-cleanup"); err != nil {
		t.Fatal(err)
	}
	cancel()
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	if _, err := NewService(t.Context(), dir, &fakeRunner{}, Options{
		Now: now, SessionTTL: time.Minute, CleanupInterval: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if got := persistedSessionCount(t, dir); got != 0 {
		t.Fatalf("persisted sessions after startup cleanup = %d", got)
	}
}

func TestServicePeriodicCleanupBoundsPersistenceRetention(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{
		Now: now, SessionTTL: time.Minute, CleanupInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-periodic-cleanup"); err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	deadline := time.Now().Add(time.Second)
	for persistedSessionCount(t, dir) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("periodic cleanup did not remove expired persisted session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func persistedSessionCount(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".analysis-chat", stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return len(state.Sessions)
}

func TestServiceTurnContinuesAfterWaiterDisconnect(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	service, err := NewService(t.Context(), dir, runner, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-disconnect")
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := service.Send(waitCtx, created.ID, "alice", "turn-disconnect", "question")
		done <- err
	}()
	<-runner.started
	if err := <-done; !errors.Is(err, ErrRequestPending) {
		t.Fatalf("disconnected waiter error = %v", err)
	}
	close(runner.release)
	deadline := time.Now().Add(time.Second)
	for {
		got, err := service.Get(created.ID, "alice")
		if err == nil && len(got.Messages) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background turn did not finish: session=%+v err=%v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServiceStreamReconnectsToPendingTurn(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
		phases: []string{PhaseReadingEvidence, PhaseValidationRetrying, PhaseEvaluating},
	}
	service, err := NewService(t.Context(), dir, runner, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-stream")
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Stream(firstCtx, created.ID, "alice", "turn-stream", "question", nil)
		firstDone <- err
	}()
	<-runner.started
	if err := <-firstDone; !errors.Is(err, ErrRequestPending) {
		t.Fatalf("first stream error = %v", err)
	}
	pending, err := service.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	pendingAttempt := requireAttempt(t, pending, "turn-stream")
	if pendingAttempt.Outcome != requestPending || pendingAttempt.Question != "question" || pendingAttempt.Turn != 1 {
		t.Fatalf("pending attempt = %+v", pendingAttempt)
	}

	var phases []string
	var latestProgress Progress
	progressed := make(chan struct{}, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "alice", "turn-stream", "question", func(progress Progress) error {
			phases = append(phases, progress.Phase)
			latestProgress = progress
			select {
			case progressed <- struct{}{}:
			default:
			}
			return nil
		})
		secondDone <- err
	}()
	select {
	case <-progressed:
	case <-time.After(time.Second):
		t.Fatal("reconnected stream received no persisted progress")
	}
	close(runner.release)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if len(phases) == 0 {
		t.Fatal("reconnected stream received no persisted progress")
	}
	if latestProgress.TurnsUsed != 1 || latestProgress.MaxTurns != 10 {
		t.Fatalf("progress usage = %d/%d", latestProgress.TurnsUsed, latestProgress.MaxTurns)
	}
	if latestProgress.StartedAt == "" || latestProgress.ValidationRetries != 1 || latestProgress.MaxValidationRetries != 1 {
		t.Fatalf("progress retry metadata = %+v", latestProgress)
	}
	runner.mu.Lock()
	calls := len(runner.turns)
	runner.mu.Unlock()
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestServiceCancelAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	opts := Options{PollInterval: 10 * time.Millisecond}
	first, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-cancel")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Stream(t.Context(), created.ID, "alice", "turn-cancel", "question", nil)
		done <- err
	}()
	<-runner.started
	if err := second.Cancel(created.ID, "bob", "turn-cancel"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("observer cancel error = %v", err)
	}
	if err := second.Cancel(created.ID, "alice", "turn-cancel"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream error = %v", err)
	}
	if err := second.Cancel(created.ID, "alice", "turn-cancel"); err != nil {
		t.Fatalf("idempotent terminal cancel = %v", err)
	}
	cancelled, err := second.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.TurnsUsed != 1 || cancelled.MaxTurns != 10 {
		t.Fatalf("cancelled usage = %d/%d", cancelled.TurnsUsed, cancelled.MaxTurns)
	}
	cancelledAttempt := requireAttempt(t, cancelled, "turn-cancel")
	if cancelledAttempt.Outcome != failureCancelled || cancelledAttempt.Question != "question" {
		t.Fatalf("cancelled attempt = %+v", cancelledAttempt)
	}
	shared, err := second.Get(created.ID, "bob")
	if err != nil || requireAttempt(t, shared, "turn-cancel").Actor != "alice" {
		t.Fatalf("shared attempt history = %+v err=%v", shared.Attempts, err)
	}
}

func TestServiceOwnerActiveTurnAndRateLimits(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(
		analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z"),
		analyzedTest("TestOther", "junit.xml", "2026-07-23T12:00:00Z"),
	))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	opts := Options{
		PollInterval:                 10 * time.Millisecond,
		MaxActiveTurnsPerOwner:       1,
		MaxRequestsPerOwnerPerMinute: 2,
	}
	service, err := NewService(t.Context(), dir, runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	first, err := service.Create(ref, "alice", "create-limit-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestOther"}, "alice", "create-limit-2")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), first.ID, "alice", "turn-limit-1", "question", nil)
		done <- err
	}()
	<-runner.started
	if _, err := service.Send(context.Background(), second.ID, "alice", "turn-limit-2", "question"); !errors.Is(err, ErrActiveTurnLimit) {
		t.Fatalf("active turn limit error = %v", err)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), second.ID, "alice", "turn-limit-3", "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), second.ID, "alice", "turn-limit-4", "question"); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate limit error = %v", err)
	}
}

func TestServiceLifecycleCancelsActiveTurn(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	lifecycle, cancelLifecycle := context.WithCancel(t.Context())
	service, err := NewService(lifecycle, dir, runner, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "alice", "turn-lifecycle", "question", nil)
		done <- err
	}()
	<-runner.started
	cancelLifecycle()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle cancellation error = %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := service.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for lifecycle-cancelled turn: %v", err)
	}
}

func TestServiceRateLimitWindowExpires(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	runner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "supports"}}
	service, err := NewService(t.Context(), dir, runner, Options{
		Now: now, PollInterval: 10 * time.Millisecond, MaxRequestsPerOwnerPerMinute: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-rate-window")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "turn-rate-window-1", "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "turn-rate-window-2", "question"); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate limit error = %v", err)
	}
	nowNanos.Store(start.Add(time.Minute + time.Second).UnixNano())
	if _, err := service.Send(context.Background(), created.ID, "alice", "turn-rate-window-3", "question"); err != nil {
		t.Fatalf("expired rate window error = %v", err)
	}
}

func TestServicePersistedCancellationWinsOverSuccessfulReply(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}), ignoreContext: true,
	}
	service, err := NewService(t.Context(), dir, runner, Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-cancel-race")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "alice", "turn-cancel-race", "question", nil)
		done <- err
	}()
	<-runner.started
	if err := service.Cancel(created.ID, "alice", "turn-cancel-race"); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel-versus-success result = %v", err)
	}
	got, err := service.Get(created.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("cancelled reply was published: %+v", got.Messages)
	}
}

func TestServiceLocalNotificationAvoidsPollDelay(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{
		reply:   Reply{Answer: "answer", Assessment: "supports"},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	service, err := NewService(t.Context(), dir, runner, Options{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-local-notify")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "alice", "turn-local-notify", "question", nil)
		done <- err
	}()
	<-runner.started
	close(runner.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("local waiter slept until the cross-replica poll interval")
	}
}

func recurringPattern() models.PatternAnalysis {
	pattern := models.PatternAnalysis{
		Subject: "controller retry failures", JobID: "periodic-demo", GeneratedAt: "2026-07-25T12:00:00Z",
		BuildsAnalyzed: 4, Systemic: true, Confidence: "high",
		SharedRootCause: "terminal failures are retried", SharedBuilds: []string{"104", "103", "102", "101"},
		SuggestedFix: "stop retrying terminal failures", RelevantFiles: []string{"pkg/retry.go"}, Summary: "shared retry failure",
	}
	pattern.ID = models.PatternID(pattern)
	pattern.ContentHash = models.PatternHash(pattern)
	return pattern
}

func patternDetail() models.JobDetail {
	detail := models.JobDetail{Name: "periodic-demo", JobID: "periodic-demo", JobType: models.JobTypePeriodic}
	for _, id := range []string{"104", "103", "102", "101"} {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{BuildID: id, JobName: "periodic-demo"}})
	}
	detail.PatternAnalyses = []models.PatternAnalysis{recurringPattern()}
	return detail
}

func TestServiceFindSeparatesTestAndPatternSessions(t *testing.T) {
	dir := t.TempDir()
	detail := patternDetail()
	detail.Runs[0].TestCases = []models.TestCase{analyzedTest("TestCluster", "junit.xml", "2026-07-26T12:00:00Z")}
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	testRef := AnalysisRef{JobID: "periodic-demo", BuildID: "104", TestName: "TestCluster"}
	testSession, err := service.Create(testRef, "alice", "create-test-session")
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	patternRef := AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}
	patternSession, err := service.Create(patternRef, "alice", "create-pattern-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), testSession.ID, "alice", "turn-test-session", "test question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), patternSession.ID, "alice", "turn-pattern-session", "pattern question"); err != nil {
		t.Fatal(err)
	}
	foundTest, err := service.Find(testRef, "alice")
	if err != nil {
		t.Fatal(err)
	}
	foundPattern, err := service.Find(patternRef, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if foundTest.ID != testSession.ID || foundPattern.ID != patternSession.ID || foundTest.ID == foundPattern.ID {
		t.Fatalf("test=%q pattern=%q", foundTest.ID, foundPattern.ID)
	}
	if requireAttempt(t, foundTest, "turn-test-session").Question != "test question" ||
		requireAttempt(t, foundPattern, "turn-pattern-session").Question != "pattern question" {
		t.Fatalf("test attempts=%+v pattern attempts=%+v", foundTest.Attempts, foundPattern.Attempts)
	}
	for _, attempt := range foundTest.Attempts {
		if attempt.RequestID == "turn-pattern-session" {
			t.Fatalf("pattern attempt leaked into test session: %+v", foundTest.Attempts)
		}
	}
	for _, attempt := range foundPattern.Attempts {
		if attempt.RequestID == "turn-test-session" {
			t.Fatalf("test attempt leaked into pattern session: %+v", foundPattern.Attempts)
		}
	}
}

func TestServicePatternChatUsesBoundedAffectedBuilds(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	runner := &fakeRunner{reply: Reply{Answer: "The pattern spans the three newest retained builds.", Assessment: "explains"}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	created, err := service.Create(AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if created.Analysis.Scope != ScopePattern || created.Analysis.BuildID != "" || created.Analysis.PatternHash != pattern.ContentHash {
		t.Fatalf("created pattern session = %+v", created.Analysis)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", testRequestID(t), "What builds support this?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.Pattern == nil || turn.Pattern.ID != pattern.ID || len(turn.EvidenceBuilds) != maxPatternEvidenceBuilds {
		t.Fatalf("pattern turn = %+v", turn)
	}
	for i, want := range []string{"104", "103", "102"} {
		if turn.EvidenceBuilds[i].Build.BuildID != want {
			t.Fatalf("evidence builds = %+v", turn.EvidenceBuilds)
		}
	}
}

func TestServicePatternChatRejectsStaleContentHash(t *testing.T) {
	dir := t.TempDir()
	detail := patternDetail()
	pattern := detail.PatternAnalyses[0]
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	detail.PatternAnalyses[0].SuggestedFix = "replace the controller"
	detail.PatternAnalyses[0].ContentHash = models.PatternHash(detail.PatternAnalyses[0])
	writeJobDetail(t, dir, detail)
	_, err = service.Create(AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}, "alice", testRequestID(t))
	if !errors.Is(err, ErrPatternChanged) {
		t.Fatalf("stale pattern error = %v", err)
	}
}

func TestPatternChatSnapshotPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	stateDir := filepath.Join(dir, ".pattern-chat")
	first, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	created, err := first.Create(AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: Reply{Answer: "persisted", Assessment: "explains"}}
	restarted, err := NewService(t.Context(), dir, runner, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Send(t.Context(), created.ID, "Alice", testRequestID(t), "What persisted?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.Pattern == nil || turn.Pattern.ContentHash != pattern.ContentHash || len(turn.EvidenceBuilds) != 3 {
		t.Fatalf("restored turn = %+v", turn)
	}
}

func TestPatternChatRejectsTestOnlyExtensions(t *testing.T) {
	service, err := NewService(t.Context(), t.TempDir(), &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []AnalysisRef{
		{Scope: ScopePattern, JobID: "job", PatternID: "pattern"},
		{Scope: ScopePattern, JobID: "job", BuildID: "123", PatternID: "pattern", PatternHash: "hash"},
		{Scope: ScopePattern, JobID: "job", PatternID: "pattern", PatternHash: "hash", TestName: "test"},
		{Scope: ScopePattern, JobID: "job", PatternID: "pattern", PatternHash: "hash", CausalGroupID: "cause", CausalGroupHash: "cause-hash"},
		{Scope: ScopeCause, JobID: "job", PatternID: "pattern", PatternHash: "hash", CausalGroupID: "cause"},
		{Scope: ScopeCause, JobID: "job", PatternID: "pattern", PatternHash: "hash", CausalGroupID: "cause", CausalGroupHash: "cause-hash", BuildID: "123"},
		{Scope: "other", JobID: "job", PatternID: "pattern", PatternHash: "hash"},
	} {
		if _, err := service.Create(ref, "alice", testRequestID(t)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("ref %+v error = %v", ref, err)
		}
	}
}

func TestVersionTwoDuplicateSessionsCannotRunParallelAfterMigration(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	stateDir := filepath.Join(dir, ".shared-migration-chat")
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{Scope: ScopeTest, JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	resolved, err := service.resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	persisted := persistResolved(resolved, sourceinvestigation.Repository{})
	legacy := &persistedState{
		Version: 2,
		Sessions: map[string]*persistedSession{
			"alice-session": {
				Owner: "alice", Resolved: persisted, ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "alice-session", Analysis: resolved.ref, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339)},
			},
			"bob-session": {
				Owner: "bob", Resolved: persisted, Turns: 1, ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "bob-session", Analysis: resolved.ref, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
				Requests: map[string]persistedRequest{
					"active": {QuestionHash: hashText("question"), Question: "question", Status: requestPending, Turn: 1},
				},
				Active: &persistedActiveTurn{RequestID: "active", Question: "question", LeaseID: "lease", ExpiresAt: now.Add(time.Minute), Phase: PhaseInvestigating, UpdatedAt: now},
			},
		},
		OwnerRequests: map[string][]time.Time{},
	}
	if err := writePrivateJSON(service.store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Get("alice-session", "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("retired duplicate Get error = %v", err)
	}
	if _, err := restarted.Send(t.Context(), "alice-session", "alice", "old-tab-turn", "parallel question"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("retired duplicate Send error = %v", err)
	}
	shared, err := restarted.Find(ref, "alice")
	if err != nil || shared.ID != "bob-session" || shared.Active == nil || shared.Active.Actor != "bob" {
		t.Fatalf("canonical shared session = %+v err=%v", shared, err)
	}
}

func TestVersionOneCreateIdempotencyMigratesOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	stateDir := filepath.Join(dir, ".migration-chat")
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	legacyRef := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	resolved, err := service.resolve(legacyRef)
	if err != nil {
		t.Fatal(err)
	}
	legacyHash, err := hashAnalysisRef(legacyRef)
	if err != nil {
		t.Fatal(err)
	}
	persisted := persistResolved(resolved, sourceinvestigation.Repository{})
	persisted.Ref.Scope = ""
	expires := time.Now().UTC().Add(time.Hour)
	legacy := &persistedState{
		Version: 1,
		Sessions: map[string]*persistedSession{
			"legacy-session": {
				Owner: "alice", Resolved: persisted, ExpiresAt: expires,
				CreateRequestID: "legacy-create", CreateRequestHash: legacyHash,
				View: SessionView{ID: "legacy-session", Analysis: legacyRef, ExpiresAt: expires.Format(time.RFC3339)},
			},
		},
		OwnerRequests: map[string][]time.Time{},
	}
	if err := writePrivateJSON(service.store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Create(legacyRef, "Alice", "legacy-create")
	if err != nil || got.ID != "legacy-session" {
		t.Fatalf("retry session=%+v err=%v", got, err)
	}
	state, _, err := restarted.store.load()
	if err != nil {
		t.Fatal(err)
	}
	migrated := state.Sessions["legacy-session"]
	if migrated.CreateRequestVersion != createVersion || migrated.CreateRequestHash == legacyHash {
		t.Fatalf("create migration = %+v", migrated)
	}
}

func TestRetainedPatternChatRequiresCompleteEvidence(t *testing.T) {
	dir := t.TempDir()
	detail := patternDetail()
	detail.PatternRefresh = &models.PatternRefreshStatus{State: models.PatternRefreshRetained, EvidenceAvailable: false}
	detail.Runs = detail.Runs[:1]
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	_, err = service.Create(AnalysisRef{Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash}, "Alice", testRequestID(t))
	if !errors.Is(err, ErrAnalysisNotFound) {
		t.Fatalf("Create error = %v", err)
	}
}

func TestServiceCreateBuildAnalysisWithoutJUnitFile(t *testing.T) {
	dir := t.TempDir()
	build := analyzedTest("Prow job execution", "", "2026-07-30T12:00:00Z")
	build.Source = models.TestCaseSourceBuild
	writeJobDetail(t, dir, testDetail(build))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: build.Name,
		Source: models.TestCaseSourceBuild, SuiteName: build.SuiteName, ClassName: build.ClassName,
		AnalysisGeneratedAt: build.AIAnalysis.GeneratedAt,
	}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if created.Analysis.Source != models.TestCaseSourceBuild || created.Analysis.JUnitFile != "" {
		t.Fatalf("build analysis reference = %+v", created.Analysis)
	}
	if _, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: build.Name}, "alice", testRequestID(t)); !errors.Is(err, ErrAnalysisNotFound) {
		t.Fatalf("legacy test reference resolved build subject: %v", err)
	}
	if _, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: build.Name, Source: models.TestCaseSourceBuild,
		AnalysisGeneratedAt: "2026-07-30T13:00:00Z",
	}, "alice", testRequestID(t)); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed build analysis error = %v", err)
	}
}

type usageTestRunner struct{}

func (usageTestRunner) Reply(ctx context.Context, _ Turn) (Reply, error) {
	aiusage.ObserveModelRequest(ctx, aiusage.TokenUsage{Reported: true, InputTokens: 8, OutputTokens: 2})
	return Reply{Answer: "answer", Assessment: "explains"}, nil
}

func TestServiceRecordsTurnUsage(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	usage, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 30, RecentOperations: 10})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), dir, usageTestRunner{}, Options{UsageRecorder: usage, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-usage")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", "turn-usage", "question"); err != nil {
		t.Fatal(err)
	}
	snapshot := usage.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.Days[0].Totals.InputTokens != 8 || snapshot.RecentOperations[0].Feature != aiusage.FeatureAnalysisChat {
		t.Fatalf("usage = %+v", snapshot)
	}
}

func causalPatternForChat(groups []models.PatternCausalGroup, unclassified []string) models.PatternAnalysis {
	pattern := models.PatternAnalysis{
		Subject: "causal retry failures", JobID: "periodic-demo", GeneratedAt: "2026-08-12T12:00:00Z",
		BuildsAnalyzed: 10, Systemic: true, Confidence: "medium",
		Recurrence: models.PatternRecurrenceMixedCauses, CausalGroups: groups,
		UnclassifiedBuilds: unclassified, Summary: "distinct causal groups",
		Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleObserving, Reason: "watching later passing builds", SourceRevision: "private-source"},
	}
	for index := range pattern.CausalGroups {
		if pattern.CausalGroups[index].ContentHash == "" {
			pattern.CausalGroups[index].ContentHash = models.PatternCausalGroupHash(pattern.CausalGroups[index])
		}
		if pattern.CausalGroups[index].ID == "" {
			pattern.CausalGroups[index].ID = fmt.Sprintf("group-%d", index+1)
		}
		if len(pattern.CausalGroups[index].Builds) >= 2 {
			pattern.SharedBuilds = append(pattern.SharedBuilds, pattern.CausalGroups[index].Builds...)
		}
	}
	pattern.ID = models.PatternID(pattern)
	pattern.ContentHash = models.PatternHash(pattern)
	return pattern
}

func causalPatternDetail(pattern models.PatternAnalysis, buildIDs ...string) models.JobDetail {
	detail := models.JobDetail{Name: "periodic-demo", JobID: "periodic-demo", JobType: models.JobTypePeriodic}
	for index, id := range buildIDs {
		detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
			BuildID: id, JobName: "periodic-demo", Started: time.Date(2026, time.August, 12, 12, index, 0, 0, time.UTC),
		}})
	}
	detail.PatternAnalyses = []models.PatternAnalysis{pattern}
	return detail
}

func TestSelectPatternEvidenceRunsPrioritizesRepeatedGroups(t *testing.T) {
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{ID: "group-a", Builds: []string{"101", "104"}, RootCause: "cause a", Confidence: "high"},
		{ID: "singleton", Builds: []string{"999"}, RootCause: "outlier", Confidence: "low"},
		{ID: "group-b", Builds: []string{"102", "103"}, RootCause: "cause b", Confidence: "medium"},
	}, []string{"98"})
	detail := causalPatternDetail(pattern, "101", "104", "999", "102", "103", "98", "outside")
	runs, available, eligible := selectPatternEvidenceRuns(pattern, detail.Runs)
	got := make([]string, 0, len(runs))
	for _, run := range runs {
		got = append(got, run.BuildID)
	}
	if !slices.Equal(got, []string{"104", "103", "102"}) || available != 4 || eligible != 4 {
		t.Fatalf("runs=%v available=%d eligible=%d", got, available, eligible)
	}
	for _, forbidden := range []string{"999", "98", "outside"} {
		if slices.Contains(got, forbidden) {
			t.Fatalf("selected non-repeated build %q: %v", forbidden, got)
		}
	}
}

func TestSelectPatternEvidenceRunsMoreGroupsThanSlots(t *testing.T) {
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{ID: "a", Builds: []string{"8", "7"}, RootCause: "a", Confidence: "high"},
		{ID: "b", Builds: []string{"6", "5"}, RootCause: "b", Confidence: "high"},
		{ID: "c", Builds: []string{"4", "3"}, RootCause: "c", Confidence: "high"},
		{ID: "d", Builds: []string{"2", "1"}, RootCause: "d", Confidence: "high"},
	}, nil)
	detail := causalPatternDetail(pattern, "1", "2", "3", "4", "5", "6", "7", "8")
	runs, _, _ := selectPatternEvidenceRuns(pattern, detail.Runs)
	got := []string{runs[0].BuildID, runs[1].BuildID, runs[2].BuildID}
	if !slices.Equal(got, []string{"8", "6", "4"}) {
		t.Fatalf("selected runs = %v", got)
	}
}

func TestServicePatternChatPersistsCurrentCausalContext(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{ID: "group-a", Builds: []string{"104", "103"}, RootCause: "cause a", Confidence: "high"},
		{ID: "singleton", Builds: []string{"102"}, RootCause: "outlier", Confidence: "low"},
		{ID: "group-b", Builds: []string{"101", "100"}, RootCause: "cause b", Confidence: "medium"},
	}, []string{"99"})
	pattern.ContentHash = models.PatternHash(pattern)
	writeJobDetail(t, dir, causalPatternDetail(pattern, "104", "103", "102", "101", "100", "99"))
	stateDir := filepath.Join(dir, ".causal-pattern-chat")
	first, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(AnalysisRef{Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: Reply{Answer: "persisted", Assessment: "explains"}}
	restarted, err := NewService(t.Context(), dir, runner, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Send(t.Context(), created.ID, "Alice", testRequestID(t), "What persisted?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.Pattern == nil || turn.Pattern.Recurrence != models.PatternRecurrenceMixedCauses || len(turn.Pattern.CausalGroups) != 3 ||
		!slices.Equal(turn.Pattern.UnclassifiedBuilds, []string{"99"}) || turn.Pattern.Lifecycle == nil || turn.Pattern.Lifecycle.State != models.PatternLifecycleObserving {
		t.Fatalf("restored pattern = %+v", turn.Pattern)
	}
	gotBuilds := make([]string, 0, len(turn.EvidenceBuilds))
	for _, build := range turn.EvidenceBuilds {
		gotBuilds = append(gotBuilds, build.Build.BuildID)
	}
	if !slices.Equal(gotBuilds, []string{"103", "100", "101"}) {
		t.Fatalf("evidence builds = %v", gotBuilds)
	}
}

func TestServiceCauseChatAddsNewestCompletedComparisonBuild(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{ID: "other", Builds: []string{"103"}, RootCause: "different cause", Confidence: "low"},
		{
			ID: "selected", Builds: []string{"101", "102"}, RootCause: "shared selected cause", Confidence: "high",
			CauseLocation: &models.AnalysisCauseLocation{Repository: "example/repo", Files: []string{"pkg/cause.go"}},
			Remediation:   &models.PatternCausalGroupRemediation{SuggestedFix: "change the selected cause", BuildID: "102"},
		},
	}, nil)
	pattern.Systemic = false
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "101", "102", "103", "outside")
	for index := range detail.Runs {
		run := &detail.Runs[index]
		switch run.BuildID {
		case "101", "102":
			run.Result = "FAILURE"
			run.TestCases = []models.TestCase{analyzedTest("TestCluster", "junit.xml", "2026-08-12T12:00:00Z")}
		case "103":
			run.Result = "PENDING"
		case "outside":
			run.Result = "SUCCESS"
			run.Passed = true
			run.Commit = "comparison-commit"
		}
	}
	slices.Reverse(detail.Runs)
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: Reply{Answer: "The selected cause spans two builds.", Assessment: "explains"}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	group := pattern.CausalGroups[1]
	created, err := service.Create(AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if created.Analysis.Scope != ScopeCause || created.Analysis.CausalGroupID != group.ID || created.Analysis.BuildID != "" {
		t.Fatalf("created cause session = %+v", created.Analysis)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", testRequestID(t), "What should change?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.Scope != ScopeCause || turn.Pattern == nil || len(turn.Pattern.CausalGroups) != 1 || turn.Pattern.CausalGroups[0].ID != group.ID {
		t.Fatalf("cause turn = %+v", turn)
	}
	if turn.Pattern.Systemic || turn.TestCase.AIAnalysis == nil || turn.TestCase.AIAnalysis.RootCause != group.RootCause ||
		turn.TestCase.AIAnalysis.SuggestedFix != group.Remediation.SuggestedFix ||
		!slices.Equal(turn.TestCase.AIAnalysis.RelevantFiles, []string{"pkg/cause.go"}) {
		t.Fatalf("cause analysis = %+v pattern=%+v", turn.TestCase.AIAnalysis, turn.Pattern)
	}
	gotBuilds := make([]string, 0, len(turn.EvidenceBuilds))
	for _, build := range turn.EvidenceBuilds {
		gotBuilds = append(gotBuilds, build.Build.BuildID)
	}
	if !slices.Equal(gotBuilds, []string{"102", "101"}) {
		t.Fatalf("cause evidence builds = %v", gotBuilds)
	}
	if turn.Build.BuildID != "102" || turn.BuildPrefix != "logs/periodic-demo/102/" {
		t.Fatalf("member anchor changed: build=%s prefix=%s", turn.Build.BuildID, turn.BuildPrefix)
	}
	if turn.Comparison == nil || turn.Comparison.ArtifactBuild.Build.BuildID != "outside" || !turn.Comparison.ArtifactBuild.Build.Passed ||
		turn.Comparison.ArtifactBuild.Build.Commit != "comparison-commit" || !slices.Equal(turn.Comparison.TestNames, []string{"TestCluster"}) {
		t.Fatalf("comparison = %+v", turn.Comparison)
	}
	if turn.Pattern.Lifecycle == nil || turn.Pattern.Lifecycle.RecoveryStreak != 1 || turn.Pattern.Lifecycle.State != models.PatternLifecycleActive {
		t.Fatalf("cause lifecycle = %+v", turn.Pattern.Lifecycle)
	}
}

func TestSelectCauseComparisonRunUsesNewestCompletedRun(t *testing.T) {
	base := time.Date(2026, time.August, 28, 0, 0, 0, 0, time.UTC)
	runs := []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "pending", Result: "PENDING", Started: base.Add(4 * time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "comparison", Result: "SUCCESS", Passed: true, Started: base.Add(3 * time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "member-new", Result: "FAILURE", Started: base.Add(2 * time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "member-old", Result: "FAILURE", Started: base.Add(time.Hour)}},
	}
	comparison := selectCauseComparisonRun(models.PatternCausalGroup{Builds: []string{"member-old", "member-new"}}, runs)
	if comparison == nil || comparison.BuildID != "comparison" {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func TestServiceCauseChatDoesNotReuseSessionAfterComparisonChanges(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"}}, nil)
	pattern.Systemic = false
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1", "2")
	detail.Runs[0].Result = "FAILURE"
	detail.Runs[0].TestCases = []models.TestCase{analyzedTest("TestCluster", "junit.xml", "2026-08-12T12:00:00Z")}
	detail.Runs[1].Result = "SUCCESS"
	detail.Runs[1].Passed = true
	writeJobDetail(t, dir, detail)
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Create(ref, "alice", "create-one")
	if err != nil {
		t.Fatal(err)
	}
	newer := models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: "3", JobName: detail.Name, Result: "SUCCESS", Passed: true,
		Started: detail.Runs[1].Started.Add(time.Minute),
	}}
	detail.Runs = append(detail.Runs, newer)
	writeJobDetail(t, dir, detail)
	if _, err := service.Find(ref, "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Find error = %v", err)
	}
	second, err := service.Create(ref, "alice", "create-two")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("reused stale session %s", first.ID)
	}
}

func TestServiceCauseChatRejectsChangedIdentityAndMissingEvidence(t *testing.T) {
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{Builds: []string{"2", "1"}, RootCause: "original", Confidence: "high"},
		{Builds: []string{"3"}, RootCause: "other", Confidence: "low"},
	}, nil)
	models.AssignPatternIdentity(&pattern)
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	for _, testCase := range []struct {
		name string
		edit func(*models.JobDetail, *AnalysisRef)
		want error
	}{
		{name: "parent changed", edit: func(detail *models.JobDetail, _ *AnalysisRef) {
			detail.PatternAnalyses[0].CausalGroups[1].RootCause = "changed other cause"
			detail.PatternAnalyses[0].ContentHash = models.PatternHash(detail.PatternAnalyses[0])
		}, want: ErrPatternChanged},
		{name: "cause hash changed", edit: func(_ *models.JobDetail, ref *AnalysisRef) {
			ref.CausalGroupHash = strings.Repeat("f", 64)
		}, want: ErrCauseChanged},
		{name: "cause missing", edit: func(detail *models.JobDetail, ref *AnalysisRef) {
			ref.CausalGroupID = "missing"
		}, want: ErrCauseNotFound},
		{name: "build missing", edit: func(detail *models.JobDetail, _ *AnalysisRef) {
			detail.Runs = detail.Runs[:1]
		}, want: ErrAnalysisNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			patternCopy := clonePatternAnalyses([]models.PatternAnalysis{pattern})[0]
			detail := causalPatternDetail(patternCopy, "2", "1", "3")
			caseRef := ref
			testCase.edit(&detail, &caseRef)
			dir := t.TempDir()
			writeJobDetail(t, dir, detail)
			service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Create(caseRef, "alice", testRequestID(t)); !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestServicePatternChatRejectsChangedCausalGroupHash(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{ID: "group", Builds: []string{"2", "1"}, RootCause: "original", Confidence: "high"}}, nil)
	detail := causalPatternDetail(pattern, "2", "1")
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash}
	detail.PatternAnalyses[0].CausalGroups[0].RootCause = "changed"
	detail.PatternAnalyses[0].ContentHash = models.PatternHash(detail.PatternAnalyses[0])
	writeJobDetail(t, dir, detail)
	if _, err := service.Create(ref, "Alice", testRequestID(t)); !errors.Is(err, ErrPatternChanged) {
		t.Fatalf("error = %v", err)
	}
}

func TestServicePatternChatRejectsOversizedCausalShape(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat(make([]models.PatternCausalGroup, maxPatternChatCausalGroups+1), nil)
	writeJobDetail(t, dir, causalPatternDetail(pattern, "1"))
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Create(AnalysisRef{Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash}, "Alice", testRequestID(t))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
}

func TestServiceReportsValidationGateToTheCaller(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	service, err := NewService(t.Context(), dir, &fakeRunner{err: &ValidationError{Gate: GateJSON}}, Options{
		StateDir: filepath.Join(dir, ".private-chat"), PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-23T12:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	_, sendErr := service.Send(t.Context(), session.ID, "Alice", requestID, "What does the log show?")
	gate, ok := ValidationGateOf(sendErr)
	if !errors.Is(sendErr, ErrResponseValidationFailed) || !ok || gate != GateJSON {
		t.Fatalf("send error = %v gate = %q", sendErr, gate)
	}
	// The gate must survive the persisted idempotent replay too.
	_, replayErr := service.Send(t.Context(), session.ID, "Alice", requestID, "What does the log show?")
	if gate, ok := ValidationGateOf(replayErr); !ok || gate != GateJSON {
		t.Fatalf("replayed error = %v gate = %q", replayErr, gate)
	}
}

// A retained pattern is one the correlation did not refresh this pass. That says
// nothing about the subject: its identity is still checked by content hash and
// its evidence by the run selection, so a pattern chat opens on it while its
// builds are readable and is refused once they are not.
func TestServicePatternChatTurnsOnEvidenceNotRefreshState(t *testing.T) {
	pattern := recurringPattern()
	ref := AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}

	dir := t.TempDir()
	retained := patternDetail()
	retained.PatternRefresh = &models.PatternRefreshStatus{State: models.PatternRefreshRetained, EvidenceAvailable: true}
	writeJobDetail(t, dir, retained)
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ref, "alice", testRequestID(t)); err != nil {
		t.Fatalf("retained pattern with readable evidence: %v", err)
	}

	// The same pattern once the window has rolled past its correlated builds.
	expiredDir := t.TempDir()
	expired := patternDetail()
	expired.PatternRefresh = &models.PatternRefreshStatus{State: models.PatternRefreshRetained}
	expired.Runs = []models.BuildResult{{BuildInfo: models.BuildInfo{BuildID: "999", JobName: "periodic-demo"}}}
	writeJobDetail(t, expiredDir, expired)
	expiredService, err := NewService(t.Context(), expiredDir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiredService.Create(ref, "alice", testRequestID(t)); !errors.Is(err, ErrAnalysisNotFound) {
		t.Fatalf("retained pattern with expired evidence err=%v", err)
	}
}
