package fixpr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// errNoEdits signals the model returned an empty edit set. generateFix treats
// it as retryable, then reports a clear "not auto-fixable" error.
var errNoEdits = errors.New("model proposed no edits")

// Completer is the subset of the AI client this package needs (an interface so
// generation is unit-testable).
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// edit is one anchored search/replace within a single file.
type edit struct {
	File string `json:"file"`
	Old  string `json:"old"`
	New  string `json:"new"`
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

// genParams holds the inputs for generateFix.
type genParams struct {
	completer Completer
	// critique reviews the proposed change; nil (or critiqueRetries 0) skips it.
	critique Completer
	source   sourceReader
	owner    string
	repo     string
	ref      string
	maxFiles int
	// critiqueRetries bounds how many times the edit step is re-prompted to
	// resolve a reviewer's objections or a validation error before dropping.
	critiqueRetries int
}

// generateFix turns a pattern into a validated minimal edit: pick target
// file(s) from the repo's real tree, fetch them, propose anchored edits, apply
// them (exact-match-once), parse-check, and (optionally) pass an LLM review.
// Validation or review failures re-prompt up to critiqueRetries, then the fix is
// dropped.
func generateFix(ctx context.Context, gp genParams, p models.PatternAnalysis) (*proposedFix, error) {
	tree, err := gp.source.ListTree(ctx, gp.owner, gp.repo, gp.ref)
	if err != nil {
		return nil, fmt.Errorf("listing %s/%s tree: %w", gp.owner, gp.repo, err)
	}
	candidates := rankCandidates(tree, p)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate files in the repo matched the failure")
	}

	files, err := locateTargets(ctx, gp.completer, p, candidates, gp.maxFiles)
	if err != nil {
		return nil, err
	}

	contents := make(map[string]string, len(files))
	for _, f := range files {
		body, found, err := gp.source.FileContent(ctx, gp.owner, gp.repo, gp.ref, f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if !found {
			return nil, fmt.Errorf("model named a file that does not exist in the source repo: %s", f)
		}
		contents[f] = body
	}

	// Propose, validate, and review; re-prompt with feedback on a fixable
	// problem (no edits, broken syntax, or reviewer objections) up to
	// critiqueRetries.
	var feedback string
	for attempt := 0; ; attempt++ {
		edits, rationale, err := proposeEdits(ctx, gp.completer, p, contents, feedback)
		if errors.Is(err, errNoEdits) {
			// The model returned an empty edit set. Nudge it once more with
			// explicit feedback before giving up, like the other retry paths.
			if attempt < gp.critiqueRetries {
				feedback = "You returned no edits. Propose at least one concrete anchored edit that addresses the root cause, or if the fix is purely operational (infrastructure, quota, or timing) and cannot be a code change, this failure is not auto-fixable."
				continue
			}
			return nil, fmt.Errorf("the model could not produce a code change for this failure (its fix may be operational rather than a code edit)")
		}
		if err != nil {
			return nil, err
		}
		changed, applyErr := applyEdits(contents, edits, gp.maxFiles)
		if applyErr == nil {
			applyErr = validateSyntax(changed)
		}
		if applyErr != nil {
			if attempt < gp.critiqueRetries {
				feedback = "The previous edits were rejected: " + applyErr.Error()
				continue
			}
			return nil, applyErr
		}

		fix := &proposedFix{files: changed, diff: renderDiff(contents, edits), rationale: rationale}
		if gp.critique == nil || gp.critiqueRetries == 0 {
			return fix, nil
		}
		issues, err := critiqueFix(ctx, gp.critique, p, contents, changed, edits)
		if err != nil {
			// An inconclusive review (endpoint down or an unreadable verdict)
			// fails closed: drop the fix rather than skip the gate.
			return nil, fmt.Errorf("fix review failed: %w", err)
		}
		if issues == "" {
			return fix, nil
		}
		if attempt >= gp.critiqueRetries {
			return nil, fmt.Errorf("fix rejected by review after %d attempt(s): %s", attempt+1, oneLine(issues))
		}
		feedback = "A reviewer found problems with the previous edits; address them: " + issues
	}
}

const locateSystemPrompt = `You identify which source files a known CI failure fix should touch. You are given a candidate list of REAL repository file paths. Choose the smallest set of paths FROM THAT LIST that must change to fix the failure, preferring configuration, template, or manifest files. You MUST only return paths that appear verbatim in the candidate list; if none fit, return an empty list. Answer only with one-line JSON, no prose.`

// locateTargets has the model pick target file(s) from the candidate paths,
// capped at maxFiles. Picks outside the candidate set are rejected.
func locateTargets(ctx context.Context, c Completer, p models.PatternAnalysis, candidates []string, maxFiles int) ([]string, error) {
	user := fmt.Sprintf(`Recurring failure pattern:
- subject: %s
- shared_root_cause: %s
- suggested_fix: %s
- summary: %s

Candidate repository files (choose only from these exact paths):
%s

Choose the file path(s) to change, fewest first, only from the list above.
Answer with one line of JSON:
{"files": ["path/one", "path/two"]}`,
		p.Subject, oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), oneLine(p.Summary),
		strings.Join(candidates, "\n"))

	out, err := c.Complete(ctx, locateSystemPrompt, user)
	if err != nil {
		return nil, err
	}
	var v struct {
		Files []string `json:"files"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return nil, fmt.Errorf("locate response: %w", err)
	}
	files := dedupeNonEmpty(v.Files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no candidate file fit the failure")
	}
	if len(files) > maxFiles {
		return nil, fmt.Errorf("model named %d files, exceeds max_files %d", len(files), maxFiles)
	}
	candSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candSet[c] = true
	}
	for _, f := range files {
		if !candSet[f] {
			return nil, fmt.Errorf("model named %q, which is not a real repo file", f)
		}
	}
	return files, nil
}

const editSystemPrompt = `You propose a MINIMAL fix as anchored search/replace edits. You are given the current content of one or more files and a known failure root cause. For each change, return an "old" snippet copied VERBATIM from the file (long enough to be unique within that file) and the "new" snippet to replace it with. Do not reformat unrelated lines. Make the smallest change that fixes the root cause. Answer only with JSON, no prose.`

// proposeEdits asks the model for anchored edits given the real file contents.
// feedback, when non-empty, tells the model to address problems from a prior
// attempt.
func proposeEdits(ctx context.Context, c Completer, p models.PatternAnalysis, contents map[string]string, feedback string) ([]edit, string, error) {
	var sb strings.Builder
	for _, path := range sortedKeys(contents) {
		fmt.Fprintf(&sb, "=== FILE: %s ===\n%s\n", path, contents[path])
	}
	fb := ""
	if strings.TrimSpace(feedback) != "" {
		fb = "\n" + feedback + "\n"
	}
	user := fmt.Sprintf(`Root cause: %s
Suggested fix: %s
%s
Current file content(s):
%s
Return anchored edits as JSON:
{"rationale": "<one sentence>", "edits": [{"file": "<path>", "old": "<verbatim snippet>", "new": "<replacement>"}]}
The "old" snippet for each edit MUST appear exactly once in that file.`,
		oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), fb, sb.String())

	out, err := c.Complete(ctx, editSystemPrompt, user)
	if err != nil {
		return nil, "", err
	}
	var v struct {
		Rationale string `json:"rationale"`
		Edits     []edit `json:"edits"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return nil, "", fmt.Errorf("edit response: %w", err)
	}
	if len(v.Edits) == 0 {
		return nil, "", errNoEdits
	}
	return v.Edits, strings.TrimSpace(v.Rationale), nil
}

