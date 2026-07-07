package skillsuggest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// keyPrefix namespaces suggestion dedup keys.
const keyPrefix = "skill-suggest::"

// markerPrefix tags the hidden HTML comment embedded in every suggestion PR so
// the search-based dedup can find it again when local state is lost.
const markerPrefix = "prow-ai-dashboard-skill"

// prClient is the subset of *ghpr.Client the manager needs.
type prClient interface {
	OpenPR(ctx context.Context, req ghpr.Request) (string, error)
	SearchOpenPR(ctx context.Context, owner, repo, queryToken, confirmMarker string) (int, string, bool, error)
}

// Options tunes the reconcile.
type Options struct {
	// MinConfidence is the lowest pattern confidence that qualifies.
	MinConfidence string
	// MaxNewPerRun caps suggestion PRs opened this run.
	MaxNewPerRun int
	// Labels are applied to each suggestion PR.
	Labels []string
	// DashboardURL is linked in the PR body for context.
	DashboardURL string
}

// Manager reconciles systemic recurring patterns into skill-suggestion PRs.
type Manager struct {
	pr        prClient
	completer Completer
	existing  *skills.Set
	owner     string
	repo      string
	stateFile string
	opts      Options
	state     *State
}

// State persists suggestion PRs so open suggestions are not re-proposed.
type State = statefile.State[TrackedPR]

// TrackedPR records the suggestion PR opened for a pattern key.
type TrackedPR struct {
	URL      string `json:"url"`
	OpenedAt string `json:"opened_at"`
}

// Stats reports what a reconcile did, for logging.
type Stats struct {
	Suggested int
	Adopted   int
	Covered   int
}

// NewManager builds a Manager and loads prior state from stateFile if present.
func NewManager(pr prClient, completer Completer, existing *skills.Set, owner, repo, stateFile string, opts Options) *Manager {
	return &Manager{
		pr:        pr,
		completer: completer,
		existing:  existing,
		owner:     owner,
		repo:      repo,
		stateFile: stateFile,
		opts:      opts,
		state:     statefile.Load[TrackedPR](stateFile, owner+"/"+repo, "skill suggestions"),
	}
}

// SaveState writes the tracking state to disk.
func (m *Manager) SaveState() error {
	return m.state.Save(m.stateFile)
}

// Reconcile suggests skill recipes for eligible patterns. Per-pattern errors are
// logged and skipped; the run is best-effort.
func (m *Manager) Reconcile(ctx context.Context, patterns []models.PatternAnalysis) (Stats, error) {
	var stats Stats

	existing := m.existingSkills()
	existingIDs := make([]string, 0, len(existing))
	for _, e := range existing {
		existingIDs = append(existingIDs, e.ID)
	}

	for _, p := range eligible(patterns, m.opts.MinConfidence) {
		key := keyFor(p)
		if _, tracked := m.state.Tracked[key]; tracked {
			continue // already suggested
		}

		// Check whether an existing skill already covers this pattern.
		matched := m.triggerMatches(p)
		covered, reason, err := coveredCheck(ctx, m.completer, existing, matched, p)
		if err != nil {
			log.Printf("  ⚠ skill covered-check failed for %s: %v", p.Subject, err)
			continue
		}
		if covered {
			stats.Covered++
			log.Printf("  ⓘ pattern %q already covered by a skill (%s); skipping", p.Subject, reason)
			continue
		}

		// A prior run may have an open suggestion PR even if local state is lost.
		if url, found, err := m.searchOpen(ctx, key); err != nil {
			log.Printf("  ⚠ suggestion PR search failed for %s: %v", key, err)
			continue
		} else if found {
			m.state.Tracked[key] = TrackedPR{URL: url, OpenedAt: now()}
			stats.Adopted++
			log.Printf("  🔗 adopted existing suggestion PR for %q", p.Subject)
			continue
		}

		if stats.Suggested >= m.opts.MaxNewPerRun {
			log.Printf("  ⓘ suggestion cap (%d) reached; deferring %q to next run", m.opts.MaxNewPerRun, p.Subject)
			continue
		}

		id, recipe, err := generateRecipe(ctx, m.completer, p, existingIDs)
		if err != nil {
			log.Printf("  ⚠ skill recipe generation failed for %q: %v", p.Subject, err)
			continue
		}

		url, err := m.pr.OpenPR(ctx, ghpr.Request{
			Owner:        m.owner,
			Repo:         m.repo,
			Files:        map[string]string{"skills/" + id + ".yaml": recipe},
			BranchPrefix: "skill-suggest",
			Title:        "Add skill recipe: " + id,
			Body:         prBody(p, id, key, m.opts.DashboardURL),
			Draft:        true,
			Labels:       m.opts.Labels,
		})
		if url == "" {
			// Retry next run if the PR did not open.
			log.Printf("  ⚠ failed to open suggestion PR for %q: %v", p.Subject, err)
			continue
		}
		if err != nil {
			// Track opened PRs even when a follow-up such as labeling failed.
			log.Printf("  ⚠ suggestion PR opened with a warning for %q: %v", p.Subject, err)
		}
		m.state.Tracked[key] = TrackedPR{URL: url, OpenedAt: now()}
		existingIDs = append(existingIDs, id) // avoid id collisions within the run
		stats.Suggested++
		log.Printf("  🧩 opened skill-suggestion PR for %q: %s", p.Subject, url)
	}
	return stats, nil
}

