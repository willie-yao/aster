package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
)

// The semantic judge is the second line of the critique gate. Where the
// deterministic pass in critique.go catches structural faults (punts,
// hallucinated citations, missing skill evidence), this focused LLM-as-judge
// pass catches a fluent, well-cited root cause that is nonetheless the wrong
// conclusion. It runs at most once per analysis and only re-prompts; it never
// changes the cache contract, so cache invariance stays deterministic.

// semanticJudgeSystemPrompt drives the judge. It is a fixed checklist aimed at
// the recurring semantic failure modes, not an open-ended "is this good?", so a
// same-model judge has a concrete rubric to apply.
const semanticJudgeSystemPrompt = `You are a skeptical senior SRE reviewing another engineer's root-cause analysis of a CI test failure before it is published. You are NOT redoing the investigation; you are checking the REASONING for specific defects. Report concrete problems ONLY, never style. Apply this checklist:

1. Causal ordering. Is the stated root_cause the EARLIEST, initiating failure, or a downstream symptom? Be suspicious of a root cause that rests on end-of-run noise: namespace-deletion / cleanup timeouts, credential or token expiry, resource-group or VM deletion, "DNS no longer resolves", or a cascade of dependent timeouts. Those usually happen AFTER the real failure and are teardown artifacts, not the trigger.

2. Culprit attribution. If the job tests a specific change (a pull request), does the analysis actually establish that change is at fault, or does it assume so because the PR is what changed? Consider whether the true cause could instead be the test harness, the cluster or networking configuration, a default value, or a DIFFERENT component or repository than the one under test.

3. Grounding. Is each claim tied to specific evidence the engineer actually read, or is it plausible-sounding speculation dressed up with artifact names?

4. Fix validity. Would the suggested_fix actually resolve the stated root_cause, or is it a generic "re-run / check credentials" that does not follow from it?

Answer with one line of JSON and nothing else. An empty array means the reasoning is sound and should be published as-is:
{"objections": ["<specific defect>", "<specific defect>"]}`

