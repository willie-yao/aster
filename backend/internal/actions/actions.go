// Package actions performs single-failure GitHub actions on demand: filing an
// issue or drafting a fix PR for one specific analyzed subject, using a per-user token.
// It reuses the issue and fix-PR engines for exactly one item, so the
// on-demand and scheduled paths stay behaviorally identical.
package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patternstate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// ErrNotFound means no pattern in the published data matched the given id.
var ErrNotFound = errors.New("failure not found")

// ErrPreviewRejected means generation safely declined an actionable fix preview.
var ErrPreviewRejected = errors.New("fix preview rejected")

// ErrDraftRefinementRejected means a replacement issue draft failed validation.
var ErrDraftRefinementRejected = errors.New("draft refinement rejected")

// ErrPatternMismatch means the selected recurring pattern does not include the chat analysis.
var ErrPatternMismatch = errors.New("pattern does not include selected analysis")

// ErrPreviewPending means confirmation is already running for this preview.
var ErrPreviewPending = errors.New("preview confirmation is pending")

// ErrPreviewSuperseded means a newer confirmation attempt owns the preview.
var ErrPreviewSuperseded = errors.New("preview confirmation was superseded")

// ErrPreviewOutcomeUnknown means GitHub may have accepted the write. Retrying
// checks for the marked object without creating another one.
var ErrPreviewOutcomeUnknown = errors.New("preview confirmation outcome unknown; retry to check GitHub")

// ErrPreviewTargetChanged means the configured write repository no longer
// matches the repository used to generate the preview.
var ErrPreviewTargetChanged = errors.New("preview target repository changed; generate a new preview")

// ErrPreviewNotFound means the confirm token is unknown, expired, or belongs to
// a different admin.
var ErrPreviewNotFound = errors.New("preview not found or expired")

// previewTTL bounds how long a generated draft is held for confirmation.
const previewTTL = 15 * time.Minute

// AIConfig is the resolved chat-completions configuration used to draft fixes.
type AIConfig struct {
	Token    string
	API      string
	Endpoint string
	Model    string
	Headers  map[string]string
}

// FixTarget identifies the published build selected by analysis chat.
type FixTarget struct {
	JobID   string
	BuildID string
}

// PreviewResult is the draft shown to the admin before they confirm it. Token
// is passed back to Confirm to file the exact previewed issue or open the exact
// previewed PR. Diff is set for fixes only.
type PreviewResult struct {
	Token string `json:"token,omitempty"`
	Kind  string `json:"kind"` // "issue" | "fix"
	Title string `json:"title"`
	Body  string `json:"body"`
	Diff  string `json:"diff,omitempty"`
	// Verify reports the pre-PR build/vet verdict for a fix ("passed" |
	// "failed" | "skipped"). Empty for an issue preview.
	VerifyStatus  string `json:"verify_status,omitempty"`
	VerifySummary string `json:"verify_summary,omitempty"`
	// VerifyOutput is the tail of the failing command's output, set only when
	// verification failed, so the reviewer sees why before confirming.
	VerifyOutput string `json:"verify_output,omitempty"`
}

// previewEntry is a cached draft awaiting confirmation. Exactly one of spec or
// fix is set per kind, and patternHash stores the selected subject content hash.
type previewEntry struct {
	failureID    string
	patternHash  string
	kind         string
	targetRepo   string
	targetConfig string
	spec         issues.IssueSpec    // issue drafts
	fix          *fixpr.GeneratedFix // fix drafts
}

type actionSubjectKind string

const (
	actionSubjectPattern actionSubjectKind = "pattern"
	actionSubjectBuild   actionSubjectKind = "build"
)

// ActionSubject is one current published analysis eligible for a preview.
type ActionSubject struct {
	Kind        actionSubjectKind
	ID          string
	ContentHash string
	Pattern     *models.PatternAnalysis
	Build       *BuildActionSubject
}

// BuildActionSubject is one analyzed build failure without a JUnit assertion.
type BuildActionSubject struct {
	JobID         string
	JobName       string
	Build         models.BuildInfo
	Failure       models.TestCase
	RelevantFiles []string
}

