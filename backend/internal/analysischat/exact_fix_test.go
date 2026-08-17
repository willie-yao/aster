package analysischat

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const exactFixSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func exactFixService(t *testing.T, reply Reply, runnerErr error) (*Service, SessionView, string) {
	t.Helper()
	service, session, requestID, _ := exactFixServiceRunner(t, reply, runnerErr)
	return service, session, requestID
}

func exactFixServiceRunner(t *testing.T, reply Reply, runnerErr error) (*Service, SessionView, string, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": exactFixSourceRevision}
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: reply, err: runnerErr}
	service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat"), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
		AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	_, sendErr := service.Send(t.Context(), session.ID, "Alice", requestID, "What exact change does the artifact support?")
	if runnerErr == nil && sendErr != nil {
		t.Fatal(sendErr)
	}
	if runnerErr != nil && sendErr == nil {
		t.Fatal("failed chat turn unexpectedly succeeded")
	}
	return service, session, requestID, runner
}

func TestServiceTestFixCandidateBindsExactOwnerAnalysisAndEvidence(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations:        []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		ProposedRevision: &Revision{RootCause: "The terminal branch omits Ready.", SuggestedFix: "Record Ready before returning."},
	}, nil)
	candidate, err := service.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SessionID != session.ID || candidate.RequestID != requestID || candidate.Analysis.Scope != ScopeTest ||
		candidate.Analysis.JobID != "periodic-demo" || candidate.Analysis.BuildID != "123" || candidate.Analysis.TestName != "TestCluster" ||
		candidate.Analysis.AnalysisGeneratedAt != "2026-08-13T01:00:00Z" || candidate.ResponseHash == "" || candidate.AnalysisContentHash == "" ||
		candidate.SourceRepositorySnapshot.Revision != exactFixSourceRevision || len(candidate.ArtifactCitations) != 1 {
		t.Fatalf("candidate = %+v", candidate)
	}
	if _, err := service.TestFixCandidate(session.ID, "Bob", requestID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("wrong owner error = %v", err)
	}
}

func TestServiceTestFixCandidateRejectsChangedAnalysisEvidenceAndSource(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready through `markReady`.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": exactFixSourceRevision}
	detail.Runs[0].TestCases[0].AIAnalysis.FileLinks = map[string]string{"pkg/controller.go": "https://github.com/example/repo/blob/" + exactFixSourceRevision + "/pkg/controller.go"}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed source evidence error = %v", err)
	}

	detail = testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "fedcba9876543210fedcba9876543210fedcba98"}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed source revision error = %v", err)
	}
}

func TestServiceTestFixCandidateRejectsContextOnlyAndFailedTurns(t *testing.T) {
	service, session, requestID, runner := exactFixServiceRunner(t, Reply{Answer: "No artifact evidence was needed.", Assessment: "explains"}, nil)
	if _, err := service.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("context-only candidate error = %v", err)
	}
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "Which function should change?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TestFixCandidate(session.ID, "Alice", secondRequestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ungrounded conversation candidate error = %v", err)
	}
	runner.mu.Lock()
	turns := len(runner.turns)
	runner.mu.Unlock()
	if turns != 2 {
		t.Fatalf("provider calls = %d", turns)
	}

	failed, failedSession, failedRequest := exactFixService(t, Reply{}, ErrProviderRequestFailed)
	if _, err := failed.TestFixCandidate(failedSession.ID, "Alice", failedRequest); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("failed turn candidate error = %v", err)
	}
}

func TestServiceTestFixPreflightReportsMissingSourcePathsForCitedAnswer(t *testing.T) {
	service, session, requestID, runner := exactFixServiceRunner(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "no verified immutable source paths") {
		t.Fatalf("missing source path preflight error = %v", err)
	}
	runner.mu.Lock()
	turns := len(runner.turns)
	runner.mu.Unlock()
	if turns != 1 {
		t.Fatalf("provider calls = %d", turns)
	}
}

