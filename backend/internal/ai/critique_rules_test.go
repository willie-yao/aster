package ai

import (
	"slices"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

func TestCritiqueRuleIDs(t *testing.T) {
	out := critiqueOutcome{
		PuntMatches:     []string{"Check logs"},
		UnreadCitations: []string{"build-log.txt", "pkg/controller.go"},
		CitationIssues: []string{
			"citation 1 has an invalid path",
			"citation 2 quote does not match the requested range",
			"citation 3 line range exceeds the stored artifact",
			"prose line claim build-log.txt:10-10 has no matching citation",
		},
		MissingSkillEvidence: []skillEvidenceMiss{{
			Skill: skills.Skill{ID: "skill-a"}, Missing: []skills.EvidenceGroup{{ID: "group-a"}},
		}},
		UnavailableSkillEvidence: []skillEvidenceMiss{{
			Skill: skills.Skill{ID: "skill-b"}, Missing: []skills.EvidenceGroup{{ID: "group-b"}},
		}},
		TransientPersistCount: 3,
	}
	want := []CritiqueRuleID{
		CritiqueRuleCitationInvalidRange,
		CritiqueRuleCitationQuoteMismatch,
		CritiqueRuleCitationUnread,
		CritiqueRuleClaimUncitedLine,
		CritiqueRuleEvidenceAvailableUnread,
		CritiqueRuleEvidenceUnavailable,
		CritiqueRulePathUnsafe,
		CritiqueRuleRemediationPunt,
		CritiqueRuleSourceUnverified,
		CritiqueRuleTransientConflict,
	}
	if got := out.RuleIDs(); !slices.Equal(got, want) {
		t.Fatalf("RuleIDs() = %v, want %v", got, want)
	}
}

func TestPruneAbsentSkillEvidenceRecordsUnavailableGroups(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{"skill-a": `
id: skill-a
triggers: ["trigger"]
required_evidence:
  - id: available
    any_of: ["available\\.log"]
  - id: absent
    any_of: ["absent\\.log"]
`})
	matched := set.Match("trigger")
	out := critiqueDraft(analysisResponse{Summary: "trigger", SuggestedFix: "Apply fix."}, map[string]bool{}, map[string]bool{}, matched, 0)
	if dropped := pruneAbsentSkillEvidence(analysisResponse{Summary: "trigger", SuggestedFix: "Apply fix."}, &out, map[string]bool{"available.log": true}); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if got := critiqueEvidenceGroupIDs(out.MissingSkillEvidence); !slices.Equal(got, []string{"skill-a/available"}) {
		t.Fatalf("missing groups = %v", got)
	}
	if got := critiqueEvidenceGroupIDs(out.UnavailableSkillEvidence); !slices.Equal(got, []string{"skill-a/absent"}) {
		t.Fatalf("unavailable groups = %v", got)
	}
	if got := out.RuleIDs(); !slices.Equal(got, []CritiqueRuleID{CritiqueRuleEvidenceAvailableUnread, CritiqueRuleEvidenceUnavailable}) {
		t.Fatalf("rules = %v", got)
	}
}
