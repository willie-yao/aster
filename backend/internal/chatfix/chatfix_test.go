package chatfix

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeChatStore struct {
	candidate       analysischat.FixCandidate
	candidateErr    error
	onReturn        func()
	sessionID       string
	owner           string
	requestID       string
	patternID       string
	patternHash     string
	sourceRequestID string
}

func (f *fakeChatStore) FixCandidate(sessionID, owner, requestID, patternID, patternHash, sourceRequestID string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.patternID, f.patternHash, f.sourceRequestID = patternID, patternHash, sourceRequestID
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

type fakeFixPreviewer struct {
	pattern           models.PatternAnalysis
	userToken         string
	instruction       string
	target            actions.FixTarget
	generationContext fixpr.GenerationContext
	called            bool
}

func (f *fakeFixPreviewer) PreviewFixWithContext(
	_ context.Context, pattern models.PatternAnalysis, userToken, instruction string, target actions.FixTarget, generationContext fixpr.GenerationContext,
) (actions.PreviewResult, error) {
	f.pattern, f.userToken, f.instruction = pattern, userToken, instruction
	f.target, f.generationContext, f.called = target, generationContext, true
	return actions.PreviewResult{Token: "preview", Kind: "fix"}, nil
}

func TestPreviewChatFixBuildsSelectedContext(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		Analysis: analysischat.AnalysisRef{JobID: "periodic-x", BuildID: "123"},
		Pattern: models.PatternAnalysis{
			ID: "pattern", JobID: "periodic-x", SharedBuilds: []string{"123"}, SharedRootCause: "snapshot cause",
		},
		AssistantAnswer:   "selected answer",
		ProposedRevision:  &analysischat.Revision{RootCause: "new cause", SuggestedFix: "new fix"},
		ArtifactCitations: []analysischat.Citation{{Path: "build-log.txt", LineStart: 4, LineEnd: 5, Quote: "failure"}},
		SourceRequestID:   "source-request",
		SourceRepository:  sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
		SourceRevision:    "0123456789abcdef0123456789abcdef01234567",
		SourceResult: &sourceinvestigation.Result{
			State:   sourceinvestigation.StateActionableCodeChange,
			Target:  &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "retry"},
			Finding: "source finding", Confidence: sourceinvestigation.ConfidenceHigh,
			Relationship: sourceinvestigation.RelationshipSupports, Direction: "modify retry",
			Citations: []sourceinvestigation.Citation{{Path: "pkg/retry.go", LineStart: 10, LineEnd: 12, Quote: "retry", Verified: true}},
		},
	}}
	fixes := &fakeFixPreviewer{}
	service := NewService(chat, fixes)
	preview, err := service.PreviewChatFix(
		t.Context(), "session", "Alice", "chat-request", "pattern", "pattern-hash", "source-request", "user-token", "keep compatibility",
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token != "preview" || !fixes.called || fixes.pattern.ID != "pattern" || fixes.userToken != "user-token" {
		t.Fatalf("preview=%+v fixes=%+v", preview, fixes)
	}
	if chat.sessionID != "session" || chat.owner != "Alice" || chat.requestID != "chat-request" ||
		chat.patternID != "pattern" || chat.patternHash != "pattern-hash" || chat.sourceRequestID != "source-request" {
		t.Fatalf("chat call = %+v", chat)
	}
	if fixes.pattern.SharedRootCause != "snapshot cause" || fixes.target.JobID != "periodic-x" ||
		fixes.target.BuildID != "123" || fixes.instruction != "keep compatibility" {
		t.Fatalf("target=%+v instruction=%q", fixes.target, fixes.instruction)
	}
	context := fixes.generationContext
	if context.AssistantAnswer != "selected answer" || context.ProposedRevision == nil || len(context.ArtifactCitations) != 1 ||
		context.Source == nil || context.Source.Finding != "source finding" || len(context.Source.Citations) != 1 {
		t.Fatalf("generation context = %+v", context)
	}
}

func TestPreviewChatFixStopsBeforeGenerationOnChatErrors(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "ownership", err: analysischat.ErrSessionNotFound},
		{name: "stale", err: analysischat.ErrAnalysisChanged},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			chat := &fakeChatStore{candidateErr: testCase.err}
			fixes := &fakeFixPreviewer{}
			_, err := NewService(chat, fixes).PreviewChatFix(t.Context(), "session", "alice", "request", "pattern", "pattern-hash", "source-request", "token", "")
			if !errors.Is(err, testCase.err) {
				t.Fatalf("error = %v", err)
			}
			if fixes.called {
				t.Fatal("fix generation ran after chat validation failed")
			}
		})
	}
}

func TestPreviewChatFixRejectsInvalidSelectionBeforeReadingChat(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		patternID   string
		patternHash string
		instruction string
	}{
		{name: "missing pattern", patternHash: "pattern-hash"},
		{name: "missing pattern hash", patternID: "pattern"},
		{name: "oversized instruction", patternID: "pattern", patternHash: "pattern-hash", instruction: strings.Repeat("x", 4097)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			chat := &fakeChatStore{}
			fixes := &fakeFixPreviewer{}
			_, err := NewService(chat, fixes).PreviewChatFix(
				t.Context(), "session", "alice", "request", testCase.patternID, testCase.patternHash, "", "token", testCase.instruction,
			)
			if !errors.Is(err, analysischat.ErrInvalidRequest) {
				t.Fatalf("error = %v", err)
			}
			if chat.sessionID != "" || fixes.called {
				t.Fatal("invalid selection reached chat or fix generation")
			}
		})
	}
}

func TestPreviewChatFixKeepsAtomicPatternSnapshotAfterPublishedReplacement(t *testing.T) {
	original := models.PatternAnalysis{
		ID: "stable-pattern", JobID: "periodic-x", SharedBuilds: []string{"123"}, SharedRootCause: "original cause",
	}
	original.ContentHash = models.PatternHash(original)
	published := original
	chat := &fakeChatStore{
		candidate: analysischat.FixCandidate{
			Analysis: analysischat.AnalysisRef{JobID: "periodic-x", BuildID: "123"},
			Pattern:  original, AssistantAnswer: "selected answer",
			ArtifactCitations: []analysischat.Citation{{Path: "build-log.txt", Quote: "failure"}},
			SourceRequestID:   "source-request",
			SourceRepository:  sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
			SourceRevision:    "0123456789abcdef0123456789abcdef01234567",
			SourceResult:      &sourceinvestigation.Result{State: sourceinvestigation.StateActionableCodeChange, Target: &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "retry"}, Finding: "source", Confidence: sourceinvestigation.ConfidenceHigh, Relationship: sourceinvestigation.RelationshipSupports, Direction: "modify retry", Citations: []sourceinvestigation.Citation{{Path: "pkg/retry.go", LineStart: 1, LineEnd: 1, Quote: "retry", Verified: true}}},
		},
		onReturn: func() {
			published.SharedRootCause = "replacement cause"
		},
	}
	fixes := &fakeFixPreviewer{}
	if _, err := NewService(chat, fixes).PreviewChatFix(
		t.Context(), "session", "alice", "request", original.ID, original.ContentHash, "source-request", "token", "",
	); err != nil {
		t.Fatal(err)
	}
	if published.SharedRootCause != "replacement cause" || fixes.pattern.SharedRootCause != "original cause" {
		t.Fatalf("published=%+v generated=%+v", published, fixes.pattern)
	}
}
