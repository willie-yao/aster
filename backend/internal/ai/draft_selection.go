package ai

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

type critiqueQuality struct {
	Passed               bool
	HardRules            []string
	SoftRules            []string
	HardIssueCount       int
	MissingEvidenceCount int
	PuntCount            int
}

type critiqueDraftCandidate struct {
	parsed                        analysisResponse
	content                       string
	providerItems                 []json.RawMessage
	rawQuality                    critiqueQuality
	quality                       critiqueQuality
	attempt                       int
	evidenceRevision              int
	createdEvidenceRevision       int
	supportedFacts                []supportedCausalFact
	semanticInitialFindingClasses []string
	semanticFindingClasses        []string
	semanticReviewPassed          bool
	semanticRevision              bool
}

type draftReplacementDecision struct {
	accepted                 bool
	reason                   string
	rootCauseChanged         bool
	rawSemanticRegression    bool
	publishedStrictDominance bool
	currentQualityRefreshed  bool
	currentSupportedFacts    int
	candidateSupportedFacts  int
	supportedFactsRetained   int
	supportedFactsAdded      int
	supportedFactsDropped    int
	supportedCauseRegression bool
}

const (
	draftReasonCandidatePublishedDominates  = "candidate_published_dominates"
	draftReasonCandidatePublishedHard       = "candidate_has_published_hard_failure"
	draftReasonCandidateSemanticRegression  = "candidate_adds_semantic_regression"
	draftReasonCandidateRootWithoutEvidence = "candidate_changes_root_without_evidence"
	draftReasonCandidateEvidenceBackedRoot  = "candidate_evidence_backed_root_change"
	draftReasonCandidateSemanticFindings    = "candidate_has_semantic_findings"
	draftReasonCandidateSemanticUnavailable = "candidate_semantic_review_unavailable"
	draftReasonCandidatePolicyUnaccepted    = "candidate_not_accepted_for_policy"
	draftReasonCandidateDropsSupportedCause = "candidate_drops_supported_cause"
	draftReasonCandidateNotBetter           = "candidate_not_strictly_better"
	draftReasonTiePreservesEarlier          = "tie_preserves_earlier"
	draftReasonSemanticTieReplaces          = "semantic_tie_replaces"
	draftReasonFallbackPromoted             = "fallback_promoted"
)

func (s *agentState) observeDraft(phase string, parsed analysisResponse, out, publishedOut critiqueOutcome) int {
	s.draftAttempt++
	attempt := s.draftAttempt
	if s.draftObserver == nil {
		return attempt
	}
	summary := parsed.Summary
	if summary == "" {
		summary = firstSentence(parsed.RootCause)
	}
	s.draftObserver(DraftObservation{
		Attempt:             attempt,
		Phase:               phase,
		Summary:             summary,
		RootCause:           parsed.RootCause,
		SuggestedFix:        parsed.SuggestedFix,
		IsTransient:         parsed.IsTransient,
		Severity:            parsed.Severity,
		RelevantFiles:       append([]string(nil), parsed.RelevantFiles...),
		PuntCount:           len(out.PuntMatches),
		UnreadCitationCount: len(out.UnreadCitations),
		CitationIssueCount:  len(out.CitationIssues),
		MissingGroupCount:   out.MissingEvidenceCount(),
		TransientConflict:   out.TransientPersistCount > 0,
		RuleIDs:             critiqueRuleStrings(out.RuleIDs()),
		MatchedSkillIDs:     append([]string(nil), out.MatchedSkillIDs...),
		MissingGroups:       critiqueEvidenceGroupRefs(out.MissingSkillEvidence),
		UnavailableGroups:   critiqueEvidenceGroupRefs(out.UnavailableSkillEvidence),
		PublishedRuleIDs:    critiqueRuleStrings(publishedOut.RuleIDs()),
		PublishedHardRules:  critiqueRuleStrings(publishedOut.HardRuleIDs()),
		PublishedSoftRules:  critiqueRuleStrings(publishedOut.SoftRuleIDs()),
		PublishedHardIssues: critiqueHardIssueCount(publishedOut),
		PublishedPuntCount:  len(publishedOut.PuntMatches),
		PublishedMissing:    publishedOut.MissingEvidenceCount(),
		ToolCalls:           s.calls,
		EvidenceReads:       len(s.readArtifactsFull),
	})
	return attempt
}

