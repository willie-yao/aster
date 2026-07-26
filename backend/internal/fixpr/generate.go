package fixpr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
	// rationale is the model's short explanation of the change.
	rationale string
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
	// reviewer's objections before the fix is dropped.
	critiqueRetries int
	// instruction is an optional maintainer directive that steers the fix
	// (e.g. "patch the kustomize base instead"). Empty for the batch path.
	instruction string
	context     *GenerationContext
	// agent generates the fix with a coding-agent CLI in a real workspace clone.
	agent *AgentConfig
}

// critiqueSystemPrompt is the reviewer contract shared by the fix critique.
const critiqueSystemPrompt = `You are a skeptical senior code reviewer checking a proposed fix for a CI failure before it becomes a draft PR. Judge whether the change is a reasonable, correct starting point. Flag concrete defects ONLY: wrong logic, values, or comparisons; references to undefined symbols, fields, or unimported packages; changes that break adjacent code; or a change that does not actually address the stated root cause. Do NOT flag style, formatting, or minor preferences, and remember it is a draft for a human to refine. If the change is a reasonable fix, return no issues.`

// parseReviewIssues selects the final usable review object from a response,
// tolerating prose, code snippets, repeated drafts, and code fences.
func parseReviewIssues(s string) ([]string, error) {
	bestEnd, bestStart := -1, len(s)+1
	var best []string
	var lastErr error
	for start := 0; start < len(s); start++ {
		if s[start] != '{' {
			continue
		}
		for _, candidate := range []string{s[start:], escapeStringControlChars(s[start:])} {
			issues, consumed, err := decodeReviewIssues(candidate)
			if err != nil {
				lastErr = err
				continue
			}
			end := start + consumed
			if end > bestEnd || end == bestEnd && start < bestStart {
				bestEnd, bestStart, best = end, start, issues
			}
		}
	}
	if bestEnd >= 0 {
		return best, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no JSON review object in response")
}

func decodeReviewIssues(value string) ([]string, int, error) {
	var response struct {
		Issues *[]string `json:"issues"`
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&response); err != nil {
		return nil, 0, err
	}
	if response.Issues == nil {
		return nil, 0, fmt.Errorf("issues field is required")
	}
	return *response.Issues, int(decoder.InputOffset()), nil
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
