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
	candidate    analysischat.FixCandidate
	candidateErr error
	retainErr    error
	retained     bool
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

func (f *fakeChatStore) AnalysisFixCandidate(sessionID, owner, requestID string) (analysischat.FixCandidate, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	if f.onReturn != nil {
		f.onReturn()
	}
	return f.candidate, f.candidateErr
}

type fakeFixPreviewer struct {
	existingRequest   *actions.ActionRequestView
	lookupErr         error
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

func (f *fakeFixPreviewer) FindAnalysisFixRequest(_, _, _, _ string) (actions.ActionRequestView, bool, error) {
	if f.existingRequest != nil {
		return *f.existingRequest, true, f.lookupErr
	}
	return actions.ActionRequestView{}, false, f.lookupErr
}

func (f *fakeFixPreviewer) PreviewFixWithContext(
	_ context.Context, pattern models.PatternAnalysis, owner, userToken, instruction string, target actions.FixTarget, generationContext fixpr.GenerationContext,
) (actions.PreviewResult, error) {
	f.pattern, f.owner, f.userToken, f.instruction = pattern, owner, userToken, instruction
	f.target, f.generationContext, f.called = target, generationContext, true
	return actions.PreviewResult{Token: "preview", Kind: "fix"}, nil
}

func (f *fakeFixPreviewer) CreateAnalysisFixRequest(
	_ context.Context, input actions.AnalysisFixInput, owner, userToken, instruction string, _ ...string,
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
	if request.ID != "async-request" || !fixes.requestCalled || fixes.called || fixes.pattern.ID != "" || fixes.analysisInput.ChatResponseHash != "response-hash" {
		t.Fatalf("request=%+v fixes=%+v", request, fixes)
	}
	input := fixes.analysisInput
	if input.Identity.Project != "" || input.Identity.JobID != "periodic-capz" || input.Identity.BuildID != "123" || input.Identity.TestName != "TestCluster" ||
		input.Identity.JUnitFile != "junit.xml" || input.ChatSessionID != "session" || input.ChatRequestID != "request" ||
		input.AnalysisContentHash != "analysis-hash" || input.SourceRepository.Name != "repo" ||
		input.FailureRevision != "" ||
		input.GenerationBaseRevision != "" ||
		input.SourceBranch != "main" ||
		len(input.ArtifactCitations) != 1 || !slices.Equal(input.EvidenceWarnings, []string{"citation 2 was omitted"}) ||
		input.ProposedRevision == nil || fixes.userToken != "write-token" {
		t.Fatalf("analysis input = %+v", input)
	}
}

func TestCreateAnalysisFixRequestCarriesUnverifiedUncitedFinding(t *testing.T) {
	chat := &fakeChatStore{candidate: analysischat.FixCandidate{
		SessionID: "session", RequestID: "request", ResponseHash: "qualified-response",
		AnalysisContentHash: "analysis",
		AssistantAnswer:     "Investigate retries around the recurring timeout.",
		AssistantUnverified: true, AssistantUnverifiedReason: analysischat.UnverifiedCitation,
		FixTarget: analysischat.AnalysisRef{
			Scope: analysischat.ScopeTest, JobID: "job", BuildID: "123", TestName: "test",
			JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-09-10T00:00:00Z",
		},
	}}
	fixes := &fakeFixPreviewer{}
	request, err := NewService(chat, fixes).CreateAnalysisFixRequest(
		t.Context(), "session", "Alice", "request", "token", "",
	)
	if err != nil || request.ID != "async-request" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	if !fixes.analysisInput.AssistantUnverified || fixes.analysisInput.AssistantUnverifiedReason != analysischat.UnverifiedCitation ||
		len(fixes.analysisInput.ArtifactCitations) != 0 || fixes.analysisInput.ChatResponseHash != "qualified-response" {
		t.Fatalf("input=%+v", fixes.analysisInput)
	}
}

func TestCreateAnalysisFixRequestReturnsAdmissionFailure(t *testing.T) {
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
	if !errors.Is(err, admissionErr) || !fixes.requestCalled {
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

func TestExactPreviewRequestHashChangesWithRegenerationFeedback(t *testing.T) {
	candidate := analysischat.FixCandidate{SessionID: "session", RequestID: "request", ResponseHash: "response"}
	first := exactPreviewRequestHash(candidate, "keep compatibility")
	second := exactPreviewRequestHash(candidate, "retry conflicts")
	if first == second || first != exactPreviewRequestHash(candidate, " keep compatibility ") {
		t.Fatalf("hashes first=%q second=%q", first, second)
	}
}

func TestCreateAnalysisFixRequestRejectsIneligibleSource(t *testing.T) {
	chat := &fakeChatStore{candidateErr: analysischat.ErrAnalysisChanged}
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
	if fixes.analysisInput.Origin.Analysis != chat.candidate.Analysis || fixes.analysisInput.Origin.FixTarget != target {
		t.Fatalf("origin = %+v", fixes.analysisInput.Origin)
	}
}

func TestCreateAnalysisFixRequestReconnectsWithoutChat(t *testing.T) {
	chat := &fakeChatStore{candidateErr: analysischat.ErrSessionNotFound}
	fixes := &fakeFixPreviewer{existingRequest: &actions.ActionRequestView{ID: "admitted", Owner: "alice", Status: actions.RequestReady}}
	view, err := NewService(chat, fixes).CreateAnalysisFixRequest(t.Context(), "deleted", "Alice", "answer", "token", "")
	if err != nil || view.ID != "admitted" || fixes.requestCalled || chat.sessionID != "" {
		t.Fatalf("view=%+v err=%v chat=%+v", view, err, chat)
	}
}

func (f *fakeChatStore) RetainForFix(_, _, _ string) error { f.retained = true; return f.retainErr }

func TestCreateAnalysisFixRetainsSelectedFindingBeforeAdmission(t *testing.T) {
	for _, fail := range []bool{false, true} {
		chat := &fakeChatStore{candidate: analysischat.FixCandidate{SessionID: "session", RequestID: "finding", Analysis: analysischat.AnalysisRef{Scope: analysischat.ScopeTest}}}
		if fail {
			chat.retainErr = errors.New("history storage failed")
		}
		fixes := &fakeFixPreviewer{}
		_, err := NewService(chat, fixes).CreateAnalysisFixRequest(t.Context(), "session", "alice", "finding", "token", "")
		if !chat.retained || (fail && (!errors.Is(err, chat.retainErr) || fixes.requestCalled)) || (!fail && !fixes.requestCalled) {
			t.Fatalf("retention: chat=%+v called=%v err=%v", chat, fixes.requestCalled, err)
		}
	}
}
