package ai

import (
	"slices"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
)

func TestCritiqueRuleIDs(t *testing.T) {
	out := critiqueOutcome{
		PuntMatches:     []string{"Check logs"},
		UnreadCitations: []string{"build-log.txt", "pkg/controller.go"},
		CitationIssues: []string{
			"citation 1 has an invalid path",
			"citation 2 quote does not occur at the claimed lines",
			"citation 3 line range exceeds the stored artifact",
			"citation 4 names an unread artifact",
			"prose line claim build-log.txt:10-10 has no matching citation",
		},
		MissingArtifactCitation: true,
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
		CritiqueRuleCitationMissing,
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
	if got := critiqueEvidenceGroupRefs(out.MissingSkillEvidence); !slices.Equal(got, []CritiqueEvidenceGroupRef{{SkillID: "skill-a", GroupID: "available"}}) {
		t.Fatalf("missing groups = %v", got)
	}
	if got := critiqueEvidenceGroupRefs(out.UnavailableSkillEvidence); !slices.Equal(got, []CritiqueEvidenceGroupRef{{SkillID: "skill-a", GroupID: "absent"}}) {
		t.Fatalf("unavailable groups = %v", got)
	}
	if got := out.RuleIDs(); !slices.Equal(got, []CritiqueRuleID{CritiqueRuleEvidenceAvailableUnread, CritiqueRuleEvidenceUnavailable}) {
		t.Fatalf("rules = %v", got)
	}
}

func TestCritiqueEvidenceGroupRefsPreserveIdentity(t *testing.T) {
	got := critiqueEvidenceGroupRefs([]skillEvidenceMiss{
		{Skill: skills.Skill{ID: "a/b"}, Missing: []skills.EvidenceGroup{{ID: "c"}}},
		{Skill: skills.Skill{ID: "a"}, Missing: []skills.EvidenceGroup{{ID: "b/c"}}},
	})
	want := []CritiqueEvidenceGroupRef{
		{SkillID: "a", GroupID: "b/c"},
		{SkillID: "a/b", GroupID: "c"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestCritiqueAcceptedForPolicy(t *testing.T) {
	soft := critiqueOutcome{PuntMatches: []string{"Check logs"}}
	hard := critiqueOutcome{UnreadCitations: []string{"build-log.txt"}}
	missingCitation := critiqueOutcome{MissingArtifactCitation: true}
	unavailable := critiqueOutcome{UnavailableSkillEvidence: []skillEvidenceMiss{{Skill: skills.Skill{ID: "skill"}, Missing: []skills.EvidenceGroup{{ID: "group"}}}}}
	for _, tc := range []struct {
		name   string
		out    critiqueOutcome
		policy CritiqueCachePolicy
		want   bool
	}{
		{name: "strict rejects soft", out: soft, policy: CritiqueCachePolicyStrict},
		{name: "hard accepts soft", out: soft, policy: CritiqueCachePolicyHard, want: true},
		{name: "hard rejects hard", out: hard, policy: CritiqueCachePolicyHard},
		{name: "hard rejects missing citation", out: missingCitation, policy: CritiqueCachePolicyHard},
		{name: "advisory accepts hard", out: hard, policy: CritiqueCachePolicyAdvisory, want: true},
		{name: "strict accepts unavailable", out: unavailable, policy: CritiqueCachePolicyStrict, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := critiqueAcceptedForPolicy(tc.out, tc.policy); got != tc.want {
				t.Fatalf("accepted = %t, want %t", got, tc.want)
			}
		})
	}
}
