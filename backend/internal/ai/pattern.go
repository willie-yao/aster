package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// patternPromptVersion is bumped when the pattern prompt or output contract
// changes, so cached verdicts from an older contract are re-run.
const patternPromptVersion = 3

// maxPatternBuilds caps how many per-build analyses are fed into one pattern
// call, keeping the prompt bounded for a test that failed in many builds.
const maxPatternBuilds = 10

// patternMaxIters bounds the repotree tool rounds the grounded correlation loop
// spends verifying file and config paths against the real source repo.
const patternMaxIters = 6

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

// PatternInput is the bounded, deterministic model input for one job-level
// correlation. Failures are newest-first and capped to the prompt limit.
type PatternInput struct {
	SystemPrompt string
	UserPrompt   string
	Failures     []PatternFailure
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

// patternGroundedSystemPrompt is patternSystemPrompt plus the repotree tool
// contract. It is used when a source-repo reader is wired, so the model
// verifies every file, template, or config path it names against the real
// repository instead of guessing one.
const patternGroundedSystemPrompt = patternSystemPrompt + `

You also have read-only tools over the source repository:
- list_repo_tree(path): immediate children of a directory
- read_repo_file(path, offset, len): byte-range read of one file
- grep_repo(pattern, path_glob?): RE2 search over a bounded file set

Ground every path you cite. BEFORE naming any file, template, manifest, or config in shared_root_cause or suggested_fix, confirm it exists by grepping for its name or the symbols/keys involved, listing the directory, and reading the file. NEVER invent or guess a path: an unread path is a hallucination. If you cannot find a real file that fits, describe the change without a fabricated path and lower confidence accordingly. When you are done investigating, respond with ONLY the JSON object described above and nothing else.`

// AnalyzePattern correlates the per-build analyses of one repeatedly-failing
// job into a single PatternAnalysis. When a source-repo reader is wired it runs
// a repotree tool loop so the model verifies file and config paths against the
// real repo; otherwise it makes one tool-free model call. Either way a
// path-verification guard flags citations that do not exist. The verdict is
// cached keyed by the exact model input, so it only re-runs when the evidence
// changes. Returns nil when there are fewer than two analyzed builds.
func (s *Service) AnalyzePattern(ctx context.Context, jobID, subject string, failures []PatternFailure) (*models.PatternAnalysis, error) {
	input := BuildPatternInput(subject, failures)
	if len(input.Failures) < 2 {
		return nil, nil
	}
	failures = input.Failures
	userPrompt := input.UserPrompt
	grounded := s.patternRepo != nil
	// groundKey namespaces the cache entry by grounding mode and, when grounded,
	// the source repo identity, so a repo change or a switch between grounded and
	// tool-free never serves a stale verdict.
	groundKey := "toolfree"
	if grounded {
		groundKey = "grounded:" + s.sourceRepoOwner + "/" + s.sourceRepoName
	}

	// Key the verdict to the exact model input, including prompt version, the
	// grounding mode, and the rendered user prompt, so any evidence change or a
	// switch between grounded and tool-free invalidates the entry.
	key := patternCacheKey(s.module.Name(), jobID, subject, userPrompt, groundKey)
	if raw, ok := s.client.cache.Get(key); ok {
		var cached patternResponse
		if json.Unmarshal(raw, &cached) == nil && validPatternResponse(cached) {
			return buildPatternAnalysis(subject, len(failures), cached, collectRelevantFiles(failures)), nil
		}
	}

	var parsed patternResponse
	var err error
	if grounded {
		parsed, err = s.groundedPatternVerdict(ctx, userPrompt)
	} else {
		parsed, err = s.toolFreePatternVerdict(ctx, userPrompt)
	}
	if err != nil {
		return nil, err
	}
	if !validPatternResponse(parsed) {
		return nil, fmt.Errorf("pattern analysis: incomplete verdict (empty summary, or systemic without a root cause)")
	}

	// Flag any file path the verdict names that does not exist in the source
	// repo, so a fabricated citation is marked rather than asserted as fact.
	s.guardPatternPaths(ctx, &parsed)

	_ = s.client.cache.Set(key, parsed)
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures)), nil
}

// BuildPatternInput renders the shared pattern-analysis contract used by the
// in-process and Orka backends.
func BuildPatternInput(subject string, failures []PatternFailure) PatternInput {
	prepared := append([]PatternFailure(nil), failures...)
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].BuildID > prepared[j].BuildID })
	if len(prepared) > maxPatternBuilds {
		prepared = prepared[:maxPatternBuilds]
	}
	return PatternInput{
		SystemPrompt: patternSystemPrompt,
		UserPrompt:   buildPatternUserPrompt(subject, prepared),
		Failures:     prepared,
	}
}

