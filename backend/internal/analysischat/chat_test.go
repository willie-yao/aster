package analysischat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

type fakeRunner struct {
	mu      sync.Mutex
	turns   []Turn
	reply   Reply
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeRunner) Reply(ctx context.Context, turn Turn) (Reply, error) {
	f.mu.Lock()
	f.turns = append(f.turns, turn)
	started, release := f.started, f.release
	reply, err := f.reply, f.err
	f.mu.Unlock()
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
	service, err := NewService(dir, runner, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-23T12:00:00Z",
	}, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Analysis.JUnitFile != "junit_01.xml" || len(created.Messages) != 0 {
		t.Fatalf("created session = %+v", created)
	}

	got, err := service.Send(context.Background(), created.ID, "alice", "  What proves this?  ")
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

	if _, err := service.Send(context.Background(), created.ID, "alice", "What should I check next?"); err != nil {
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
	service, err := NewService(dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ambiguous Create error = %v", err)
	}
	_, err = service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit_01.xml",
		AnalysisGeneratedAt: "2026-07-23T11:00:00Z",
	}, "alice")
	if !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed Create error = %v", err)
	}
}

func TestServiceBoundsSessionsTurnsAndQuestions(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-23T12:00:00Z")))
	runner := &fakeRunner{reply: Reply{Answer: "answer", Assessment: "explains"}}
	service, err := NewService(dir, runner, Options{
		MaxSessions: 2, MaxSessionsPerOwner: 1, MaxTurns: 1, MaxQuestionBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	created, err := service.Create(ref, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ref, "alice"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("owner session limit error = %v", err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "123456789"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("question bound error = %v", err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "again"); !errors.Is(err, ErrTurnLimit) {
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
	service, err := NewService(dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), created.ID, "alice", "first")
		done <- err
	}()
	<-runner.started
	if _, err := service.Send(context.Background(), created.ID, "alice", "second"); !errors.Is(err, ErrSessionBusy) {
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
	service, err := NewService(dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: detail.JobID, BuildID: "123", TestName: "TestCluster"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "explain"); err != nil {
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
	service, err := NewService(dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), created.ID, "alice", "first"); err == nil {
		t.Fatal("runner error was not returned")
	}
	runner.mu.Lock()
	runner.err = nil
	runner.reply = Reply{Answer: "recovered", Assessment: "explains"}
	runner.mu.Unlock()
	if _, err := service.Send(context.Background(), created.ID, "alice", "retry"); err != nil {
		t.Fatalf("retry after runner error: %v", err)
	}
}

func TestServiceRejectsOversizedAnalysisReference(t *testing.T) {
	service, err := NewService(t.TempDir(), &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Create(AnalysisRef{JobID: strings.Repeat("x", maxJobIDBytes+1), BuildID: "1", TestName: "Test"}, "alice")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized reference error = %v", err)
	}
}