func TestServiceTestFixCandidateAccumulatesConversationEvidence(t *testing.T) {
	cited := Citation{Path: "artifacts/build-log.txt", LineStart: 42, LineEnd: 44, Quote: "terminal bootstrap failure"}
	service, session, firstRequestID, runner := exactFixServiceRunner(t, Reply{
		Answer: "The build log records the terminal bootstrap failure.", Assessment: "supports",
		Citations: []Citation{cited},
	}, nil)
	runner.mu.Lock()
	runner.reply = Reply{
		Answer:     "The retry branch in `markReady` should stop requeueing that condition.",
		Assessment: "supports",
	}
	runner.mu.Unlock()
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "Which function should change to stop this?"); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.TestFixCandidate(session.ID, "Alice", secondRequestID)
	if err != nil {
		t.Fatalf("conversation-scoped candidate error = %v", err)
	}
	if candidate.RequestID != secondRequestID ||
		candidate.AssistantAnswer != "The retry branch in `markReady` should stop requeueing that condition." ||
		!slices.Equal(candidate.ArtifactCitations, []Citation{cited}) {
		t.Fatalf("candidate = %+v", candidate)
	}
	first, err := service.TestFixCandidate(session.ID, "Alice", firstRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResponseHash == candidate.ResponseHash {
		t.Fatal("promoted turns shared one response identity")
	}
}

func TestConversationCitationsBoundsOrderAndScope(t *testing.T) {
	early := Citation{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "early"}
	shared := Citation{Path: "build-log.txt", LineStart: 2, LineEnd: 2, Quote: "shared"}
	promoted := Citation{Path: "junit.xml", LineStart: 3, LineEnd: 3, Quote: "promoted"}
	later := Citation{Path: "junit.xml", LineStart: 4, LineEnd: 4, Quote: "later"}
	messages := []Message{
		{Role: "user", RequestID: "one"},
		{Role: "assistant", RequestID: "one", Citations: []Citation{early, shared}},
		{Role: "user", RequestID: "two"},
		{Role: "assistant", RequestID: "two", Citations: []Citation{promoted, shared}},
		{Role: "user", RequestID: "three"},
		{Role: "assistant", RequestID: "three", Citations: []Citation{later}},
	}
	got := conversationCitations(messages, "two")
	if !slices.Equal(got, []Citation{promoted, shared, early}) {
		t.Fatalf("accumulated citations = %+v", got)
	}
	if got := conversationCitations(messages, "missing"); got != nil {
		t.Fatalf("unknown request citations = %+v", got)
	}

	overflow := []Message{{Role: "assistant", RequestID: "one"}, {Role: "assistant", RequestID: "two"}}
	for i := 0; i < maxConversationFixCitations; i++ {
		overflow[0].Citations = append(overflow[0].Citations, Citation{Path: "build-log.txt", LineStart: i + 1, LineEnd: i + 1, Quote: "old"})
		overflow[1].Citations = append(overflow[1].Citations, Citation{Path: "junit.xml", LineStart: i + 1, LineEnd: i + 1, Quote: "new"})
	}
	bounded := conversationCitations(overflow, "two")
	if len(bounded) != maxConversationFixCitations || !slices.Equal(bounded, overflow[1].Citations) {
		t.Fatalf("bounded citations = %d entries: %+v", len(bounded), bounded)
	}
}

func TestServiceTestFixCandidateSurvivesRestartAndRejectsStaleAnalysis(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{StateDir: service.opts.StateDir, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	before, err := restarted.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ResponseHash == "" {
		t.Fatal("restart lost selected response identity")
	}
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T02:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": exactFixSourceRevision}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := restarted.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("stale analysis error = %v", err)
	}
}

