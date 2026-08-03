package analysischat

import (
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// AnalysisSnapshot is the complete published analysis context shown to chat.
type AnalysisSnapshot struct {
	GeneratedAt   string
	RootCause     string
	Severity      string
	SuggestedFix  string
	RelevantFiles []string
}

// FixCandidate is one selected successful answer and optional source result.
type FixCandidate struct {
	SessionID         string
	RequestID         string
	Analysis          AnalysisRef
	Original          AnalysisSnapshot
	AssistantAnswer   string
	ProposedRevision  *Revision
	ArtifactCitations []Citation
	SourceRequestID   string
	SourceRevision    string
	SourceResult      *sourceinvestigation.Result
	Pattern           models.PatternAnalysis
}

// FixCandidate returns one owner-bound evidence-backed assistant response.
func (s *Service) FixCandidate(sessionID, owner, requestID, patternID, patternHash, sourceRequestID string) (FixCandidate, error) {
	owner = normalizeOwner(owner)
	patternID = strings.TrimSpace(patternID)
	patternHash = strings.TrimSpace(patternHash)
	if patternID == "" || patternHash == "" {
		return FixCandidate{}, fmt.Errorf("%w: pattern_id and pattern_hash are required", ErrInvalidRequest)
	}
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
		if current.View.Analysis.Scope == ScopePattern &&
			(patternID != current.View.Analysis.PatternID || patternHash != current.View.Analysis.PatternHash) {
			return changed, fmt.Errorf("%w: requested pattern does not match the conversation", ErrInvalidRequest)
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
			SessionID:         current.View.ID,
			RequestID:         requestID,
			Analysis:          current.View.Analysis,
			Original:          analysisSnapshot(analysis),
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
			revision, ok := exactRepoRevision(record.Revision)
			if !ok {
				return changed, sourceinvestigation.ErrUnavailable
			}
			candidate.SourceRequestID = sourceRequestID
			candidate.SourceRevision = revision
			candidate.SourceResult = sourceinvestigation.CloneResult(record.View.Result)
			return changed, nil
		default:
			return changed, fmt.Errorf("%w: source investigation has invalid status", ErrInvalidRequest)
		}
	})
	if err != nil {
		return FixCandidate{}, err
	}
	resolved, err := s.resolve(candidate.Analysis)
	if err != nil {
		return FixCandidate{}, err
	}
	if candidate.Analysis.Scope == ScopePattern && !resolved.patternFresh {
		return FixCandidate{}, ErrPatternChanged
	}
	analysis := resolved.testCase.AIAnalysis
	if candidate.Analysis.Scope != ScopePattern && (analysis == nil || !sameAnalysisSnapshot(candidate.Original, analysisSnapshot(analysis))) {
		return FixCandidate{}, ErrAnalysisChanged
	}
	for _, pattern := range resolved.patterns {
		if pattern.ID == patternID {
			if models.PatternHash(pattern) != patternHash {
				return FixCandidate{}, ErrPatternChanged
			}
			if candidate.Analysis.Scope == ScopePattern {
				candidate.Analysis.BuildID = resolved.build.BuildID
			}
			candidate.Pattern = pattern
			return candidate, nil
		}
	}
	return FixCandidate{}, ErrPatternNotFound
}

func analysisSnapshot(analysis *models.AIAnalysis) AnalysisSnapshot {
	if analysis == nil {
		return AnalysisSnapshot{}
	}
	return AnalysisSnapshot{
		GeneratedAt:   strings.TrimSpace(analysis.GeneratedAt),
		RootCause:     clampPersistedText(analysis.RootCause, 32<<10),
		Severity:      strings.TrimSpace(analysis.Severity),
		SuggestedFix:  clampPersistedText(analysis.SuggestedFix, 16<<10),
		RelevantFiles: boundedPersistedFiles(analysis.RelevantFiles),
	}
}

func sameAnalysisSnapshot(left, right AnalysisSnapshot) bool {
	return left.GeneratedAt == right.GeneratedAt && left.RootCause == right.RootCause &&
		left.Severity == right.Severity && left.SuggestedFix == right.SuggestedFix &&
		slices.Equal(left.RelevantFiles, right.RelevantFiles)
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