func critiqueQualityFor(out critiqueOutcome) critiqueQuality {
	return critiqueQuality{
		Passed:               out.Passed,
		HardRules:            critiqueRuleStrings(out.HardRuleIDs()),
		SoftRules:            critiqueRuleStrings(out.SoftRuleIDs()),
		HardIssueCount:       critiqueHardIssueCount(out),
		MissingEvidenceCount: out.MissingEvidenceCount(),
		PuntCount:            len(out.PuntMatches),
	}
}

func critiqueHardIssueCount(out critiqueOutcome) int {
	n := len(out.UnreadCitations)
	if out.MissingArtifactCitation {
		n++
	}
	for _, issue := range out.CitationIssues {
		if critiqueRuleSeverity(critiqueCitationRule(issue)) == CritiqueRuleHard {
			n++
		}
	}
	if out.TransientPersistCount > 0 {
		n++
	}
	if out.RerunOnlyRemediation {
		n++
	}
	return n
}

// compareCritiqueQuality returns positive only when a strictly dominates b.
func compareCritiqueQuality(a, b critiqueQuality) int {
	if critiqueQualityDominates(a, b) {
		return 1
	}
	if critiqueQualityDominates(b, a) {
		return -1
	}
	return 0
}

func critiqueQualityDominates(candidate, current critiqueQuality) bool {
	if !critiqueQualityNoWorse(candidate, current) {
		return false
	}
	hardImproved := candidate.HardIssueCount < current.HardIssueCount
	if hardImproved {
		return true
	}
	return candidate.PuntCount < current.PuntCount || candidate.MissingEvidenceCount < current.MissingEvidenceCount
}

func critiqueQualityNoWorse(candidate, current critiqueQuality) bool {
	if critiqueHardRegression(candidate, current) {
		return false
	}
	if candidate.HardIssueCount < current.HardIssueCount {
		return true
	}
	return candidate.PuntCount <= current.PuntCount && candidate.MissingEvidenceCount <= current.MissingEvidenceCount
}

func critiqueHardRegression(candidate, current critiqueQuality) bool {
	return candidate.HardIssueCount > current.HardIssueCount || !stringSetSubset(candidate.HardRules, current.HardRules)
}

func critiqueQualityAcceptedForPolicy(quality critiqueQuality, policy CritiqueCachePolicy) bool {
	switch policy {
	case CritiqueCachePolicyAdvisory:
		return true
	case CritiqueCachePolicyHard:
		return quality.HardIssueCount == 0
	case CritiqueCachePolicyStrict:
		return quality.Passed
	default:
		return false
	}
}

func stringSetSubset(candidate, current []string) bool {
	if len(candidate) > len(current) {
		return false
	}
	set := make(map[string]bool, len(current))
	for _, value := range current {
		set[value] = true
	}
	for _, value := range candidate {
		if !set[value] {
			return false
		}
	}
	return true
}

var rootCauseTokenRE = regexp.MustCompile(`[a-z0-9]+(?:[._/-][a-z0-9]+)*`)

var rootCauseStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "because": true, "before": true, "by": true, "caused": true,
	"causes": true, "due": true, "error": true, "failed": true, "failure": true,
	"for": true, "from": true, "in": true, "is": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "this": true,
	"to": true, "was": true, "were": true, "with": true,
}

// rootCauseMateriallyChanged ignores formatting but treats diagnosis token
// additions, deletions, reordering, and negation as material.
func rootCauseMateriallyChanged(a, b string) bool {
	return rootCauseFingerprint(a) != rootCauseFingerprint(b)
}

func rootCauseFingerprint(rootCause string) string {
	var tokens []string
	for _, token := range rootCauseTokenRE.FindAllString(strings.ToLower(rootCause), -1) {
		if !rootCauseStopwords[token] {
			tokens = append(tokens, token)
		}
	}
	return strings.Join(tokens, " ")
}

