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
	failureID, err := f.failureID(sessionID, login)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	return f.actions.PreviewFix(ctx, failureID, login, "", instruction)
}

// CreateAnalysisFixRequest starts an asynchronous fix draft from one chat answer.
func (f *ChatFix) CreateAnalysisFixRequest(
	_ context.Context,
	sessionID, login, _, _, instruction string, replaces ...string,
) (actions.ActionRequestView, error) {
	failureID, err := f.failureID(sessionID, login)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	return f.actions.createAnalysisFixRequest(failureID, login, instruction, replaces...)
}

// failureID resolves the analysis a session is about into the failure id the
// action service addresses.
func (f *ChatFix) failureID(sessionID, login string) (string, error) {
	session, err := f.chat.Get(sessionID, login)
	if err != nil {
		return "", err
	}
	ref := session.Analysis
	if ref.PatternID != "" {
		return ref.PatternID, nil
	}
	return strings.Join([]string{ref.JobID, ref.BuildID, ref.TestName}, "/"), nil
}
