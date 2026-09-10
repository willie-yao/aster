// Package chatfix bridges one selected analysis-chat response into fix generation.
package chatfix

import (
	"context"
	"fmt"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/models"
)

type chatStore interface {
	FixCandidate(sessionID, owner, requestID, patternID, patternHash string) (analysischat.FixCandidate, error)
	AnalysisFixCandidate(sessionID, owner, requestID string) (analysischat.FixCandidate, error)
}

type fixPreviewer interface {
	PreviewFixWithContext(
		context.Context, models.PatternAnalysis, string, string, string, actions.FixTarget, fixpr.GenerationContext,
	) (actions.PreviewResult, error)
}

type analysisFixRequester interface {
	FindAnalysisFixRequest(string, string, string, string) (actions.ActionRequestView, bool, error)
	CreateAnalysisFixRequest(context.Context, actions.AnalysisFixInput, string, string, string, ...string) (actions.ActionRequestView, error)
}

// Service validates shared chat context before fix generation.
type Service struct {
	chat     chatStore
	fixes    fixPreviewer
	requests analysisFixRequester
}

// NewService builds the chat-to-fix bridge.
func NewService(chat chatStore, fixes fixPreviewer) *Service {
	requests, _ := fixes.(analysisFixRequester)
	return &Service{chat: chat, fixes: fixes, requests: requests}
}

// PreviewChatFix generates an existing fix preview from one selected answer.
func (s *Service) PreviewChatFix(
	ctx context.Context,
	sessionID, owner, requestID, patternID, patternHash, userToken, instruction string,
) (actions.PreviewResult, error) {
	patternID = strings.TrimSpace(patternID)
	patternHash = strings.TrimSpace(patternHash)
	instruction = strings.TrimSpace(instruction)
	if len(instruction) > 4096 {
		return actions.PreviewResult{}, fmt.Errorf("%w: instruction must not exceed 4096 bytes", analysischat.ErrInvalidRequest)
	}
	if patternID == "" && patternHash == "" {
		return actions.PreviewResult{}, fmt.Errorf("%w: exact JUnit fix previews use asynchronous requests", analysischat.ErrInvalidRequest)
	}
	if patternID == "" || patternHash == "" {
		return actions.PreviewResult{}, fmt.Errorf("%w: legacy pattern fix requires pattern_id and pattern_hash", analysischat.ErrInvalidRequest)
	}
	candidate, err := s.chat.FixCandidate(sessionID, owner, requestID, patternID, patternHash)
	if err != nil {
		return actions.PreviewResult{}, err
	}
	if !models.PatternAllowsActions(candidate.Pattern) {
		return actions.PreviewResult{}, fmt.Errorf("%w: causal-group results are analysis-only", analysischat.ErrInvalidRequest)
	}
	generationContext := fixpr.GenerationContext{
		AssistantAnswer:           candidate.AssistantAnswer,
		AssistantUnverified:       candidate.AssistantUnverified,
		AssistantUnverifiedReason: candidate.AssistantUnverifiedReason,
		ArtifactCitations:         artifactEvidence(candidate.ArtifactCitations),
		EvidenceWarnings:          append([]string(nil), candidate.EvidenceWarnings...),
	}
	if candidate.ProposedRevision != nil {
		generationContext.ProposedRevision = &fixpr.RevisionContext{
			RootCause: candidate.ProposedRevision.RootCause, SuggestedFix: candidate.ProposedRevision.SuggestedFix,
		}
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

// CreateAnalysisFixRequest admits one exact JUnit chat finding for durable
// background preview generation.
func (s *Service) CreateAnalysisFixRequest(
	ctx context.Context, sessionID, owner, requestID, userToken, instruction string, replacesRequestIDs ...string,
) (actions.ActionRequestView, error) {
	instruction = strings.TrimSpace(instruction)
	if len(instruction) > 4096 {
		return actions.ActionRequestView{}, fmt.Errorf("%w: instruction must not exceed 4096 bytes", analysischat.ErrInvalidRequest)
	}
	if s.requests == nil {
		return actions.ActionRequestView{}, fmt.Errorf("%w: asynchronous exact JUnit fix previews are unavailable", analysischat.ErrInvalidRequest)
	}
	if request, found, err := s.requests.FindAnalysisFixRequest(sessionID, requestID, owner, instruction); err != nil || found {
		return request, err
	}
	candidate, err := s.chat.AnalysisFixCandidate(sessionID, owner, requestID)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	return s.requests.CreateAnalysisFixRequest(ctx, exactAnalysisFixInput(candidate, instruction), owner, userToken, instruction, replacesRequestIDs...)
}

func exactAnalysisFixInput(candidate analysischat.FixCandidate, instruction string) actions.AnalysisFixInput {
	input := actions.AnalysisFixInput{
		Origin: analysischat.FixOrigin{Analysis: candidate.Analysis, Original: candidate.Original, FixTarget: candidate.FixTarget},
		Identity: actions.AnalysisIdentity{
			JobID: candidate.FixTarget.JobID, BuildID: candidate.FixTarget.BuildID, TestName: candidate.FixTarget.TestName,
			Source: candidate.FixTarget.Source, SuiteName: candidate.FixTarget.SuiteName, ClassName: candidate.FixTarget.ClassName,
			JUnitFile: candidate.FixTarget.JUnitFile, AnalysisGeneratedAt: candidate.FixTarget.AnalysisGeneratedAt,
		},
		ChatSessionID: candidate.SessionID, ChatRequestID: candidate.RequestID, ChatResponseHash: candidate.ResponseHash,
		PreviewRequestHash: exactPreviewRequestHash(candidate, instruction), AnalysisContentHash: candidate.AnalysisContentHash,
		SourceRepository: candidate.SourceRepositorySnapshot,
		SourceBranch:     candidate.SourceBranch,
		AssistantAnswer:  candidate.AssistantAnswer, ArtifactCitations: artifactEvidence(candidate.ArtifactCitations),
		AssistantUnverified:       candidate.AssistantUnverified,
		AssistantUnverifiedReason: candidate.AssistantUnverifiedReason,
		EvidenceWarnings:          append([]string(nil), candidate.EvidenceWarnings...),
	}
	if candidate.ProposedRevision != nil {
		input.ProposedRevision = &fixpr.RevisionContext{RootCause: candidate.ProposedRevision.RootCause, SuggestedFix: candidate.ProposedRevision.SuggestedFix}
	}
	return input
}

func exactPreviewRequestHash(candidate analysischat.FixCandidate, instruction string) string {
	return actions.AnalysisFixRequestHash(candidate.SessionID, candidate.RequestID, instruction)
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