// semanticCritique asks the model to review its own accepted draft as a
// skeptical reviewer and returns concrete objections. An empty slice means the
// reasoning is sound. A transport or parse error is returned so the caller can
// fail open (publish the draft) rather than block a good answer on a flaky judge.
func (c *Client) semanticCritique(ctx context.Context, parsed analysisResponse, readPaths []string, headroom contextHeadroom) ([]string, error) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr(semanticJudgeSystemPrompt)},
		{Role: "user", Content: strPtr(formatSemanticJudgeInput(parsed, readPaths))},
	}
	var safe bool
	messages, safe = prepareContextRequest(ctx, messages, 0, headroom, "semantic_judge")
	if !safe {
		return nil, ErrContextHeadroom
	}
	resp, err := c.callModel(ctx, messages, nil, nil)
	if err != nil {
		return nil, err
	}
	if !resp.HasMessage || resp.Message.Content == nil {
		return nil, fmt.Errorf("empty completion response")
	}
	out := *resp.Message.Content
	var v struct {
		Objections []string `json:"objections"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &v); err != nil {
		return nil, fmt.Errorf("semantic judge response: %w", err)
	}
	kept := make([]string, 0, len(v.Objections))
	for _, o := range v.Objections {
		if s := strings.TrimSpace(o); s != "" {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

// formatSemanticJudgeInput renders the draft plus the list of artifacts the
// agent actually read, so the judge can weigh grounding against real evidence.
func formatSemanticJudgeInput(parsed analysisResponse, readPaths []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "is_transient: %v\n", parsed.IsTransient)
	fmt.Fprintf(&b, "summary: %s\n", oneLineTrim(parsed.Summary))
	fmt.Fprintf(&b, "root_cause: %s\n", parsed.RootCause)
	fmt.Fprintf(&b, "suggested_fix: %s\n", parsed.SuggestedFix)
	if len(parsed.RelevantFiles) > 0 {
		fmt.Fprintf(&b, "relevant_files: %s\n", strings.Join(parsed.RelevantFiles, ", "))
	}
	if len(readPaths) > 0 {
		fmt.Fprintf(&b, "\nArtifacts the engineer actually read (%d):\n", len(readPaths))
		for _, p := range readPaths {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	} else {
		b.WriteString("\nThe engineer read NO artifacts before concluding.\n")
	}
	return b.String()
}

// formatSemanticObjections turns the judge's objections into a re-prompt that
// pushes the model to re-investigate rather than restate its draft.
func formatSemanticObjections(objs []string) string {
	var b strings.Builder
	b.WriteString("A skeptical reviewer raised concrete problems with the REASONING in your analysis (not its format):\n\n")
	for _, o := range objs {
		fmt.Fprintf(&b, "  - %s\n", o)
	}
	b.WriteString("\nThese are not style notes. Before re-emitting your JSON:\n")
	b.WriteString("1. Address each objection with EVIDENCE: use your tools to read the specific artifacts that confirm or refute the true cause. Do not simply restate your draft.\n")
	b.WriteString("2. If the reviewer suggests the cause may lie elsewhere (the test harness, cluster or networking configuration, a default, or a different component or repository than the one under test), investigate that possibility before dismissing it.\n")
	b.WriteString("3. Re-emit corrected JSON grounded in what the artifacts actually show. If your original analysis holds up, defend it with the specific evidence, not by repeating it.")
	return b.String()
}

// oneLineTrim collapses whitespace in a short field for the judge input.
func oneLineTrim(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// readPathList returns the sorted set of artifact paths the agent successfully
// read, for the semantic judge's grounding check.
func (s *agentState) readPathList() []string {
	if len(s.readArtifactsFull) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.readArtifactsFull))
	for p := range s.readArtifactsFull {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// applySemanticJudgePostLoop runs the judge on an accepted force-finalize draft
// and, on objections, drives one tools-free refinalize round. The post-loop path
// has no tools, so the model reconsiders from evidence already in context rather
// than re-investigating. The revised draft is used only if it still clears the
// deterministic critique, so the judge can never downgrade an answer below the
// gate it already passed. Returns the draft to publish.
func (c *Client) applySemanticJudgePostLoop(ctx context.Context, state *agentState, messages []modelMessage, finalContent string, finalProviderItems []json.RawMessage, parsed analysisResponse, headroom contextHeadroom) analysisResponse {
	if state.bestDraft == nil {
		out := critiqueDraftWithContent(parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
		candidate := state.newDraftCandidate("finalize", finalContent, finalProviderItems, parsed, out)
		state.considerFallbackDraft(candidate, false)
		state.considerDraft(candidate, false)
	}
	state.judgeRan = true
	objs, err := c.semanticCritiqueTracked(ctx, state, parsed, state.readPathList(), headroom)
	if err != nil {
		recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "error", ErrorCode: "semantic_judge_error"})
		log.Printf("  ⓘ semantic judge (post-loop): skipped (%v)", err)
		return state.bestDraft.parsed
	}
	if len(objs) == 0 {
		recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "passed"})
		log.Printf("  ✓ semantic judge (post-loop): no objections")
		return state.bestDraft.parsed
	}
	recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "objected", IssueCount: len(objs)})
	state.judgeObjected = true
	msgs := append(messages,
		modelMessage{Role: "assistant", Content: strPtr(finalContent), ProviderItems: finalProviderItems},
		modelMessage{Role: "user", Content: strPtr(formatSemanticObjections(objs))})
	revised, revisedProviderItems, safe := c.runFinalizeRoundTracked(ctx, state, msgs, headroom)
	if !safe {
		return state.bestDraft.parsed
	}
	rp, ok := tryParseAnalysis(revised)
	if !ok {
		recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "revision_unparseable", IssueCount: len(objs)})
		log.Printf("  ✗ semantic judge (post-loop): %d objection(s); refinalize did not parse, keeping draft", len(objs))
		return state.bestDraft.parsed
	}
	out := critiqueDraftWithContent(rp, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, rp), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(rp, &out, treeSet)
		}
	}
	candidate := state.newDraftCandidate("semantic_retry", revised, revisedProviderItems, rp, out)
	state.considerFallbackDraft(candidate, true)
	selected := state.considerDraft(candidate, true)
	if !out.Passed {
		recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "revision_rejected", IssueCount: len(out.Matches())})
		log.Printf("  ✗ semantic judge (post-loop): %d objection(s); revised draft failed critique %v, keeping original", len(objs), out.Matches())
		return state.bestDraft.parsed
	}
	if !selected {
		recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "revision_not_selected", IssueCount: len(objs)})
		log.Printf("  ✗ semantic judge (post-loop): %d objection(s); refinalized draft was not better, keeping original", len(objs))
		return state.bestDraft.parsed
	}
	state.judgeRevised = true
	recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Outcome: "revised", IssueCount: len(objs)})
	log.Printf("  ✗ semantic judge (post-loop): %d objection(s); accepted refinalized draft", len(objs))
	return state.bestDraft.parsed
}
