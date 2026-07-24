// Package chatfix bridges one selected analysis-chat response into fix generation.
package chatfix

import (
	"context"
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type chatStore interface {
	FixCandidate(sessionID, owner, requestID, sourceRequestID string) (analysischat.FixCandidate, error)
	ValidateFixCandidate(analysischat.FixCandidate) error
}

type fixPreviewer interface {
	PreviewFixWithContext(
		context.Context, string, string, string, actions.FixTarget, fixpr.GenerationContext,
	) (actions.PreviewResult, error)
}

// Service validates owner-bound chat context before fix generation.
type Service struct {
	chat  chatStore
	fixes fixPreviewer
}

// NewService builds the chat-to-fix bridge.
func NewService(chat chatStore, fixes fixPreviewer) *Service {
	return &Service{chat: chat, fixes: fixes}
}

// PreviewChatFix generates an existing fix preview from one selected answer.
func (s *Service) PreviewChatFix(
	ctx context.Context,
	sessionID, owner, requestID, patternID, sourceRequestID, userToken, instruction string,
) (actions.PreviewResult, error) {
	patternID = strings.TrimSpace(patternID)
	sourceRequestID = strings.TrimSpace(sourceRequestID)
	instruction = strings.TrimSpace(instruction)
	if patternID == "" || len(instruction) > 4096 {
		return actions.PreviewResult{}, fmt.Errorf("%w: pattern_id is required and instruction must not exceed 4096 bytes", analysischat.ErrInvalidRequest)
	}
	candidate, err := s.chat.FixCandidate(sessionID, owner, requestID, sourceRequestID)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	if err := s.chat.ValidateFixCandidate(candidate); err != nil {
		return actions.PreviewResult{}, err
	}
	generationContext := fixpr.GenerationContext{
		AssistantAnswer:   candidate.AssistantAnswer,
		ArtifactCitations: artifactEvidence(candidate.ArtifactCitations),
	}
	if candidate.ProposedRevision != nil {
		generationContext.ProposedRevision = &fixpr.RevisionContext{
			RootCause: candidate.ProposedRevision.RootCause, SuggestedFix: candidate.ProposedRevision.SuggestedFix,
		}
	}
	if candidate.SourceResult != nil {
		generationContext.Source = &fixpr.SourceContext{
			Finding:   candidate.SourceResult.Finding,
			Citations: sourceEvidence(candidate.SourceResult.Citations),
		}
	}
	return s.fixes.PreviewFixWithContext(
		ctx,
		patternID,
		userToken,
		instruction,
		actions.FixTarget{JobID: candidate.Analysis.JobID, BuildID: candidate.Analysis.BuildID},
		generationContext,
	)
}

func artifactEvidence(citations []analysischat.Citation) []fixpr.Evidence {
	out := make([]fixpr.Evidence, 0, len(citations))
	for _, citation := range citations {
		out = append(out, fixpr.Evidence{
			Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Quote: citation.Quote,
		})
	}
	return out
}

func sourceEvidence(citations []sourceinvestigation.Citation) []fixpr.Evidence {
	out := make([]fixpr.Evidence, 0, len(citations))
	for _, citation := range citations {
		out = append(out, fixpr.Evidence{
			Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Quote: citation.Quote,
		})
	}
	return out
}
