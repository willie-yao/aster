// Package chatfix bridges one selected analysis-chat response into fix generation.
package chatfix

import (
	"context"
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type chatStore interface {
	FixCandidate(sessionID, owner, requestID, patternID, patternHash, sourceRequestID string) (analysischat.FixCandidate, error)
}

type fixPreviewer interface {
	PreviewFixWithContext(
		context.Context, models.PatternAnalysis, string, string, actions.FixTarget, fixpr.GenerationContext,
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
	sessionID, owner, requestID, patternID, patternHash, sourceRequestID, userToken, instruction string,
) (actions.PreviewResult, error) {
	patternID = strings.TrimSpace(patternID)
	patternHash = strings.TrimSpace(patternHash)
	sourceRequestID = strings.TrimSpace(sourceRequestID)
	instruction = strings.TrimSpace(instruction)
	if patternID == "" || patternHash == "" || sourceRequestID == "" || len(instruction) > 4096 {
		return actions.PreviewResult{}, fmt.Errorf("%w: pattern_id, pattern_hash, and source_request_id are required and instruction must not exceed 4096 bytes", analysischat.ErrInvalidRequest)
	}
	candidate, err := s.chat.FixCandidate(sessionID, owner, requestID, patternID, patternHash, sourceRequestID)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	if candidate.SourceRequestID != sourceRequestID || candidate.SourceResult == nil || candidate.SourceResult.Target == nil ||
		sourceinvestigation.ValidateVerifiedResult(*candidate.SourceResult) != nil ||
		(candidate.SourceResult.State != sourceinvestigation.StateActionableCodeChange && candidate.SourceResult.State != sourceinvestigation.StateActionableConfigurationChange) ||
		sourceinvestigation.ValidateRepository(candidate.SourceRepository) != nil || !strings.EqualFold(candidate.SourceRepository.Revision, candidate.SourceRevision) {
		return actions.PreviewResult{}, fmt.Errorf("%w: completed actionable source investigation is required", sourceinvestigation.ErrInvalidResult)
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
	generationContext.Source = &fixpr.SourceContext{
		Repository: candidate.SourceRepository.Owner + "/" + candidate.SourceRepository.Name,
		State:      candidate.SourceResult.State, Target: *candidate.SourceResult.Target,
		Finding: candidate.SourceResult.Finding, Revision: candidate.SourceRevision,
		Citations: sourceEvidence(candidate.SourceResult.Citations),
	}
	return s.fixes.PreviewFixWithContext(
		ctx,
		candidate.Pattern,
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
