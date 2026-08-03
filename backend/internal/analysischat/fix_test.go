package analysischat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func fixCandidatePattern() models.PatternAnalysis {
	pattern := models.PatternAnalysis{
		Subject: "retry failure", JobID: "periodic-demo", Systemic: true, Confidence: "high",
		SharedRootCause: "the controller retries terminal failures", SharedBuilds: []string{"123"},
		SuggestedFix: "bound the retry path",
	}
	pattern.ID = models.PatternID(pattern)
	pattern.ContentHash = models.PatternHash(pattern)
	return pattern
}

func fixCandidateReadyService(t *testing.T) (*Service, SessionView, string, string) {
	t.Helper()
	dir := t.TempDir()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{
		"example/repo": "main:0123456789abcdef0123456789abcdef01234567",
	}
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, dir, detail)
	chatRunner := &fakeRunner{reply: Reply{
		Answer:     "The retry path keeps treating the terminal condition as recoverable.",
		Assessment: "challenges",
		Citations: []Citation{{
			Path: "build-log.txt", LineStart: 42, LineEnd: 44, Quote: "terminal bootstrap failure",
		}},
		ProposedRevision: &Revision{
			RootCause:    "The controller requeues after terminal bootstrap failure.",
			SuggestedFix: "Stop requeueing after the terminal condition is persisted.",
		},
	}}
	service, err := NewService(t.Context(), dir, chatRunner, Options{
		StateDir: filepath.Join(dir, ".private-chat"), PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRunner := &fakeSourceInvestigator{result: sourceResult()}
	if err := service.ConfigureSourceInvestigation(
		sourceRunner,
		sourceinvestigation.Repository{Owner: "example", Name: "repo"},
		SourceInvestigationOptions{Timeout: time.Second, LeaseTTL: 2 * time.Second},
	); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	chatRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", chatRequestID, "Could the retry path be wrong?"); err != nil {
		t.Fatal(err)
	}
	sourceRequestID := testRequestID(t)
	if _, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", sourceRequestID, chatRequestID); err != nil {
		t.Fatal(err)
	}
	return service, session, chatRequestID, sourceRequestID
}

func TestServiceFixCandidateSelectsBoundedAnswerAndSource(t *testing.T) {
	service, session, chatRequestID, sourceRequestID := fixCandidateReadyService(t)
	candidate, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SessionID != session.ID || candidate.RequestID != chatRequestID ||
		candidate.Analysis.JobID != "periodic-demo" || candidate.Analysis.BuildID != "123" ||
		candidate.Pattern.ID != fixCandidatePattern().ID || candidate.Pattern.SharedBuilds[0] != "123" {
		t.Fatalf("candidate identity = %+v", candidate)
	}
	if candidate.AssistantAnswer != "The retry path keeps treating the terminal condition as recoverable." ||
		candidate.ProposedRevision == nil || len(candidate.ArtifactCitations) != 1 {
		t.Fatalf("candidate answer = %+v", candidate)
	}
	if candidate.SourceRequestID != sourceRequestID || candidate.SourceResult == nil ||
		len(candidate.SourceResult.Citations) != 1 || !candidate.SourceResult.Citations[0].Verified {
		t.Fatalf("candidate source = %+v", candidate.SourceResult)
	}
	if _, err := service.FixCandidate(session.ID, "Bob", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-owner error = %v", err)
	}
}

func TestServiceFixCandidateRetainsInvestigatedRevisionAcrossRestart(t *testing.T) {
	service, session, chatRequestID, sourceRequestID := fixCandidateReadyService(t)
	const refreshedRevision = "fedcba9876543210fedcba9876543210fedcba98"
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:" + refreshedRevision}
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, service.dataDir, detail)

	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{
		StateDir: service.opts.StateDir, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureSourceInvestigation(
		&fakeSourceInvestigator{result: sourceResult()},
		sourceinvestigation.Repository{Owner: "example", Name: "repo"},
		SourceInvestigationOptions{Timeout: time.Second, LeaseTTL: 2 * time.Second},
	); err != nil {
		t.Fatal(err)
	}
	candidate, err := restarted.FixCandidate(
		session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SourceRevision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("source revision = %q, refreshed build revision = %q", candidate.SourceRevision, refreshedRevision)
	}
}

