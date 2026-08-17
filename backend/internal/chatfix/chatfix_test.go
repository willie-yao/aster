package chatfix

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type fakeChatStore struct {
	candidate    analysischat.FixCandidate
	candidateErr error
	onReturn     func()
	sessionID    string
	owner        string
	requestID    string
	patternID    string
	patternHash  string
}

func (f *fakeChatStore) FixCandidate(sessionID, owner, requestID, patternID, patternHash string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.patternID, f.patternHash = patternID, patternHash
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

func (f *fakeChatStore) TestFixCandidate(sessionID, owner, requestID string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

type fakeFixPreviewer struct {
	pattern           models.PatternAnalysis
	owner             string
	userToken         string
	instruction       string
	target            actions.FixTarget
	generationContext fixpr.GenerationContext
	called            bool
	requestCalled     bool
	analysisInput     actions.AnalysisFixInput
}

func (f *fakeFixPreviewer) PreviewFixWithContext(
	_ context.Context, pattern models.PatternAnalysis, owner, userToken, instruction string, target actions.FixTarget, generationContext fixpr.GenerationContext,
) (actions.PreviewResult, error) {
	f.pattern, f.owner, f.userToken, f.instruction = pattern, owner, userToken, instruction
	f.target, f.generationContext, f.called = target, generationContext, true
	return actions.PreviewResult{Token: "preview", Kind: "fix"}, nil
}

func (f *fakeFixPreviewer) PreviewAnalysisFix(
	_ context.Context, input actions.AnalysisFixInput, owner, userToken, instruction string,
) (actions.PreviewResult, error) {
	f.analysisInput, f.owner, f.userToken, f.instruction, f.called = input, owner, userToken, instruction, true
	return actions.PreviewResult{Token: "preview", Kind: "fix"}, nil
}

func (f *fakeFixPreviewer) CreateAnalysisFixRequest(
	input actions.AnalysisFixInput, owner, userToken, instruction string, _ ...string,
) (actions.ActionRequestView, error) {
	f.analysisInput, f.owner, f.userToken, f.instruction, f.requestCalled = input, owner, userToken, instruction, true
	return actions.ActionRequestView{ID: "async-request", Kind: "analysis-fix", Owner: owner, Status: actions.RequestPending}, nil
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
	}}
	fixes := &fakeFixPreviewer{}
	service := NewService(chat, fixes)
	preview, err := service.PreviewChatFix(
		t.Context(), "session", "Alice", "chat-request", "pattern", "pattern-hash", "user-token", "keep compatibility",
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token != "preview" || !fixes.called || fixes.pattern.ID != "pattern" || fixes.owner != "Alice" || fixes.userToken != "user-token" {
		t.Fatalf("preview=%+v fixes=%+v", preview, fixes)
	}
	if chat.sessionID != "session" || chat.owner != "Alice" || chat.requestID != "chat-request" ||
		chat.patternID != "pattern" || chat.patternHash != "pattern-hash" {
		t.Fatalf("chat call = %+v", chat)
	}
	if fixes.pattern.SharedRootCause != "snapshot cause" || fixes.target.JobID != "periodic-x" ||
		fixes.target.BuildID != "123" || fixes.instruction != "keep compatibility" {
		t.Fatalf("target=%+v instruction=%q", fixes.target, fixes.instruction)
	}
	context := fixes.generationContext
	if context.AssistantAnswer != "selected answer" || context.ProposedRevision == nil || len(context.ArtifactCitations) != 1 {
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
			_, err := NewService(chat, fixes).PreviewChatFix(t.Context(), "session", "alice", "request", "pattern", "pattern-hash", "token", "")
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
				t.Context(), "session", "alice", "request", testCase.patternID, testCase.patternHash, "token", testCase.instruction,
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
		},
		onReturn: func() {
			published.SharedRootCause = "replacement cause"
		},
	}
	fixes := &fakeFixPreviewer{}
	if _, err := NewService(chat, fixes).PreviewChatFix(
		t.Context(), "session", "alice", "request", original.ID, original.ContentHash, "token", "",
	); err != nil {
		t.Fatal(err)
	}
	if published.SharedRootCause != "replacement cause" || fixes.pattern.SharedRootCause != "original cause" {
		t.Fatalf("published=%+v generated=%+v", published, fixes.pattern)
	}
}

func TestPreviewChatFixRejectsAnalysisOnlyCausalGroup(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{Pattern: models.PatternAnalysis{
		Recurrence:   models.PatternRecurrenceSharedCause,
		CausalGroups: []models.PatternCausalGroup{{ID: "group", ContentHash: "hash", Builds: []string{"2", "1"}, RootCause: "cause", Confidence: "high"}},
		RemediationInvestigations: []models.PatternRemediationInvestigationSummary{{
			CausalGroupID: "group", CausalGroupHash: "hash", State: models.PatternRemediationActionable,
		}},
	}}}
	fixes := &fakeFixPreviewer{}
	_, err := NewService(chat, fixes).PreviewChatFix(
		t.Context(), "session", "Alice", "chat-request", "pattern", "pattern-hash", "user-token", "",
	)
	if !errors.Is(err, analysischat.ErrInvalidRequest) || fixes.called {
		t.Fatalf("error=%v called=%t", err, fixes.called)
	}
}