// BuildFailureID returns the stable action ID for one typed build subject.
func BuildFailureID(jobID, buildID string) string {
	encode := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	return "build::" + encode(jobID) + "::" + encode(buildID)
}

type issuePreviewManager interface {
	Reconcile(context.Context, []issues.IssueSpec) (issues.Stats, error)
	TrackedURL(string) (string, bool)
	FindOpen(context.Context, string) (string, bool, error)
	FindAny(context.Context, string) (string, bool, error)
	Forget(string)
	SaveState() error
}

type issueManagerFactory func(userToken, owner, repo string) issuePreviewManager

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

	previewStore        *previewStore
	issueManagerFactory issueManagerFactory

	rmu             sync.Mutex
	requests        *actionRequestState
	requestTimeout  time.Duration
	requestNotify   RequestReadyNotifier
	requestCancels  map[string]context.CancelFunc
	requestConfirms map[string]struct{}
}

// NewService builds a Service. dataDir is the fetcher output directory holding
// jobs/*.json and the *_state.json files.
func NewService(cfg *project.Config, dataDir string, ai AIConfig) *Service {
	s := &Service{
		cfg: cfg, dataDir: dataDir, ai: ai,
		previewStore: newPreviewStore(dataDir),
		issueManagerFactory: func(token, owner, repo string) issuePreviewManager {
			return issues.NewManager(issues.NewClient(token, owner, repo), filepath.Join(dataDir, "issue_state.json"), owner+"/"+repo, issues.Options{MaxNewPerRun: 1})
		},
		requestCancels: map[string]context.CancelFunc{}, requestConfirms: map[string]struct{}{},
		requestTimeout: defaultRequestTimeout,
	}
	s.loadActionRequests()
	return s
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
		API:          s.ai.API,
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
				if detail.PatternRefresh != nil && (detail.PatternRefresh.State != models.PatternRefreshCurrent || !detail.PatternRefresh.EvidenceAvailable) {
					return nil, fmt.Errorf("failure %s is not actionable with stale pattern evidence", id)
				}
				return &detail.PatternAnalyses[i], nil
			}
		}
	}
	return nil, ErrNotFound
}

func (s *Service) resolveSubject(id string) (*ActionSubject, error) {
	if !strings.HasPrefix(id, "build::") {
		pattern, err := s.findPattern(id)
		if err != nil {
			return nil, err
		}
		return &ActionSubject{Kind: actionSubjectPattern, ID: pattern.ID, ContentHash: pattern.ContentHash, Pattern: pattern}, nil
	}
	parts := strings.Split(id, "::")
	if len(parts) != 3 || parts[0] != "build" {
		return nil, ErrNotFound
	}
	decode := func(value string) (string, error) {
		data, err := base64.RawURLEncoding.DecodeString(value)
		return string(data), err
	}
	jobID, err := decode(parts[1])
	if err != nil {
		return nil, ErrNotFound
	}
	buildID, err := decode(parts[2])
	if err != nil || jobID == "" || buildID == "" || BuildFailureID(jobID, buildID) != id {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.dataDir, "jobs", models.JobDataFilename(jobID)))
	if err != nil {
		return nil, ErrNotFound
	}
	var detail models.JobDetail
	if json.Unmarshal(data, &detail) != nil {
		return nil, ErrNotFound
	}
	for ri := range detail.Runs {
		run := &detail.Runs[ri]
		if run.BuildID != buildID {
			continue
		}
		for ti := range run.TestCases {
			testCase := &run.TestCases[ti]
			if testCase.Source != models.TestCaseSourceBuild || testCase.Status != "failed" || testCase.AIAnalysis == nil {
				continue
			}
			analysis := testCase.AIAnalysis
			if !ai.MeetsCurrentCritiqueContract(analysis) || strings.TrimSpace(analysis.GeneratedAt) == "" || strings.TrimSpace(analysis.RootCause) == "" || strings.TrimSpace(analysis.SuggestedFix) == "" {
				return nil, fmt.Errorf("build analysis does not pass current action quality gates")
			}
			relevant := slices.Clone(analysis.RelevantFiles)
			slices.Sort(relevant)
			build := &BuildActionSubject{JobID: detail.JobID, JobName: detail.Name, Build: run.BuildInfo, Failure: *testCase, RelevantFiles: relevant}
			return &ActionSubject{Kind: actionSubjectBuild, ID: id, ContentHash: buildSubjectHash(build), Build: build}, nil
		}
	}
	return nil, ErrNotFound
}