func TestServiceFixCandidateValidatesSourceStateAndAttachment(t *testing.T) {
	service, session, chatRequestID, sourceRequestID := fixCandidateReadyService(t)
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "What else supports it?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.FixCandidate(session.ID, "Alice", secondRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-turn source error = %v", err)
	}

	ctx, cancel := service.store.context()
	err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		record := state.Sessions[session.ID].Investigations[sourceRequestID]
		record.View.Status = sourceinvestigation.StatusPending
		record.View.Result = nil
		state.Sessions[session.ID].Investigations[sourceRequestID] = record
		return true, nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("pending source error = %v", err)
	}
}

func TestServiceFixCandidateRejectsUngroundedAndStaleAnswers(t *testing.T) {
	service, session, chatRequestID, _ := fixCandidateReadyService(t)
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].TestCases[0].AIAnalysis.RootCause = "a replacement analysis"
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, ""); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("stale analysis error = %v", err)
	}

	ctx, cancel := service.store.context()
	err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		for i := range state.Sessions[session.ID].View.Messages {
			message := &state.Sessions[session.ID].View.Messages[i]
			if message.Role == "assistant" && message.RequestID == chatRequestID {
				message.Citations = nil
			}
		}
		return true, nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ungrounded answer error = %v", err)
	}
}

func TestServiceFixCandidateRejectsTerminalSourceFailures(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		status      string
		failureKind string
		mutate      func(*persistedInvestigation)
		want        error
	}{
		{name: "unknown", status: sourceinvestigation.StatusUnknown, want: ErrRequestOutcomeUnknown},
		{name: "failed", status: sourceinvestigation.StatusFailed, failureKind: failureSource, want: sourceinvestigation.ErrUnavailable},
		{name: "unverified", status: sourceinvestigation.StatusSucceeded, mutate: func(record *persistedInvestigation) {
			record.View.Result.Citations[0].Verified = false
		}, want: sourceinvestigation.ErrInvalidResult},
		{name: "invalid revision", status: sourceinvestigation.StatusSucceeded, mutate: func(record *persistedInvestigation) {
			record.Revision = "main"
		}, want: sourceinvestigation.ErrUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, session, chatRequestID, sourceRequestID := fixCandidateReadyService(t)
			ctx, cancel := service.store.context()
			err := service.store.update(ctx, func(state *persistedState) (bool, error) {
				record := state.Sessions[session.ID].Investigations[sourceRequestID]
				record.View.Status = testCase.status
				record.FailureKind = testCase.failureKind
				if testCase.mutate != nil {
					testCase.mutate(&record)
				}
				state.Sessions[session.ID].Investigations[sourceRequestID] = record
				return true, nil
			})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, sourceRequestID); !errors.Is(err, testCase.want) {
				t.Fatalf("source state error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestServiceFixCandidateRejectsSameTimestampAnalysisContentReplacement(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*models.AIAnalysis)
	}{
		{name: "severity", mutate: func(analysis *models.AIAnalysis) { analysis.Severity = "Low" }},
		{name: "relevant files", mutate: func(analysis *models.AIAnalysis) { analysis.RelevantFiles = []string{"different.go"} }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, session, chatRequestID, _ := fixCandidateReadyService(t)
			detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
			testCase.mutate(detail.Runs[0].TestCases[0].AIAnalysis)
			detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
			writeJobDetail(t, service.dataDir, detail)
			if _, err := service.FixCandidate(
				session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, "",
			); !errors.Is(err, ErrAnalysisChanged) {
				t.Fatalf("same-timestamp replacement error = %v", err)
			}
		})
	}
}