// ParsePatternResult validates a model correlation result and converts it to
// the published PatternAnalysis shape.
func ParsePatternResult(subject string, failures []PatternFailure, result string) (*models.PatternAnalysis, error) {
	var parsed patternResponse
	if err := json.Unmarshal([]byte(extractJSON(result)), &parsed); err != nil {
		return nil, fmt.Errorf("pattern analysis: parse response: %w", err)
	}
	if !validPatternResponse(parsed) {
		return nil, fmt.Errorf("pattern analysis: incomplete verdict (empty summary, or systemic without a root cause)")
	}
	// The Orka correlation task has no source-repository tools. Mark every path
	// it introduces as unverified rather than presenting a guessed target as fact.
	parsed.SuggestedFix = annotateUnverifiedPaths(parsed.SuggestedFix, func(string) bool { return false })
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures)), nil
}

// toolFreePatternVerdict makes the single correlation call with no tools, used
// when no source-repo reader is wired.
func (s *Service) toolFreePatternVerdict(ctx context.Context, userPrompt string) (patternResponse, error) {
	messages := []modelMessage{
		{Role: "system", Content: strPtr(patternSystemPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	resp, err := s.client.callModel(ctx, messages, nil, nil)
	if err != nil {
		return patternResponse{}, fmt.Errorf("pattern analysis chat: %w", err)
	}
	if !resp.HasMessage || resp.Message.Content == nil {
		return patternResponse{}, fmt.Errorf("pattern analysis: empty response")
	}
	var parsed patternResponse
	if err := json.Unmarshal([]byte(extractJSON(*resp.Message.Content)), &parsed); err != nil {
		return patternResponse{}, fmt.Errorf("pattern analysis: parse response: %w", err)
	}
	return parsed, nil
}

// groundedPatternVerdict runs the correlation as a repotree tool loop so the
// model greps and reads real source files before naming a fix target. It
// recovers a non-JSON final message with one cheap extraction completion rather
// than re-running the loop.
func (s *Service) groundedPatternVerdict(ctx context.Context, userPrompt string) (patternResponse, error) {
	reg := tools.NewRegistry()
	repotree.Register(reg)
	enabled, err := reg.Enable([]string{repotree.Group})
	if err != nil {
		return patternResponse{}, fmt.Errorf("enabling repo tools: %w", err)
	}
	env := &tools.Env{Repo: s.patternRepo, Cache: s.patternToolCache()}
	out, err := s.client.ToolLoop(ctx, patternGroundedSystemPrompt, userPrompt, reg, enabled, env,
		ToolLoopOptions{MaxIters: patternMaxIters, MinToolCalls: 1, SingleToolCall: true})
	if err != nil {
		return patternResponse{}, fmt.Errorf("pattern analysis tool loop: %w", err)
	}
	// A content-free loop carries no evidence to correlate; reject it rather
	// than letting the recovery completion synthesize a verdict from nothing.
	if strings.TrimSpace(out) == "" {
		return patternResponse{}, fmt.Errorf("pattern analysis: empty tool-loop output")
	}
	var parsed patternResponse
	if perr := json.Unmarshal([]byte(extractJSON(out)), &parsed); perr != nil {
		extract := "Extract the correlation verdict this investigation reached as JSON with exactly these keys: " +
			`{"systemic": true|false, "confidence": "high"|"medium"|"low", "shared_root_cause": "...", "shared_builds": ["..."], "suggested_fix": "...", "summary": "..."}. ` +
			"Reply with ONLY the JSON.\n\nInvestigation:\n" + out
		out2, ferr := s.client.Complete(ctx, "You output only a JSON object.", extract)
		if ferr != nil {
			return patternResponse{}, fmt.Errorf("pattern analysis: parse response: %w", perr)
		}
		if perr2 := json.Unmarshal([]byte(extractJSON(out2)), &parsed); perr2 != nil {
			return patternResponse{}, fmt.Errorf("pattern analysis: parse response: %w", perr2)
		}
	}
	return parsed, nil
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
// the grounding namespace (mode plus, when grounded, the source repo), and the
// rendered model input, so the verdict is reused only while the exact evidence
// the model saw is unchanged.
func patternCacheKey(module, jobID, subject, userPrompt, groundKey string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\x00%s\x00%s\x00%s\x00%s", patternPromptVersion, groundKey, jobID, subject, userPrompt)
	return fmt.Sprintf("pattern:%s:%s", module, hex.EncodeToString(h.Sum(nil)[:12]))
}

// clampPattern trims a field to max bytes so one verbose analysis can't blow
// the pattern prompt budget.
func clampPattern(s string, max int) string {
	return textutil.Truncate(strings.TrimSpace(s), max)
}

// patternPathRe matches repo-relative-looking file paths embedded in prose,
// with or without a directory prefix, ending in a source or config extension.
// Bounded to real file references so it does not flag incidental words. Log and
// prose extensions (.log/.txt/.md) are excluded because suggested_fix names
// files to change, not evidence artifacts.
var patternPathRe = regexp.MustCompile(`(?:[A-Za-z0-9_.\-]+/)*[A-Za-z0-9_.\-]+\.(?:go|ya?ml|sh|json|tpl|star|bzl|toml|cfg|conf|mod)`)

// guardPatternPaths annotates any file path in the verdict's suggested fix that
// does not exist in the source repo, so a fabricated citation is marked
// "(unverified path)" rather than asserted as fact. It is a no-op when no repo
// is available to verify against. Only suggested_fix is guarded: shared_root_cause
// and summary describe evidence and legitimately cite GCS artifact paths that are
// not in the source tree.
func (s *Service) guardPatternPaths(ctx context.Context, p *patternResponse) {
	exists := s.patternPathVerifier(ctx)
	if exists == nil {
		return
	}
	p.SuggestedFix = annotateUnverifiedPaths(p.SuggestedFix, exists)
}

// patternPathVerifier returns a func reporting whether a cited path exists, or
// nil when nothing can verify. The memoized repo tree verifies both full paths
// and bare basenames; if the tree is unavailable it falls back to a raw-CDN
// existence check against branding.source_repo so the guard stays active rather
// than silently disabling. The CDN fallback verifies only explicit repo-relative
// paths, leaving bare names unflagged since it cannot locate them to a subdir.
func (s *Service) patternPathVerifier(ctx context.Context) func(string) bool {
	if s.patternRepo != nil {
		if tree, err := s.patternRepoTree(ctx); err == nil && len(tree) > 0 {
			full := make(map[string]bool, len(tree))
			base := make(map[string]bool, len(tree))
			for _, t := range tree {
				full[t] = true
				base[path.Base(t)] = true
			}
			return func(p string) bool {
				if strings.Contains(p, "/") {
					return full[p]
				}
				return base[p]
			}
		}
	}
	if s.sourceRepoOwner != "" && s.sourceRepoName != "" {
		client := &http.Client{Timeout: 10 * time.Second}
		return func(p string) bool {
			if !strings.Contains(p, "/") {
				return true
			}
			return s.verifyGitHubFile(ctx, client, s.sourceRepoOwner, s.sourceRepoName, p)
		}
	}
	return nil
}

// annotateUnverifiedPaths marks each path-like token that fails exists with a
// trailing "(unverified path)" note, once per distinct token.
func annotateUnverifiedPaths(text string, exists func(string) bool) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	seen := map[string]bool{}
	for _, m := range patternPathRe.FindAllString(text, -1) {
		clean := strings.TrimPrefix(m, "./")
		if seen[m] || exists(clean) {
			seen[m] = true
			continue
		}
		seen[m] = true
		text = strings.ReplaceAll(text, m, m+" (unverified path)")
	}
	return text
}

// patternRepoTree returns the source repo's blob paths, memoized for the run so
// the recursive tree listing costs one API call across every job.
func (s *Service) patternRepoTree(ctx context.Context) ([]string, error) {
	s.patternTreeMu.Lock()
	defer s.patternTreeMu.Unlock()
	if s.patternTreeDone {
		return s.patternTree, s.patternTreeErr
	}
	s.patternTree, s.patternTreeErr = s.patternRepo.ListTree(ctx)
	s.patternTreeDone = true
	return s.patternTree, s.patternTreeErr
}

// patternToolCache returns the tools.Cache shared across all pattern tool loops
// in a run, so repotree memoizes the tree and file reads once across jobs.
func (s *Service) patternToolCache() *tools.Cache {
	s.patternTreeMu.Lock()
	defer s.patternTreeMu.Unlock()
	if s.patternCache == nil {
		s.patternCache = tools.NewCache()
	}
	return s.patternCache
}
