// Package fixpr drafts minimal code fixes for systemic recurring patterns and
// opens draft pull requests against the source repo via fork-and-PR. It is
// opt-in (ai.fix_prs), idempotent (a hidden marker dedupes per pattern), and
// guardrailed: draft-only, bounded file scope, a CLA-signed commit author with a
// DCO sign-off, and a dry-run mode that proposes without opening any PR.
package fixpr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// keyPrefix namespaces a fix's dedup key (one per recurring pattern + cause).
const keyPrefix = "fix-pr::"

// markerPrefix tags the hidden HTML comment embedded in every fix PR so the
// search-based dedup can find it again when local state is lost.
const markerPrefix = "prow-ai-dashboard-fix"

// prClient is the subset of *ghpr.Client the manager needs.
type prClient interface {
	OpenPR(ctx context.Context, req ghpr.Request) (string, error)
	SearchOpenPR(ctx context.Context, owner, repo, queryToken, confirmMarker string) (int, string, bool, error)
	ResolveBase(ctx context.Context, owner, repo string) (ghpr.Base, error)
}

// Options tunes the reconcile.
type Options struct {
	// SourceOwner / SourceName are the repo fix PRs target.
	SourceOwner string
	SourceName  string
	// Fork uses fork-and-PR when true, else a direct branch + same-repo PR.
	Fork bool
	// AuthorName / AuthorEmail are the CLA-signed commit author identity.
	AuthorName  string
	AuthorEmail string
	// MinConfidence is the lowest pattern confidence that qualifies.
	MinConfidence string
	// MaxFiles caps how many files a single fix may touch.
	MaxFiles int
	// MaxNewPerRun caps fix PRs opened (or previews produced) this run.
	MaxNewPerRun int
	// Labels are applied to each fix PR.
	Labels []string
	// DryRun proposes fixes without opening any PR; previews are written to
	// PreviewFile and logged.
	DryRun bool
	// PreviewFile is where dry-run previews are written (JSON). Ignored unless
	// DryRun is set.
	PreviewFile string
	// DashboardURL is linked in the PR body for context.
	DashboardURL string
	// Critique reviews the proposed change before a PR is opened; nil (or
	// CritiqueRetries 0) skips the review. May be the same Completer as
	// generation or a separate provider.
	Critique Completer
	// CritiqueRetries bounds re-prompts to resolve a reviewer's objections or a
	// validation error before the fix is dropped.
	CritiqueRetries int
	// PRFiller, when set, reformats the PR description to follow the repo's PR
	// template. A nil filler (or one that finds no template) is a pass-through.
	PRFiller PRBodyFiller
	// Verify, when set, builds and vets the proposed change in a Runtime before
	// the PR is opened, recording the verdict in the PR body and preview. nil
	// skips verification (the verdict is "skipped").
	Verify *VerifyConfig
	// Agent generates the fix with a coding-agent CLI in a real workspace clone
	// (multi-file edits, can build/test). Required: it is the fix generator.
	Agent *AgentConfig
}

// AgentConfig configures the coding-agent fix generator.
type AgentConfig struct {
	// Runtime runs the coding agent against a clone of the source repo.
	Runtime runtime.AgentRuntime
	// Model, Endpoint, and ModelToken point the agent at the model. Empty Model
	// uses the CLI default.
	Model      string
	Endpoint   string
	ModelToken string
	// MaxTurns bounds the agent loop; zero uses the CLI default.
	MaxTurns int
	// AllowBash lets the agent run build/tests while fixing.
	AllowBash bool
	// Timeout bounds the whole generation. Zero uses the Runtime default.
	Timeout time.Duration
	// GitToken authenticates the source clone. Empty clones anonymously, which
	// is enough for a public repo.
	GitToken string
}

// VerifyConfig configures pre-PR verification of a proposed fix.
type VerifyConfig struct {
	// Runtime executes the verification commands against the patched repo.
	Runtime runtime.Runtime
	// Commands each run in the checked-out, patched repo; all must pass for a
	// "passed" verdict. Defaults to `go build ./...` then `go vet ./...`.
	Commands [][]string
	// Timeout bounds each command. Zero uses the Runtime default.
	Timeout time.Duration
	// Token authenticates the clone of a private source repo. Empty clones
	// anonymously, which is enough for a public repo.
	Token string
}

// VerifyStatus is the outcome of pre-PR verification.
type VerifyStatus string