func (s *agentState) newDraftCandidate(phase, content string, providerItems []json.RawMessage, parsed analysisResponse, out critiqueOutcome) *critiqueDraftCandidate {
	published := s.publishedAnalysis(parsed)
	publishedOut := s.currentCritiqueOutcome(published)
	return &critiqueDraftCandidate{
		parsed:                  parsed,
		content:                 content,
		providerItems:           providerItems,
		rawQuality:              critiqueQualityFor(out),
		quality:                 critiqueQualityFor(publishedOut),
		attempt:                 s.observeDraft(phase, parsed, out, publishedOut),
		evidenceRevision:        s.evidenceRevision,
		createdEvidenceRevision: s.evidenceRevision,
		supportedFacts:          supportedCausalFacts(published, s.analysisEvidence, s.analysisEvidenceRevision),
		semanticRevision:        phase == "semantic_retry",
	}
}

func (s *agentState) publishedCritiqueOutcome(parsed analysisResponse) critiqueOutcome {
	return s.currentCritiqueOutcome(s.publishedAnalysis(parsed))
}

func (s *agentState) publishedAnalysis(parsed analysisResponse) analysisResponse {
	parsed = sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: s.analysisEvidence, Full: s.analysisEvidenceFull})
	parsed = s.preparePublishedAnalysis(parsed)
	return parsed
}

func (s *agentState) currentCritiqueOutcome(parsed analysisResponse) critiqueOutcome {
	out := critiqueDraftWithContent(parsed, s.readArtifactsFull, s.readArtifactsBase, s.evidenceContentByPath, s.readSourceFull, matchSkillsForDraft(s, parsed), s.consecutiveFailures, analysisCitationContext{Evidence: s.analysisEvidence, Full: s.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := s.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(parsed, &out, treeSet)
		}
	}
	return out
}

// considerDraft applies deterministic quality ordering and records the decision.
func (s *agentState) considerDraft(candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	return s.considerDraftDecision(candidate, semanticAccepted).accepted
}

func (s *agentState) considerDraftDecision(candidate *critiqueDraftCandidate, semanticAccepted bool) draftReplacementDecision {
	policy := effectiveCritiqueCachePolicy(s.opts.CritiqueCachePolicy)
	return s.considerDraftDecisionForPolicy(candidate, semanticAccepted, policy)
}

func (s *agentState) considerDraftDecisionForPolicy(candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	decision := s.evaluateDraftReplacement(s.bestDraft, candidate, semanticAccepted, policy)
	s.recordDraftDecision("best", s.bestDraft, candidate, decision)
	if decision.accepted {
		s.bestDraft = candidate
	}
	return decision
}

func (s *agentState) considerFallbackDraft(candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	policy := effectiveCritiqueCachePolicy(s.opts.CritiqueCachePolicy)
	return s.considerFallbackDraftForPolicy(candidate, semanticAccepted, policy)
}

func (s *agentState) considerFallbackDraftForPolicy(candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) bool {
	decision := s.evaluateDraftReplacement(s.fallbackDraft, candidate, semanticAccepted, policy)
	s.recordDraftDecision("fallback", s.fallbackDraft, candidate, decision)
	if decision.accepted {
		s.fallbackDraft = candidate
	}
	return decision.accepted
}

func (s *agentState) evaluateDraftReplacement(current, candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	refreshed := false
	if current != nil && candidate != nil {
		rootChanged := rootCauseMateriallyChanged(current.parsed.RootCause, candidate.parsed.RootCause)
		newEvidence := candidate.evidenceRevision > current.evidenceRevision
		// Preserve the earlier draft's historical quality when new evidence drove a
		// different diagnosis. Re-evaluating it with evidence fetched for the new
		// diagnosis can give the earlier text credit for evidence it never used.
		preserveHistorical := !semanticAccepted && rootChanged && newEvidence
		if semanticAccepted {
			current.rawQuality = critiqueQualityFor(s.currentCritiqueOutcome(current.parsed))
		}
		if current.evidenceRevision < candidate.evidenceRevision && !preserveHistorical {
			s.refreshPublishedDraftQuality(current)
			current.evidenceRevision = candidate.evidenceRevision
			refreshed = true
		}
	}
	decision := decideDraftReplacement(current, candidate, semanticAccepted, policy)
	decision.currentQualityRefreshed = refreshed
	return decision
}

