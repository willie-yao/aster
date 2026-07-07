// Package issues opens and maintains GitHub issues for the dashboard's
// highest-signal findings: systemic recurring patterns and persistent test
// failures. It is opt-in through project.yaml `issues:` and ISSUE_TOKEN, and
// idempotent: each tracked finding carries a hidden marker so the same issue is
// reused across runs, and recovered findings get a closing comment.
package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// markerPrefix tags the hidden HTML comment embedded in every filed issue. The
// per-key token after it lets the search-based dedup find the issue again.
const markerPrefix = "prow-ai-dashboard-key"

// Key prefixes namespace finding kinds so recovery can be scoped to triggers
// evaluated by this fetch.
const (
	KeyPrefixPattern    = "pattern::"
	KeyPrefixPersistent = "persistent::"
)

// RecoverPrefixesFor maps enabled trigger names to the key prefixes whose
// tracked issues may be recovered this run. A finding kind that isn't enabled
// not evaluated is left untouched rather than wrongly marked recovered.
func RecoverPrefixesFor(triggers []string) []string {
	var out []string
	for _, t := range triggers {
		switch t {
		case "patterns":
			out = append(out, KeyPrefixPattern)
		case "persistent":
			out = append(out, KeyPrefixPersistent)
		}
	}
	return out
}

// IssueSpec is the desired issue for one finding.
type IssueSpec struct {
	// Key is the stable dedup identity of the finding.
	Key    string
	Title  string
	Body   string
	Labels []string
}

func markerFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("<!-- %s:%s -->", markerPrefix, hex.EncodeToString(sum[:8]))
}