func TestServiceFixCandidateRejectsChangedPatternContent(t *testing.T) {
	dir := t.TempDir()
	reviewed := fixCandidatePattern()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.PatternAnalyses = []models.PatternAnalysis{reviewed}
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{
		Answer: "The retry path remains active after a terminal failure.", Assessment: "supports",
		Citations: []Citation{{Path: "build-log.txt", Quote: "terminal bootstrap failure"}},
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	chatRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", chatRequestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	changed := reviewed
	changed.SuggestedFix = "replace the retry controller"
	changed.ContentHash = models.PatternHash(changed)
	if changed.ID != reviewed.ID || changed.ContentHash == reviewed.ContentHash {
		t.Fatalf("test pattern identity did not isolate content change: reviewed=%+v changed=%+v", reviewed, changed)
	}
	detail.PatternAnalyses = []models.PatternAnalysis{changed}
	writeJobDetail(t, dir, detail)
	if _, err := service.FixCandidate(
		session.ID, "Alice", chatRequestID, reviewed.ID, reviewed.ContentHash, "",
	); !errors.Is(err, ErrPatternChanged) {
		t.Fatalf("changed pattern error = %v", err)
	}
}

func TestLegacySourceRefreshCannotReplaceFixCandidateAnalysis(t *testing.T) {
	service, session, chatRequestID, _ := fixCandidateReadyService(t)
	ctx, cancel := service.store.context()
	err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		state.Sessions[session.ID].Resolved.Build.RepoRefs = nil
		return true, nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}

	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{
		"example/repo": "main:0123456789abcdef0123456789abcdef01234567",
	}
	detail.Runs[0].TestCases[0].AIAnalysis.Severity = "Low"
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, service.dataDir, detail)

	if _, err := service.SourceInvestigation(
		t.Context(), session.ID, "Alice", testRequestID(t), chatRequestID,
	); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("legacy source refresh error = %v", err)
	}
	if _, err := service.FixCandidate(
		session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, "",
	); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("fix candidate after rejected refresh error = %v", err)
	}

	ctx, cancel = service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		analysis := state.Sessions[session.ID].Resolved.TestCase.AIAnalysis
		if analysis == nil || analysis.Severity != "High" || len(state.Sessions[session.ID].Resolved.Build.RepoRefs) != 0 {
			t.Fatalf("legacy refresh mutated original snapshot: %+v", state.Sessions[session.ID].Resolved)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceFixCandidateAcceptsUnchangedCanonicalizedAnalysis(t *testing.T) {
	dir := t.TempDir()
	testCase := analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z")
	testCase.AIAnalysis.RootCause = strings.Repeat("root ", 9000)
	testCase.AIAnalysis.SuggestedFix = strings.Repeat("fix ", 5000)
	for i := 0; i < 55; i++ {
		testCase.AIAnalysis.RelevantFiles = append(testCase.AIAnalysis.RelevantFiles, fmt.Sprintf("  pkg/file-%02d.go  ", i))
	}
	detail := testDetail(testCase)
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: Reply{
		Answer: "selected answer", Assessment: "supports",
		Citations: []Citation{{Path: "build-log.txt", Quote: "failure"}},
	}}
	service, err := NewService(t.Context(), dir, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.FixCandidate(session.ID, "Alice", requestID, fixCandidatePattern().ID, fixCandidatePattern().ContentHash, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRootCause := clampPersistedText(testCase.AIAnalysis.RootCause, 32<<10)
	wantSuggestedFix := clampPersistedText(testCase.AIAnalysis.SuggestedFix, 16<<10)
	if candidate.Original.RootCause != wantRootCause || candidate.Original.SuggestedFix != wantSuggestedFix ||
		len(candidate.Original.RelevantFiles) != 50 || candidate.Original.RelevantFiles[0] != "build-log.txt" ||
		candidate.Original.RelevantFiles[1] != "pkg/file-00.go" {
		t.Fatalf("canonical snapshot was not preserved")
	}
}

func TestServicePatternFixCandidateUsesBoundPattern(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	runner := &fakeRunner{reply: Reply{
		Answer: "The retry controller should stop after terminal failures.", Assessment: "supports",
		Citations: []Citation{{Path: "builds/104/build-log.txt", Quote: "terminal failure"}},
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
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.FixCandidate(session.ID, "Alice", requestID, pattern.ID, pattern.ContentHash, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Pattern.ID != pattern.ID || candidate.Analysis.Scope != ScopePattern || candidate.Analysis.BuildID != "104" {
		t.Fatalf("candidate = %+v", candidate)
	}
}

func TestServicePatternFixCandidateRejectsDifferentPattern(t *testing.T) {
	dir := t.TempDir()
	writeJobDetail(t, dir, patternDetail())
	runner := &fakeRunner{reply: Reply{
		Answer: "The retry controller should stop after terminal failures.", Assessment: "supports",
		Citations: []Citation{{Path: "builds/104/build-log.txt", Quote: "terminal failure"}},
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
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.FixCandidate(session.ID, "Alice", requestID, "other-pattern", "other-hash", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("different pattern error = %v", err)
	}
}
