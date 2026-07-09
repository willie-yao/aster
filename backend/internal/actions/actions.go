// Package actions performs single-failure GitHub actions on demand: filing an
// issue or drafting a fix PR for one specific pattern, using a per-user token.
// It reuses the batch issue and fix-PR engines for exactly one item, so the
// on-demand and scheduled paths stay behaviorally identical.
package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
)

// ErrNotFound means no pattern in the published data matched the given id.
var ErrNotFound = errors.New("failure not found")

// ErrPreviewNotFound means the confirm token is unknown, expired, or belongs to
// a different admin.
var ErrPreviewNotFound = errors.New("preview not found or expired")

// previewTTL bounds how long a generated draft is held for confirmation.
const previewTTL = 15 * time.Minute

// AIConfig is the resolved chat-completions configuration used to draft fixes.
type AIConfig struct {
	Token    string
	Endpoint string
	Model    string
	Headers  map[string]string
}

// PreviewResult is the draft shown to the admin before they confirm it. Token
// is passed back to Confirm to file the exact previewed issue or open the exact
// previewed PR. Diff is set for fixes only.
type PreviewResult struct {
	Token string `json:"token"`
	Kind  string `json:"kind"` // "issue" | "fix"
	Title string `json:"title"`
	Body  string `json:"body"`
	Diff  string `json:"diff,omitempty"`
}

// previewEntry is a cached draft awaiting confirmation. Exactly one of spec or
// fix is set, per kind. owner binds the draft to the admin who generated it.
type previewEntry struct {
	owner     string
	kind      string
	spec      issues.IssueSpec    // issue drafts
	fix       *fixpr.GeneratedFix // fix drafts
	createdAt time.Time
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

	pmu      sync.Mutex
	previews map[string]*previewEntry
}

// NewService builds a Service. dataDir is the fetcher output directory holding
// jobs/*.json and the *_state.json files.
func NewService(cfg *project.Config, dataDir string, ai AIConfig) *Service {
	return &Service{cfg: cfg, dataDir: dataDir, ai: ai, previews: map[string]*previewEntry{}}
}

// aiClient returns a chat client when AI is fully configured, else a nil
// pointer (callers must nil-check the concrete type). Both the template fillers
// and the revise step use it.
func (s *Service) aiClient() *ai.Client {
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

// aiCompleter returns an AI client for template reformatting when AI is fully
// configured, else nil (which makes the fillers a pass-through). It returns a
// true nil interface so callers' nil checks work.
func (s *Service) aiCompleter() repotemplate.Completer {
	c := s.aiClient()
	if c == nil {
		return nil
	}
	return c
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

// buildIssueSpec resolves the failure to a single issue spec and the target
// repo. It forces the patterns trigger: the admin explicitly asked to file
// this, so the project's configured triggers do not gate the on-demand action.
func (s *Service) buildIssueSpec(failureID string) (issues.IssueSpec, string, error) {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return issues.IssueSpec{}, "", err
	}
	eff := s.cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return issues.IssueSpec{}, "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
	}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{*pa}}
	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		Triggers:     []string{project.IssueTriggerPatterns},
		Labels:       eff.Labels,
		DashboardURL: s.cfg.Branding.SiteURL,
	})
	if len(specs) == 0 {
		return issues.IssueSpec{}, "", fmt.Errorf("failure %s is not an actionable systemic pattern", failureID)
	}
	return specs[0], eff.Repo.Owner + "/" + eff.Repo.Name, nil
}

// buildFixManager builds the fix-PR manager for the source repo using
// userToken. It does not resolve a failure; callers that need the pattern look
// it up separately.
func (s *Service) buildFixManager(userToken string) (*fixpr.Manager, error) {
	eff := s.cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return nil, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	aiClient := s.aiClient()
	if aiClient == nil {
		return nil, fmt.Errorf("AI is not configured on the server; cannot draft a fix")
	}

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
	return mgr, nil
}

