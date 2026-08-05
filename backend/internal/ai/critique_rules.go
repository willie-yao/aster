package ai

import (
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

// CritiqueRuleID is a stable, privacy-safe deterministic critique identifier.
type CritiqueRuleID string

// CritiqueEvidenceGroupRef identifies one evidence group without ambiguous
// string concatenation.
type CritiqueEvidenceGroupRef struct {
	SkillID string `json:"skill_id"`
	GroupID string `json:"group_id"`
}

const (
	CritiqueRulePathUnsafe              CritiqueRuleID = "path.unsafe"
	CritiqueRuleCitationInvalidRange    CritiqueRuleID = "citation.invalid_range"
	CritiqueRuleCitationQuoteMismatch   CritiqueRuleID = "citation.quote_mismatch"
	CritiqueRuleCitationUnread          CritiqueRuleID = "citation.unread"
	CritiqueRuleClaimUncitedLine        CritiqueRuleID = "claim.uncited_line"
	CritiqueRuleEvidenceAvailableUnread CritiqueRuleID = "evidence.available_unread"
	CritiqueRuleEvidenceUnavailable     CritiqueRuleID = "evidence.unavailable"
	CritiqueRuleRemediationPunt         CritiqueRuleID = "remediation.punt"
	CritiqueRuleTransientConflict       CritiqueRuleID = "transient.conflict"
	CritiqueRuleStructuredInvalid       CritiqueRuleID = "structured.invalid"
	CritiqueRuleSourceUnverified        CritiqueRuleID = "source.unverified"
)

func (o critiqueOutcome) RuleIDs() []CritiqueRuleID {
	rules := map[CritiqueRuleID]bool{}
	if len(o.PuntMatches) > 0 {
		rules[CritiqueRuleRemediationPunt] = true
	}
	for _, candidate := range o.UnreadCitations {
		if isSourceCitation(candidate) {
			rules[CritiqueRuleSourceUnverified] = true
		} else {
			rules[CritiqueRuleCitationUnread] = true
		}
	}
	for _, issue := range o.CitationIssues {
		rules[critiqueCitationRule(issue)] = true
	}
	if len(o.MissingSkillEvidence) > 0 {
		rules[CritiqueRuleEvidenceAvailableUnread] = true
	}
	if len(o.UnavailableSkillEvidence) > 0 {
		rules[CritiqueRuleEvidenceUnavailable] = true
	}
	if o.TransientPersistCount > 0 {
		rules[CritiqueRuleTransientConflict] = true
	}
	out := make([]CritiqueRuleID, 0, len(rules))
	for rule := range rules {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func critiqueCitationRule(issue string) CritiqueRuleID {
	issue = strings.ToLower(issue)
	switch {
	case strings.Contains(issue, "invalid path"):
		return CritiqueRulePathUnsafe
	case strings.Contains(issue, "quote does not match"), strings.Contains(issue, "quote does not occur"):
		return CritiqueRuleCitationQuoteMismatch
	case strings.Contains(issue, "line range"), strings.Contains(issue, "line_start"), strings.Contains(issue, "line_end"):
		return CritiqueRuleCitationInvalidRange
	case strings.Contains(issue, "prose line claim"):
		return CritiqueRuleClaimUncitedLine
	case strings.Contains(issue, "was not read"), strings.Contains(issue, "names an unread artifact"):
		return CritiqueRuleCitationUnread
	case strings.Contains(issue, "evidence read budget was exceeded"):
		return CritiqueRuleEvidenceUnavailable
	default:
		return CritiqueRuleStructuredInvalid
	}
}

func critiqueMatchedSkillIDs(matched []skills.Skill) []string {
	out := make([]string, 0, len(matched))
	for _, skill := range matched {
		if skill.ID != "" {
			out = append(out, skill.ID)
		}
	}
	sort.Strings(out)
	return out
}

func critiqueEvidenceGroupRefs(skills []skillEvidenceMiss) []CritiqueEvidenceGroupRef {
	seen := map[CritiqueEvidenceGroupRef]bool{}
	for _, miss := range skills {
		for _, group := range miss.Missing {
			if miss.Skill.ID != "" && group.ID != "" {
				seen[CritiqueEvidenceGroupRef{SkillID: miss.Skill.ID, GroupID: group.ID}] = true
			}
		}
	}
	out := make([]CritiqueEvidenceGroupRef, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SkillID != out[j].SkillID {
			return out[i].SkillID < out[j].SkillID
		}
		return out[i].GroupID < out[j].GroupID
	})
	return out
}

func critiqueRuleStrings(rules []CritiqueRuleID) []string {
	out := make([]string, len(rules))
	for i, rule := range rules {
		out[i] = string(rule)
	}
	return out
}