const (
	// VerifyPassed means every verification command succeeded.
	VerifyPassed VerifyStatus = "passed"
	// VerifyFailed means a command failed or timed out; the change likely does
	// not build. The draft is still produced (annotate, do not block).
	VerifyFailed VerifyStatus = "failed"
	// VerifySkipped means verification did not run (not configured, or the
	// toolchain was unavailable, or an infra error prevented it).
	VerifySkipped VerifyStatus = "skipped"
)

// VerifyResult records a verification verdict for the PR body and preview.
type VerifyResult struct {
	Status VerifyStatus `json:"status"`
	// Summary is a one-line description of what ran and its verdict.
	Summary string `json:"summary"`
	// Output is the tail of the failing command's output, empty unless failed.
	Output string `json:"output,omitempty"`
}

// PRBodyFiller reformats a PR description to follow the repo's PR template.
// repotemplate.PRFiller satisfies it. Implementations must be safe to call with
// a nil receiver and must return the input on any error.
type PRBodyFiller interface {
	FillBody(ctx context.Context, description string) string
}

// Manager reconciles systemic recurring patterns into fix PRs.
type Manager struct {
	pr        prClient
	stateFile string
	opts      Options
	state     *State
}

// State persists which patterns already have a fix PR.
type State = statefile.State[TrackedFix]

// TrackedFix records the fix PR opened for a pattern key.
type TrackedFix struct {
	URL      string `json:"url"`
	OpenedAt string `json:"opened_at"`
}

// Preview is a dry-run proposed fix (no PR opened).
type Preview struct {
	Subject   string            `json:"subject"`
	Rationale string            `json:"rationale"`
	Diff      string            `json:"diff"`
	Files     map[string]string `json:"-"`
	Verify    VerifyResult      `json:"verify"`
}

// Stats reports what a reconcile did, for logging.
type Stats struct {
	Proposed  int // PRs opened (draft mode)
	Adopted   int // existing open PR adopted
	Previewed int // dry-run previews produced
	// Failures records why a fix was not opened for a pattern. The batch path
	// logs and ignores these; on-demand callers surface the reason.
	Failures []Failure
}

// Failure is a per-pattern reason a fix could not be proposed.
type Failure struct {
	Subject string
	Reason  string
}

// NewClients builds the GitHub PR client from a token.
func NewClients(token string) *ghpr.Client {
	return ghpr.NewClient(nil, token)
}

// NewManager builds a Manager and loads prior state from stateFile if present.
func NewManager(pr prClient, stateFile string, opts Options) *Manager {
	repo := opts.SourceOwner + "/" + opts.SourceName
	return &Manager{
		pr:        pr,
		stateFile: stateFile,
		opts:      opts,
		state:     statefile.Load[TrackedFix](stateFile, repo, "fix PRs"),
	}
}

// SaveState writes the tracking state to disk.
func (m *Manager) SaveState() error {
	return m.state.Save(m.stateFile)
}

