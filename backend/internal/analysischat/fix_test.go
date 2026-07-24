package analysischat

import (
	"errors"
	"path/filepath"
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
	candidate, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, sourceRequestID)
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
	if _, err := service.FixCandidate(session.ID, "Bob", chatRequestID, fixCandidatePattern().ID, sourceRequestID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-owner error = %v", err)
	}
}

func TestServiceFixCandidateValidatesSourceStateAndAttachment(t *testing.T) {
	service, session, chatRequestID, sourceRequestID := fixCandidateReadyService(t)
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "What else supports it?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.FixCandidate(session.ID, "Alice", secondRequestID, fixCandidatePattern().ID, sourceRequestID); !errors.Is(err, ErrRequestNotFound) {
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
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, sourceRequestID); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("pending source error = %v", err)
	}
}

func TestServiceFixCandidateRejectsUngroundedAndStaleAnswers(t *testing.T) {
	service, session, chatRequestID, _ := fixCandidateReadyService(t)
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].TestCases[0].AIAnalysis.RootCause = "a replacement analysis"
	detail.PatternAnalyses = []models.PatternAnalysis{fixCandidatePattern()}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, ""); !errors.Is(err, ErrAnalysisChanged) {
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
	if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, ""); !errors.Is(err, ErrInvalidRequest) {
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
			if _, err := service.FixCandidate(session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, sourceRequestID); !errors.Is(err, testCase.want) {
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
				session.ID, "Alice", chatRequestID, fixCandidatePattern().ID, "",
			); !errors.Is(err, ErrAnalysisChanged) {
				t.Fatalf("same-timestamp replacement error = %v", err)
			}
		})
	}
}