func buildSubjectHash(subject *BuildActionSubject) string {
	analysis := subject.Failure.AIAnalysis
	fileLinks := make([]string, 0, len(analysis.FileLinks))
	for file, link := range analysis.FileLinks {
		fileLinks = append(fileLinks, file+"\x00"+link)
	}
	slices.Sort(fileLinks)
	payload, _ := json.Marshal(struct {
		JobID, BuildID, Source, Suite, Class, Name, GeneratedAt, RootCause, SuggestedFix string
		RelevantFiles, FileLinks                                                         []string
	}{subject.JobID, subject.Build.BuildID, subject.Failure.Source, subject.Failure.SuiteName, subject.Failure.ClassName, subject.Failure.Name, analysis.GeneratedAt, analysis.RootCause, analysis.SuggestedFix, subject.RelevantFiles, fileLinks})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func verifiedBuildSourceFiles(subject *BuildActionSubject, owner, repo string) []string {
	analysis := subject.Failure.AIAnalysis
	if analysis == nil || len(analysis.FileLinks) == 0 {
		return nil
	}
	var files []string
	links := make([]string, 0, len(analysis.FileLinks))
	for _, raw := range analysis.FileLinks {
		links = append(links, raw)
	}
	slices.Sort(links)
	for _, link := range links {
		raw := strings.TrimSpace(link)
		parsed, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
		if len(parts) < 5 || !strings.EqualFold(parts[0], owner) || !strings.EqualFold(parts[1], repo) || parts[2] != "blob" {
			continue
		}
		decoded, err := url.PathUnescape(strings.Join(parts[4:], "/"))
		if err != nil {
			continue
		}
		clean := path.Clean(decoded)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			continue
		}
		files = append(files, clean)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// buildIssueSpecForPattern renders one current pattern into an issue spec.
func (s *Service) buildIssueSpecForPattern(pattern models.PatternAnalysis) (issues.IssueSpec, string, error) {
	eff := s.cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return issues.IssueSpec{}, "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
	}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}}
	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		Triggers:     []string{project.IssueTriggerPatterns},
		Labels:       eff.Labels,
		DashboardURL: s.cfg.Branding.SiteURL,
	})
	if len(specs) == 0 {
		return issues.IssueSpec{}, "", fmt.Errorf("failure %s is not an actionable systemic pattern", pattern.ID)
	}
	return specs[0], eff.Repo.Owner + "/" + eff.Repo.Name, nil
}