// Reconcile drafts fixes for eligible patterns. Per-pattern errors are collected
// while independent patterns continue.
func (m *Manager) Reconcile(ctx context.Context, patterns []models.PatternAnalysis) (Stats, error) {
	var stats Stats
	var previews []Preview
	var reconcileErrs []error

	work := eligible(patterns, m.opts.MinConfidence)
	if len(work) == 0 {
		return stats, nil
	}

	// Pin one upstream commit so the reads, edits, and commit share a snapshot.
	base, err := m.pr.ResolveBase(ctx, m.opts.SourceOwner, m.opts.SourceName)
	if err != nil {
		return stats, fmt.Errorf("resolving %s/%s base: %w", m.opts.SourceOwner, m.opts.SourceName, err)
	}
	gen := func(ctx context.Context, p models.PatternAnalysis) (*proposedFix, error) {
		return m.generate(ctx, p, base.HeadSHA, "")
	}

	for _, p := range work {
		key := keyFor(p)

		// Dry-run: propose without GitHub writes or state, capped per run.
		if m.opts.DryRun {
			if stats.Previewed >= m.opts.MaxNewPerRun {
				break
			}
			fix, err := gen(ctx, p)
			if err != nil {
				log.Printf("  ⚠ fix generation failed for %q: %v", p.Subject, err)
				stats.Failures = append(stats.Failures, Failure{Subject: p.Subject, Reason: err.Error()})
				reconcileErrs = append(reconcileErrs, fmt.Errorf("generate preview for %q: %w", p.Subject, err))
				continue
			}
			v := m.verify(ctx, base, fix.files)
			previews = append(previews, Preview{Subject: p.Subject, Rationale: fix.rationale, Diff: fix.diff, Files: fix.files, Verify: v})
			stats.Previewed++
			log.Printf("  🧪 fix preview for %q (%d file(s), verify=%s):\n%s", p.Subject, len(fix.files), v.Status, fix.diff)
			continue
		}

		if _, tracked := m.state.Tracked[key]; tracked {
			continue
		}
		// A prior run may have an open fix PR even if local state is lost.
		if _, url, found, err := m.pr.SearchOpenPR(ctx, m.opts.SourceOwner, m.opts.SourceName, markerToken(key), markerFor(key)); err != nil {
			log.Printf("  ⚠ fix-PR search failed for %s: %v", key, err)
			stats.Failures = append(stats.Failures, Failure{Subject: p.Subject, Reason: err.Error()})
			reconcileErrs = append(reconcileErrs, fmt.Errorf("search fix PR for %q: %w", p.Subject, err))
			continue
		} else if found {
			m.state.Tracked[key] = TrackedFix{URL: url, OpenedAt: now()}
			stats.Adopted++
			log.Printf("  🔗 adopted existing fix PR for %q", p.Subject)
			continue
		}

		if stats.Proposed >= m.opts.MaxNewPerRun {
			log.Printf("  ⓘ fix-PR cap (%d) reached; deferring %q to next run", m.opts.MaxNewPerRun, p.Subject)
			continue
		}

		fix, err := gen(ctx, p)
		if err != nil {
			log.Printf("  ⚠ fix generation failed for %q: %v", p.Subject, err)
			stats.Failures = append(stats.Failures, Failure{Subject: p.Subject, Reason: err.Error()})
			reconcileErrs = append(reconcileErrs, fmt.Errorf("generate fix for %q: %w", p.Subject, err))
			continue
		}

		// Reformat the description to follow the repo PR template when configured.
		v := m.verify(ctx, base, fix.files)
		_, body := m.renderBody(ctx, p, fix, v, key)

		url, err := m.openPR(ctx, prTitle(p), body, fix.files, base)
		if url == "" {
			log.Printf("  ⚠ failed to open fix PR for %q: %v", p.Subject, err)
			stats.Failures = append(stats.Failures, Failure{Subject: p.Subject, Reason: "opening the pull request failed"})
			if err == nil {
				err = fmt.Errorf("empty pull request URL")
			}
			reconcileErrs = append(reconcileErrs, fmt.Errorf("open fix PR for %q: %w", p.Subject, err))
			continue
		}
		if err != nil {
			// PR opened but a follow-up (e.g. labeling) failed; still track it.
			log.Printf("  ⚠ fix PR opened with a warning for %q: %v", p.Subject, err)
			reconcileErrs = append(reconcileErrs, fmt.Errorf("finish fix PR for %q: %w", p.Subject, err))
		}
		m.state.Tracked[key] = TrackedFix{URL: url, OpenedAt: now()}
		stats.Proposed++
		log.Printf("  🛠️ opened draft fix PR for %q: %s", p.Subject, url)
	}

	if m.opts.DryRun && len(previews) > 0 && m.opts.PreviewFile != "" {
		if err := writePreviews(m.opts.PreviewFile, previews); err != nil {
			log.Printf("Warning: failed to write fix previews: %v", err)
			reconcileErrs = append(reconcileErrs, fmt.Errorf("write fix previews: %w", err))
		}
	}
	return stats, errors.Join(reconcileErrs...)
}

// generate runs the fix generation for one pattern against ref. instruction is
// an optional maintainer directive that steers the edit; empty for the batch
// path.
func (m *Manager) generate(ctx context.Context, p models.PatternAnalysis, ref, instruction string) (*proposedFix, error) {
	return generateWithAgent(ctx, genParams{
		critique:        m.opts.Critique,
		owner:           m.opts.SourceOwner,
		repo:            m.opts.SourceName,
		ref:             ref,
		maxFiles:        m.opts.MaxFiles,
		critiqueRetries: m.opts.CritiqueRetries,
		instruction:     instruction,
		agent:           m.opts.Agent,
	}, p)
}

