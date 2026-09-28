package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actiondraft"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

// Completer is the subset of the AI client this package needs (an interface so
// the reviewer step is unit-testable). Complete drives the critique.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// proposedFix is a validated, ready-to-commit change.
type proposedFix struct {
	// files maps repo path to the full new content (only changed files).
	files map[string]string
	// diff is a human-readable rendering for the PR body.
	diff string
	// rationale describes the generated change for the preview and PR.
	rationale string
	// executionVerification retains the tokenless validator contract.
	executionVerification *ExecutionVerification
	warnings              []string
}

// genParams holds the inputs for fix generation.
type genParams struct {
	// critique reviews the proposed change; nil (or critiqueRetries 0) skips it.
	critique Completer
	owner    string
	repo     string
	ref      string
	maxFiles int
	// critiqueRetries bounds how many times the agent is re-run to resolve a
	// reviewer's objections. Remaining objections become preview warnings.
	critiqueRetries int
	// instruction is an optional maintainer directive that steers the fix
	// (e.g. "patch the kustomize base instead"). Empty for the batch path.
	instruction string
	context     *GenerationContext
	// agent generates the fix with a coding-agent CLI in a real workspace clone.
	agent *AgentConfig
}

// repositoryChangeGuidance tells every Fix entry point when a patch is justified.
const repositoryChangeGuidance = `Decide whether a repository change is justified:
- A change is justified when code in this repository made, or failed to guard, the request or operation that hit the observed failure condition, even when the condition itself is external. Changing that code so it avoids the condition, or fails fast with a clear message, is a valid fix. Cite that code path by file and line.
- The published suggested fix is one candidate, not the default. An operational remedy does not rule out a repository guard.
- When such a code path exists, prefer a minimal patch with its caveats stated in your summary over making no change.
- Make no change when no code in this repository is causally involved, for example when the fix belongs in another repository or the cause is purely operational. Do not edit unrelated code, and do not skip or weaken the failing test, just to produce a patch.
`

// changeSummaryInstruction asks for the text that describes the patch in the draft PR.
const changeSummaryInstruction = "End with a short plain-text summary for the pull request description: what you changed and why, the causal code path by file and line, and any caveats. If you made no change, say why.\n"

const (
	patchVerifyWarning              = "One or more authentic verification commands failed; the generated patch may need maintainer revisions."
	patchCritiqueUnavailableWarning = "Automated review of the generated patch did not complete; review the patch without it."
	maxCritiqueWarningBytes         = 600
	maxChangeSummaryBytes           = 1500
)

// patchCritiqueWarning keeps the reviewer's concerns with the patch for the maintainer.
func patchCritiqueWarning(issues string) string {
	return "Automated review raised concerns; confirm them before opening: " + textutil.Truncate(oneLine(issues), maxCritiqueWarningBytes)
}

// changeRationale describes the generated patch from the coding agent's own
// summary, falling back to the changed files when the summary is unusable.
func changeRationale(summary string, files map[string]string) string {
	if text := oneLine(summary); text != "" && actiondraft.ValidateBody(text) == nil {
		return textutil.Truncate(text, maxChangeSummaryBytes)
	}
	paths := sortedKeys(files)
	for i, path := range paths {
		paths[i] = "`" + path + "`"
	}
	return "Edits " + strings.Join(paths, ", ") + "; see the proposed diff."
}

// runAgentFix runs the coding agent and an optional critique. Failed validators
// and critique concerns become preview warnings; runtime, integrity, and scope
// failures reject the attempt.
func runAgentFix(
	ctx context.Context, gp genParams,
	instruction func(reviewFeedback string) string,
	review func(files map[string]string, diff string) (string, error),
) (*proposedFix, error) {
	a := gp.agent
	if a != nil && a.SharedModelEndpoint && a.API == "responses" {
		return nil, fmt.Errorf("agent fix generation with the local OpenCode runtime requires Chat Completions; use ai.api=chat_completions or select a remote agent runtime")
	}
	if a == nil || a.Runtime == nil {
		return nil, fmt.Errorf("agent fix generation: no agent runtime configured")
	}
	var reviewFeedback string
	for attempt := 0; ; attempt++ {
		fix, err := runAgentAttempt(ctx, gp, instruction(reviewFeedback))
		if err != nil {
			return nil, err
		}
		if gp.critique == nil || gp.critiqueRetries == 0 {
			return fix, nil
		}
		issues, err := review(fix.files, fix.diff)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fix.warnings = append(fix.warnings, patchCritiqueUnavailableWarning)
			return fix, nil
		}
		if issues == "" {
			return fix, nil
		}
		fix.warnings = append(fix.warnings, patchCritiqueWarning(issues))
		if attempt >= gp.critiqueRetries {
			return fix, nil
		}
		reviewFeedback = issues
	}
}

