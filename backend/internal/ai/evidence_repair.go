package ai

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

const (
	// maxEvidenceNudges bounds how many times the loop reopens a finalize
	// attempt because available planned evidence is still unread.
	maxEvidenceNudges = 2
	// evidenceNudgeMaxGroups bounds how many unread groups one nudge names.
	evidenceNudgeMaxGroups = 3
	// evidenceNudgeMaxCandidates bounds candidate paths listed per named group.
	evidenceNudgeMaxCandidates = 3
)

// evidenceGateOutcome is the recorded reason the finalize branch either
// reopened the investigation or accepted the draft with evidence still unread.
type evidenceGateOutcome string

const (
	evidenceGateNudge              evidenceGateOutcome = "nudge"
	evidenceGateCovered            evidenceGateOutcome = "covered"
	evidenceGateNoProgress         evidenceGateOutcome = "no_progress"
	evidenceGateNudgeBudget        evidenceGateOutcome = "nudge_budget"
	evidenceGateBudgetExhausted    evidenceGateOutcome = "budget_exhausted"
	evidenceGateTimeHeadroom       evidenceGateOutcome = "time_headroom"
	evidenceGateIterationExhausted evidenceGateOutcome = "iteration_exhausted"
)

// evidenceGate is the anti-thrash state for reopening a finalize attempt while
// available planned evidence remains unread.
type evidenceGate struct {
	nudges           int
	nudgedAtRevision int
}

// decide reports whether the loop should reopen the investigation for the
// unread groups, and why. Only groups with resolved candidate paths are
// actionable; a group with no candidate is deterministically unavailable.
func (g *evidenceGate) decide(state *agentState, unread []skills.PlanGroupRef) (evidenceGateOutcome, []skills.PlanGroupRef) {
	actionable := make([]skills.PlanGroupRef, 0, len(unread))
	for _, group := range unread {
		if len(group.CandidatePaths) > 0 {
			actionable = append(actionable, group)
		}
	}
	switch {
	case len(actionable) == 0:
		return evidenceGateCovered, nil
	case state.budgetExhausted:
		return evidenceGateBudgetExhausted, actionable
	case g.nudges >= maxEvidenceNudges:
		return evidenceGateNudgeBudget, actionable
	case state.evidenceRevision <= g.nudgedAtRevision:
		return evidenceGateNoProgress, actionable
	case time.Until(state.deadline) < 2*state.recentModelRequest+critiqueFinalizationReserve:
		return evidenceGateTimeHeadroom, actionable
	}
	return evidenceGateNudge, actionable
}

func (g *evidenceGate) recordNudge(state *agentState) {
	g.nudges++
	g.nudgedAtRevision = state.evidenceRevision
}

// draftTriggeredEvidenceGroups returns critique-required groups the initial
// plan never surfaced, because the recipe fired on the draft's own prose rather
// than on the failure signal. Candidates are resolved from the bounded artifact
// tree so the nudge can name real paths; a group with no candidate is dropped.
func (s *agentState) draftTriggeredEvidenceGroups(out critiqueOutcome, planned []skills.PlanGroupRef) []skills.PlanGroupRef {
	if len(out.MissingSkillEvidence) == 0 {
		return nil
	}
	seen := make(map[[2]string]bool, len(planned))
	for _, group := range planned {
		seen[[2]string{group.SkillID, group.GroupID}] = true
	}
	var refs []skills.PlanGroupRef
	for _, miss := range out.MissingSkillEvidence {
		for _, group := range miss.Missing {
			key := [2]string{miss.Skill.ID, group.ID}
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates := group.CandidatePaths(s.initialFailureSignal, s.initialArtifactTree.paths, evidenceplan.CandidatePathLimit)
			if len(candidates) == 0 {
				continue
			}
			refs = append(refs, skills.PlanGroupRef{
				SkillID:        miss.Skill.ID,
				GroupID:        group.ID,
				Description:    group.Description,
				CandidatePaths: candidates,
			})
		}
	}
	return refs
}

// formatEvidenceNudge names the unread evidence groups and their ranked
// candidate paths so the model can finish the plan it was given.
func formatEvidenceNudge(groups []skills.PlanGroupRef) string {
	return formatEvidenceNudgeWithLead("You attempted to finalize while required evidence for this failure class is still unread.", groups)
}

func formatEvidenceHeadroomNudge(groups []skills.PlanGroupRef) string {
	return formatEvidenceNudgeWithLead("The investigation loop is on its final tools-enabled iteration while required evidence for this failure class is still unread.", groups)
}

