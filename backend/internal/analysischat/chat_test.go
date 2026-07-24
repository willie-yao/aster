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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

var testRequestCounter atomic.Int64

func testRequestID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d", testRequestCounter.Add(1))
}

type fakeRunner struct {
	mu      sync.Mutex
	turns   []Turn
	reply   Reply
	err     error
	started chan struct{}
	release chan struct{}
	phases  []string
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
		select {
		case <-release:
		case <-ctx.Done():
			return Reply{}, ctx.Err()
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
	if created.ID == "" || created.Analysis.JUnitFile != "junit_01.xml" || len(created.Messages) != 0 {
		t.Fatalf("created session = %+v", created)
	}

	got, err := service.Send(context.Background(), created.ID, "alice", testRequestID(t), "  What proves this?  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "What proves this?" {
		t.Fatalf("messages = %+v", got.Messages)
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
	opts := Options{Now: now, SessionTTL: time.Hour, TurnLeaseTTL: time.Minute}
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
	if _, err := second.Send(context.Background(), created.ID, "alice", "turn-after-stale", "question"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	calls := len(runner.turns)
	runner.mu.Unlock()
	if calls != 2 {
		t.Fatalf("runner calls = %d, want abandoned plus explicit retry", calls)
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
		phases: []string{PhaseReadingEvidence, PhaseEvaluating},
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

	var phases []string
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Stream(t.Context(), created.ID, "alice", "turn-stream", "question", func(progress Progress) error {
			phases = append(phases, progress.Phase)
			return nil
		})
		secondDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(runner.release)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if len(phases) == 0 {
		t.Fatal("reconnected stream received no persisted progress")
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