func runAgentAttempt(ctx context.Context, gp genParams, instruction string) (*proposedFix, error) {
	a := gp.agent
	res, err := a.Runtime.Generate(ctx, agentRuntimeSpec(a, runtime.RepoRef{Owner: gp.owner, Name: gp.repo, Ref: gp.ref, Token: a.GitToken}, instruction))
	if err != nil {
		if errors.Is(err, runtime.ErrUnavailable) || errors.Is(err, runtime.ErrSandboxUnavailable) {
			return nil, fmt.Errorf("agent fix generation unavailable: %w", err)
		}
		return nil, fmt.Errorf("agent fix generation: %w", err)
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("the coding agent produced no repository change; the remediation may be external or operational")
	}
	if gp.maxFiles > 0 && len(res.Files) > gp.maxFiles {
		return nil, fmt.Errorf("the coding agent changed %d files, exceeding max_files=%d; dropping as too broad for review", len(res.Files), gp.maxFiles)
	}
	executionVerification, err := executionVerificationForAgent(a, res, gp.ref)
	if err != nil {
		return nil, err
	}
	fix := &proposedFix{
		files: res.Files, diff: res.Diff, rationale: changeRationale(res.AgentSummary, res.Files),
		executionVerification: executionVerification,
	}
	if executionVerification != nil && executionVerification.verifyResult().Status == VerifyFailed {
		fix.warnings = append(fix.warnings, patchVerifyWarning)
	}
	return fix, nil
}

// critiqueSystemPrompt is the reviewer contract shared by the fix critique.
const critiqueSystemPrompt = `You are a skeptical senior code reviewer checking a proposed fix for a CI failure before it becomes a draft PR. Judge whether the change is a reasonable, correct starting point. Flag concrete defects ONLY: wrong logic, values, or comparisons; references to undefined symbols, fields, or unimported packages; changes that break adjacent code; or a change that does not actually address the stated root cause. Do NOT flag style, formatting, or minor preferences, and remember it is a draft for a human to refine. If the change is a reasonable fix, return no issues.`

const (
	maxReviewResponseBytes = 1 << 20
	maxReviewCandidates    = 256
)

// parseReviewIssues selects the final usable review object from a response,
// tolerating prose, code snippets, repeated drafts, and code fences.
func parseReviewIssues(s string) ([]string, error) {
	if len(s) > maxReviewResponseBytes {
		return nil, fmt.Errorf("review response exceeds %d bytes", maxReviewResponseBytes)
	}
	candidates := reviewJSONCandidates(s)
	var lastErr error
	for i := len(candidates) - 1; i >= 0; i-- {
		for _, candidate := range []string{candidates[i], escapeStringControlChars(candidates[i])} {
			issues, err := decodeReviewIssues(candidate)
			if err != nil {
				lastErr = err
				continue
			}
			return issues, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no JSON review object in response")
}

func decodeReviewIssues(value string) ([]string, error) {
	var response struct {
		Issues *[]string `json:"issues"`
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&response); err != nil {
		return nil, err
	}
	if response.Issues == nil {
		return nil, fmt.Errorf("issues field is required")
	}
	return *response.Issues, nil
}

type reviewCandidateState struct {
	start    int
	depth    int
	inString bool
	escaped  bool
}

func reviewJSONCandidates(s string) []string {
	active := make([]reviewCandidateState, 0, 16)
	candidates := make([]string, 0, 16)
	for index := 0; index < len(s); index++ {
		ch := s[index]
		closed := make([]reviewCandidateState, 0, 2)
		next := active[:0]
		for _, state := range active {
			if state.inString {
				if state.escaped {
					state.escaped = false
				} else if ch == '\\' {
					state.escaped = true
				} else if ch == '"' {
					state.inString = false
				}
				next = append(next, state)
				continue
			}
			switch ch {
			case '"':
				state.inString = true
			case '{':
				state.depth++
			case '}':
				state.depth--
			}
			if state.depth == 0 {
				closed = append(closed, state)
			} else {
				next = append(next, state)
			}
		}
		active = next
		if len(closed) > 0 {
			sort.Slice(closed, func(i, j int) bool { return closed[i].start > closed[j].start })
			for _, state := range closed {
				candidates = append(candidates, s[state.start:index+1])
				if len(candidates) > maxReviewCandidates {
					candidates = candidates[len(candidates)-maxReviewCandidates:]
				}
			}
		}
		if ch == '{' {
			if len(active) == maxReviewCandidates {
				active = active[1:]
			}
			active = append(active, reviewCandidateState{start: index, depth: 1})
		}
	}
	return candidates
}

// escapeStringControlChars escapes raw control characters (tab, newline, and
// other bytes below 0x20) that appear inside JSON string literals, leaving
// structural whitespace between tokens untouched. Already-escaped sequences and
// characters outside strings pass through unchanged.
func escapeStringControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false
	for _, r := range s {
		if !inString {
			if r == '"' {
				inString = true
			}
			b.WriteRune(r)
			continue
		}
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		switch {
		case r == '\\':
			b.WriteRune(r)
			escaped = true
		case r == '"':
			b.WriteRune(r)
			inString = false
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func dedupeNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