// verify builds and vets the proposed change against the pinned base in the
// configured Runtime. It never blocks: a build failure yields a "failed"
// verdict (annotated on the draft), and an unavailable toolchain or infra error
// yields "skipped". Verification runs once, at generation time.
func (m *Manager) verify(ctx context.Context, base ghpr.Base, files map[string]string) VerifyResult {
	if m.opts.Verify == nil || m.opts.Verify.Runtime == nil {
		return VerifyResult{Status: VerifySkipped, Summary: "verification not configured"}
	}
	cmds := m.opts.Verify.Commands
	if len(cmds) == 0 {
		cmds = defaultVerifyCommands
	}
	repo := runtime.RepoRef{Owner: m.opts.SourceOwner, Name: m.opts.SourceName, Ref: base.HeadSHA, Token: m.opts.Verify.Token}
	var ran []string
	for _, cmd := range cmds {
		label := strings.Join(cmd, " ")
		res, err := m.opts.Verify.Runtime.Run(ctx, runtime.Spec{
			Repo: repo, Overlay: files, Command: cmd, Timeout: m.opts.Verify.Timeout,
		})
		if errors.Is(err, runtime.ErrUnavailable) {
			return VerifyResult{Status: VerifySkipped, Summary: "verification skipped: toolchain unavailable"}
		}
		if err != nil {
			return VerifyResult{Status: VerifySkipped, Summary: "verification skipped: " + oneLine(err.Error())}
		}
		if res.TimedOut {
			return VerifyResult{Status: VerifyFailed, Summary: label + " timed out", Output: res.Output}
		}
		if !res.Passed() {
			return VerifyResult{Status: VerifyFailed, Summary: label + " failed", Output: res.Output}
		}
		ran = append(ran, label)
	}
	return VerifyResult{Status: VerifyPassed, Summary: strings.Join(ran, " and ") + " passed"}
}

// defaultVerifyCommands compile-check and vet a Go repo, catching the common
// failure modes of a model-generated diff: it does not build, uses a
// hallucinated API, or breaks a signature.
var defaultVerifyCommands = [][]string{
	{"go", "build", "./..."},
	{"go", "vet", "./..."},
}

// renderBody builds the final PR description (reformatted to follow the repo PR
// template when a filler is configured) and the full PR body that embeds it.
func (m *Manager) renderBody(ctx context.Context, p models.PatternAnalysis, fix *proposedFix, v VerifyResult, key string) (description, body string) {
	description = prDescription(p, fix)
	if m.opts.PRFiller != nil {
		description = m.opts.PRFiller.FillBody(ctx, description)
	}
	return description, prBody(p, fix, v, key, m.opts.DashboardURL, description)
}

// openPR opens a draft fix PR with the given title, body, and files against the
// pinned base.
func (m *Manager) openPR(ctx context.Context, title, body string, files map[string]string, base ghpr.Base) (string, error) {
	return m.pr.OpenPR(ctx, ghpr.Request{
		Owner:        m.opts.SourceOwner,
		Repo:         m.opts.SourceName,
		Files:        files,
		BranchPrefix: "ai-fix",
		Title:        title,
		Body:         body,
		Draft:        true,
		Fork:         m.opts.Fork,
		Base:         &base,
		Labels:       m.opts.Labels,
		AuthorName:   m.opts.AuthorName,
		AuthorEmail:  m.opts.AuthorEmail,
		SignOff:      true,
	})
}

// GeneratedFix is an in-memory, ready-to-open fix produced by GeneratePreview.
// A caller previews Title/Description/Diff, then passes the value back to
// OpenFromPreview to open exactly the previewed PR. The base is pinned at
// generation time so confirm opens the same diff against the same snapshot.
type GeneratedFix struct {
	Preview     Preview // Subject, Rationale, Diff, Files, Verify
	Title       string  // final PR title
	Description string  // PR description (after any repo-template reformat)
	Body        string  // full PR body that embeds Description + diff + marker

	pattern models.PatternAnalysis
	key     string
	base    ghpr.Base
}