func (m *Manager) searchOpen(ctx context.Context, key string) (string, bool, error) {
	_, url, found, err := m.pr.SearchOpenPR(ctx, m.owner, m.repo, markerToken(key), markerFor(key))
	return url, found, err
}

// existingSkills projects the loaded Set into the minimal covered-check view.
func (m *Manager) existingSkills() []existingSkill {
	if m.existing == nil {
		return nil
	}
	var out []existingSkill
	for _, s := range m.existing.Skills() {
		out = append(out, existingSkill{ID: s.ID, Description: s.Description, Triggers: s.Triggers})
	}
	return out
}

// triggerMatches returns the ids of existing skills whose triggers fire on the
// pattern text before the covered-check.
func (m *Manager) triggerMatches(p models.PatternAnalysis) []string {
	if m.existing == nil {
		return nil
	}
	text := p.SharedRootCause + "\n" + p.Summary
	var ids []string
	for _, s := range m.existing.Match(text) {
		ids = append(ids, s.ID)
	}
	return ids
}

// eligible filters to systemic patterns at or above minConfidence that carry a
// concrete root cause, ranked highest-confidence first for a stable cap order.
func eligible(patterns []models.PatternAnalysis, minConfidence string) []models.PatternAnalysis {
	floor := confidenceRank(minConfidence)
	var out []models.PatternAnalysis
	for _, p := range patterns {
		if !p.Systemic || strings.TrimSpace(p.SharedRootCause) == "" {
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

// keyFor is the stable dedup identity: job plus shared-root-cause fingerprint.
// Distinct causes on the same job get separate suggestions.
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

func prBody(p models.PatternAnalysis, id, key, dashboardURL string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Auto-drafted skill recipe `skills/%s.yaml` for a recurring failure pattern the dashboard keeps seeing.\n\n", id)
	fmt.Fprintf(&sb, "**Pattern:** %s\n", p.Subject)
	if c := strings.TrimSpace(p.SharedRootCause); c != "" {
		fmt.Fprintf(&sb, "**Shared root cause:** %s\n", oneLine(c))
	}
	fmt.Fprintf(&sb, "**Builds analyzed:** %d (confidence: %s)\n\n", p.BuildsAnalyzed, p.Confidence)
	sb.WriteString("**Before merging, a human must:**\n")
	sb.WriteString("- Review the triggers (do they match this failure class without over-firing?).\n")
	sb.WriteString("- Review required_evidence paths against the real artifact tree.\n")
	sb.WriteString("- Tighten the procedure so it directs the agent to cite real logs.\n")
	if dashboardURL != "" {
		fmt.Fprintf(&sb, "\nDashboard: %s\n", dashboardURL)
	}
	fmt.Fprintf(&sb, "\n%s\n", markerFor(key))
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

func now() string { return time.Now().UTC().Format(time.RFC3339) }