func formatEvidenceNudgeWithLead(lead string, groups []skills.PlanGroupRef) string {
	if len(groups) > evidenceNudgeMaxGroups {
		groups = groups[:evidenceNudgeMaxGroups]
	}
	var b strings.Builder
	b.WriteString(lead)
	b.WriteString(" Read at least one candidate path from every group below with read_artifact, tail_artifact, or grep_artifact before producing the final JSON:\n")
	for _, group := range groups {
		label := strings.TrimSpace(group.Description)
		if label == "" {
			label = group.GroupID
		}
		candidates := group.CandidatePaths
		if len(candidates) > evidenceNudgeMaxCandidates {
			candidates = candidates[:evidenceNudgeMaxCandidates]
		}
		fmt.Fprintf(&b, "\n- %s (%s): %s", group.GroupID, label, strings.Join(candidates, ", "))
	}
	b.WriteString("\n\nA candidate path is a starting point, not the whole answer: if it does not contain the evidence, use list_artifacts or find_artifacts on the relevant subtree. Do not repeat reads you have already made. If the evidence genuinely is not present in this build, say so explicitly in root_cause instead of leaving the group unexplained.")
	return b.String()
}

func evidencePlanTraceEvent(outcome evidenceGateOutcome, coverage skills.PlanCoverage, unread []skills.PlanGroupRef, draftTriggered int) TraceEvent {
	plan := &EvidencePlanTrace{
		Applicable:     coverage.Applicable,
		Satisfied:      coverage.Satisfied,
		Unavailable:    coverage.Unavailable,
		Unmet:          coverage.Unmet,
		DraftTriggered: draftTriggered,
	}
	for _, group := range unread {
		plan.UnreadGroups = append(plan.UnreadGroups, EvidencePlanGroupTrace{SkillID: group.SkillID, GroupID: group.GroupID})
	}
	return TraceEvent{Kind: "evidence_plan", Outcome: string(outcome), EvidencePlan: plan}
}

func recordEvidencePlanTrace(ctx context.Context, status string, outcome evidenceGateOutcome, coverage skills.PlanCoverage, unread []skills.PlanGroupRef, draftTriggered int) {
	event := evidencePlanTraceEvent(outcome, coverage, unread, draftTriggered)
	event.Status = status
	recordTrace(ctx, event)
}

// buildEvidenceInjection fetches evidence a critique-failing draft needed but
// did not read, and returns a feedback addendum embedding it. Ranked initial
// candidates direct skill repair; unresolved groups and unread basenames share
// one bounded fallback tree walk. Content-aware groups try ranked candidates in
// order until one artifact provides positive proof or the shared cap is reached.
func (c *Client) buildEvidenceInjection(ctx context.Context, state *agentState, out critiqueOutcome) string {
	ctx = withEvidenceReadSource(ctx, EvidenceReadSourceRepairInjection)
	if state == nil || state.browser == nil {
		return ""
	}
	var sections []string
	fetched := 0
	attempted := map[string]bool{}
	fetchTail := func(rawPath string) (string, bool) {
		if fetched >= evidenceInjectionMaxArtifacts {
			return "", false
		}
		realPath, err := artifacts.SafePath(strings.TrimSpace(rawPath))
		if err != nil || realPath == "" {
			return "", false
		}
		if attempted[realPath] {
			return "", false
		}
		attempted[realPath] = true
		res, err := state.browser.Tail(ctx, realPath, 200, evidenceInjectionPerArtifactBytes)
		if err != nil || res == nil || len(bytes.TrimSpace(res.Content)) == 0 {
			return "", false
		}
		content := res.Content
		if len(content) > evidenceInjectionPerArtifactBytes {
			content = content[len(content)-evidenceInjectionPerArtifactBytes:]
		}
		return string(content), true
	}
	add := func(realPath, label, content string) {
		state.gcsBytes += len(content)
		state.modelBytes += len(content)
		state.recordSuccessfulRead(realPath)
		state.recordEvidenceSnippets(realPath, []string{content})
		sections = append(sections, fmt.Sprintf("### %s\n%s", label, content))
		fetched++
	}

	type walkTarget struct {
		match func(string) bool
		label func(string) string
	}
	var walkTargets []walkTarget
	type unreadWalk struct {
		target int
	}
	var unreadWalks []unreadWalk

	// Fetch exact unread citations first. Bare names and failed exact reads use
	// the shared fallback walk without retrying the same path.
	for _, cited := range out.UnreadCitations {
		if strings.Contains(cited, "/") {
			if content, ok := fetchTail(cited); ok {
				add(cited, cited+" (tail)", content)
				continue
			}
		}
		base := path.Base(cited)
		citedCopy := cited
		unreadWalks = append(unreadWalks, unreadWalk{target: len(walkTargets)})
		walkTargets = append(walkTargets, walkTarget{
			match: func(p string) bool { return strings.EqualFold(path.Base(p), base) },
			label: func(real string) string {
				return fmt.Sprintf("%s (tail; nearest match for cited %q)", real, citedCopy)
			},
		})
	}

	type groupTarget struct {
		skillID    string
		group      skills.EvidenceGroup
		candidates []string
		walkIndex  int
	}
	var groups []groupTarget
	for _, miss := range out.MissingSkillEvidence {
		for _, group := range miss.Missing {
			if group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
				continue
			}
			target := groupTarget{skillID: miss.Skill.ID, group: group, walkIndex: -1}
			candidates, planned := initialPlanCandidates(state.initialEvidencePlan, miss.Skill.ID, group.ID)
			if planned {
				for _, candidate := range candidates {
					realPath, err := artifacts.SafePath(strings.TrimSpace(candidate))
					norm := NormalizeArtifactCitation(realPath)
					if err == nil && realPath != "" && norm != "" && group.Satisfied(map[string]bool{norm: true}) {
						target.candidates = append(target.candidates, realPath)
					}
				}
			}
			if len(target.candidates) == 0 {
				groupCopy := group
				target.walkIndex = len(walkTargets)
				walkTargets = append(walkTargets, walkTarget{
					match: func(p string) bool {
						norm := NormalizeArtifactCitation(p)
						return norm != "" && groupCopy.Satisfied(map[string]bool{norm: true})
					},
				})
			}
			groups = append(groups, target)
		}
	}

	var walked [][]string
	if len(walkTargets) > 0 && fetched < evidenceInjectionMaxArtifacts {
		preds := make([]func(string) bool, len(walkTargets))
		for i := range walkTargets {
			preds[i] = walkTargets[i].match
		}
		walked = resolveEvidenceCandidatesByWalk(ctx, state.browser, preds, evidenceplan.CandidatePathLimit)
	}
	for _, target := range unreadWalks {
		if target.target >= len(walked) || len(walked[target.target]) == 0 || fetched >= evidenceInjectionMaxArtifacts {
			continue
		}
		realPath := walked[target.target][0]
		if content, ok := fetchTail(realPath); ok {
			add(realPath, walkTargets[target.target].label(realPath), content)
		}
	}
	for i := range groups {
		if fetched >= evidenceInjectionMaxArtifacts {
			break
		}
		target := &groups[i]
		if target.group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
			continue
		}
		if len(target.candidates) == 0 && target.walkIndex >= 0 && target.walkIndex < len(walked) {
			target.candidates = append(target.candidates, walked[target.walkIndex]...)
		}
		for _, candidate := range target.candidates {
			if fetched >= evidenceInjectionMaxArtifacts {
				break
			}
			realPath, err := artifacts.SafePath(strings.TrimSpace(candidate))
			norm := NormalizeArtifactCitation(realPath)
			if err != nil || realPath == "" || norm == "" || attempted[realPath] || !target.group.Satisfied(map[string]bool{norm: true}) {
				continue
			}
			content, ok := fetchTail(realPath)
			if !ok {
				continue
			}
			add(realPath, fmt.Sprintf("%s (tail; required evidence %q for skill %q)", realPath, target.group.ID, target.skillID), content)
			if target.group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
				break
			}
		}
	}

	if fetched == 0 {
		return ""
	}
	log.Printf("  📎 evidence injection: fetched %d artifact(s) into the retry", fetched)
	return "The engine fetched evidence you cited but had not read, and/or evidence required for this failure class. Ground your root_cause in what these artifacts ACTUALLY show below; correct or drop any claim they do not support.\n\n" + strings.Join(sections, "\n\n")
}