// GeneratedFixSnapshot is the serializable form of a generated fix. It keeps
// the exact files and pinned base needed to open the reviewed draft later.
type GeneratedFixSnapshot struct {
	Subject     string                 `json:"subject"`
	Rationale   string                 `json:"rationale"`
	Diff        string                 `json:"diff"`
	Files       map[string]string      `json:"files"`
	Verify      VerifyResult           `json:"verify"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	Body        string                 `json:"body"`
	Pattern     models.PatternAnalysis `json:"pattern"`
	Key         string                 `json:"key"`
	Base        ghpr.Base              `json:"base"`
}

// Snapshot returns a deep-copy serializable representation of gf.
func (gf *GeneratedFix) Snapshot() *GeneratedFixSnapshot {
	if gf == nil {
		return nil
	}
	files := make(map[string]string, len(gf.Preview.Files))
	for path, content := range gf.Preview.Files {
		files[path] = content
	}
	return &GeneratedFixSnapshot{
		Subject: gf.Preview.Subject, Rationale: gf.Preview.Rationale,
		Diff: gf.Preview.Diff, Files: files, Verify: gf.Preview.Verify,
		Title: gf.Title, Description: gf.Description, Body: gf.Body,
		Pattern: gf.pattern, Key: gf.key, Base: gf.base,
	}
}

// RestoreGeneratedFix rebuilds the opaque generated fix from a persisted snapshot.
func RestoreGeneratedFix(snapshot *GeneratedFixSnapshot) *GeneratedFix {
	if snapshot == nil {
		return nil
	}
	files := make(map[string]string, len(snapshot.Files))
	for path, content := range snapshot.Files {
		files[path] = content
	}
	return &GeneratedFix{
		Preview: Preview{Subject: snapshot.Subject, Rationale: snapshot.Rationale,
			Diff: snapshot.Diff, Files: files, Verify: snapshot.Verify},
		Title: snapshot.Title, Description: snapshot.Description, Body: snapshot.Body,
		pattern: snapshot.Pattern, key: snapshot.Key, base: snapshot.Base,
	}
}

// GeneratePreview generates a fix for one pattern and renders the exact PR title
// and body without opening anything. instruction is an optional maintainer
// directive that steers the edit. The returned *GeneratedFix is opaque to
// callers and is passed back to OpenFromPreview to open the PR.
func (m *Manager) GeneratePreview(ctx context.Context, p models.PatternAnalysis, instruction string) (*GeneratedFix, error) {
	// Apply the same eligibility gate as the batch path so an on-demand preview
	// cannot draft a fix for a failure the engine would not consider actionable
	// (non-systemic or without a suggested fix).
	if len(eligible([]models.PatternAnalysis{p}, m.opts.MinConfidence)) == 0 {
		return nil, fmt.Errorf("this failure is not auto-fixable (needs a systemic pattern with a suggested fix)")
	}
	base, err := m.pr.ResolveBase(ctx, m.opts.SourceOwner, m.opts.SourceName)
	if err != nil {
		return nil, fmt.Errorf("resolving %s/%s base: %w", m.opts.SourceOwner, m.opts.SourceName, err)
	}
	fix, err := m.generate(ctx, p, base.HeadSHA, instruction)
	if err != nil {
		return nil, err
	}
	key := keyFor(p)
	v := m.verify(ctx, base, fix.files)
	description, body := m.renderBody(ctx, p, fix, v, key)
	return &GeneratedFix{
		Preview:     Preview{Subject: p.Subject, Rationale: fix.rationale, Diff: fix.diff, Files: fix.files, Verify: v},
		Title:       prTitle(p),
		Description: description,
		Body:        body,
		pattern:     p,
		key:         key,
		base:        base,
	}, nil
}

// OpenFromPreview opens the draft PR for a previously generated fix, applying
// the same dedup guard as Reconcile: skip if the pattern is already tracked or
// an open fix PR already exists. It returns the PR URL and records tracking
// state; the caller saves state.
func (m *Manager) OpenFromPreview(ctx context.Context, gf *GeneratedFix) (string, error) {
	if gf == nil {
		return "", fmt.Errorf("no generated fix to open")
	}
	key := gf.key
	if t, tracked := m.state.Tracked[key]; tracked {
		return t.URL, nil
	}
	if _, url, found, err := m.pr.SearchOpenPR(ctx, m.opts.SourceOwner, m.opts.SourceName, markerToken(key), markerFor(key)); err != nil {
		return "", fmt.Errorf("fix-PR search failed: %w", err)
	} else if found {
		m.state.Tracked[key] = TrackedFix{URL: url, OpenedAt: now()}
		return url, nil
	}
	url, err := m.openPR(ctx, gf.Title, gf.Body, gf.Preview.Files, gf.base)
	if url == "" {
		return "", fmt.Errorf("opening the pull request failed")
	}
	if err != nil {
		// PR opened but a follow-up (e.g. labeling) failed; still track it.
		log.Printf("  ⚠ fix PR opened with a warning for %q: %v", gf.pattern.Subject, err)
	}
	m.state.Tracked[key] = TrackedFix{URL: url, OpenedAt: now()}
	return url, nil
}

// eligible filters to systemic patterns at or above minConfidence that carry a
// concrete suggested fix, ranked highest-confidence first.
func eligible(patterns []models.PatternAnalysis, minConfidence string) []models.PatternAnalysis {
	floor := confidenceRank(minConfidence)
	var out []models.PatternAnalysis
	for _, p := range patterns {
		if !p.Systemic || strings.TrimSpace(p.SuggestedFix) == "" {
			continue
		}
		if confidenceRank(p.Confidence) < floor {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return confidenceRank(out[i].Confidence) > confidenceRank(out[j].Confidence)
	})
	return out
}

// keyFor is the dedup identity of a pattern: the job plus a fingerprint of the
// shared root cause, so distinct causes on one job dedupe separately.
func keyFor(p models.PatternAnalysis) string {
	job := p.JobID
	if strings.TrimSpace(job) == "" {
		job = p.Subject
	}
	cause := oneLine(strings.ToLower(p.SharedRootCause))
	sum := sha256.Sum256([]byte(cause))
	return keyPrefix + job + "::" + hex.EncodeToString(sum[:6])
}

func markerFor(key string) string {
	return fmt.Sprintf("<!-- %s:%s -->", markerPrefix, markerToken(key))
}

func markerToken(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func prTitle(p models.PatternAnalysis) string {
	subj := strings.TrimSpace(p.Subject)
	if subj == "" {
		subj = "a recurring CI failure"
	}
	return "fix: address recurring failure in " + subj
}

func prBody(p models.PatternAnalysis, fix *proposedFix, v VerifyResult, key, dashboardURL, description string) string {
	var sb strings.Builder
	sb.WriteString("> [!WARNING]\n> Draft PR auto-proposed by a CI failure-analysis dashboard. Review carefully before use; the change is a starting point, not a merge-ready fix.\n\n")
	sb.WriteString(verifyBanner(v))
	sb.WriteString(strings.TrimSpace(description))
	sb.WriteString("\n\n")
	sb.WriteString("<details><summary>Proposed diff</summary>\n\n```diff\n")
	sb.WriteString(fix.diff)
	sb.WriteString("\n```\n</details>\n")
	if strings.TrimSpace(v.Output) != "" {
		sb.WriteString("\n<details><summary>Verification output</summary>\n\n```\n")
		sb.WriteString(strings.TrimSpace(v.Output))
		sb.WriteString("\n```\n</details>\n")
	}
	if dashboardURL != "" {
		fmt.Fprintf(&sb, "\nDashboard: %s\n", dashboardURL)
	}
	fmt.Fprintf(&sb, "\n%s\n", markerFor(key))
	return sb.String()
}