const critiqueSystemPrompt = `You are a skeptical senior code reviewer checking a proposed fix for a CI failure before it becomes a draft PR. Judge whether the change is a reasonable, correct starting point. Flag concrete defects ONLY: wrong logic, values, or comparisons; references to undefined symbols, fields, or unimported packages; changes that break adjacent code; or a change that does not actually address the stated root cause. Do NOT flag style, formatting, or minor preferences, and remember it is a draft for a human to refine. If the change is a reasonable fix, return no issues.`

// critiqueFix asks a reviewer model whether the change has concrete defects. It
// returns a "; "-joined issue string (empty when the change is acceptable). The
// reviewer sees the diff plus the full edited file(s) so it can judge context
// (imports, surrounding code).
func critiqueFix(ctx context.Context, c Completer, p models.PatternAnalysis, orig, changed map[string]string, edits []edit) (string, error) {
	var sb strings.Builder
	sb.WriteString(renderDiff(orig, edits))
	sb.WriteString("\n\n")
	for _, path := range sortedKeys(changed) {
		fmt.Fprintf(&sb, "=== FILE AFTER EDIT: %s ===\n%s\n", path, changed[path])
	}
	user := fmt.Sprintf(`Root cause: %s
Suggested fix: %s

Proposed change:
%s
Does this change have concrete defects (not style)? Answer with one line of JSON, no comments. Use an empty array if it is a reasonable fix:
{"issues": ["<problem>", "<problem>"]}`,
		oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), sb.String())

	out, err := c.Complete(ctx, critiqueSystemPrompt, user)
	if err != nil {
		return "", err
	}
	var v struct {
		Issues []string `json:"issues"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return "", fmt.Errorf("review response: %w", err)
	}
	return strings.Join(dedupeNonEmpty(v.Issues), "; "), nil
}

// Scope caps that keep a fix minimal (so exact-match-once can't be abused with a
// whole-file or oversized anchor).
const (
	maxEdits             = 20   // anchored edits per fix
	maxEditBytes         = 8192 // per old/new snippet
	maxChangedLinesTotal = 120  // summed old+new lines across all edits
)

// applyEdits applies each anchored edit, requiring its "old" snippet to match
// exactly once, within the scope caps. Returns only the changed files.
func applyEdits(orig map[string]string, edits []edit, maxFiles int) (map[string]string, error) {
	if len(edits) > maxEdits {
		return nil, fmt.Errorf("fix proposes %d edits, exceeds the cap of %d", len(edits), maxEdits)
	}
	work := make(map[string]string, len(orig))
	for k, v := range orig {
		work[k] = v
	}
	touched := map[string]bool{}
	changedLines := 0
	for _, e := range edits {
		cur, ok := work[e.File]
		if !ok {
			return nil, fmt.Errorf("edit references a file that was not fetched: %s", e.File)
		}
		if e.Old == e.New {
			continue
		}
		if e.Old == "" {
			return nil, fmt.Errorf("edit for %s has an empty anchor", e.File)
		}
		if len(e.Old) > maxEditBytes || len(e.New) > maxEditBytes {
			return nil, fmt.Errorf("edit for %s is too large (snippet exceeds %d bytes); fixes must be minimal", e.File, maxEditBytes)
		}
		// Reject a whole-file rewrite; the anchor must be a fragment.
		if strings.TrimSpace(e.Old) == strings.TrimSpace(cur) {
			return nil, fmt.Errorf("edit for %s rewrites the whole file; fixes must be minimal anchored changes", e.File)
		}
		switch strings.Count(cur, e.Old) {
		case 1:
			work[e.File] = strings.Replace(cur, e.Old, e.New, 1)
			touched[e.File] = true
			changedLines += lineCount(e.Old) + lineCount(e.New)
		case 0:
			return nil, fmt.Errorf("edit anchor not found in %s (the model's snippet did not match the file)", e.File)
		default:
			return nil, fmt.Errorf("edit anchor is ambiguous in %s (matches more than once)", e.File)
		}
	}
	if len(touched) == 0 {
		return nil, fmt.Errorf("the proposed edits made no effective change")
	}
	if changedLines > maxChangedLinesTotal {
		return nil, fmt.Errorf("fix changes ~%d lines, exceeds the cap of %d; fixes must be minimal", changedLines, maxChangedLinesTotal)
	}
	if len(touched) > maxFiles {
		return nil, fmt.Errorf("fix touches %d files, exceeds max_files %d", len(touched), maxFiles)
	}
	changed := make(map[string]string, len(touched))
	for f := range touched {
		changed[f] = work[f]
	}
	return changed, nil
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// validateSyntax checks each changed file by extension: Go is parse- and
// (best-effort) type-checked, YAML and JSON are decoded. An edit that leaves the
// file broken is dropped rather than proposed. Extensions without a validator
// are skipped.
func validateSyntax(files map[string]string) error {
	for path, content := range files {
		switch ext(strings.ToLower(path)) {
		case ".go":
			if err := checkGo(path, content); err != nil {
				return err
			}
		case ".yaml", ".yml":
			dec := yaml.NewDecoder(strings.NewReader(content))
			for {
				var doc any
				err := dec.Decode(&doc)
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("edited %s is not valid YAML: %w", path, err)
				}
			}
		case ".json":
			var doc any
			if err := json.Unmarshal([]byte(content), &doc); err != nil {
				return fmt.Errorf("edited %s is not valid JSON: %w", path, err)
			}
		}
	}
	return nil
}

// checkGo parse-checks an edited Go file, then runs a best-effort go/types
// check. It only type-checks stdlib-only files with no build constraint (by
// //go:build comment or GOOS/GOARCH filename); otherwise it stays parse-only,
// since external deps and sibling package files aren't available in this runner.
func checkGo(path, content string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("edited %s is not valid Go: %w", path, err)
	}
	if hasBuildConstraint(file) || hasFilenameConstraint(path) || importsNonStdlib(file) {
		return nil
	}
	var firstErr error
	inconclusive := false
	conf := types.Config{
		Importer: importer.Default(),
		Error: func(e error) {
			msg := e.Error()
			// Errors that stem from same-package context this single-file view
			// can't see are inconclusive, so a good fix is never dropped.
			if strings.Contains(msg, "undefined") ||
				strings.Contains(msg, "undeclared") ||
				strings.Contains(msg, "missing method") ||
				strings.Contains(msg, "ambiguous selector") ||
				strings.Contains(msg, "could not import") {
				inconclusive = true
				return
			}
			if firstErr == nil {
				firstErr = e
			}
		},
	}
	_, _ = conf.Check(file.Name.Name, fset, []*ast.File{file}, nil)
	if inconclusive {
		return nil
	}
	if firstErr != nil {
		return fmt.Errorf("edited %s has a type error: %w", path, firstErr)
	}
	return nil
}

// importsNonStdlib reports whether the file imports any package outside the
// standard library, identified by a dot in the import path's first segment.
func importsNonStdlib(file *ast.File) bool {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return true
		}
		first, _, _ := strings.Cut(p, "/")
		if strings.Contains(first, ".") {
			return true
		}
	}
	return false
}

// hasBuildConstraint reports whether the file carries a build constraint, in
// which case it may be written for a different build context than this runner's.
func hasBuildConstraint(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return true
			}
		}
	}
	return false
}

// hasFilenameConstraint reports whether the path uses a GOOS or GOARCH filename
// suffix (e.g. foo_linux.go, foo_arm64.go), Go's implicit build constraint.
func hasFilenameConstraint(p string) bool {
	name := strings.TrimSuffix(path.Base(p), ".go")
	parts := strings.Split(name, "_")
	if n := len(parts); n > 1 && parts[n-1] == "test" {
		parts = parts[:n-1]
	}
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	return knownGOOS[last] || knownGOARCH[last]
}

var knownGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
}

var knownGOARCH = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "arm64": true,
	"arm64be": true, "armbe": true, "loong64": true, "mips": true, "mips64": true,
	"mips64le": true, "mips64p32": true, "mips64p32le": true, "mipsle": true,
	"ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// renderDiff produces a simple, human-readable per-edit diff for the PR body.
func renderDiff(orig map[string]string, edits []edit) string {
	var sb strings.Builder
	for _, e := range edits {
		if e.Old == e.New {
			continue
		}
		fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", e.File, e.File)
		for _, line := range strings.Split(e.Old, "\n") {
			fmt.Fprintf(&sb, "- %s\n", line)
		}
		for _, line := range strings.Split(e.New, "\n") {
			fmt.Fprintf(&sb, "+ %s\n", line)
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// parseJSONObject extracts the first {...} object from s and unmarshals it,
// tolerating prose or code fences around the JSON.
func parseJSONObject(s string, v any) error {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return fmt.Errorf("no JSON object in response")
	}
	obj := s[start : end+1]
	if err := json.Unmarshal([]byte(obj), v); err == nil {
		return nil
	}
	// Models copying verbatim code snippets into "old"/"new" often emit literal
	// tabs and newlines inside JSON strings, which strict JSON rejects. Escape
	// raw control characters inside string literals and retry.
	return json.Unmarshal([]byte(escapeStringControlChars(obj)), v)
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

// maxCandidates caps how many real repo paths are offered to the locate step,
// keeping the prompt bounded on large repos.
const maxCandidates = 200

// candidateStopwords are generic failure-narrative words that shouldn't drive
// path matching.
var candidateStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"failure": true, "test": true, "error": true, "during": true, "when": true,
	"node": true, "pod": true, "cluster": true, "azure": true, "kubernetes": true,
	"fix": true, "ensure": true, "issue": true, "from": true, "into": true, "not": true,
	"are": true, "has": true, "was": true, "due": true, "which": true, "their": true,
}

// preferredDirHints boost config/template/manifest locations a fix usually lives
// in; noisy or generated locations are penalized.
var (
	preferredDirHints = []string{"templates/", "config/", "charts/", "manifests/", "hack/", "scripts/", "test/e2e/", "kustomize"}
	penalizedDirHints = []string{"vendor/", "third_party/", "docs/", "node_modules/", "testdata/", ".github/", "examples/"}
	preferredExts     = map[string]bool{".yaml": true, ".yml": true, ".go": true, ".sh": true, ".tpl": true, ".json": true, ".toml": true, ".cfg": true}
)

// rankCandidates returns the repo paths most relevant to the pattern, scored by
// keyword overlap with the failure text plus directory/extension preferences.
func rankCandidates(tree []string, p models.PatternAnalysis) []string {
	keywords := extractKeywords(p.SharedRootCause + " " + p.SuggestedFix + " " + p.Subject + " " + p.Summary)
	type scored struct {
		path  string
		score int
	}
	var ranked []scored
	for _, path := range tree {
		lp := strings.ToLower(path)
		keywordHits := 0
		for kw := range keywords {
			if strings.Contains(lp, kw) {
				keywordHits++
			}
		}
		dirPreferred := false
		for _, h := range preferredDirHints {
			if strings.Contains(lp, h) {
				dirPreferred = true
				break
			}
		}
		// Require a real signal (keyword or preferred dir); a matching extension
		// alone is not enough.
		if keywordHits == 0 && !dirPreferred {
			continue
		}
		score := 2 * keywordHits
		if dirPreferred {
			score++
		}
		for _, h := range penalizedDirHints {
			if strings.Contains(lp, h) {
				score -= 2
				break
			}
		}
		if preferredExts[ext(lp)] {
			score++
		}
		if score > 0 {
			ranked = append(ranked, scored{path, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].path < ranked[j].path
	})
	out := make([]string, 0, maxCandidates)
	for _, s := range ranked {
		if len(out) >= maxCandidates {
			break
		}
		out = append(out, s.path)
	}
	return out
}

// extractKeywords tokenizes failure text into distinctive lowercase terms.
func extractKeywords(text string) map[string]bool {
	kws := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(tok) < 4 || candidateStopwords[tok] {
			continue
		}
		kws[tok] = true
	}
	return kws
}

func ext(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i:]
	}
	return ""
}