func initialPlanCandidates(plan []skills.PlannedSkill, skillID, groupID string) ([]string, bool) {
	for _, plannedSkill := range plan {
		if plannedSkill.ID != skillID {
			continue
		}
		for _, group := range plannedSkill.RequiredEvidence {
			if group.ID == groupID {
				return group.CandidatePaths, true
			}
		}
		return nil, false
	}
	return nil, false
}

// resolveEvidenceByWalk lists the build's artifact tree once and returns the
// first matching real path for each predicate, or "" if unmatched. Bounded by
// evidenceTreeMaxPaths to cap GCS list cost. Stops early once every predicate
// has a match.
func resolveEvidenceByWalk(ctx context.Context, browser artifacts.Browser, preds []func(string) bool) []string {
	candidates := resolveEvidenceCandidatesByWalk(ctx, browser, preds, 1)
	found := make([]string, len(candidates))
	for i := range candidates {
		if len(candidates[i]) > 0 {
			found[i] = candidates[i][0]
		}
	}
	return found
}

func resolveEvidenceCandidatesByWalk(ctx context.Context, browser artifacts.Browser, preds []func(string) bool, limit int) [][]string {
	found := make([][]string, len(preds))
	if browser == nil || len(preds) == 0 || limit <= 0 {
		return found
	}
	paths, _, err := browser.ListTree(ctx, evidenceTreeMaxPaths)
	if err != nil {
		return found
	}
	sort.Strings(paths)
	for _, p := range paths {
		for i, pred := range preds {
			if len(found[i]) < limit && pred(p) {
				found[i] = append(found[i], p)
			}
		}
	}
	return found
}

// matchSkillsForDraft joins the candidate draft's prose fields and matches
// them against the loaded recipe set. Returns nil if skills are disabled or
// no recipes are loaded. Used by both the in-loop and post-loop critique so
// both paths match against the same draft text.
func matchSkillsForDraft(state *agentState, parsed analysisResponse) []skills.Skill {
	if state == nil || state.skillSet == nil {
		return nil
	}
	return state.skillSet.Match(strings.Join(parsed.proseFields(), "\n"))
}
