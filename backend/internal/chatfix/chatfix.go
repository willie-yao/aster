// Package chatfix bridges one selected analysis-chat response into fix generation.
package chatfix

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	PreflightAnalysisFix(ctx context.Context, sessionID, owner, requestID string) error
	ReserveAnalysisFix(sessionID, owner, requestID, reservationID string) error
	CommitAnalysisFix(sessionID, owner, requestID, reservationID, referenceID string) error
	ReleaseAnalysisFix(sessionID, owner, requestID, reservationID string) error
}

type fixPreviewer interface {
	PreviewFixWithContext(
		context.Context, models.PatternAnalysis, string, string, string, actions.FixTarget, fixpr.GenerationContext,
	) (actions.PreviewResult, error)
}

type analysisFixRequester interface {
	CreateAnalysisFixRequest(actions.AnalysisFixInput, string, string, string, ...string) (actions.ActionRequestView, error)
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
		AssistantAnswer:   candidate.AssistantAnswer,
		ArtifactCitations: artifactEvidence(candidate.ArtifactCitations),
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
	// Pin the source the patch will be generated against. Chat turns do not do
	// this, so that asking a question never depends on source verification.
	if err := s.chat.PreflightAnalysisFix(ctx, sessionID, owner, requestID); err != nil {
		return actions.ActionRequestView{}, err
	}
	candidate, err := s.chat.AnalysisFixCandidate(sessionID, owner, requestID)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	input := exactAnalysisFixInput(candidate, instruction)
	reservationID, err := newFixReservationID()
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	if err := s.chat.ReserveAnalysisFix(sessionID, owner, requestID, reservationID); err != nil {
		return actions.ActionRequestView{}, err
	}
	request, err := s.requests.CreateAnalysisFixRequest(input, owner, userToken, instruction, replacesRequestIDs...)
	if err != nil {
		if releaseErr := s.chat.ReleaseAnalysisFix(sessionID, owner, requestID, reservationID); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("releasing analysis Fix reservation: %w", releaseErr))
		}
		return actions.ActionRequestView{}, err
	}
	if err := s.chat.CommitAnalysisFix(sessionID, owner, requestID, reservationID, request.ID); err != nil {
		return actions.ActionRequestView{}, fmt.Errorf("committing analysis Fix reference: %w", err)
	}
	return request, nil
}

func newFixReservationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("creating analysis Fix reservation: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func exactAnalysisFixInput(candidate analysischat.FixCandidate, instruction string) actions.AnalysisFixInput {
	input := actions.AnalysisFixInput{
		Identity: actions.AnalysisIdentity{
			JobID: candidate.FixTarget.JobID, BuildID: candidate.FixTarget.BuildID, TestName: candidate.FixTarget.TestName,
			Source: candidate.FixTarget.Source, SuiteName: candidate.FixTarget.SuiteName, ClassName: candidate.FixTarget.ClassName,
			JUnitFile: candidate.FixTarget.JUnitFile, AnalysisGeneratedAt: candidate.FixTarget.AnalysisGeneratedAt,
		},
		ChatSessionID: candidate.SessionID, ChatRequestID: candidate.RequestID, ChatResponseHash: candidate.ResponseHash,
		PreviewRequestHash: exactPreviewRequestHash(candidate, instruction), AnalysisContentHash: candidate.AnalysisContentHash,
		SourceRepository: candidate.SourceRepositorySnapshot,
		FailureRevision:  candidate.FailureRevision, GenerationBaseRevision: candidate.GenerationBaseRevision,
		VerifiedSourceFileHashes: cloneStringMap(candidate.VerifiedSourceFileHashes),
		SourceBranch:             candidate.SourceBranch,
		AssistantAnswer:          candidate.AssistantAnswer, ArtifactCitations: artifactEvidence(candidate.ArtifactCitations),
		EvidenceWarnings: append([]string(nil), candidate.EvidenceWarnings...),
	}
	if candidate.ProposedRevision != nil {
		input.ProposedRevision = &fixpr.RevisionContext{RootCause: candidate.ProposedRevision.RootCause, SuggestedFix: candidate.ProposedRevision.SuggestedFix}
	}
	return input
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func exactPreviewRequestHash(candidate analysischat.FixCandidate, instruction string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		candidate.SessionID, candidate.RequestID, candidate.ResponseHash, strings.TrimSpace(instruction),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ValidateAnalysisPreview rechecks the exact shared chat response.
func (s *Service) ValidateAnalysisPreview(_ context.Context, owner string, binding actions.AnalysisPreviewBinding) error {
	candidate, err := s.chat.AnalysisFixCandidate(binding.ChatSessionID, owner, binding.ChatRequestID)
	if err != nil {
		return err
	}
	ref := candidate.FixTarget
	identity := binding.Identity
	if candidate.ResponseHash != binding.ChatResponseHash ||
		(candidate.Analysis.Scope != analysischat.ScopeTest && candidate.Analysis.Scope != analysischat.ScopeCause) || ref.Scope != analysischat.ScopeTest ||
		candidate.AnalysisContentHash == "" || candidate.AnalysisContentHash != binding.AnalysisContentHash ||
		candidate.SourceRepositorySnapshot != binding.SourceRepository ||
		ref.JobID != identity.JobID || ref.BuildID != identity.BuildID || ref.TestName != identity.TestName ||
		ref.Source != identity.Source || ref.SuiteName != identity.SuiteName || ref.ClassName != identity.ClassName ||
		ref.JUnitFile != identity.JUnitFile || ref.AnalysisGeneratedAt != identity.AnalysisGeneratedAt {
		return analysischat.ErrAnalysisChanged
	}
	if candidate.GenerationBaseRevision != "" &&
		(!strings.EqualFold(candidate.FailureRevision, binding.FailureRevision) ||
			!strings.EqualFold(candidate.GenerationBaseRevision, binding.GenerationBaseRevision) ||
			!stringMapsEqual(candidate.VerifiedSourceFileHashes, binding.VerifiedSourceFileHashes)) {
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
