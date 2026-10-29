package devmock

import (
	"context"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actions"
)

// ChatFix bridges one selected chat answer into a fix draft. The real bridge
// re-pins the source repository over the network before admitting the request;
// the mock admits any answer from a session it can find.
type ChatFix struct {
	actions *Actions
	chat    *Chat
}

func newChatFix(mockActions *Actions, chat *Chat) *ChatFix {
	return &ChatFix{actions: mockActions, chat: chat}
}

// PreviewChatFix drafts a fix from one chat answer.
func (f *ChatFix) PreviewChatFix(
	ctx context.Context,
	sessionID, login, _, _, _, _, instruction string,
) (actions.PreviewResult, error) {
	failureID, analysis, err := f.source(sessionID, login)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	return f.actions.preview(ctx, "fix", failureID, instruction, analysis)
}

// CreateAnalysisFixRequest starts an asynchronous fix draft from one chat answer.
func (f *ChatFix) CreateAnalysisFixRequest(
	_ context.Context,
	sessionID, login, _, _ string, _ actions.FixBranchTarget, instruction string, replaces ...string,
) (actions.ActionRequestView, error) {
	failureID, analysis, err := f.source(sessionID, login)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	return f.actions.createAnalysisFixRequest(failureID, login, instruction, analysis, replaces...)
}

// AnalysisFixBranches reports differing branches so the dialog shows its target choice.
func (f *ChatFix) AnalysisFixBranches(_ context.Context, sessionID, login, _ string) (actions.AnalysisFixBranches, error) {
	if _, _, err := f.source(sessionID, login); err != nil {
		return actions.AnalysisFixBranches{}, err
	}
	return actions.AnalysisFixBranches{Tested: "release-1.0", Default: "main"}, nil
}

// source resolves the analysis a session is about into the failure id the
// action service addresses, plus the published analysis the draft quotes. A
// pattern id names no single test, so the draft comes from the session's
// analysis rather than a lookup by id.
func (f *ChatFix) source(sessionID, login string) (string, publishedAnalysis, error) {
	session, analysis, err := f.chat.sessionAnalysis(sessionID, login)
	if err != nil {
		return "", publishedAnalysis{}, err
	}
	ref := session.Analysis
	if ref.PatternID != "" {
		return ref.PatternID, analysis, nil
	}
	return strings.Join([]string{ref.JobID, ref.BuildID, ref.TestName}, "/"), analysis, nil
}