// markerToken returns the hex token used for GitHub body search.
func markerToken(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// State persists filed issues so an active tracked finding needs no API calls.
type State = statefile.State[TrackedIssue]

// TrackedIssue records the issue filed for a finding key.
type TrackedIssue struct {
	Number       int    `json:"number"`
	URL          string `json:"url"`
	FirstFiledAt string `json:"first_filed_at"`
}

// gh is the subset of the GitHub client the manager needs.
type gh interface {
	SearchOpenIssue(ctx context.Context, queryToken, confirmMarker string) (int, string, bool, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, string, error)
	CommentIssue(ctx context.Context, number int, body string) error
	CloseIssue(ctx context.Context, number int) error
}

// Options tunes recovery behavior and the per-run create cap.
type Options struct {
	CommentOnRecovery bool
	CloseOnRecovery   bool
	MaxNewPerRun      int
	// RecoverPrefixes limits which key prefixes may be recovered this run.
	// A tracked key outside the set is left as-is so disabled or skipped
	// triggers never wrongly resolve their issues.
	RecoverPrefixes []string
	// TemplateFiller, when set, reformats a new issue's title and body to follow
	// the repo's issue template before filing. A nil filler (or one that finds
	// no template) is a pass-through.
	TemplateFiller TemplateFiller
}

// TemplateFiller reformats an issue's title and body to follow the repo's issue
// template. repotemplate.IssueFiller satisfies it. Implementations must be safe
// to call with a nil receiver and must return the input on any error.
type TemplateFiller interface {
	FillIssue(ctx context.Context, title, body string) (string, string)
}

// Manager reconciles the current set of findings against tracked issues.
type Manager struct {
	client     gh
	state      *State
	stateFile  string
	targetRepo string
	opts       Options
}

// Stats reports what a reconcile did, for logging.
type Stats struct {
	Created   int
	Adopted   int
	Recovered int
}

// TrackedURL returns the issue URL recorded for a finding key, if one has been
// filed or adopted. Used by on-demand callers to report the result after
// Reconcile.
func (m *Manager) TrackedURL(key string) (string, bool) {
	t, ok := m.state.Tracked[key]
	return t.URL, ok
}

// NewManager builds a Manager and loads prior state from stateFile if present.
// targetRepo scopes state by owner/name so issue numbers are never mixed
// across repos.
func NewManager(client gh, stateFile, targetRepo string, opts Options) *Manager {
	return &Manager{
		client:     client,
		stateFile:  stateFile,
		targetRepo: targetRepo,
		opts:       opts,
		state:      statefile.Load[TrackedIssue](stateFile, targetRepo, "issues"),
	}
}

// SaveState writes the tracking state to disk.
func (m *Manager) SaveState() error {
	return m.state.Save(m.stateFile)
}

// Reconcile files issues for new findings, adopts a pre-existing open issue when
// local state was lost, and comments/closes issues whose finding has recovered.
// Per-finding API errors are logged and skipped; the run is best-effort.
func (m *Manager) Reconcile(ctx context.Context, specs []IssueSpec) (Stats, error) {
	var stats Stats

	current := make(map[string]IssueSpec, len(specs))
	for _, s := range specs {
		current[s.Key] = s
	}

	for key, spec := range current {
		if _, tracked := m.state.Tracked[key]; tracked {
			continue // already has an issue
		}
		// Local state doesn't know this finding: it may still have an open
		// issue from a prior run whose state was lost. Search before creating.
		if num, urlStr, found, err := m.client.SearchOpenIssue(ctx, markerToken(key), markerFor(key)); err != nil {
			log.Printf("  ⚠ issue search failed for %s: %v", key, err)
			continue
		} else if found {
			m.state.Tracked[key] = TrackedIssue{Number: num, URL: urlStr, FirstFiledAt: now()}
			stats.Adopted++
			log.Printf("  🔗 adopted existing issue #%d for %s", num, key)
			continue
		}
		if stats.Created >= m.opts.MaxNewPerRun {
			log.Printf("  ⓘ issue create cap (%d) reached; deferring %s to next run", m.opts.MaxNewPerRun, key)
			continue
		}
		title, body := spec.Title, spec.Body
		if m.opts.TemplateFiller != nil {
			// Reformat to follow the repo issue template, then guarantee the
			// dedup marker survives so tracking and adoption keep working.
			marker := markerFor(key)
			title, body = m.opts.TemplateFiller.FillIssue(ctx, title, strings.ReplaceAll(body, marker, ""))
			title = clampTitle(title)
			if !strings.Contains(body, marker) {
				body = strings.TrimRight(body, "\n") + "\n\n" + marker
			}
		}
		num, urlStr, err := m.client.CreateIssue(ctx, title, body, spec.Labels)
		if err != nil {
			log.Printf("  ⚠ failed to create issue for %s: %v", key, err)
			continue
		}
		m.state.Tracked[key] = TrackedIssue{Number: num, URL: urlStr, FirstFiledAt: now()}
		stats.Created++
		log.Printf("  📝 filed issue #%d for %s", num, key)
	}

	// Recover tracked findings that are absent from trigger namespaces evaluated
	// by this run.
	for key, tracked := range m.state.Tracked {
		if _, stillActive := current[key]; stillActive {
			continue
		}
		if !recoverable(key, m.opts.RecoverPrefixes) {
			continue
		}
		if m.opts.CommentOnRecovery {
			if err := m.client.CommentIssue(ctx, tracked.Number, recoveryComment()); err != nil {
				log.Printf("  ⚠ failed to comment recovery on #%d (%s): %v", tracked.Number, key, err)
				continue // keep tracking so we retry next run
			}
		}
		if m.opts.CloseOnRecovery {
			if err := m.client.CloseIssue(ctx, tracked.Number); err != nil {
				log.Printf("  ⚠ failed to close #%d (%s): %v", tracked.Number, key, err)
				continue
			}
		}
		delete(m.state.Tracked, key)
		stats.Recovered++
		log.Printf("  ✅ marked issue #%d recovered for %s", tracked.Number, key)
	}

	return stats, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// recoverable reports whether key's prefix is in the enabled set. An empty set
// recovers nothing.
func recoverable(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func recoveryComment() string {
	return "✅ This failure has not recurred in the most recent builds, so the dashboard now considers it recovered. " +
		"_(managed by prow-ai-dashboard)_"
}