// PreviewIssue renders the exact issue that would be filed for the failure,
// without filing it, and caches it for confirmation. instruction is an optional
// maintainer directive that revises the draft body.
func (s *Service) PreviewIssue(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	spec, _, err := s.buildIssueSpec(failureID)
	if err != nil {
		return PreviewResult{}, err
	}
	// Reformat to follow the target repo's issue template when AI is available;
	// a nil filler leaves the default body untouched.
	var filler issues.TemplateFiller
	if c := s.aiCompleter(); c != nil {
		eff := s.cfg.EffectiveIssues()
		filler = repotemplate.NewIssueFiller(userToken, c, eff.Repo.Owner, eff.Repo.Name)
	}
	title, body := issues.RenderSpec(ctx, filler, spec)
	final := issues.IssueSpec{Key: spec.Key, Title: title, Body: body, Labels: spec.Labels}
	if strings.TrimSpace(instruction) != "" {
		if c := s.aiClient(); c != nil {
			final = issues.ReviseBody(ctx, c, final, instruction)
		}
	}
	token := s.stash(userToken, &previewEntry{kind: "issue", spec: final})
	return PreviewResult{Token: token, Kind: "issue", Title: final.Title, Body: final.Body}, nil
}

// PreviewFix generates the fix and renders the exact PR that would be opened,
// without opening it, and caches it for confirmation. instruction is an
// optional maintainer directive that steers the edit.
func (s *Service) PreviewFix(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return PreviewResult{}, err
	}
	mgr, err := s.buildFixManager(userToken)
	if err != nil {
		return PreviewResult{}, err
	}
	gf, err := mgr.GeneratePreview(ctx, *pa, instruction)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%s", safeReason(err.Error()))
	}
	token := s.stash(userToken, &previewEntry{kind: "fix", fix: gf})
	return PreviewResult{
		Token: token, Kind: "fix", Title: gf.Title,
		Body: gf.Description, Diff: gf.Preview.Diff,
	}, nil
}

// Confirm files the issue or opens the PR previously cached under token, using
// userToken. It posts the exact previewed content and returns the created URL.
func (s *Service) Confirm(ctx context.Context, token, userToken string) (string, error) {
	entry, err := s.take(userToken, token)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch entry.kind {
	case "issue":
		eff := s.cfg.EffectiveIssues()
		if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
			return "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
		}
		client := issues.NewClient(userToken, eff.Repo.Owner, eff.Repo.Name)
		targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
		// The spec body was already rendered at preview time; a nil filler posts
		// it verbatim. RecoverPrefixes is empty so a single create only files or
		// adopts this one spec, never recovering other tracked issues.
		mgr := issues.NewManager(client, filepath.Join(s.dataDir, "issue_state.json"), targetRepo, issues.Options{
			MaxNewPerRun: 1,
		})
		if _, err := mgr.Reconcile(ctx, []issues.IssueSpec{entry.spec}); err != nil {
			return "", fmt.Errorf("filing issue: %w", err)
		}
		if err := mgr.SaveState(); err != nil {
			return "", fmt.Errorf("saving issue state: %w", err)
		}
		url, ok := mgr.TrackedURL(entry.spec.Key)
		if !ok {
			return "", fmt.Errorf("issue was not filed")
		}
		return url, nil

	case "fix":
		// Rebuild the manager with userToken to open the previewed fix. State is
		// loaded fresh so the open dedupes against any concurrent writer.
		mgr, err := s.buildFixManager(userToken)
		if err != nil {
			return "", err
		}
		url, err := mgr.OpenFromPreview(ctx, entry.fix)
		if err != nil {
			return "", fmt.Errorf("%s", safeReason(err.Error()))
		}
		if err := mgr.SaveState(); err != nil {
			return "", fmt.Errorf("saving fix-PR state: %w", err)
		}
		return url, nil
	}
	return "", ErrPreviewNotFound
}

// stash caches a draft under a fresh token bound to the admin's identity and
// returns the token, evicting expired entries first.
func (s *Service) stash(userToken string, e *previewEntry) string {
	e.owner = tokenHash(userToken)
	e.createdAt = time.Now()
	token := newToken()
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.evictExpiredLocked()
	s.previews[token] = e
	return token
}

// take removes and returns the draft cached under token if it exists, has not
// expired, and belongs to the same admin.
func (s *Service) take(userToken, token string) (*previewEntry, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.evictExpiredLocked()
	e, ok := s.previews[token]
	if !ok || e.owner != tokenHash(userToken) {
		return nil, ErrPreviewNotFound
	}
	delete(s.previews, token)
	return e, nil
}

func (s *Service) evictExpiredLocked() {
	cutoff := time.Now().Add(-previewTTL)
	for k, e := range s.previews {
		if e.createdAt.Before(cutoff) {
			delete(s.previews, k)
		}
	}
}

// tokenHash binds a preview to the admin who generated it without retaining the
// raw token.
func tokenHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