func (s *Service) buildIssueSpecForBuild(subject *BuildActionSubject, id string) (issues.IssueSpec, string, error) {
	eff := s.cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return issues.IssueSpec{}, "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
	}
	analysis := subject.Failure.AIAnalysis
	summary := ""
	if subject.Failure.AISummary != nil {
		summary = subject.Failure.AISummary.Summary
	}
	spec := issues.BuildFailureSpec(issues.BuildFailureInput{
		ID: id, JobID: subject.JobID, JobName: subject.JobName, BuildID: subject.Build.BuildID,
		BuildURL: subject.Build.ProwURL, BuildLogURL: subject.Build.BuildLogURL, Summary: summary,
		RootCause: analysis.RootCause, SuggestedFix: analysis.SuggestedFix, RelevantFiles: subject.RelevantFiles,
		DashboardURL: s.cfg.Branding.SiteURL, Labels: eff.Labels,
	})
	return spec, eff.Repo.Owner + "/" + eff.Repo.Name, nil
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
	ar := eff.AgentRuntime
	if ar.Type != "orka" && s.ai.API == ai.APIResponses {
		return nil, fmt.Errorf("local fix runtime requires chat_completions or an Orka fix runtime")
	}
	if aiClient == nil && ar.Type != "orka" {
		return nil, fmt.Errorf("AI is not configured on the server; cannot draft a local fix")
	}

	critique, critiqueRetries, err := fixruntime.Critique(aiClient, eff.CritiqueRetries)
	if err != nil {
		return nil, err
	}

	var prFiller fixpr.PRBodyFiller
	if aiClient != nil {
		prFiller = repotemplate.NewPRFiller(userToken, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}
	prClient := fixpr.NewClients(userToken)
	opts := fixpr.Options{
		SourceOwner:     eff.Repo.Owner,
		SourceName:      eff.Repo.Name,
		Fork:            eff.Fork == nil || *eff.Fork,
		AuthorName:      eff.AuthorName,
		AuthorEmail:     eff.AuthorEmail,
		MinConfidence:   eff.MinConfidence,
		MaxFiles:        eff.MaxFiles,
		MaxNewPerRun:    1,
		Labels:          eff.Labels,
		DashboardURL:    s.cfg.Branding.SiteURL,
		Critique:        critique,
		CritiqueRetries: critiqueRetries,
		PRFiller:        prFiller,
	}
	if eff.Verify != nil && eff.Verify.Enabled {
		opts.Verify = &fixpr.VerifyConfig{
			Runtime:  runtime.NewLocal(),
			Commands: eff.Verify.ParsedCommands(),
			Timeout:  eff.Verify.ParsedTimeout(),
			Token:    userToken,
		}
	}
	allowBash := ar.AllowBash == nil || *ar.AllowBash
	agentRuntime, err := fixruntime.New(ar)
	if err != nil {
		return nil, err
	}
	model := ar.Model
	if model == "" {
		model = s.ai.Model
	}
	opts.Agent = &fixpr.AgentConfig{
		Runtime:             agentRuntime,
		API:                 s.ai.API,
		SharedModelEndpoint: ar.Type != "orka",
		Model:               model,
		Endpoint:            s.ai.Endpoint,
		ModelToken:          s.ai.Token,
		MaxTurns:            ar.MaxTurns,
		AllowBash:           allowBash,
		Timeout:             ar.ParsedTimeout(),
		GitToken:            userToken,
	}
	mgr := fixpr.NewManager(prClient,
		filepath.Join(s.dataDir, "fix_pr_state.json"), opts)
	return mgr, nil
}

// generateIssuePreview renders an issue draft without caching or posting it.
func (s *Service) generateIssuePreview(ctx context.Context, failureID, userToken, instruction string, baseIssue *issues.IssueSpec, baseTargetRepo, basePatternHash string) (PreviewResult, *previewEntry, error) {
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	var spec issues.IssueSpec
	var targetRepo string
	if subject.Kind == actionSubjectPattern {
		spec, targetRepo, err = s.buildIssueSpecForPattern(*subject.Pattern)
	} else {
		spec, targetRepo, err = s.buildIssueSpecForBuild(subject.Build, subject.ID)
	}
	if err != nil {
		return PreviewResult{}, nil, err
	}
	var final issues.IssueSpec
	if baseIssue != nil {
		if basePatternHash == "" || basePatternHash != subject.ContentHash || baseIssue.Key != spec.Key || (baseTargetRepo != "" && baseTargetRepo != targetRepo) {
			return PreviewResult{}, nil, ErrPreviewTargetChanged
		}
		final = *baseIssue
		final.Labels = slices.Clone(baseIssue.Labels)
	} else {
		var filler issues.TemplateFiller
		if c := s.aiCompleter(); c != nil {
			eff := s.cfg.EffectiveIssues()
			filler = repotemplate.NewIssueFiller(userToken, c, eff.Repo.Owner, eff.Repo.Name)
		}
		title, body := issues.RenderSpec(ctx, filler, spec)
		final = issues.IssueSpec{Key: spec.Key, Title: title, Body: body, Labels: spec.Labels}
	}
	preview := PreviewResult{Kind: "issue", Title: final.Title, Body: final.Body}
	entry := &previewEntry{failureID: subject.ID, patternHash: subject.ContentHash, kind: "issue", targetRepo: targetRepo, spec: final}
	if strings.TrimSpace(instruction) != "" {
		c := s.aiClient()
		if c == nil {
			return preview, entry, fmt.Errorf("%w: AI is unavailable", ErrDraftRefinementRejected)
		}
		revised, reviseErr := issues.ReviseBody(ctx, c, final, instruction)
		if reviseErr != nil {
			return preview, entry, fmt.Errorf("%w: safe structured revision was not produced", ErrDraftRefinementRejected)
		}
		final = revised
		preview = PreviewResult{Kind: "issue", Title: final.Title, Body: final.Body}
		entry.spec = final
	}
	return preview, entry, nil
}

