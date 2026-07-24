package analysischat

import (
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// FixCandidate is one selected successful answer and optional source result.
type FixCandidate struct {
	SessionID         string
	RequestID         string
	Analysis          AnalysisRef
	Original          Revision
	AssistantAnswer   string
	ProposedRevision  *Revision
	ArtifactCitations []Citation
	SourceRequestID   string
	SourceResult      *sourceinvestigation.Result
}

// FixCandidate returns one owner-bound evidence-backed assistant response.
func (s *Service) FixCandidate(sessionID, owner, requestID, sourceRequestID string) (FixCandidate, error) {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return FixCandidate{}, err
	}
	if strings.TrimSpace(sourceRequestID) != "" {
		sourceRequestID, err = normalizeRequestID(sourceRequestID)
		if err != nil {
			return FixCandidate{}, err
		}
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var candidate FixCandidate
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		request, ok := current.Requests[requestID]
		if !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		answer := assistantResponse(current.View.Messages, requestID)
		if answer == nil || strings.TrimSpace(answer.Content) == "" || len(answer.Citations) == 0 {
			return changed, fmt.Errorf("%w: request has no evidence-backed assistant answer", ErrInvalidRequest)
		}
		analysis := current.Resolved.TestCase.AIAnalysis
		if analysis == nil {
			return changed, ErrAnalysisNotFound
		}
		candidate = FixCandidate{
			SessionID: current.View.ID,
			RequestID: requestID,
			Analysis:  current.View.Analysis,
			Original: Revision{
				RootCause:    strings.TrimSpace(analysis.RootCause),
				SuggestedFix: strings.TrimSpace(analysis.SuggestedFix),
			},
			AssistantAnswer:   strings.TrimSpace(answer.Content),
			ProposedRevision:  cloneRevision(answer.ProposedRevision),
			ArtifactCitations: slices.Clone(answer.Citations),
		}
		if sourceRequestID == "" {
			return changed, nil
		}
		record, ok := current.Investigations[sourceRequestID]
		if !ok || record.View.ChatRequestID != requestID {
			return changed, ErrRequestNotFound
		}
		switch record.View.Status {
		case sourceinvestigation.StatusPending:
			return changed, ErrRequestPending
		case sourceinvestigation.StatusUnknown:
			return changed, ErrRequestOutcomeUnknown
		case sourceinvestigation.StatusFailed:
			return changed, sourceinvestigation.ErrUnavailable
		case sourceinvestigation.StatusSucceeded:
			if record.View.Result == nil || sourceinvestigation.ValidateVerifiedResult(*record.View.Result) != nil {
				return changed, sourceinvestigation.ErrInvalidResult
			}
			candidate.SourceRequestID = sourceRequestID
			candidate.SourceResult = sourceinvestigation.CloneResult(record.View.Result)
			return changed, nil
		default:
			return changed, fmt.Errorf("%w: source investigation has invalid status", ErrInvalidRequest)
		}
	})
	return candidate, err
}

// ValidateFixCandidate rejects generation after the published analysis changes.
func (s *Service) ValidateFixCandidate(candidate FixCandidate) error {
	resolved, err := s.resolve(candidate.Analysis)
	if err != nil {
		return err
	}
	analysis := resolved.testCase.AIAnalysis
	if analysis == nil || strings.TrimSpace(analysis.RootCause) != candidate.Original.RootCause ||
		strings.TrimSpace(analysis.SuggestedFix) != candidate.Original.SuggestedFix {
		return ErrAnalysisChanged
	}
	return nil
}

func assistantResponse(messages []Message, requestID string) *Message {
	for i := range messages {
		message := &messages[i]
		if message.Role == "assistant" && message.RequestID == requestID {
			return message
		}
		if message.Role == "user" && message.RequestID == requestID && i+1 < len(messages) {
			next := &messages[i+1]
			if next.Role == "assistant" {
				return next
			}
		}
	}
	return nil
}