func (s *agentState) refreshPublishedDraftQuality(candidate *critiqueDraftCandidate) {
	if candidate == nil {
		return
	}
	candidate.quality = critiqueQualityFor(s.publishedCritiqueOutcome(candidate.parsed))
}

func decideDraftReplacement(current, candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	decision := draftReplacementDecision{reason: draftReasonCandidateNotBetter}
	if candidate == nil {
		return decision
	}
	if current == nil {
		decision.accepted = true
		decision.publishedStrictDominance = true
		decision.reason = draftReasonCandidatePublishedDominates
		return decision
	}
	factDelta := compareSupportedCausalFacts(
		current.supportedFacts,
		candidate.supportedFacts,
		semanticInitialFindingsAllowCauseReplacement(candidate.semanticInitialFindingClasses),
		current.createdEvidenceRevision,
	)
	decision.currentSupportedFacts = len(current.supportedFacts)
	decision.candidateSupportedFacts = len(candidate.supportedFacts)
	decision.supportedFactsRetained = factDelta.retained
	decision.supportedFactsAdded = factDelta.added
	decision.supportedFactsDropped = factDelta.dropped
	decision.rootCauseChanged = rootCauseMateriallyChanged(current.parsed.RootCause, candidate.parsed.RootCause)
	publishedHardRegression := critiqueHardRegression(candidate.quality, current.quality)
	decision.rawSemanticRegression = semanticAccepted &&
		critiqueHardRegression(candidate.rawQuality, current.rawQuality) && publishedHardRegression
	if decision.rawSemanticRegression {
		decision.reason = draftReasonCandidateSemanticRegression
		return decision
	}
	if publishedHardRegression {
		decision.reason = draftReasonCandidatePublishedHard
		return decision
	}
	if semanticAccepted && !critiqueQualityAcceptedForPolicy(candidate.quality, policy) {
		if candidate.quality.HardIssueCount > 0 {
			decision.reason = draftReasonCandidatePublishedHard
		} else {
			// A revision the semantic judge passed can still be blocked by a
			// retained soft rule such as unread available evidence. Name that
			// distinctly so telemetry does not read as "not better".
			decision.reason = draftReasonCandidatePolicyUnaccepted
		}
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && len(candidate.semanticFindingClasses) > 0 {
		decision.reason = draftReasonCandidateSemanticFindings
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && !candidate.semanticReviewPassed {
		decision.reason = draftReasonCandidateSemanticUnavailable
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && decision.rootCauseChanged && factDelta.dropped > 0 && !factDelta.strongerReplacement {
		decision.supportedCauseRegression = true
		decision.reason = draftReasonCandidateDropsSupportedCause
		return decision
	}
	if decision.rootCauseChanged && candidate.evidenceRevision <= current.evidenceRevision && !semanticAccepted {
		decision.reason = draftReasonCandidateRootWithoutEvidence
		return decision
	}
	comparison := compareCritiqueQuality(candidate.quality, current.quality)
	evidenceBackedChange := !semanticAccepted && decision.rootCauseChanged && candidate.evidenceRevision > current.evidenceRevision && critiqueQualityNoWorse(candidate.quality, current.quality)
	semanticTie := semanticAccepted && critiqueQualityEqual(candidate.quality, current.quality)
	switch {
	case comparison > 0:
		decision.accepted = true
		decision.publishedStrictDominance = true
		decision.reason = draftReasonCandidatePublishedDominates
	case evidenceBackedChange:
		decision.accepted = true
		decision.reason = draftReasonCandidateEvidenceBackedRoot
	case semanticTie:
		decision.accepted = true
		decision.reason = draftReasonSemanticTieReplaces
	case critiqueQualityEqual(candidate.quality, current.quality):
		decision.reason = draftReasonTiePreservesEarlier
	default:
		decision.reason = draftReasonCandidateNotBetter
	}
	return decision
}

func draftShouldReplace(current, candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	return decideDraftReplacement(current, candidate, semanticAccepted, CritiqueCachePolicyHard).accepted
}

func critiqueQualityEqual(a, b critiqueQuality) bool {
	return a.Passed == b.Passed && a.HardIssueCount == b.HardIssueCount &&
		slices.Equal(a.HardRules, b.HardRules) && slices.Equal(a.SoftRules, b.SoftRules) &&
		a.MissingEvidenceCount == b.MissingEvidenceCount && a.PuntCount == b.PuntCount
}

func (s *agentState) recordDraftDecision(target string, current, candidate *critiqueDraftCandidate, decision draftReplacementDecision) {
	if candidate == nil {
		return
	}
	trace := &DraftDecisionTrace{
		Target:                          target,
		CandidateAttempt:                candidate.attempt,
		CandidateRawHardRules:           append([]string(nil), candidate.rawQuality.HardRules...),
		CandidateRawSoftRules:           append([]string(nil), candidate.rawQuality.SoftRules...),
		CandidatePublishedHardRules:     append([]string(nil), candidate.quality.HardRules...),
		CandidatePublishedSoftRules:     append([]string(nil), candidate.quality.SoftRules...),
		CandidatePublishedHardIssues:    candidate.quality.HardIssueCount,
		CandidatePublishedMissingGroups: candidate.quality.MissingEvidenceCount,
		CandidatePublishedPunts:         candidate.quality.PuntCount,
		CandidateEvidenceRevision:       candidate.evidenceRevision,
		RootCauseMateriallyChanged:      decision.rootCauseChanged,
		RawSemanticRegression:           decision.rawSemanticRegression,
		PublishedStrictDominance:        decision.publishedStrictDominance,
		CurrentQualityRefreshed:         decision.currentQualityRefreshed,
		CurrentSupportedFacts:           decision.currentSupportedFacts,
		CandidateSupportedFacts:         decision.candidateSupportedFacts,
		SupportedFactsRetained:          decision.supportedFactsRetained,
		SupportedFactsAdded:             decision.supportedFactsAdded,
		SupportedFactsDropped:           decision.supportedFactsDropped,
		SupportedCauseRegression:        decision.supportedCauseRegression,
		ReplacementAccepted:             decision.accepted,
		ReplacementReason:               decision.reason,
	}
	if current != nil {
		trace.CurrentAttempt = current.attempt
		trace.CurrentRawHardRules = append([]string(nil), current.rawQuality.HardRules...)
		trace.CurrentRawSoftRules = append([]string(nil), current.rawQuality.SoftRules...)
		trace.CurrentPublishedHardRules = append([]string(nil), current.quality.HardRules...)
		trace.CurrentPublishedSoftRules = append([]string(nil), current.quality.SoftRules...)
		trace.CurrentPublishedHardIssues = current.quality.HardIssueCount
		trace.CurrentPublishedMissingGroups = current.quality.MissingEvidenceCount
		trace.CurrentPublishedPunts = current.quality.PuntCount
		trace.CurrentEvidenceRevision = current.evidenceRevision
	}
	outcome := "rejected"
	if decision.accepted {
		outcome = "accepted"
	}
	recordTrace(s.traceCtx, TraceEvent{Kind: "draft_selection", Outcome: outcome, Status: target, DraftDecision: trace})
}

func (s *agentState) promoteFallbackDraft() *critiqueDraftCandidate {
	if s.fallbackDraft != nil {
		decision := draftReplacementDecision{accepted: true, reason: draftReasonFallbackPromoted}
		s.recordDraftDecision("promotion", s.bestDraft, s.fallbackDraft, decision)
		s.bestDraft = s.fallbackDraft
	}
	return s.bestDraft
}

func (s *agentState) notifyDraftSelection() {
	if s.selectionObserver != nil && s.bestDraft != nil {
		s.selectionObserver(s.bestDraft.attempt)
	}
}
