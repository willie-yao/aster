package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// patternPromptVersion is bumped when the pattern prompt or output contract
// changes, so cached verdicts from an older contract are re-run.
const patternPromptVersion = 2

// maxPatternBuilds caps how many per-build analyses are fed into one pattern
// call, keeping the prompt bounded for a test that failed in many builds.
const maxPatternBuilds = 10

// PatternFailure is one build's analyzed job failure, used as input to
// cross-failure correlation. FailingTest is the specific test or spec that
// failed in this build and may differ across builds.
type PatternFailure struct {
	BuildID        string
	FailingTest    string
	FailureMessage string
	RootCause      string
	// SuggestedFix is this build's per-failure remediation. The correlation
	// preserves the most specific one rather than regressing to the symptom.
	SuggestedFix string
	// RelevantFiles are the source files this build's analysis implicated,
	// so the correlation can name concrete targets in the cross-cutting fix.
	RelevantFiles []string
	// LocationFile is the failing test's own source file (from the JUnit
	// failure location). It seeds the fix harness but is deliberately kept out
	// of the correlation prompt so it never perturbs the pattern cache key.
	LocationFile string
	IsTransient  bool
	Severity     string
}

// patternResponse is the model's JSON contract for the correlation verdict.
type patternResponse struct {
	Systemic        bool     `json:"systemic"`
	Confidence      string   `json:"confidence"`
	SharedRootCause string   `json:"shared_root_cause"`
	SharedBuilds    []string `json:"shared_builds"`
	SuggestedFix    string   `json:"suggested_fix"`
	Summary         string   `json:"summary"`
}

// patternSystemPrompt instructs the model to correlate several per-build
// analyses of the same job into one systemic-vs-transient verdict.
const patternSystemPrompt = `You correlate multiple failed builds of the SAME CI job to decide whether they share one underlying root cause.

You are given N independent per-build failure analyses from recent failed builds of one job. Each build was analyzed in isolation, so each may have called its own failure "transient". The specific test or spec that failed may differ from build to build. Each per-build analysis may also carry its own root_cause, suggested_fix, and the source files it implicated. Your job is the cross-build view those single analyses cannot have.

Key principle: a failure mode that recurs across most builds is NOT a flake, it is a systemic bug. "Transient" infrastructure errors (timeouts, resource exhaustion, slow disk, quota, image-pull) that appear in the majority of recent runs almost always have a fixable systemic cause (e.g. an undersized VM, a tight timeout, a missing image, a misconfigured template). Weigh the underlying MECHANISM, not the surface symptom: the same root cause can present as different-looking failures (different test flavors, different failing specs, different error strings).

Preserve signal, do not flatten it. The per-build analyses often already pinpoint the mechanism (a specific error, a named operation, an implicated file). Carry the MOST SPECIFIC evidence-backed cause and fix forward; do NOT regress to the lowest-common-denominator symptom that every build merely shares. If one build identified a concrete mechanism (e.g. "concurrent agent-pool update -> Azure OperationNotAllowed") and another only saw the symptom, the shared cause is the concrete mechanism, not the symptom.

Distinguish symptom from root cause. "VM bootstrapping failed", "test timed out", "node never joined" are SYMPTOMS. The root cause is WHY: the specific operation, condition, config, or code path that produced them.

The suggested_fix must be ACTIONABLE: name the specific change, the mechanism it addresses, and the component / file / config to change (cite a relevant_file when one is implicated). Do NOT emit non-fixes like "investigate the logs", "check why X fails", or "look into Y" - those are next steps, not fixes. If the evidence genuinely does not determine a concrete fix, say so plainly in suggested_fix AND lower confidence accordingly (do not claim high confidence on an undetermined fix).

Decide:
- systemic=true when most builds share one underlying cause. Name it precisely and give the concrete cross-cutting fix.
- systemic=false when the failures are genuinely unrelated or independently one-off.

Respond with ONLY a JSON object, no prose, no code fences:
{
  "systemic": true|false,
  "confidence": "high"|"medium"|"low",  // high only when the cause is specific AND the fix is concrete
  "shared_root_cause": "the one underlying MECHANISM (empty if not systemic); not a restated symptom",
  "shared_builds": ["buildID", ...],   // builds you judge to share the cause
  "suggested_fix": "the concrete, actionable cross-cutting fix naming the change and target (empty if not systemic)",
  "summary": "one short paragraph: the verdict and the evidence for it"
}`