// PreviewIssue renders the exact issue that would be filed for the failure,
// without filing it, and caches it for confirmation.
func (s *Service) PreviewIssue(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	preview, entry, err := s.generateIssuePreview(ctx, failureID, userToken, instruction, nil, "", "")
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: generated draft did not pass safety validation", ErrPreviewRejected)
	}
	token, err := s.stash(userToken, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// generateFixPreview creates a fix draft without caching or opening it.
func (s *Service) generateFixPreview(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, *previewEntry, error) {
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	if subject.Kind == actionSubjectPattern {
		return s.generateFixPreviewForPattern(ctx, *subject.Pattern, userToken, instruction, nil)
	}
	eff := s.cfg.EffectiveFixPRs()
	if eff.Repo == nil {
		return PreviewResult{}, nil, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	sourceFiles := verifiedBuildSourceFiles(subject.Build, eff.Repo.Owner, eff.Repo.Name)
	if len(sourceFiles) == 0 {
		return PreviewResult{}, nil, fmt.Errorf("%w: repository source investigation did not identify a verified local path; create an issue or investigate source before proposing a fix", ErrPreviewRejected)
	}
	mgr, err := s.buildFixManager(userToken)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	analysis := subject.Build.Failure.AIAnalysis
	gf, err := mgr.GenerateBuildPreview(ctx, fixpr.BuildFailure{
		ID: subject.ID, JobID: subject.Build.JobID, JobName: subject.Build.JobName, BuildID: subject.Build.Build.BuildID,
		RootCause: analysis.RootCause, SuggestedFix: analysis.SuggestedFix, RelevantFiles: subject.Build.RelevantFiles, SourceFiles: sourceFiles,
	}, instruction)
	if err != nil {
		return PreviewResult{}, nil, safeFixPreviewError(err)
	}
	return PreviewResult{Kind: gfKind, Title: gf.Title, Body: gf.Description, Diff: gf.Preview.Diff,
			VerifyStatus: string(gf.Preview.Verify.Status), VerifySummary: gf.Preview.Verify.Summary, VerifyOutput: gf.Preview.Verify.Output},
		&previewEntry{failureID: subject.ID, patternHash: subject.ContentHash, kind: gfKind, targetRepo: eff.Repo.Owner + "/" + eff.Repo.Name, targetConfig: fixTargetFingerprint(eff), fix: gf}, nil
}

func (s *Service) generateFixPreviewForPattern(
	ctx context.Context, pattern models.PatternAnalysis, userToken, instruction string,
	generationContext *fixpr.GenerationContext,
) (PreviewResult, *previewEntry, error) {
	mgr, err := s.buildFixManager(userToken)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	var gf *fixpr.GeneratedFix
	if generationContext == nil {
		gf, err = mgr.GeneratePreview(ctx, pattern, instruction)
	} else {
		gf, err = mgr.GeneratePreviewWithContext(ctx, pattern, instruction, *generationContext)
	}
	if err != nil {
		return PreviewResult{}, nil, safeFixPreviewError(err)
	}
	return PreviewResult{
			Kind: gfKind, Title: gf.Title, Body: gf.Description, Diff: gf.Preview.Diff,
			VerifyStatus: string(gf.Preview.Verify.Status), VerifySummary: gf.Preview.Verify.Summary,
			VerifyOutput: gf.Preview.Verify.Output,
		}, &previewEntry{failureID: pattern.ID, patternHash: pattern.ContentHash, kind: gfKind, targetRepo: s.cfg.EffectiveFixPRs().Repo.Owner + "/" + s.cfg.EffectiveFixPRs().Repo.Name,
			targetConfig: fixTargetFingerprint(s.cfg.EffectiveFixPRs()), fix: gf}, nil
}

func safeFixPreviewError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrPreviewRejected, safeReason(err.Error()))
}

