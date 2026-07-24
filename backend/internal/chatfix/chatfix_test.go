package chatfix

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeChatStore struct {
	candidate       analysischat.FixCandidate
	candidateErr    error
	validateErr     error
	sessionID       string
	owner           string
	requestID       string
	sourceRequestID string
}

func (f *fakeChatStore) FixCandidate(sessionID, owner, requestID, sourceRequestID string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID, f.sourceRequestID = sessionID, owner, requestID, sourceRequestID
	return f.candidate, f.candidateErr
}

func (f *fakeChatStore) ValidateFixCandidate(candidate analysischat.FixCandidate) error {
	return f.validateErr
}

type fakeFixPreviewer struct {
	patternID         string
	userToken         string
	instruction       string
	target            actions.FixTarget
	generationContext fixpr.GenerationContext
	called            bool
}

func (f *fakeFixPreviewer) PreviewFixWithContext(
	_ context.Context, patternID, userToken, instruction string, target actions.FixTarget, generationContext fixpr.GenerationContext,
) (actions.PreviewResult, error) {
	f.patternID, f.userToken, f.instruction = patternID, userToken, instruction
	f.target, f.generationContext, f.called = target, generationContext, true
	return actions.PreviewResult{Token: "preview", Kind: "fix"}, nil
}

func TestPreviewChatFixBuildsSelectedContext(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		Analysis:          analysischat.AnalysisRef{JobID: "periodic-x", BuildID: "123"},
		AssistantAnswer:   "selected answer",
		ProposedRevision:  &analysischat.Revision{RootCause: "new cause", SuggestedFix: "new fix"},
		ArtifactCitations: []analysischat.Citation{{Path: "build-log.txt", LineStart: 4, LineEnd: 5, Quote: "failure"}},
		SourceResult: &sourceinvestigation.Result{
			Finding:   "source finding",
			Citations: []sourceinvestigation.Citation{{Path: "pkg/retry.go", LineStart: 10, LineEnd: 12, Quote: "retry", Verified: true}},
		},
	}}
	fixes := &fakeFixPreviewer{}
	service := NewService(chat, fixes)
	preview, err := service.PreviewChatFix(
		t.Context(), "session", "Alice", "chat-request", "pattern", "source-request", "user-token", "keep compatibility",
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token != "preview" || !fixes.called || fixes.patternID != "pattern" || fixes.userToken != "user-token" {
		t.Fatalf("preview=%+v fixes=%+v", preview, fixes)
	}
	if chat.sessionID != "session" || chat.owner != "Alice" || chat.requestID != "chat-request" || chat.sourceRequestID != "source-request" {
		t.Fatalf("chat call = %+v", chat)
	}
	if fixes.target.JobID != "periodic-x" || fixes.target.BuildID != "123" || fixes.instruction != "keep compatibility" {
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
		name         string
		candidateErr error
		validateErr  error
	}{
		{name: "ownership", candidateErr: analysischat.ErrSessionNotFound},
		{name: "stale", validateErr: analysischat.ErrAnalysisChanged},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			chat := &fakeChatStore{
				candidate:    analysischat.FixCandidate{Analysis: analysischat.AnalysisRef{JobID: "job", BuildID: "build"}},
				candidateErr: testCase.candidateErr, validateErr: testCase.validateErr,
			}
			fixes := &fakeFixPreviewer{}
			_, err := NewService(chat, fixes).PreviewChatFix(t.Context(), "session", "alice", "request", "pattern", "", "token", "")
			if !errors.Is(err, testCase.candidateErr) && !errors.Is(err, testCase.validateErr) {
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
		instruction string
	}{
		{name: "missing pattern"},
		{name: "oversized instruction", patternID: "pattern", instruction: strings.Repeat("x", 4097)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			chat := &fakeChatStore{}
			fixes := &fakeFixPreviewer{}
			_, err := NewService(chat, fixes).PreviewChatFix(
				t.Context(), "session", "alice", "request", testCase.patternID, "", "token", testCase.instruction,
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
