package analysischat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// maxConversationFixCitations bounds accumulated conversation evidence to the
// citation limit fix generation accepts.
const maxConversationFixCitations = 16

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
	SessionID                string
	RequestID                string
	Analysis                 AnalysisRef
	Original                 AnalysisSnapshot
	AssistantAnswer          string
	ProposedRevision         *Revision
	ArtifactCitations        []Citation
	SourceRequestID          string
	SourceRepository         sourceinvestigation.Repository
	SourceRevision           string
	SourceResult             *sourceinvestigation.Result
	Pattern                  models.PatternAnalysis
	ResponseHash             string
	AnalysisContentHash      string
	SourceRepositorySnapshot sourceinvestigation.Repository
	FailureRevision          string
	GenerationBaseRevision   string
	VerifiedSourceFileHashes map[string]string
	SourceBranch             string
	SourceBranchKnown        bool
}

// TestFixCandidate returns one owner-bound answer for the exact JUnit analysis session.
func (s *Service) TestFixCandidate(sessionID, owner, requestID string) (FixCandidate, error) {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return FixCandidate{}, err
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
		if current.View.Analysis.Scope != ScopeTest {
			return changed, fmt.Errorf("%w: exact JUnit fix requires a test-scoped conversation", ErrInvalidRequest)
		}
		request, ok := current.Requests[requestID]
		if !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		answer := assistantResponse(current.View.Messages, requestID)
		citations := conversationCitations(current.View.Messages, requestID)
		if answer == nil || strings.TrimSpace(answer.Content) == "" || len(citations) == 0 {
			return changed, fmt.Errorf("%w: conversation has no evidence-backed assistant answer", ErrInvalidRequest)
		}
		analysis := current.Resolved.TestCase.AIAnalysis
		if analysis == nil {
			return changed, ErrAnalysisNotFound
		}
		candidate = FixCandidate{
			SessionID: current.View.ID, RequestID: requestID, Analysis: current.View.Analysis,
			Original: analysisSnapshot(analysis), AssistantAnswer: strings.TrimSpace(answer.Content),
			ProposedRevision: cloneRevision(answer.ProposedRevision), ArtifactCitations: citations,
			AnalysisContentHash: current.Resolved.AnalysisHash, SourceRepositorySnapshot: current.Resolved.Source,
		}
		if binding, ok := current.FixSources[requestID]; ok {
			candidate.FailureRevision = binding.FailureRevision
			candidate.GenerationBaseRevision = binding.GenerationBaseRevision
			candidate.VerifiedSourceFileHashes = cloneTestFixHashes(binding.VerifiedSourceFileHashes)
			candidate.SourceBranch, candidate.SourceBranchKnown = buildsource.Branch(
				current.Resolved.Build, current.Resolved.Source.Owner, current.Resolved.Source.Name,
			)
		}
		return changed, nil
	})
	if err != nil {
		return FixCandidate{}, err
	}
	resolved, err := s.resolve(candidate.Analysis)
	if err != nil {
		return FixCandidate{}, err
	}
	analysis := resolved.testCase.AIAnalysis
	currentSource, sourceOK := resolveBuildSourceRepository(resolved.build, candidate.SourceRepositorySnapshot)
	if analysis == nil || candidate.AnalysisContentHash == "" || models.TestAnalysisContentHash(resolved.testCase) != candidate.AnalysisContentHash ||
		!sameAnalysisSnapshot(candidate.Original, analysisSnapshot(analysis)) || sourceinvestigation.ValidateRepository(candidate.SourceRepositorySnapshot) != nil ||
		!sourceOK || currentSource != candidate.SourceRepositorySnapshot {
		return FixCandidate{}, ErrAnalysisChanged
	}
	if candidate.GenerationBaseRevision != "" {
		currentBranch, currentBranchKnown := buildsource.Branch(
			resolved.build, candidate.SourceRepositorySnapshot.Owner, candidate.SourceRepositorySnapshot.Name,
		)
		if currentBranchKnown != candidate.SourceBranchKnown || candidate.SourceBranchKnown && currentBranch != candidate.SourceBranch {
			return FixCandidate{}, ErrAnalysisChanged
		}
		files := buildsource.VerifiedPaths(analysis.FileLinks, buildsource.Source{
			Owner: candidate.SourceRepositorySnapshot.Owner, Name: candidate.SourceRepositorySnapshot.Name,
			Revision: candidate.SourceRepositorySnapshot.Revision,
		})
		if !validTestFixSource(persistedTestFixSource{
			FailureRevision: candidate.FailureRevision, GenerationBaseRevision: candidate.GenerationBaseRevision,
			VerifiedSourceFileHashes: candidate.VerifiedSourceFileHashes,
		}, candidate.SourceRepositorySnapshot.Revision, files) {
			return FixCandidate{}, ErrAnalysisChanged
		}
	}
	candidate.ResponseHash, err = fixCandidateResponseHash(candidate)
	if err != nil {
		return FixCandidate{}, err
	}
	return candidate, nil
}

