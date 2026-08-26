package chatfix

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type fakeChatStore struct {
	candidate             analysischat.FixCandidate
	candidateErr          error
	preflightErr          error
	reserveErr            error
	releaseErr            error
	preflighted           bool
	reserveCalled         bool
	commitCalled          bool
	releaseCalled         bool
	reservations          map[string]bool
	references            map[string]bool
	pinnedFailureRevision string
	pinnedGenerationBase  string
	onReturn              func()
	sessionID             string
	owner                 string
	requestID             string
	patternID             string
	patternHash           string
}

func (f *fakeChatStore) PreflightAnalysisFix(_ context.Context, sessionID, owner, requestID string) error {
	f.preflighted = true
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.preflightErr != nil {
		return f.preflightErr
	}
	// Pinning the source is what produces the binding the candidate carries, so
	// the fake supplies it only once preflight has run.
	if f.pinnedFailureRevision != "" {
		f.candidate.FailureRevision = f.pinnedFailureRevision
	}
	if f.pinnedGenerationBase != "" {
		f.candidate.GenerationBaseRevision = f.pinnedGenerationBase
	}
	return nil
}

func (f *fakeChatStore) FixCandidate(sessionID, owner, requestID, patternID, patternHash string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.patternID, f.patternHash = patternID, patternHash
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

func (f *fakeChatStore) AnalysisFixCandidate(sessionID, owner, requestID string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

func (f *fakeChatStore) ReserveAnalysisFix(sessionID, owner, requestID, reservationID string) error {
	f.reserveCalled = true
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.reserveErr != nil {
		return f.reserveErr
	}
	if f.reservations == nil {
		f.reservations = map[string]bool{}
	}
	f.reservations[reservationID] = true
	return nil
}

func (f *fakeChatStore) CommitAnalysisFix(sessionID, owner, requestID, reservationID, referenceID string) error {
	f.commitCalled = true
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	delete(f.reservations, reservationID)
	if f.references == nil {
		f.references = map[string]bool{}
	}
	f.references[referenceID] = true
	return nil
}

func (f *fakeChatStore) ReleaseAnalysisFix(sessionID, owner, requestID, reservationID string) error {
	f.releaseCalled = true
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.releaseErr != nil {
		return f.releaseErr
	}
	delete(f.reservations, reservationID)
	return nil
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
	requestErr        error
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
	if f.requestErr != nil {
		return actions.ActionRequestView{}, f.requestErr
	}
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
		FixTarget: analysischat.AnalysisRef{
			Scope: analysischat.ScopeTest, JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster",
			SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
		},
		AssistantAnswer:   "The artifact supports changing the terminal branch.",
		ArtifactCitations: []analysischat.Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		EvidenceWarnings:  []string{"citation 2 was omitted"},
		ProposedRevision:  &analysischat.Revision{RootCause: "terminal branch omits Ready", SuggestedFix: "record Ready"},
	}}
	fixes := &fakeFixPreviewer{}
	request, err := NewService(chat, fixes).CreateAnalysisFixRequest(context.Background(), "session", "Alice", "request", "write-token", "keep compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "async-request" || !fixes.requestCalled || fixes.called || fixes.pattern.ID != "" || fixes.analysisInput.ChatResponseHash != "response-hash" ||
		!chat.reserveCalled || !chat.commitCalled || len(chat.references) != 1 || len(chat.reservations) != 0 || chat.releaseCalled {
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
		len(input.ArtifactCitations) != 1 || !slices.Equal(input.EvidenceWarnings, []string{"citation 2 was omitted"}) ||
		input.ProposedRevision == nil || fixes.userToken != "write-token" {
		t.Fatalf("analysis input = %+v", input)
	}
}

func TestCreateAnalysisFixRequestRollsBackReservationWhenAdmissionFails(t *testing.T) {
	admissionErr := errors.New("action request admission failed")
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		SessionID: "session", RequestID: "request", ResponseHash: "response",
		Analysis:  analysischat.AnalysisRef{Scope: analysischat.ScopeTest},
		FixTarget: analysischat.AnalysisRef{Scope: analysischat.ScopeTest},
	}}
	fixes := &fakeFixPreviewer{requestErr: admissionErr}
	_, err := NewService(chat, fixes).CreateAnalysisFixRequest(
		t.Context(), "session", "Alice", "request", "token", "",
	)
	if !errors.Is(err, admissionErr) || !chat.reserveCalled || chat.commitCalled || !chat.releaseCalled || len(chat.reservations) != 0 || len(chat.references) != 0 {
		t.Fatalf("error=%v chat=%+v", err, chat)
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
		FixTarget: analysischat.AnalysisRef{
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

// The chat turn no longer pins the source, so the fix request must, and it must
// do so BEFORE building the candidate. Pinning afterwards would rebuild the
// weaker path this change removed, so the pin has to reach the action input.
func TestCreateAnalysisFixRequestPinsSourceBeforeBuildingCandidate(t *testing.T) {
	chat := &fakeChatStore{
		pinnedFailureRevision: "0123456789abcdef0123456789abcdef01234567",
		pinnedGenerationBase:  "fedcba9876543210fedcba9876543210fedcba98",
		candidate: analysischat.FixCandidate{
			SessionID: "session-1", RequestID: "request-1",
			Analysis: analysischat.AnalysisRef{Scope: analysischat.ScopeTest, JobID: "job", BuildID: "1", TestName: "Test"},
		},
	}
	fixes := &fakeFixPreviewer{}
	if _, err := NewService(chat, fixes).CreateAnalysisFixRequest(
		context.Background(), "session-1", "Alice", "request-1", "token", "make it retry",
	); err != nil {
		t.Fatalf("CreateAnalysisFixRequest error = %v", err)
	}
	if !chat.preflighted {
		t.Fatal("fix request did not pin the source")
	}
	if fixes.analysisInput.FailureRevision != chat.pinnedFailureRevision ||
		fixes.analysisInput.GenerationBaseRevision != chat.pinnedGenerationBase {
		t.Fatalf("pinned source did not reach the fix request: %+v", fixes.analysisInput)
	}
}

func TestCreateAnalysisFixRequestRejectsIneligibleSource(t *testing.T) {
	chat := &fakeChatStore{preflightErr: analysischat.ErrAnalysisChanged}
	fixes := &fakeFixPreviewer{}
	_, err := NewService(chat, fixes).CreateAnalysisFixRequest(
		context.Background(), "session-1", "Alice", "request-1", "token", "make it retry",
	)
	if !errors.Is(err, analysischat.ErrAnalysisChanged) {
		t.Fatalf("CreateAnalysisFixRequest error = %v", err)
	}
	if fixes.requestCalled {
		t.Fatal("a fix request was admitted despite an ineligible source")
	}
}

func TestCreateAnalysisFixRequestUsesCauseRepresentativeFailure(t *testing.T) {
	target := analysischat.AnalysisRef{
		Scope: analysischat.ScopeTest, JobID: "periodic-capz", BuildID: "209", TestName: "TestFlatcar",
		JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-08-25T01:00:00Z",
	}
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		SessionID: "session", RequestID: "request", ResponseHash: "response-hash",
		Analysis: analysischat.AnalysisRef{
			Scope: analysischat.ScopeCause, JobID: "periodic-capz", PatternID: "pattern", PatternHash: "pattern-hash",
			CausalGroupID: "cause", CausalGroupHash: "cause-hash",
		},
		FixTarget: target, AnalysisContentHash: "analysis-hash",
		SourceRepositorySnapshot: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"},
		FailureRevision:          "0123456789abcdef0123456789abcdef01234567",
		GenerationBaseRevision:   "fedcba9876543210fedcba9876543210fedcba98",
		VerifiedSourceFileHashes: map[string]string{"pkg/controller.go": strings.Repeat("d", 64)},
		AssistantAnswer:          "The two cause builds support changing the controller.",
		ArtifactCitations:        []analysischat.Citation{{Path: "builds/209/build-log.txt", Quote: "resource not found"}},
	}}
	fixes := &fakeFixPreviewer{}
	request, err := NewService(chat, fixes).CreateAnalysisFixRequest(
		t.Context(), "session", "Alice", "request", "write-token", "keep compatibility",
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "async-request" || fixes.analysisInput.Identity.BuildID != target.BuildID ||
		fixes.analysisInput.Identity.TestName != target.TestName || fixes.analysisInput.Identity.JUnitFile != target.JUnitFile {
		t.Fatalf("request=%+v input=%+v", request, fixes.analysisInput)
	}
	binding := actions.AnalysisPreviewBinding{
		Identity: fixes.analysisInput.Identity, AnalysisContentHash: "analysis-hash",
		SourceRepository: chat.candidate.SourceRepositorySnapshot,
		ChatSessionID:    "session", ChatRequestID: "request", ChatResponseHash: "response-hash",
		FailureRevision: chat.candidate.FailureRevision, GenerationBaseRevision: chat.candidate.GenerationBaseRevision,
		VerifiedSourceFileHashes: chat.candidate.VerifiedSourceFileHashes,
	}
	if err := NewService(chat, fixes).ValidateAnalysisPreview(t.Context(), "Alice", binding); err != nil {
		t.Fatalf("cause preview validation error = %v", err)
	}
}