// verifyBanner renders the verification verdict line for the PR body. Passed and
// failed are stated plainly; skipped is silent to avoid noise on deployments
// without a verification toolchain.
func verifyBanner(v VerifyResult) string {
	switch v.Status {
	case VerifyPassed:
		return fmt.Sprintf("> [!NOTE]\n> Automated verification passed: %s.\n\n", oneLine(v.Summary))
	case VerifyFailed:
		return fmt.Sprintf("> [!CAUTION]\n> Automated verification failed: %s. The proposed change likely does not build as-is; treat it as a lead, not a fix.\n\n", oneLine(v.Summary))
	default:
		return ""
	}
}

// prDescription is the human-readable summary of the fix. It is the part that
// gets reformatted to follow a repo PR template when one is configured.
func prDescription(p models.PatternAnalysis, fix *proposedFix) string {
	var sb strings.Builder
	if r := strings.TrimSpace(fix.rationale); r != "" {
		fmt.Fprintf(&sb, "**Proposed change:** %s\n\n", oneLine(r))
	}
	fmt.Fprintf(&sb, "**Recurring failure:** %s\n", p.Subject)
	if c := strings.TrimSpace(p.SharedRootCause); c != "" {
		fmt.Fprintf(&sb, "**Shared root cause:** %s\n", oneLine(c))
	}
	fmt.Fprintf(&sb, "**Builds analyzed:** %d (confidence: %s)\n\n", p.BuildsAnalyzed, p.Confidence)
	sb.WriteString("**Before merging, a human must:**\n")
	sb.WriteString("- Verify the change actually fixes the root cause (run the affected job).\n")
	sb.WriteString("- Confirm it follows the project's conventions and doesn't regress other flavors.")
	return sb.String()
}

// confidenceRank orders verdict confidences. Unknown strings rank lowest.
func confidenceRank(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func writePreviews(path string, previews []Preview) error {
	return statefile.WriteJSON(path, previews)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
