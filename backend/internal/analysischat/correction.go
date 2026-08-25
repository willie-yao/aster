package analysischat

import (
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

// CorrectionCandidate is the immutable structured revision eligible for review.
type CorrectionCandidate struct {
	SessionID string      `json:"session_id"`
	RequestID string      `json:"request_id"`
	Analysis  AnalysisRef `json:"analysis"`
	Original  Revision    `json:"original"`
	Proposed  Revision    `json:"proposed"`
	Citations []Citation  `json:"citations"`
}

// CorrectionCandidate returns one successful challenges response owned by login.
func (s *Service) CorrectionCandidate(id, owner, requestID string) (CorrectionCandidate, error) {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return CorrectionCandidate{}, err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var candidate CorrectionCandidate
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope != ScopeTest {
			return changed, fmt.Errorf("%w: only test-scoped conversations can promote test-analysis corrections", ErrInvalidRequest)
		}
		request, ok := current.Requests[requestID]
		if !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		var answer *Message
		for i := range current.View.Messages {
			message := &current.View.Messages[i]
			if message.Role == "assistant" && message.RequestID == requestID {
				answer = message
				break
			}
			if message.Role == "user" && message.RequestID == requestID && i+1 < len(current.View.Messages) {
				next := &current.View.Messages[i+1]
				if next.Role == "assistant" {
					answer = next
					break
				}
			}
		}
		if answer == nil || answer.Unverified || len(answer.EvidenceWarnings) > 0 || answer.Assessment != "challenges" || answer.ProposedRevision == nil || len(answer.Citations) == 0 {
			return changed, fmt.Errorf("%w: request has no evidence-backed proposed revision", ErrInvalidRequest)
		}
		analysis := current.Resolved.TestCase.AIAnalysis
		if analysis == nil {
			return changed, ErrAnalysisNotFound
		}
		if analysis.Disposition != models.AnalysisDispositionGrounded {
			return changed, fmt.Errorf("%w: preliminary analyses cannot be promoted as corrections", ErrInvalidRequest)
		}
		candidate = CorrectionCandidate{
			SessionID: current.View.ID,
			RequestID: requestID,
			Analysis:  current.View.Analysis,
			Original: Revision{
				RootCause:    strings.TrimSpace(analysis.RootCause),
				SuggestedFix: strings.TrimSpace(analysis.SuggestedFix),
			},
			Proposed:  *cloneRevision(answer.ProposedRevision),
			Citations: slices.Clone(answer.Citations),
		}
		return changed, nil
	})
	return candidate, err
}

// ValidateCorrectionCandidate rejects promotion after the published analysis changes.
func (s *Service) ValidateCorrectionCandidate(candidate CorrectionCandidate) error {
	resolved, err := s.resolve(candidate.Analysis)
	if err != nil {
		return err
	}
	analysis := resolved.testCase.AIAnalysis
	if analysis == nil || analysis.Disposition != models.AnalysisDispositionGrounded || strings.TrimSpace(analysis.RootCause) != candidate.Original.RootCause ||
		strings.TrimSpace(analysis.SuggestedFix) != candidate.Original.SuggestedFix {
		return ErrAnalysisChanged
	}
	return nil
}
