// Package actions performs single-failure GitHub actions on demand: filing an
// issue or drafting a fix PR for one specific pattern, using a per-user token.
// It reuses the batch issue and fix-PR engines for exactly one item, so the
// on-demand and scheduled paths stay behaviorally identical.
package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
)

// ErrNotFound means no pattern in the published data matched the given id.
var ErrNotFound = errors.New("failure not found")

// AIConfig is the resolved chat-completions configuration used to draft fixes.
type AIConfig struct {
	Token    string
	Endpoint string
	Model    string
	Headers  map[string]string
}

// Service runs on-demand actions against the data written to DataDir. It reads
// jobs/*.json to resolve a failure id and reuses the issue and fix-PR state
// files alongside them. A mutex serializes state read-modify-write so
// concurrent admin requests to one server do not clobber each other; cross-
// process consistency with the fetcher/worker relies on the engines' adopt-by-
// search path, which recovers when local state is stale.
type Service struct {
	cfg     *project.Config
	dataDir string
	ai      AIConfig
	mu      sync.Mutex
}

// NewService builds a Service. dataDir is the fetcher output directory holding
// jobs/*.json and the *_state.json files.
func NewService(cfg *project.Config, dataDir string, ai AIConfig) *Service {
	return &Service{cfg: cfg, dataDir: dataDir, ai: ai}
}

// aiCompleter returns an AI client for template reformatting when AI is fully
// configured, else nil (which makes the fillers a pass-through).
func (s *Service) aiCompleter() repotemplate.Completer {
	if s.ai.Endpoint == "" || s.ai.Model == "" || s.ai.Token == "" {
		return nil
	}
	return ai.NewClientWithOptions(ai.Options{
		Token:        s.ai.Token,
		Endpoint:     s.ai.Endpoint,
		Model:        s.ai.Model,
		ExtraHeaders: s.ai.Headers,
	})
}

// findPattern resolves a failure id to its PatternAnalysis by scanning the
// published per-job details.
func (s *Service) findPattern(id string) (*models.PatternAnalysis, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	jobsDir := filepath.Join(s.dataDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return nil, fmt.Errorf("reading job details: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, e.Name()))
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(data, &detail) != nil {
			continue
		}
		for i := range detail.PatternAnalyses {
			if detail.PatternAnalyses[i].ID != "" && detail.PatternAnalyses[i].ID == id {
				return &detail.PatternAnalyses[i], nil
			}
		}
	}
	return nil, ErrNotFound
}

// CreateIssue files a single GitHub issue for the failure using userToken and
// returns the issue URL. The user's token attributes the issue to them.
func (s *Service) CreateIssue(ctx context.Context, failureID, userToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pa, err := s.findPattern(failureID)
	if err != nil {
		return "", err
	}

	eff := s.cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
	}

	// Force the patterns trigger: the admin explicitly asked to file this, so
	// the project's configured triggers do not gate the on-demand action.
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{*pa}}
	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		Triggers:     []string{project.IssueTriggerPatterns},
		Labels:       eff.Labels,
		DashboardURL: s.cfg.Branding.SiteURL,
	})
	if len(specs) == 0 {
		return "", fmt.Errorf("failure %s is not an actionable systemic pattern", failureID)
	}

	client := issues.NewClient(userToken, eff.Repo.Owner, eff.Repo.Name)
	targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
	// When AI is available, reformat the issue body to follow the target repo's
	// issue template; nil filler leaves the default body untouched.
	var filler issues.TemplateFiller
	if c := s.aiCompleter(); c != nil {
		filler = repotemplate.NewIssueFiller(userToken, c, eff.Repo.Owner, eff.Repo.Name)
	}
	// RecoverPrefixes is deliberately empty: a single on-demand create must only
	// create or adopt this one spec. A non-empty set would make Reconcile treat
	// every other tracked issue as recovered and comment/close it.
	mgr := issues.NewManager(client, filepath.Join(s.dataDir, "issue_state.json"), targetRepo, issues.Options{
		MaxNewPerRun:   1,
		TemplateFiller: filler,
	})
	if _, err := mgr.Reconcile(ctx, specs); err != nil {
		return "", fmt.Errorf("filing issue: %w", err)
	}
	if err := mgr.SaveState(); err != nil {
		return "", fmt.Errorf("saving issue state: %w", err)
	}
	url, ok := mgr.TrackedURL(specs[0].Key)
	if !ok {
		return "", fmt.Errorf("issue was not filed for %s", failureID)
	}
	return url, nil
}