// AnalyzePattern correlates the per-build analyses of one repeatedly-failing
// job into a single PatternAnalysis. It makes one tool-free model call and
// caches the verdict keyed by the exact model input, so it only re-runs when
// the evidence changes. Returns nil when there is too little to correlate
// when there are fewer than two analyzed builds.
func (s *Service) AnalyzePattern(ctx context.Context, jobID, subject string, failures []PatternFailure) (*models.PatternAnalysis, error) {
	if len(failures) < 2 {
		return nil, nil
	}
	// Deterministic newest-first order and a stable cap keep the prompt and
	// cache key from churning run to run.
	sort.Slice(failures, func(i, j int) bool { return failures[i].BuildID > failures[j].BuildID })
	if len(failures) > maxPatternBuilds {
		failures = failures[:maxPatternBuilds]
	}

	userPrompt := buildPatternUserPrompt(subject, failures)

	// Key the verdict to the exact model input, including prompt version and
	// rendered user prompt, so any evidence change invalidates the entry.
	key := patternCacheKey(s.module.Name(), jobID, subject, userPrompt)
	if raw, ok := s.client.cache.Get(key); ok {
		var cached patternResponse
		if json.Unmarshal(raw, &cached) == nil && validPatternResponse(cached) {
			return buildPatternAnalysis(subject, len(failures), cached, collectRelevantFiles(failures)), nil
		}
	}

	messages := []agChatMessage{
		{Role: "system", Content: strPtr(patternSystemPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	resp, err := s.client.callChatWithTools(ctx, messages, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("pattern analysis chat: %w", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return nil, fmt.Errorf("pattern analysis: empty response")
	}
	var parsed patternResponse
	if err := json.Unmarshal([]byte(extractJSON(*resp.Choices[0].Message.Content)), &parsed); err != nil {
		return nil, fmt.Errorf("pattern analysis: parse response: %w", err)
	}
	if !validPatternResponse(parsed) {
		return nil, fmt.Errorf("pattern analysis: incomplete verdict (empty summary, or systemic without a root cause)")
	}
	_ = s.client.cache.Set(key, parsed)
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures)), nil
}

// collectRelevantFiles unions the files each build implicated, in first-seen
// order, leading with the failing test's own source file. These carry the
// analysis's own targeting into the pattern so the fix harness can ground
// candidate selection on them rather than re-deriving targets from scratch.
func collectRelevantFiles(failures []PatternFailure) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for _, f := range failures {
		add(f.LocationFile)
		for _, rf := range f.RelevantFiles {
			add(rf)
		}
	}
	return out
}

// validPatternResponse rejects empty or self-contradictory verdicts so they are
// neither cached nor published as a misleading banner.
func validPatternResponse(p patternResponse) bool {
	if strings.TrimSpace(p.Summary) == "" {
		return false
	}
	if p.Systemic && strings.TrimSpace(p.SharedRootCause) == "" {
		return false
	}
	return true
}

// buildPatternAnalysis converts a parsed verdict into the published model.
func buildPatternAnalysis(subject string, builds int, p patternResponse, relevantFiles []string) *models.PatternAnalysis {
	conf := strings.ToLower(strings.TrimSpace(p.Confidence))
	switch conf {
	case "high", "medium", "low":
	default:
		conf = "low"
	}
	return &models.PatternAnalysis{
		Subject:         subject,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		BuildsAnalyzed:  builds,
		Systemic:        p.Systemic,
		Confidence:      conf,
		SharedRootCause: strings.TrimSpace(p.SharedRootCause),
		SharedBuilds:    p.SharedBuilds,
		SuggestedFix:    strings.TrimSpace(p.SuggestedFix),
		Summary:         strings.TrimSpace(p.Summary),
		RelevantFiles:   relevantFiles,
	}
}

// buildPatternUserPrompt renders the per-build analyses into the user message.
func buildPatternUserPrompt(subject string, failures []PatternFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job: %s\n", subject)
	fmt.Fprintf(&b, "It failed in %d recent builds. The per-build analyses follow (the failing test/spec may differ between builds).\n\n", len(failures))
	for i, f := range failures {
		fmt.Fprintf(&b, "--- Build %d (id %s) ---\n", i+1, f.BuildID)
		if f.FailingTest != "" {
			fmt.Fprintf(&b, "failing_test: %s\n", f.FailingTest)
		}
		if f.IsTransient {
			b.WriteString("classified_transient: yes\n")
		}
		if f.Severity != "" {
			fmt.Fprintf(&b, "severity: %s\n", f.Severity)
		}
		if f.RootCause != "" {
			fmt.Fprintf(&b, "root_cause: %s\n", clampPattern(f.RootCause, 1500))
		}
		if f.SuggestedFix != "" {
			fmt.Fprintf(&b, "suggested_fix: %s\n", clampPattern(f.SuggestedFix, 600))
		}
		if len(f.RelevantFiles) > 0 {
			fmt.Fprintf(&b, "relevant_files: %s\n", clampPattern(strings.Join(f.RelevantFiles, ", "), 400))
		}
		if f.FailureMessage != "" {
			fmt.Fprintf(&b, "failure_message: %s\n", clampPattern(f.FailureMessage, 600))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// patternCacheKey keys a verdict by the project module, job, prompt version,
// and the rendered model input, so the verdict is reused only while the exact
// evidence the model saw is unchanged.
func patternCacheKey(module, jobID, subject, userPrompt string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\x00%s\x00%s\x00%s", patternPromptVersion, jobID, subject, userPrompt)
	return fmt.Sprintf("pattern:%s:%s", module, hex.EncodeToString(h.Sum(nil)[:12]))
}

// clampPattern trims a field to max bytes so one verbose analysis can't blow
// the pattern prompt budget.
func clampPattern(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