func fixCandidateResponseHash(candidate FixCandidate) (string, error) {
	payload, err := json.Marshal(struct {
		SessionID, RequestID                    string
		Analysis                                AnalysisRef
		Original                                AnalysisSnapshot
		AssistantAnswer                         string
		ProposedRevision                        *Revision
		ArtifactCitations                       []Citation
		AnalysisContentHash                     string
		SourceRepository                        sourceinvestigation.Repository
		FailureRevision, GenerationBaseRevision string
		VerifiedSourceFileHashes                map[string]string
		SourceBranch                            string
		SourceBranchKnown                       bool
	}{
		candidate.SessionID, candidate.RequestID, candidate.Analysis, candidate.Original, candidate.AssistantAnswer,
		candidate.ProposedRevision, candidate.ArtifactCitations, candidate.AnalysisContentHash, candidate.SourceRepositorySnapshot,
		candidate.FailureRevision, candidate.GenerationBaseRevision, candidate.VerifiedSourceFileHashes,
		candidate.SourceBranch, candidate.SourceBranchKnown,
	})
	if err != nil {
		return "", fmt.Errorf("encoding selected chat response identity: %w", err)
	}
	return hashBytes(payload), nil
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
		citations := conversationCitations(current.View.Messages, requestID)
		if answer == nil || strings.TrimSpace(answer.Content) == "" || len(citations) == 0 {
			return changed, fmt.Errorf("%w: conversation has no evidence-backed assistant answer", ErrInvalidRequest)
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
			ArtifactCitations: citations,
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
			if record.View.Result.State != sourceinvestigation.StateActionableCodeChange &&
				record.View.Result.State != sourceinvestigation.StateActionableConfigurationChange {
				return changed, fmt.Errorf("%w: source investigation is not actionable", sourceinvestigation.ErrInvalidResult)
			}
			if record.View.Result.Target == nil {
				return changed, fmt.Errorf("%w: actionable source result has no target", sourceinvestigation.ErrInvalidResult)
			}
			if err := sourceinvestigation.ValidateRepository(record.Repository); err != nil {
				return changed, fmt.Errorf("%w: source repository identity is invalid: %v", sourceinvestigation.ErrInvalidResult, err)
			}
			revision, ok := buildsource.NormalizeRevision(record.Revision)
			if !ok {
				return changed, sourceinvestigation.ErrUnavailable
			}
			if !strings.EqualFold(revision, record.Repository.Revision) {
				return changed, fmt.Errorf("%w: source revision does not match the bound repository", sourceinvestigation.ErrInvalidResult)
			}
			candidate.SourceRequestID = sourceRequestID
			candidate.SourceRepository = record.Repository
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
	if index := assistantResponseIndex(messages, requestID); index >= 0 {
		return &messages[index]
	}
	return nil
}

func assistantResponseIndex(messages []Message, requestID string) int {
	for i := range messages {
		message := &messages[i]
		if message.Role == "assistant" && message.RequestID == requestID {
			return i
		}
		if message.Role == "user" && message.RequestID == requestID && i+1 < len(messages) &&
			messages[i+1].Role == "assistant" {
			return i + 1
		}
	}
	return -1
}

// conversationCitations returns the validated citations accumulated by the
// conversation up to and including the promoted answer, most recent first.
// Evidence validated in an earlier turn stays trustworthy, so a grounded
// conversation does not have to re-read artifacts to keep a later answer
// fix-eligible. Later turns are excluded so the promoted response identity
// stays stable as the conversation continues.
func conversationCitations(messages []Message, requestID string) []Citation {
	index := assistantResponseIndex(messages, requestID)
	if index < 0 {
		return nil
	}
	seen := make(map[Citation]struct{}, maxConversationFixCitations)
	citations := make([]Citation, 0, maxConversationFixCitations)
	collect := func(message *Message) {
		for _, citation := range message.Citations {
			if len(citations) >= maxConversationFixCitations {
				return
			}
			if _, ok := seen[citation]; ok {
				continue
			}
			seen[citation] = struct{}{}
			citations = append(citations, citation)
		}
	}
	collect(&messages[index])
	for i := index - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			collect(&messages[i])
		}
	}
	if len(citations) == 0 {
		return nil
	}
	return citations
}