const gfKind = "fix"

// PreviewFix generates the exact fix PR preview and caches it for confirmation.
func (s *Service) PreviewFix(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	preview, entry, err := s.generateFixPreview(ctx, failureID, userToken, instruction)
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: generated draft did not pass safety validation", ErrPreviewRejected)
	}
	token, err := s.stash(userToken, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// PreviewFixWithContext generates a fix from one validated selected chat response.
func (s *Service) PreviewFixWithContext(
	ctx context.Context, pattern models.PatternAnalysis, userToken, instruction string, target FixTarget, generationContext fixpr.GenerationContext,
) (PreviewResult, error) {
	if strings.TrimSpace(target.JobID) == "" || strings.TrimSpace(target.BuildID) == "" ||
		pattern.JobID != target.JobID || !slices.Contains(pattern.SharedBuilds, target.BuildID) {
		return PreviewResult{}, ErrPatternMismatch
	}
	preview, entry, err := s.generateFixPreviewForPattern(ctx, pattern, userToken, instruction, &generationContext)
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: generated draft did not pass safety validation", ErrPreviewRejected)
	}
	token, err := s.stash(userToken, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// Confirm files the issue or opens the PR previously cached under token.
func (s *Service) Confirm(ctx context.Context, token, userToken string) (string, error) {
	lease := s.requestTimeout + 30*time.Second
	entry, resultURL, attemptID, reconcile, err := s.beginConfirm(userToken, token, lease)
	if err != nil || resultURL != "" {
		return resultURL, err
	}
	if !reconcile {
		if _, validateErr := validatedPreviewEntry(entry); validateErr != nil {
			_ = s.previewStore.discard(userToken, token, attemptID)
			return "", fmt.Errorf("%w: saved draft did not pass safety validation", ErrPreviewRejected)
		}
	}
	if !reconcile && (entry.failureID != "" || entry.patternHash != "") {
		if err := s.validateSubjectSnapshot(entry.failureID, entry.patternHash, entry.kind); err != nil {
			_ = s.previewStore.discard(userToken, token, attemptID)
			return "", err
		}
	}
	if reconcile {
		resultURL, found, reconcileErr := s.reconcileEntry(ctx, entry, userToken)
		if reconcileErr != nil {
			if errors.Is(reconcileErr, ErrPreviewTargetChanged) {
				_ = s.previewStore.discard(userToken, token, attemptID)
				return "", reconcileErr
			}
			_ = s.finishConfirm(userToken, token, attemptID, "", reconcileErr)
			return "", reconcileErr
		}
		if !found {
			_ = s.finishConfirm(userToken, token, attemptID, "", ErrPreviewOutcomeUnknown)
			return "", ErrPreviewOutcomeUnknown
		}
		if err := s.finishConfirm(userToken, token, attemptID, resultURL, nil); err != nil {
			return resultURL, err
		}
		return resultURL, nil
	}
	resultURL, confirmErr := s.confirmEntry(ctx, entry, userToken)
	if errors.Is(confirmErr, ErrPreviewTargetChanged) {
		_ = s.previewStore.discard(userToken, token, attemptID)
		return "", confirmErr
	}
	finishInputErr := confirmErr
	if resultURL != "" {
		finishInputErr = nil
	}
	finishErr := s.finishConfirm(userToken, token, attemptID, resultURL, finishInputErr)
	if resultURL != "" && finishErr == nil {
		return resultURL, nil
	}
	if confirmErr != nil {
		return resultURL, confirmErr
	}
	if finishErr != nil {
		return resultURL, finishErr
	}
	return resultURL, nil
}

func (s *Service) beginConfirm(userToken, token string, lease time.Duration) (*previewEntry, string, string, bool, error) {
	return s.previewStore.begin(userToken, token, lease)
}

func (s *Service) finishConfirm(userToken, token, attemptID, resultURL string, confirmErr error) error {
	return s.previewStore.finish(userToken, token, attemptID, resultURL, confirmErr)
}

func (s *Service) reconcileEntry(ctx context.Context, entry *previewEntry, userToken string) (string, bool, error) {
	switch entry.kind {
	case "issue":
		eff := s.cfg.EffectiveIssues()
		if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
			return "", false, fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
		}
		if entry.targetRepo != eff.Repo.Owner+"/"+eff.Repo.Name {
			return "", false, ErrPreviewTargetChanged
		}
		mgr := s.issueManagerFactory(userToken, eff.Repo.Owner, eff.Repo.Name)
		if strings.HasPrefix(entry.failureID, "build::") {
			return mgr.FindAny(ctx, entry.spec.Key)
		}
		return mgr.FindOpen(ctx, entry.spec.Key)
	case gfKind:
		eff := s.cfg.EffectiveFixPRs()
		if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" || entry.fix == nil {
			return "", false, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
		}
		if entry.targetRepo != eff.Repo.Owner+"/"+eff.Repo.Name {
			return "", false, ErrPreviewTargetChanged
		}
		if entry.targetConfig != fixTargetFingerprint(eff) {
			return "", false, ErrPreviewTargetChanged
		}
		key := entry.fix.Snapshot().Key
		client := fixpr.NewClients(userToken)
		if strings.HasPrefix(entry.failureID, "build::") {
			_, url, found, err := client.SearchAnyPR(ctx, eff.Repo.Owner, eff.Repo.Name, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
			return url, found, err
		}
		_, url, found, err := client.SearchOpenPR(ctx, eff.Repo.Owner, eff.Repo.Name, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
		return url, found, err
	default:
		return "", false, ErrPreviewNotFound
	}
}

func (s *Service) confirmEntry(ctx context.Context, entry *previewEntry, userToken string) (string, error) {
	var result string
	err := patternstate.WithLock(s.dataDir, func() error {
		var err error
		result, err = s.confirmEntryUnlocked(ctx, entry, userToken)
		return err
	})
	return result, err
}

func (s *Service) confirmEntryUnlocked(ctx context.Context, entry *previewEntry, userToken string) (string, error) {
	if entry != nil && entry.failureID != "" {
		if err := s.validateSubjectSnapshot(entry.failureID, entry.patternHash, entry.kind); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	switch entry.kind {
	case "issue":
		eff := s.cfg.EffectiveIssues()
		if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
			return "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
		}
		if entry.targetRepo != eff.Repo.Owner+"/"+eff.Repo.Name {
			return "", ErrPreviewTargetChanged
		}
		mgr := s.issueManagerFactory(userToken, eff.Repo.Owner, eff.Repo.Name)
		if strings.HasPrefix(entry.failureID, "build::") {
			if url, found, err := mgr.FindAny(ctx, entry.spec.Key); err != nil {
				return "", fmt.Errorf("searching build issue: %w", err)
			} else if found {
				return url, nil
			}
		}
		if _, err := mgr.Reconcile(ctx, []issues.IssueSpec{entry.spec}); err != nil {
			if errors.Is(err, issues.ErrWriteOutcomeUnknown) {
				return "", fmt.Errorf("%w: filing issue: %v", ErrPreviewOutcomeUnknown, err)
			}
			return "", fmt.Errorf("filing issue: %w", err)
		}
		url, ok := mgr.TrackedURL(entry.spec.Key)
		if !ok {
			return "", fmt.Errorf("issue was not filed")
		}
		if strings.HasPrefix(entry.failureID, "build::") {
			mgr.Forget(entry.spec.Key)
		}
		if err := mgr.SaveState(); err != nil {
			if strings.HasPrefix(entry.failureID, "build::") {
				log.Printf("Warning: build issue state cleanup failed after creating %s: %v", url, err)
				return url, nil
			}
			return url, fmt.Errorf("saving issue state: %w", err)
		}
		return url, nil
	case gfKind:
		eff := s.cfg.EffectiveFixPRs()
		if eff.Repo == nil || entry.targetRepo != eff.Repo.Owner+"/"+eff.Repo.Name {
			return "", ErrPreviewTargetChanged
		}
		if entry.targetConfig != fixTargetFingerprint(eff) {
			return "", ErrPreviewTargetChanged
		}
		mgr, err := s.buildFixManager(userToken)
		if err != nil {
			return "", err
		}
		url, err := mgr.OpenFromPreview(ctx, entry.fix)
		if err != nil {
			if errors.Is(err, fixpr.ErrWriteOutcomeUnknown) {
				return "", fmt.Errorf("%w: opening fix: %s", ErrPreviewOutcomeUnknown, safeReason(err.Error()))
			}
			return "", fmt.Errorf("%s", safeReason(err.Error()))
		}
		if strings.HasPrefix(entry.failureID, "build::") {
			mgr.Forget(entry.fix.Snapshot().Key)
		}
		if err := mgr.SaveState(); err != nil {
			if strings.HasPrefix(entry.failureID, "build::") {
				log.Printf("Warning: build fix state cleanup failed after opening %s: %v", url, err)
				return url, nil
			}
			return url, fmt.Errorf("saving fix-PR state: %w", err)
		}
		return url, nil
	}
	return "", ErrPreviewNotFound
}

func fixTargetFingerprint(eff project.FixPRs) string {
	fork := eff.Fork == nil || *eff.Fork
	payload, _ := json.Marshal(struct {
		Fork        bool     `json:"fork"`
		AuthorName  string   `json:"author_name"`
		AuthorEmail string   `json:"author_email"`
		Labels      []string `json:"labels"`
	}{fork, eff.AuthorName, eff.AuthorEmail, eff.Labels})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Resolve marks a systemic pattern as resolved: it is hidden from the active
// view until a failing build newer than the current watermark recurs. note is
// an optional maintainer comment (e.g. the fixing PR). login attributes it.
func (s *Service) Resolve(failureID, login, note string) error {
	return patternstate.WithLock(s.dataDir, func() error { return s.resolveUnlocked(failureID, login, note) })
}

func (s *Service) resolveUnlocked(failureID, login, note string) error {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return err
	}
	if !pa.Systemic {
		return fmt.Errorf("only systemic recurring patterns can be resolved")
	}
	watermark := resolve.Watermark(*pa)
	if watermark == "" {
		return fmt.Errorf("pattern has no build history to resolve against")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return resolve.Update(s.dataDir, func(st *resolve.State) bool {
		st.Resolved[failureID] = resolve.Entry{
			ResolvedAt: time.Now().UTC().Format(time.RFC3339), ResolvedBy: login,
			Note: strings.TrimSpace(note), Watermark: watermark, Subject: pa.Subject,
		}
		return true
	})
}

// Unresolve clears a pattern's resolved mark so it returns to the active view.
func (s *Service) Unresolve(failureID string) error {
	return patternstate.WithLock(s.dataDir, func() error { return s.unresolveUnlocked(failureID) })
}

func (s *Service) unresolveUnlocked(failureID string) error {
	if !resolve.Load(s.dataDir).IsResolved(failureID) {
		return ErrNotFound
	}
	if _, err := s.findPattern(failureID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return resolve.Update(s.dataDir, func(st *resolve.State) bool {
		if !st.IsResolved(failureID) {
			return false
		}
		delete(st.Resolved, failureID)
		return true
	})
}

// stash persists a draft under a fresh token bound to the admin identity.
func (s *Service) stash(userToken string, entry *previewEntry) (string, error) {
	return s.previewStore.stash(userToken, entry)
}

// take removes one persisted preview for compatibility with direct callers.
func (s *Service) take(userToken, token string) (*previewEntry, error) {
	return s.previewStore.take(userToken, token)
}

// tokenHash binds a preview to the admin who generated it without retaining the
// raw token.
func tokenHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
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
