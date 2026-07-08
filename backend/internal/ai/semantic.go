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
func (c *Client) semanticCritique(ctx context.Context, parsed analysisResponse, readPaths []string) ([]string, error) {
	out, err := c.Complete(ctx, semanticJudgeSystemPrompt, formatSemanticJudgeInput(parsed, readPaths))
	if err != nil {
		return nil, err
	}
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
func (c *Client) applySemanticJudgePostLoop(ctx context.Context, state *agentState, messages []agChatMessage, finalContent string, parsed analysisResponse, budget int) analysisResponse {
	objs, err := c.semanticCritique(ctx, parsed, state.readPathList())
	if err != nil {
		log.Printf("  ⓘ semantic judge (post-loop): skipped (%v)", err)
		return parsed
	}
	if len(objs) == 0 {
		log.Printf("  ✓ semantic judge (post-loop): no objections")
		return parsed
	}
	msgs := append(messages,
		agChatMessage{Role: "assistant", Content: strPtr(finalContent)},
		agChatMessage{Role: "user", Content: strPtr(formatSemanticObjections(objs))})
	revised := c.runFinalizeRound(ctx, msgs, budget)
	rp, ok := tryParseAnalysis(revised)
	if !ok {
		log.Printf("  ✗ semantic judge (post-loop): %d objection(s); refinalize did not parse, keeping draft", len(objs))
		return parsed
	}
	out := critiqueDraft(rp, state.readArtifactsFull, state.readArtifactsBase, matchSkillsForDraft(state, rp), state.consecutiveFailures)
	if !out.Passed {
		log.Printf("  ✗ semantic judge (post-loop): %d objection(s); revised draft failed critique %v, keeping original", len(objs), out.Matches())
		return parsed
	}
	log.Printf("  ✗ semantic judge (post-loop): %d objection(s); accepted refinalized draft", len(objs))
	return rp
}