// ProposeFix drafts a single fix PR against the source repo for the failure
// using userToken and returns the PR URL. On-demand always opens a draft PR
// (never dry-run) since a human explicitly requested it.
func (s *Service) ProposeFix(ctx context.Context, failureID, userToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pa, err := s.findPattern(failureID)
	if err != nil {
		return "", err
	}

	eff := s.cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return "", fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	if s.ai.Endpoint == "" || s.ai.Model == "" || s.ai.Token == "" {
		return "", fmt.Errorf("AI is not configured on the server; cannot draft a fix")
	}

	aiClient := ai.NewClientWithOptions(ai.Options{
		Token:        s.ai.Token,
		Endpoint:     s.ai.Endpoint,
		Model:        s.ai.Model,
		ExtraHeaders: s.ai.Headers,
	})

	// Keep the batch critique guardrail: when the project configures critique
	// retries, reuse the generation client to review the draft before opening.
	var critique fixpr.Completer
	critiqueRetries := 0
	if eff.CritiqueRetries != nil && *eff.CritiqueRetries > 0 {
		critiqueRetries = *eff.CritiqueRetries
		critique = aiClient
	}

	prClient, source := fixpr.NewClients(userToken)
	mgr := fixpr.NewManager(prClient, aiClient, source,
		filepath.Join(s.dataDir, "fix_pr_state.json"),
		fixpr.Options{
			SourceOwner:     eff.Repo.Owner,
			SourceName:      eff.Repo.Name,
			Fork:            eff.Fork == nil || *eff.Fork,
			AuthorName:      eff.AuthorName,
			AuthorEmail:     eff.AuthorEmail,
			MaxFiles:        eff.MaxFiles,
			MaxNewPerRun:    1,
			Labels:          eff.Labels,
			DashboardURL:    s.cfg.Branding.SiteURL,
			Critique:        critique,
			CritiqueRetries: critiqueRetries,
			PRFiller:        repotemplate.NewPRFiller(userToken, aiClient, eff.Repo.Owner, eff.Repo.Name),
		})
	stats, err := mgr.Reconcile(ctx, []models.PatternAnalysis{*pa})
	if err != nil {
		return "", fmt.Errorf("drafting fix PR: %w", err)
	}
	if err := mgr.SaveState(); err != nil {
		return "", fmt.Errorf("saving fix-PR state: %w", err)
	}
	url, ok := mgr.TrackedURL(*pa)
	if !ok {
		// Surface why generation did not produce a PR, sanitized so no upstream
		// (AI provider) detail reaches the browser.
		if len(stats.Failures) > 0 {
			return "", fmt.Errorf("%s", safeReason(stats.Failures[0].Reason))
		}
		return "", fmt.Errorf("no fix PR was opened for this failure")
	}
	return url, nil
}

// safeReason turns an internal failure reason into a message safe to show a
// user. Reasons from the AI provider (which may echo an opaque response body)
// are replaced with a generic message; our own pipeline messages pass through,
// truncated. It never exposes endpoints, tokens, or provider bodies.
func safeReason(reason string) string {
	reason = strings.TrimSpace(reason)
	low := strings.ToLower(reason)
	// AI transport/provider errors: do not leak the provider's response body.
	if strings.Contains(low, "chat returned") || strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "status code") || strings.Contains(low, "http ") {
		return "the AI service could not complete the request"
	}
	const max = 300
	if len(reason) > max {
		reason = reason[:max] + "…"
	}
	if reason == "" {
		return "the fix could not be generated"
	}
	return reason
}
