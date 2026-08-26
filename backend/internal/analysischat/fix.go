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

// maxConversationFixQuoteBytes bounds the quote bytes one fix request carries.
// Fix generation rejects an oversized context outright, so the budget holds the
// citation contribution at what the per-quote cap alone used to allow. A larger
// individual quote is therefore free: it spends this budget faster instead of
// pushing generation over its limit.
const maxConversationFixQuoteBytes = 16000

// AnalysisSnapshot is the complete published analysis context shown to chat.
type AnalysisSnapshot struct {
	GeneratedAt   string
	RootCause     string
	Severity      string
	SuggestedFix  string
	RelevantFiles []string
}

// FixCandidate is one selected successful answer.
type FixCandidate struct {
	SessionID                string
	RequestID                string
	Analysis                 AnalysisRef
	FixTarget                AnalysisRef
	Original                 AnalysisSnapshot
	AssistantAnswer          string
	ProposedRevision         *Revision
	ArtifactCitations        []Citation
	EvidenceWarnings         []string
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

// AnalysisFixCandidate returns one shared answer and its exact failed-test Fix target.
func (s *Service) AnalysisFixCandidate(sessionID, owner, requestID string) (FixCandidate, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return FixCandidate{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
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
		if current == nil {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope != ScopeTest && current.View.Analysis.Scope != ScopeCause {
			return changed, fmt.Errorf("%w: Fix requires a test- or cause-scoped conversation", ErrInvalidRequest)
		}
		request, ok := current.Requests[requestID]
		if !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		answer := assistantResponse(current.View.Messages, requestID)
		citations := conversationCitations(current.View.Messages, requestID)
		if answer == nil || answer.Unverified || strings.TrimSpace(answer.Content) == "" || len(citations) == 0 {
			return changed, fmt.Errorf("%w: conversation has no evidence-backed assistant answer", ErrInvalidRequest)
		}
		analysis := current.Resolved.TestCase.AIAnalysis
		if !analysisFixConversationUsable(current.View.Analysis.Scope, analysis) {
			return changed, fmt.Errorf("%w: the analysis has no usable diagnosis to fix", ErrInvalidRequest)
		}
		target := persistedAnalysisFixTarget(current.Resolved)
		if target == nil || target.TestCase.AIAnalysis == nil || !models.AnalysisHasUsableDiagnosis(target.TestCase.AIAnalysis) {
			return changed, fmt.Errorf("%w: conversation has no eligible failed-test Fix target", ErrInvalidRequest)
		}
		candidate = FixCandidate{
			SessionID: current.View.ID, RequestID: requestID, Analysis: current.View.Analysis, FixTarget: target.Ref,
			Original: analysisSnapshot(analysis), AssistantAnswer: strings.TrimSpace(answer.Content),
			ProposedRevision: cloneRevision(answer.ProposedRevision), ArtifactCitations: citations,
			EvidenceWarnings:    conversationEvidenceWarnings(current.View.Messages, requestID),
			AnalysisContentHash: target.AnalysisHash, SourceRepositorySnapshot: target.Source,
		}
		if binding, ok := current.FixSources[requestID]; ok {
			candidate.FailureRevision = binding.FailureRevision
			candidate.GenerationBaseRevision = binding.GenerationBaseRevision
			candidate.VerifiedSourceFileHashes = cloneTestFixHashes(binding.VerifiedSourceFileHashes)
			candidate.SourceBranch, candidate.SourceBranchKnown = buildsource.Branch(
				target.Build, target.Source.Owner, target.Source.Name,
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
	target := resolvedAnalysisFixTarget(resolved)
	if target == nil {
		return FixCandidate{}, ErrAnalysisChanged
	}
	currentSource, sourceOK := resolveBuildSourceRepository(target.build, candidate.SourceRepositorySnapshot)
	if !analysisFixConversationUsable(candidate.Analysis.Scope, analysis) ||
		!sameBoundAnalysisSnapshot(candidate.Analysis.Scope, candidate.Original, analysisSnapshot(analysis)) || candidate.FixTarget != target.ref ||
		target.testCase.AIAnalysis == nil || !models.AnalysisHasUsableDiagnosis(target.testCase.AIAnalysis) ||
		candidate.AnalysisContentHash == "" || models.TestAnalysisContentHash(target.testCase) != candidate.AnalysisContentHash ||
		sourceinvestigation.ValidateRepository(candidate.SourceRepositorySnapshot) != nil || !sourceOK || currentSource != candidate.SourceRepositorySnapshot {
		return FixCandidate{}, ErrAnalysisChanged
	}
	if candidate.GenerationBaseRevision != "" {
		currentBranch, currentBranchKnown := buildsource.Branch(
			target.build, candidate.SourceRepositorySnapshot.Owner, candidate.SourceRepositorySnapshot.Name,
		)
		if currentBranchKnown != candidate.SourceBranchKnown || candidate.SourceBranchKnown && currentBranch != candidate.SourceBranch {
			return FixCandidate{}, ErrAnalysisChanged
		}
		files := buildsource.VerifiedPaths(target.testCase.AIAnalysis.FileLinks, buildsource.Source{
			Owner: candidate.SourceRepositorySnapshot.Owner, Name: candidate.SourceRepositorySnapshot.Name,
			Revision: candidate.SourceRepositorySnapshot.Revision,
		})
		if !validTestFixSource(persistedTestFixSource{
			TargetRef: candidate.FixTarget, FailureRevision: candidate.FailureRevision,
			GenerationBaseRevision:   candidate.GenerationBaseRevision,
			VerifiedSourceFileHashes: candidate.VerifiedSourceFileHashes,
		}, candidate.FixTarget, candidate.SourceRepositorySnapshot.Revision, files) {
			return FixCandidate{}, ErrAnalysisChanged
		}
	}
	candidate.ResponseHash, err = fixCandidateResponseHash(candidate)
	if err != nil {
		return FixCandidate{}, err
	}
	return candidate, nil
}

func analysisFixConversationUsable(scope string, analysis *models.AIAnalysis) bool {
	if analysis == nil {
		return false
	}
	if scope == ScopeCause {
		return strings.TrimSpace(analysis.RootCause) != ""
	}
	return models.AnalysisHasUsableDiagnosis(analysis)
}

func persistedAnalysisFixTarget(resolved persistedResolvedAnalysis) *persistedResolvedFixTarget {
	switch resolved.Ref.Scope {
	case ScopeTest:
		return &persistedResolvedFixTarget{
			Ref: resolved.Ref, AnalysisHash: resolved.AnalysisHash, Source: resolved.Source,
			Build: resolved.Build, TestCase: resolved.TestCase,
		}
	case ScopeCause:
		return resolved.FixTarget
	default:
		return nil
	}
}

func resolvedAnalysisFixTarget(resolved resolvedAnalysis) *resolvedFixTarget {
	switch resolved.ref.Scope {
	case ScopeTest:
		return &resolvedFixTarget{ref: resolved.ref, build: resolved.build, testCase: resolved.testCase}
	case ScopeCause:
		return resolved.fixTarget
	default:
		return nil
	}
}

func fixCandidateResponseHash(candidate FixCandidate) (string, error) {
	payload, err := json.Marshal(struct {
		SessionID, RequestID                    string
		Analysis                                AnalysisRef
		FixTarget                               AnalysisRef
		Original                                AnalysisSnapshot
		AssistantAnswer                         string
		ProposedRevision                        *Revision
		ArtifactCitations                       []Citation
		EvidenceWarnings                        []string
		AnalysisContentHash                     string
		SourceRepository                        sourceinvestigation.Repository
		FailureRevision, GenerationBaseRevision string
		VerifiedSourceFileHashes                map[string]string
		SourceBranch                            string
		SourceBranchKnown                       bool
	}{
		candidate.SessionID, candidate.RequestID, candidate.Analysis, candidate.FixTarget, candidate.Original, candidate.AssistantAnswer,
		candidate.ProposedRevision, candidate.ArtifactCitations, candidate.EvidenceWarnings, candidate.AnalysisContentHash, candidate.SourceRepositorySnapshot,
		candidate.FailureRevision, candidate.GenerationBaseRevision, candidate.VerifiedSourceFileHashes,
		candidate.SourceBranch, candidate.SourceBranchKnown,
	})
	if err != nil {
		return "", fmt.Errorf("encoding selected chat response identity: %w", err)
	}
	return hashBytes(payload), nil
}

// FixCandidate returns one shared evidence-backed assistant response.
func (s *Service) FixCandidate(sessionID, owner, requestID, patternID, patternHash string) (FixCandidate, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return FixCandidate{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	patternID = strings.TrimSpace(patternID)
	patternHash = strings.TrimSpace(patternHash)
	if patternID == "" || patternHash == "" {
		return FixCandidate{}, fmt.Errorf("%w: pattern_id and pattern_hash are required", ErrInvalidRequest)
	}
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
		if current == nil {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope == ScopeCause {
			return changed, fmt.Errorf("%w: cause-scoped conversations cannot create fixes", ErrInvalidRequest)
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
		if answer == nil || answer.Unverified || strings.TrimSpace(answer.Content) == "" || len(citations) == 0 {
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
			EvidenceWarnings:  conversationEvidenceWarnings(current.View.Messages, requestID),
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
	if candidate.Analysis.Scope != ScopePattern &&
		(analysis == nil || !sameBoundAnalysisSnapshot(candidate.Analysis.Scope, candidate.Original, analysisSnapshot(analysis))) {
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

// sameBoundAnalysisSnapshot compares the analysis a conversation was bound to.
// A cause-scoped conversation carries the pattern's generation timestamp, which
// republishing moves without changing the cause, so identity there is the
// pattern and causal-group hashes the resolve already verified. The remaining
// fields still matter: a causal group's remediation is in neither hash.
func sameBoundAnalysisSnapshot(scope string, left, right AnalysisSnapshot) bool {
	if scope == ScopeCause {
		left.GeneratedAt, right.GeneratedAt = "", ""
	}
	return sameAnalysisSnapshot(left, right)
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

// conversationEvidenceWarnings preserves citation qualifications over the same
// bounded history that contributes Fix evidence.
func conversationEvidenceWarnings(messages []Message, requestID string) []string {
	index := assistantResponseIndex(messages, requestID)
	if index < 0 {
		return nil
	}
	const maxWarnings = 20
	seen := make(map[string]struct{}, maxWarnings)
	warnings := make([]string, 0, maxWarnings)
	collect := func(message *Message) {
		for _, warning := range message.EvidenceWarnings {
			warning = strings.TrimSpace(warning)
			if warning == "" {
				continue
			}
			if _, ok := seen[warning]; ok {
				continue
			}
			seen[warning] = struct{}{}
			warnings = append(warnings, warning)
			if len(warnings) == maxWarnings {
				return
			}
		}
	}
	collect(&messages[index])
	for i := index - 1; i >= 0 && len(warnings) < maxWarnings; i-- {
		if messages[i].Role == "assistant" {
			collect(&messages[i])
		}
	}
	if len(warnings) == 0 {
		return nil
	}
	return warnings
}

// conversationCitations returns the validated citations accumulated by the
// conversation up to and including the promoted answer, most recent first.
// Evidence validated in an earlier turn stays trustworthy, so a grounded
// conversation does not have to re-read artifacts to keep a later answer
// fix-eligible. Turn history replays prior citations to the model, so recent
// evidence is normally what the promoted answer reasoned over; history
// compaction drops the oldest turns first, so truncation keeps the most recent.
// Later turns are excluded so the promoted response identity stays stable as
// the conversation continues.
func conversationCitations(messages []Message, requestID string) []Citation {
	index := assistantResponseIndex(messages, requestID)
	if index < 0 {
		return nil
	}
	seen := make(map[Citation]struct{}, maxConversationFixCitations)
	citations := make([]Citation, 0, maxConversationFixCitations)
	quoteBytes := 0
	collect := func(message *Message) {
		for _, citation := range message.Citations {
			if len(citations) >= maxConversationFixCitations || quoteBytes >= maxConversationFixQuoteBytes {
				return
			}
			if _, ok := seen[citation]; ok {
				continue
			}
			// Collection runs newest-first, so exhausting the budget drops the
			// oldest evidence rather than truncating a quote, which would break
			// the verification the citation carries.
			if quoteBytes+len(citation.Quote) > maxConversationFixQuoteBytes {
				continue
			}
			seen[citation] = struct{}{}
			quoteBytes += len(citation.Quote)
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
