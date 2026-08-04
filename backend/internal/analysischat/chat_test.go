package analysischat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

var testRequestCounter atomic.Int64

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
		Citations: []Citation{{Path: "build-log.txt", LineStart: 42, LineEnd: 42, Quote: "controller exited"}},
		ToolCalls: 2, GCSBytes: 1024, ElapsedMs: 50,
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
	if assistant.Assessment != "supports" || assistant.ToolCalls != 2 || len(assistant.Citations) != 1 {
		t.Fatalf("assistant = %+v", assistant)
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
	if _, err := service.Get(created.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("other owner Get error = %v", err)
	}
}

func TestServiceFindLatestSessionAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	first, err := NewService(t.Context(), dir, &fakeRunner{}, Options{Now: now})
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

	second, err := NewService(t.Context(), dir, &fakeRunner{}, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	found, err := second.Find(ref, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != newer.ID || found.ID == older.ID {
		t.Fatalf("found session = %q, want latest %q", found.ID, newer.ID)
	}
	if _, err := second.Find(ref, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-owner find error = %v", err)
	}
}

func TestServiceFindPrefersRecentlyActiveSession(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	runner := &fakeRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	service, err := NewService(t.Context(), dir, runner, Options{Now: now, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	first, err := service.Create(ref, "alice", "create-first-active")
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	second, err := service.Create(ref, "alice", "create-second-idle")
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(2 * time.Minute).UnixNano())
	done := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), first.ID, "alice", "turn-first-active", "question", nil)
		done <- err
	}()
	<-runner.started
	found, err := service.Find(ref, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != first.ID || found.ID == second.ID {
		t.Fatalf("found active session = %q, want %q", found.ID, first.ID)
	}
	if found.Active == nil || found.Active.RequestID != "turn-first-active" || found.Active.Question != "question" || found.Active.Phase == "" {
		t.Fatalf("active turn = %+v", found.Active)
	}
	if found.TurnsUsed != 1 || found.MaxTurns != 10 {
		t.Fatalf("pending usage = %d/%d", found.TurnsUsed, found.MaxTurns)
	}
	replica, err := NewService(t.Context(), dir, &fakeRunner{}, Options{Now: now, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fromReplica, err := replica.Get(first.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if fromReplica.Active == nil || fromReplica.Active.RequestID != found.Active.RequestID || fromReplica.Active.Question != found.Active.Question {
		t.Fatalf("replica active turn = %+v", fromReplica.Active)
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
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
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
	if _, err := service.Create(ref, "alice", testRequestID(t)); !errors.Is(err, ErrSessionLimit) {
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
	if _, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "second"); !errors.Is(err, ErrSessionBusy) {
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
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
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
	if _, err := service.Create(ref, "alice", testRequestID(t)); !errors.Is(err, ErrSessionLimit) {
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
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-other", "other question"); !errors.Is(err, ErrSessionBusy) {
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
		{name: "citation", err: fmt.Errorf("%w: private citation path", ErrCitationValidationFailed), want: ErrCitationValidationFailed, kind: failureCitation},
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
		{ErrCitationValidationFailed, failureCitation},
	}
	for _, testCase := range cases {
		if got := requestFailureKind(fmt.Errorf("wrapped: %w", testCase.err)); got != testCase.kind {
			t.Errorf("requestFailureKind(%v) = %q, want %q", testCase.err, got, testCase.kind)
		}
		if got := persistedRequestError(testCase.kind); !errors.Is(got, testCase.err) {
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
	opts := Options{Now: now, SessionTTL: time.Minute, TurnLeaseTTL: time.Minute}
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
	opts := Options{Now: now, SessionTTL: time.Minute, TurnLeaseTTL: time.Minute, PollInterval: 10 * time.Millisecond}
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
	if err := second.Cancel(created.ID, "bob", "turn-cancel"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-owner cancel error = %v", err)
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
	if _, err := second.Get(created.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-owner attempt history error = %v", err)
	}
}

func TestServiceOwnerActiveTurnAndRateLimits(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
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
	second, err := service.Create(ref, "alice", "create-limit-2")
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

func TestServiceCorrectionCandidateAndValidation(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z")))
	runner := &fakeRunner{reply: Reply{
		Answer: "evidence challenges it", Assessment: "challenges",
		Citations:        []Citation{{Path: "build-log.txt", Quote: "first failure"}},
		ProposedRevision: &Revision{RootCause: "new cause", SuggestedFix: "new fix"},
	}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice", "create-correction")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "turn-correction", "check another cause"); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.CorrectionCandidate(created.ID, "alice", "turn-correction")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Proposed.RootCause != "new cause" || candidate.Original.RootCause != "the controller stopped" || len(candidate.Citations) != 1 {
		t.Fatalf("candidate = %+v", candidate)
	}
	if err := service.ValidateCorrectionCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	changed := analyzedTest("TestCluster", "junit.xml", "2026-07-24T13:00:00Z")
	writeJobDetail(t, dir, testDetail(changed))
	if err := service.ValidateCorrectionCandidate(candidate); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed analysis validation error = %v", err)
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

func TestPatternChatCannotPromoteTestCorrection(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	runner := &fakeRunner{reply: Reply{
		Answer: "The pattern should be revised.", Assessment: "challenges",
		Citations:        []Citation{{Path: "builds/104/build-log.txt", Quote: "terminal failure"}},
		ProposedRevision: &Revision{RootCause: "new cause", SuggestedFix: "new fix"},
	}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	session, err := service.Create(AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "Is this conclusion wrong?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CorrectionCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("correction error = %v", err)
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
		{Scope: "other", JobID: "job", PatternID: "pattern", PatternHash: "hash"},
	} {
		if _, err := service.Create(ref, "alice", testRequestID(t)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("ref %+v error = %v", ref, err)
		}
	}
}

func TestPatternChatRejectsSourceInvestigation(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	runner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pattern := recurringPattern()
	session, err := service.Create(AnalysisRef{
		Scope: ScopePattern, JobID: "periodic-demo", PatternID: pattern.ID, PatternHash: pattern.ContentHash,
	}, "alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "alice", requestID, "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.sourceInvestigationSubject(session.ID, "alice", requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("source investigation error = %v", err)
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
	persisted := persistResolved(resolved, "")
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