func TestServiceTestFixCandidateHashChangesWithAnswerOrEvidence(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	before, err := service.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[session.ID]
		for index := range current.View.Messages {
			message := &current.View.Messages[index]
			if message.Role == "assistant" && message.RequestID == requestID {
				message.Content = "Changed answer"
				message.Citations[0].Quote = "changed evidence"
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := service.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ResponseHash == after.ResponseHash {
		t.Fatal("changed answer and evidence retained the same identity")
	}
}

func TestServiceExactFixResolvesPreservedMutableBuildSourceBeforeProvider(t *testing.T) {
	dir := t.TempDir()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "main"}
	detail.Runs[0].Commit = "a866aca055bcaa205648e81d15c67668179fdfab"
	detail.Runs[0].RepoVersion = detail.Runs[0].Commit
	detail.Runs[0].TestCases[0].AIAnalysis.FileLinks = map[string]string{
		"test/e2e/cni.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/a866aca055bcaa205648e81d15c67668179fdfab/test/e2e/cni.go",
	}
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: Reply{
		Answer: "The artifact ties the conflict to `InstallCNIManifest`.", Assessment: "supports",
		Citations: []Citation{{Path: "build-log.txt", LineStart: 2217, LineEnd: 2217, Quote: "Installing a CNI plugin"}},
	}}
	service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat"), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}); err != nil {
		t.Fatal(err)
	}
	sourcePreflightErr := errors.New("relevant source drift")
	generationBase := detail.Runs[0].Commit
	if err := service.ConfigureTestFixPreflight(func(_ context.Context, repo sourceinvestigation.Repository, branch string, files []string) (string, map[string]string, error) {
		if repo.Revision != detail.Runs[0].Commit || branch != "main" || !slices.Equal(files, []string{"test/e2e/cni.go"}) {
			t.Fatalf("source preflight repo=%+v branch=%q files=%v", repo, branch, files)
		}
		if sourcePreflightErr != nil {
			return "", nil, sourcePreflightErr
		}
		return generationBase, map[string]string{"test/e2e/cni.go": strings.Repeat("a", 64)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
		AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if session.SourceRepository == nil || session.SourceRepository.Revision != detail.Runs[0].Commit {
		t.Fatalf("source repository = %+v", session.SourceRepository)
	}
	requestID := testRequestID(t)
	if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("source drift preflight error = %v", err)
	}
	runner.mu.Lock()
	providerCalls := len(runner.turns)
	runner.mu.Unlock()
	if providerCalls != 0 {
		t.Fatalf("failed source preflight made %d provider calls", providerCalls)
	}
	sourcePreflightErr = nil
	if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID); err != nil {
		t.Fatalf("Fix preflight error = %v", err)
	}
	generationBase = strings.Repeat("b", 40)
	if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("pre-admission branch drift error = %v", err)
	}
	generationBase = detail.Runs[0].Commit
	if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID); err != nil {
		t.Fatalf("restored Fix preflight error = %v", err)
	}
	runner.mu.Lock()
	turnsBefore := len(runner.turns)
	runner.mu.Unlock()
	if turnsBefore != 0 {
		t.Fatalf("session source preflight made %d provider calls", turnsBefore)
	}
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What evidence supports the Fix?"); err != nil {
		t.Fatal(err)
	}
	sourcePreflightErr = errors.New("branch advanced after admission")
	if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", requestID); err != nil {
		t.Fatalf("admitted idempotent Fix preflight error = %v", err)
	}
	subject, err := service.sourceInvestigationSubject(session.ID, "alice", requestID)
	if err != nil || subject.Repository.Revision != detail.Runs[0].Commit {
		t.Fatalf("source investigation subject = %+v, %v", subject, err)
	}
	candidate, err := service.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SourceRepositorySnapshot.Revision != detail.Runs[0].Commit || candidate.AnalysisContentHash == "" ||
		candidate.FailureRevision != detail.Runs[0].Commit || candidate.GenerationBaseRevision != detail.Runs[0].Commit ||
		candidate.VerifiedSourceFileHashes["test/e2e/cni.go"] != strings.Repeat("a", 64) ||
		!candidate.SourceBranchKnown || candidate.SourceBranch != "main" {
		t.Fatalf("candidate = %+v", candidate)
	}
	reloaded, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat"), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}); err != nil {
		t.Fatal(err)
	}
	restored, err := reloaded.TestFixCandidate(session.ID, "Alice", requestID)
	if err != nil || restored.FailureRevision != detail.Runs[0].Commit || restored.GenerationBaseRevision != detail.Runs[0].Commit ||
		restored.VerifiedSourceFileHashes["test/e2e/cni.go"] != strings.Repeat("a", 64) ||
		!restored.SourceBranchKnown || restored.SourceBranch != "main" {
		t.Fatalf("restored candidate = %+v, %v", restored, err)
	}
	detail.Runs[0].RepoRefs = map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "release-1.2"}
	writeJobDetail(t, dir, detail)
	if _, err := reloaded.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("refreshed branch drift error = %v", err)
	}
}

