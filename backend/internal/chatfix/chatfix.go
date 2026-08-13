// Package chatfix bridges one selected analysis-chat response into fix generation.
package chatfix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	TestFixCandidate(sessionID, owner, requestID string) (analysischat.FixCandidate, error)
}

type fixPreviewer interface {
	PreviewFixWithContext(
		context.Context, models.PatternAnalysis, string, string, string, actions.FixTarget, fixpr.GenerationContext,
	) (actions.PreviewResult, error)
	PreviewAnalysisFix(context.Context, actions.AnalysisFixInput, string, string, string) (actions.PreviewResult, error)
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
	if len(instruction) > 4096 {
		return actions.PreviewResult{}, fmt.Errorf("%w: instruction must not exceed 4096 bytes", analysischat.ErrInvalidRequest)
	}
	if patternID == "" && patternHash == "" && sourceRequestID == "" {
		candidate, err := s.chat.TestFixCandidate(sessionID, owner, requestID)
		if err != nil {
			return actions.PreviewResult{}, err
		}
		input := actions.AnalysisFixInput{
			Identity: actions.AnalysisIdentity{
				JobID: candidate.Analysis.JobID, BuildID: candidate.Analysis.BuildID, TestName: candidate.Analysis.TestName,
				Source: candidate.Analysis.Source, SuiteName: candidate.Analysis.SuiteName, ClassName: candidate.Analysis.ClassName,
				JUnitFile: candidate.Analysis.JUnitFile, AnalysisGeneratedAt: candidate.Analysis.AnalysisGeneratedAt,
			},
			ChatSessionID: candidate.SessionID, ChatRequestID: candidate.RequestID, ChatResponseHash: candidate.ResponseHash,
			PreviewRequestHash: exactPreviewRequestHash(candidate, instruction), AnalysisContentHash: candidate.AnalysisContentHash,
			SourceRepository: candidate.SourceRepositorySnapshot,
			AssistantAnswer:  candidate.AssistantAnswer, ArtifactCitations: artifactEvidence(candidate.ArtifactCitations),
		}
		if candidate.ProposedRevision != nil {
			input.ProposedRevision = &fixpr.RevisionContext{RootCause: candidate.ProposedRevision.RootCause, SuggestedFix: candidate.ProposedRevision.SuggestedFix}
		}
		return s.fixes.PreviewAnalysisFix(ctx, input, owner, userToken, instruction)
	}
	if patternID == "" || patternHash == "" || sourceRequestID == "" {
		return actions.PreviewResult{}, fmt.Errorf("%w: legacy pattern fix requires pattern_id, pattern_hash, and source_request_id", analysischat.ErrInvalidRequest)
	}
	candidate, err := s.chat.FixCandidate(sessionID, owner, requestID, patternID, patternHash, sourceRequestID)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	if !models.PatternAllowsActions(candidate.Pattern) {
		return actions.PreviewResult{}, fmt.Errorf("%w: causal-group results are analysis-only", analysischat.ErrInvalidRequest)
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
		owner,
		userToken,
		instruction,
		actions.FixTarget{JobID: candidate.Analysis.JobID, BuildID: candidate.Analysis.BuildID},
		generationContext,
	)
}

func exactPreviewRequestHash(candidate analysischat.FixCandidate, instruction string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		candidate.SessionID, candidate.RequestID, candidate.ResponseHash, strings.TrimSpace(instruction),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ValidateAnalysisPreview rechecks the exact owner-bound chat response.
func (s *Service) ValidateAnalysisPreview(_ context.Context, owner string, binding actions.AnalysisPreviewBinding) error {
	candidate, err := s.chat.TestFixCandidate(binding.ChatSessionID, owner, binding.ChatRequestID)
	if err != nil {
		return err
	}
	ref := candidate.Analysis
	identity := binding.Identity
	if candidate.ResponseHash != binding.ChatResponseHash || ref.Scope != analysischat.ScopeTest ||
		candidate.AnalysisContentHash == "" || candidate.AnalysisContentHash != binding.AnalysisContentHash ||
		candidate.SourceRepositorySnapshot != binding.SourceRepository ||
		ref.JobID != identity.JobID || ref.BuildID != identity.BuildID || ref.TestName != identity.TestName ||
		ref.Source != identity.Source || ref.SuiteName != identity.SuiteName || ref.ClassName != identity.ClassName ||
		ref.JUnitFile != identity.JUnitFile || ref.AnalysisGeneratedAt != identity.AnalysisGeneratedAt {
		return analysischat.ErrAnalysisChanged
	}
	return nil
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