func TestCreateAnalysisFixRequestUsesExactJUnitAnalysisWithoutPatternAuthority(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		SessionID: "session", RequestID: "request", ResponseHash: "response-hash",
		AnalysisContentHash:      "analysis-hash",
		SourceRepositorySnapshot: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
		FailureRevision:          "0123456789abcdef0123456789abcdef01234567",
		GenerationBaseRevision:   "fedcba9876543210fedcba9876543210fedcba98",
		VerifiedSourceFileHashes: map[string]string{"pkg/controller.go": strings.Repeat("d", 64)},
		SourceBranch:             "main", SourceBranchKnown: true,
		Analysis: analysischat.AnalysisRef{
			Scope: analysischat.ScopeTest, JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster",
			SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
		},
		AssistantAnswer:   "The artifact supports changing the terminal branch.",
		ArtifactCitations: []analysischat.Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		ProposedRevision:  &analysischat.Revision{RootCause: "terminal branch omits Ready", SuggestedFix: "record Ready"},
	}}
	fixes := &fakeFixPreviewer{}
	request, err := NewService(chat, fixes).CreateAnalysisFixRequest("session", "Alice", "request", "write-token", "keep compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "async-request" || !fixes.requestCalled || fixes.called || fixes.pattern.ID != "" || fixes.analysisInput.ChatResponseHash != "response-hash" {
		t.Fatalf("request=%+v fixes=%+v", request, fixes)
	}
	input := fixes.analysisInput
	if input.Identity.Project != "" || input.Identity.JobID != "periodic-capz" || input.Identity.BuildID != "123" || input.Identity.TestName != "TestCluster" ||
		input.Identity.JUnitFile != "junit.xml" || input.ChatSessionID != "session" || input.ChatRequestID != "request" ||
		input.AnalysisContentHash != "analysis-hash" || input.SourceRepository.Name != "repo" ||
		input.FailureRevision != "0123456789abcdef0123456789abcdef01234567" ||
		input.GenerationBaseRevision != "fedcba9876543210fedcba9876543210fedcba98" ||
		input.VerifiedSourceFileHashes["pkg/controller.go"] != strings.Repeat("d", 64) ||
		input.SourceBranch != "main" ||
		len(input.ArtifactCitations) != 1 || input.ProposedRevision == nil || fixes.userToken != "write-token" {
		t.Fatalf("analysis input = %+v", input)
	}
}

func TestPreviewChatFixRejectsSynchronousExactJUnitGeneration(t *testing.T) {
	fixes := &fakeFixPreviewer{}
	_, err := NewService(&fakeChatStore{}, fixes).PreviewChatFix(
		t.Context(), "session", "Alice", "request", "", "", "write-token", "",
	)
	if !errors.Is(err, analysischat.ErrInvalidRequest) || fixes.called || fixes.requestCalled {
		t.Fatalf("error=%v fixes=%+v", err, fixes)
	}
}

func TestValidateAnalysisPreviewRejectsChangedChatIdentity(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		SessionID: "session", RequestID: "request", ResponseHash: "new-response",
		AnalysisContentHash:      "analysis-hash",
		SourceRepositorySnapshot: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
		Analysis: analysischat.AnalysisRef{
			Scope: analysischat.ScopeTest, JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster",
			JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
		},
	}}
	service := NewService(chat, &fakeFixPreviewer{})
	binding := actions.AnalysisPreviewBinding{
		Identity: actions.AnalysisIdentity{
			JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
			AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
		},
		AnalysisHash: "action-hash", AnalysisContentHash: "analysis-hash",
		SourceRepository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
		ChatSessionID:    "session", ChatRequestID: "request", ChatResponseHash: "old-response",
	}
	if err := service.ValidateAnalysisPreview(t.Context(), "Alice", binding); !errors.Is(err, analysischat.ErrAnalysisChanged) {
		t.Fatalf("changed chat validation error = %v", err)
	}
	chat.candidate.ResponseHash = "old-response"
	if err := service.ValidateAnalysisPreview(t.Context(), "Alice", binding); err != nil {
		t.Fatalf("unchanged chat validation error = %v", err)
	}
}

func TestExactPreviewRequestHashChangesWithRegenerationFeedback(t *testing.T) {
	candidate := analysischat.FixCandidate{SessionID: "session", RequestID: "request", ResponseHash: "response"}
	first := exactPreviewRequestHash(candidate, "keep compatibility")
	second := exactPreviewRequestHash(candidate, "retry conflicts")
	if first == second || first != exactPreviewRequestHash(candidate, " keep compatibility ") {
		t.Fatalf("hashes first=%q second=%q", first, second)
	}
}