func TestServiceExactFixSourceIneligibilityIsProviderFree(t *testing.T) {
	sha := "a866aca055bcaa205648e81d15c67668179fdfab"
	other := "b866aca055bcaa205648e81d15c67668179fdfab"
	for _, tc := range []struct {
		name  string
		build func(*models.BuildInfo)
	}{
		{name: "mismatched checkout", build: func(build *models.BuildInfo) {
			build.RepoRefs = map[string]string{"example/repo": "main"}
			build.Commit, build.RepoVersion = sha, other
		}},
		{name: "multiple repositories", build: func(build *models.BuildInfo) {
			build.RepoRefs = map[string]string{"example/repo": "main", "example/other": "main"}
			build.Commit, build.RepoVersion = sha, sha
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
			tc.build(&detail.Runs[0].BuildInfo)
			writeJobDetail(t, dir, detail)
			runner := &fakeRunner{reply: Reply{Answer: "The published analysis explains the failure.", Assessment: "explains"}}
			service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat")})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
				t.Fatal(err)
			}
			session, err := service.Create(AnalysisRef{
				JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
				AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
			}, "Alice", testRequestID(t))
			if err != nil {
				t.Fatal(err)
			}
			if session.SourceRepository != nil {
				t.Fatalf("source repository = %+v", session.SourceRepository)
			}
			if err := service.PreflightTestFix(t.Context(), session.ID, "Alice", testRequestID(t)); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Fix preflight error = %v", err)
			}
			runner.mu.Lock()
			turns := len(runner.turns)
			runner.mu.Unlock()
			if turns != 0 {
				t.Fatalf("Fix preflight made %d provider calls", turns)
			}
			if _, err := service.Send(t.Context(), session.ID, "Alice", testRequestID(t), "Explain the published context."); err != nil {
				t.Fatalf("normal chat error = %v", err)
			}
			runner.mu.Lock()
			turns = len(runner.turns)
			runner.mu.Unlock()
			if turns != 1 {
				t.Fatalf("normal chat provider calls = %d", turns)
			}
		})
	}
}

func TestServiceExactFixDoesNotSalvagePersistedAmbiguousSource(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact supports `markReady`.", Assessment: "supports",
		Citations: []Citation{{Path: "build-log.txt", LineStart: 10, LineEnd: 10, Quote: "ready"}},
	}, nil)
	ctx, cancel := service.store.context()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[session.ID]
		current.Resolved.Source.Revision = ""
		current.Resolved.Build.RepoRefs = map[string]string{"example/repo": "ambiguous"}
		current.Resolved.Build.Commit = exactFixSourceRevision
		current.Resolved.Build.RepoVersion = exactFixSourceRevision
		return true, nil
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{StateDir: service.opts.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	if view, err := restarted.Get(session.ID, "Alice"); err != nil {
		t.Fatal(err)
	} else if view.SourceRepository != nil {
		t.Fatalf("persisted ambiguous source was salvaged: %+v", view.SourceRepository)
	}
	if _, err := restarted.TestFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("ambiguous persisted source error = %v", err)
	}
}
