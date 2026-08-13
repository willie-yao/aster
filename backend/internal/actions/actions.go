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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/buildsource"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patternstate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// ErrNotFound means no pattern in the published data matched the given id.
var ErrNotFound = errors.New("failure not found")

// ErrPreviewRejected means generation safely declined an actionable fix preview.
var ErrPreviewRejected = errors.New("fix preview rejected")

// ErrDraftRefinementRejected means a replacement issue draft failed validation.
var ErrDraftRefinementRejected = errors.New("draft refinement rejected")

var ErrRemediationAlreadyPresent = errors.New("proposed remediation is already present")
var ErrRemediationInconclusive = errors.New("source verification was inconclusive")

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
const sourceVerificationVersion = 5

// AIConfig is the resolved chat-completions configuration used to draft fixes.
type AIConfig struct {
	Token           string
	API             string
	Endpoint        string
	Model           string
	ReasoningEffort ai.ReasoningEffort
	Headers         map[string]string
	SourceToken     string
	UsageRecorder   *aiusage.Recorder
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
	failureID           string
	patternHash         string
	kind                string
	targetRepo          string
	targetConfig        string
	verificationVersion int
	initiatedBy         string
	initiatedAt         string
	spec                issues.IssueSpec    // issue drafts
	fix                 *fixpr.GeneratedFix // fix drafts
	analysisBinding     *AnalysisPreviewBinding
}

type actionSubjectKind string

const (
	actionSubjectPattern actionSubjectKind = "pattern"
	actionSubjectBuild   actionSubjectKind = "build"
)

// ActionSubject is one current published analysis eligible for a preview.
type ActionSubject struct {
	Kind           actionSubjectKind
	ID             string
	ContentHash    string
	Pattern        *models.PatternAnalysis
	PatternRefresh *models.PatternRefreshStatus
	Build          *BuildActionSubject
	SourceFiles    []string
	PolicyText     string
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
	writeAudit          func(botWriteAuditRecord) error

	rmu                      sync.Mutex
	requests                 *actionRequestState
	requestTimeout           time.Duration
	requestNotify            RequestReadyNotifier
	requestNotifyCancels     map[string]context.CancelFunc
	requestCancels           map[string]context.CancelFunc
	requestConfirms          map[string]struct{}
	requestDone              map[string]chan struct{}
	requestCleanups          map[string]struct{}
	requestsConfigured       bool
	stopping                 bool
	requestWG                sync.WaitGroup
	managedRuntime           func() (runtime.ManagedAgentRuntime, error)
	requestStateWriter       func(string, any) error
	sourceVerifier           func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error)
	sourceReaderFactory      sourceSnapshotReaderFactory
	analysisPreviewValidator AnalysisPreviewValidator
	fixActionsEnabled        bool
}

// NewService builds a Service. dataDir is the fetcher output directory holding
// jobs/*.json and the *_state.json files.
func NewService(cfg *project.Config, dataDir string, ai AIConfig) *Service {
	s := &Service{
		cfg: cfg, dataDir: dataDir, ai: ai,
		previewStore: newPreviewStore(dataDir),
		writeAudit:   newBotWriteAuditStore(dataDir).record,
		issueManagerFactory: func(token, owner, repo string) issuePreviewManager {
			return issues.NewManager(issues.NewClient(token, owner, repo), filepath.Join(dataDir, "issue_state.json"), owner+"/"+repo, issues.Options{MaxNewPerRun: 1})
		},
		requestCancels: map[string]context.CancelFunc{}, requestConfirms: map[string]struct{}{}, requestDone: map[string]chan struct{}{}, requestCleanups: map[string]struct{}{}, requestNotifyCancels: map[string]context.CancelFunc{},
		requestTimeout: defaultRequestTimeout, requestStateWriter: statefile.WritePrivateJSONDurable,
		fixActionsEnabled: true,
	}
	s.sourceVerifier = actionverify.Verify
	s.managedRuntime = func() (runtime.ManagedAgentRuntime, error) {
		eff := s.cfg.EffectiveFixPRs()
		rt, err := fixruntime.New(eff.AgentRuntime)
		if err != nil {
			return nil, err
		}
		managed, ok := rt.(runtime.ManagedAgentRuntime)
		if !ok {
			return nil, nil
		}
		return managed, nil
	}
	s.loadActionRequests()
	return s
}

// ConfigureFixActions controls whether Fix PR preview and confirmation are exposed.
// Production server startup disables them for the local OpenCode runtime.
func (s *Service) ConfigureFixActions(enabled bool) {
	s.fixActionsEnabled = enabled
}

func (s *Service) requireFixActions() error {
	if s == nil || !s.fixActionsEnabled {
		return fmt.Errorf("%w: no production-capable Fix PR runtime is configured", ErrPreviewRejected)
	}
	return nil
}

// aiClient returns a chat client when AI is fully configured, else a nil
// pointer (callers must nil-check the concrete type). Both the template fillers
// and the revise step use it.
func (s *Service) aiClient() *ai.Client {
	if s.ai.Endpoint == "" || s.ai.Model == "" || s.ai.Token == "" {
		return nil
	}
	return ai.NewClientWithOptions(ai.Options{
		Token:           s.ai.Token,
		API:             s.ai.API,
		Endpoint:        s.ai.Endpoint,
		Model:           s.ai.Model,
		ReasoningEffort: s.ai.ReasoningEffort,
		ExtraHeaders:    s.ai.Headers,
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

// findPattern resolves a current failure id to its PatternAnalysis.
func (s *Service) findPattern(id string) (*models.PatternAnalysis, error) {
	pattern, refresh, err := s.findPatternRecord(id)
	if err != nil {
		return nil, err
	}
	if code := patternRefreshReasonCode(refresh); code != "" {
		return nil, withReason(code, ErrRemediationInconclusive, "")
	}
	return pattern, nil
}

func (s *Service) findPatternRecord(id string) (*models.PatternAnalysis, *models.PatternRefreshStatus, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil, ErrNotFound
	}
	jobsDir := filepath.Join(s.dataDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading job details: %w", err)
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
				pattern := detail.PatternAnalyses[i]
				var refresh *models.PatternRefreshStatus
				if detail.PatternRefresh != nil {
					copy := *detail.PatternRefresh
					refresh = &copy
				}
				return &pattern, refresh, nil
			}
		}
	}
	return nil, nil, ErrNotFound
}

func patternRefreshReasonCode(refresh *models.PatternRefreshStatus) ReasonCode {
	if refresh == nil {
		return ""
	}
	switch refresh.State {
	case models.PatternRefreshCurrent:
		if !refresh.EvidenceAvailable {
			return ReasonEvidenceUnavailable
		}
		return ""
	case models.PatternRefreshRetained:
		return ReasonRetainedStale
	case models.PatternRefreshFailed, models.PatternRefreshUnavailable:
		return ReasonContractGenerationFailed
	default:
		return ReasonEvidenceUnavailable
	}
}

func (s *Service) resolveSubject(id string) (*ActionSubject, error) {
	if !strings.HasPrefix(id, "build::") {
		pattern, refresh, err := s.findPatternRecord(id)
		if err != nil {
			return nil, err
		}
		if code := patternRefreshReasonCode(refresh); code != "" {
			return nil, withReason(code, ErrRemediationInconclusive, "")
		}
		return &ActionSubject{Kind: actionSubjectPattern, ID: pattern.ID, ContentHash: pattern.ContentHash, Pattern: pattern, PatternRefresh: refresh}, nil
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

func (s *Service) resolveSubjectForEligibility(id string) (*ActionSubject, error) {
	if strings.HasPrefix(id, "build::") {
		return s.resolveSubject(id)
	}
	pattern, refresh, err := s.findPatternRecord(id)
	if err != nil {
		return nil, err
	}
	return &ActionSubject{Kind: actionSubjectPattern, ID: pattern.ID, ContentHash: pattern.ContentHash, Pattern: pattern, PatternRefresh: refresh}, nil
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
	if analysis == nil {
		return nil
	}
	return verifiedSourceFiles(analysis.FileLinks, owner, repo, "")
}

func verifiedSourceFiles(fileLinks map[string]string, owner, repo, revision string) []string {
	return buildsource.VerifiedPaths(fileLinks, buildsource.Source{Owner: owner, Name: repo, Revision: revision})
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

func (s *Service) verifyRemediation(ctx context.Context, subject *ActionSubject) error {
	return s.verifyRemediationProposal(ctx, subject, "")
}

func (s *Service) fixDestinationForPattern(pattern models.PatternAnalysis) (project.FixDestination, string, error) {
	repository, revision := "", ""
	explicit, implicit := false, false
	for _, target := range pattern.RemediationTargets {
		if target.Repository == "" {
			if explicit {
				return project.FixDestination{}, "", fmt.Errorf("remediation targets cannot mix default and explicit repositories")
			}
			implicit = true
			continue
		}
		if implicit {
			return project.FixDestination{}, "", fmt.Errorf("remediation targets cannot mix default and explicit repositories")
		}
		if repository == "" {
			repository, revision, explicit = target.Repository, target.Revision, true
		} else if !strings.EqualFold(repository, target.Repository) || revision != target.Revision {
			return project.FixDestination{}, "", fmt.Errorf("remediation targets must use one repository and revision")
		}
		if _, err := s.cfg.ResolveFixDestination(target.Repository, target.Path); err != nil {
			return project.FixDestination{}, "", err
		}
	}
	if !explicit {
		destination, err := s.cfg.ResolveFixDestination("", "")
		if err != nil {
			repo := s.cfg.EffectiveAnalysisSourceRepo()
			if repo.Owner != "" && repo.Name != "" {
				return project.FixDestination{Repo: repo, Fork: true}, "", nil
			}
		}
		return destination, "", err
	}
	destination, err := s.cfg.ResolveFixDestination(repository, pattern.RemediationTargets[0].Path)
	if err != nil {
		for _, target := range pattern.RemediationTargets {
			if target.Repository != "" {
				destination, err = s.cfg.ResolveFixDestination(repository, target.Path)
				break
			}
		}
	}
	return destination, revision, err
}

func (s *Service) fixDestinationForEntry(entry *previewEntry) (project.FixDestination, error) {
	if entry == nil || entry.fix == nil {
		return project.FixDestination{}, fmt.Errorf("fix preview is missing")
	}
	if strings.HasPrefix(entry.failureID, "build::") {
		return s.cfg.ResolveFixDestination("", "")
	}
	destination, _, err := s.fixDestinationForPattern(entry.fix.Snapshot().Pattern)
	if err != nil {
		return project.FixDestination{}, err
	}
	if err := s.validateFixFiles(destination, entry.fix.Snapshot().Files); err != nil {
		return project.FixDestination{}, err
	}
	return destination, nil
}

func (s *Service) validateFixFiles(destination project.FixDestination, files map[string]string) error {
	repository := destination.Repo.Owner + "/" + destination.Repo.Name
	for file := range files {
		resolved, err := s.cfg.ResolveFixDestination(repository, file)
		if err != nil {
			return fmt.Errorf("generated file %q is not allowed for %s: %w", file, repository, err)
		}
		if resolved.Repo != destination.Repo || resolved.Fork != destination.Fork {
			return fmt.Errorf("generated file %q changed the selected fix destination", file)
		}
	}
	return nil
}

func (s *Service) verifyOptionalRemediation(ctx context.Context, subject *ActionSubject, proposal string) error {
	proposal = strings.TrimSpace(proposal)
	if proposal == "" || !actionverify.HasImplementationSymbols(proposal) {
		return nil
	}
	if subject != nil && subject.Kind == actionSubjectPattern && subject.Pattern != nil && len(subject.Pattern.RemediationTargets) > 0 {
		allowed := make([]string, 0, len(subject.Pattern.RemediationTargets))
		for _, target := range subject.Pattern.RemediationTargets {
			if target.Symbol != "" {
				allowed = append(allowed, target.Symbol)
			}
			if target.RequiredCall != "" {
				allowed = append(allowed, target.RequiredCall)
			}
			if key, _, ok := strings.Cut(target.Value, "="); ok && regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(strings.TrimSpace(key)) {
				allowed = append(allowed, strings.TrimSpace(key))
			}
		}
		unexpected := actionverify.UnexpectedImplementationSymbols(proposal, allowed)
		if len(unexpected) == 0 {
			return nil
		}
		quoted := make([]string, 0, len(unexpected))
		for _, symbol := range unexpected {
			quoted = append(quoted, "`"+symbol+"`")
		}
		proposal = strings.Join(quoted, " ")
	}
	return s.verifyRemediationProposal(ctx, subject, proposal)
}

func (s *Service) verifyRemediationProposal(ctx context.Context, subject *ActionSubject, override string) error {
	patternSubject := subject != nil && subject.Kind == actionSubjectPattern && subject.Pattern != nil
	if code, reason := subjectEligibilityReason(subject); code != "" {
		if strings.TrimSpace(reason) == "" {
			reason = ReasonMessage(code)
		}
		state := verificationStateForCode(code)
		if err := s.setRequestVerification(ctx, state, code, reason); err != nil {
			return withReason(ReasonGenerationFailed, ErrRemediationInconclusive, "eligibility result could not be persisted")
		}
		cause := ErrRemediationInconclusive
		if state == actionverify.StateAlreadyPresent {
			cause = ErrRemediationAlreadyPresent
		}
		return withReason(code, cause, reason)
	}
	if patternSubject && strings.TrimSpace(override) != "" {
		policyText := strings.Join([]string{
			subject.Pattern.SuggestedFix, subject.Pattern.SharedRootCause, subject.Pattern.Summary,
			subject.PolicyText, override,
		}, "\n")
		if remediationpolicy.Reason(policyText, subject.Pattern.RemediationTargets) != "" {
			reason := ReasonMessage(ReasonUnsafeRemediation)
			if err := s.setRequestVerification(ctx, actionverify.StateInconclusive, ReasonUnsafeRemediation, reason); err != nil {
				return withReason(ReasonGenerationFailed, ErrRemediationInconclusive, "remediation policy result could not be persisted")
			}
			return withReason(ReasonUnsafeRemediation, ErrRemediationInconclusive, reason)
		}
	}
	if subject == nil || s.sourceVerifier == nil || s.cfg == nil {
		return nil
	}
	inconclusive := func(code ReasonCode, reason string, cause error) error {
		if strings.TrimSpace(reason) == "" {
			reason = ReasonMessage(code)
		}
		if err := s.setRequestVerification(ctx, actionverify.StateInconclusive, code, reason); err != nil {
			return withReason(ReasonGenerationFailed, ErrRemediationInconclusive, "source verification result could not be persisted")
		}
		return withReason(code, ErrRemediationInconclusive, reason)
	}
	repo := s.cfg.EffectiveAnalysisSourceRepo()
	structured := patternSubject
	if repo.Owner == "" && repo.Name == "" {
		if structured {
			return inconclusive(ReasonSourceVerificationInconclusive, "", nil)
		}
		return nil
	}
	if repo.Owner == "" || repo.Name == "" {
		return inconclusive(ReasonSourceVerificationInconclusive, "", nil)
	}
	var revision, proposal string
	var files []string
	var targets []models.RemediationTarget
	if subject.Kind == actionSubjectPattern && subject.Pattern != nil {
		revision, proposal = subject.Pattern.SourceRef, subject.Pattern.SuggestedFix
		targets = append([]models.RemediationTarget(nil), subject.Pattern.RemediationTargets...)
		destination, targetRevision, destinationErr := s.fixDestinationForPattern(*subject.Pattern)
		if destinationErr != nil {
			return inconclusive(ReasonSourceVerificationInconclusive, "remediation repository is not allowed", destinationErr)
		}
		if targetRevision != "" {
			repo, revision = destination.Repo, targetRevision
		} else {
			if sourceRepo, sourceRevision, ok := strings.Cut(revision, "@"); ok {
				if !strings.EqualFold(sourceRepo, repo.Owner+"/"+repo.Name) {
					return inconclusive(ReasonSourceVerificationInconclusive, "grounded repository does not match configured source", nil)
				}
				revision = sourceRevision
			}
			files = append(files, subject.SourceFiles...)
			if len(files) == 0 {
				files = verifiedSourceFiles(subject.Pattern.FileLinks, repo.Owner, repo.Name, revision)
			}
		}
	} else if subject.Build != nil && subject.Build.Failure.AIAnalysis != nil {
		source, ok := ai.ResolveBuildSource(subject.Build.Build, repo.Owner, repo.Name)
		if !ok {
			return inconclusive(ReasonSourceVerificationInconclusive, "build source repository revision could not be resolved", nil)
		}
		revision, proposal = source.Revision, subject.Build.Failure.AIAnalysis.SuggestedFix
		files = verifiedSourceFiles(subject.Build.Failure.AIAnalysis.FileLinks, repo.Owner, repo.Name, revision)
	}
	if strings.TrimSpace(override) != "" {
		proposal = override
		targets = nil
	}
	if !regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`).MatchString(strings.TrimSpace(revision)) {
		return inconclusive(ReasonSourceVerificationInconclusive, "source revision is not an immutable full commit", nil)
	}
	revision = strings.ToLower(revision)
	slices.Sort(files)
	files = slices.Compact(files)
	rawReader := ai.NewGitHubRepoReader(repo.Owner, repo.Name, revision, s.ai.SourceToken)
	reader, ok := rawReader.(actionverify.Reader)
	if !ok {
		return inconclusive(ReasonSourceVerificationInconclusive, "pinned source archive reader is unavailable", nil)
	}
	result, err := s.sourceVerifier(ctx, reader, actionverify.Input{Proposal: proposal, RelevantFiles: files, Targets: targets})
	if err != nil {
		return inconclusive(ReasonSourceVerificationInconclusive, "pinned source could not be checked", err)
	}
	code := ReasonActionable
	switch result.State {
	case actionverify.StateAlreadyPresent:
		code = ReasonAlreadyPresent
	case actionverify.StateInconclusive:
		code = ReasonSourceVerificationInconclusive
	}
	if err := s.setRequestVerification(ctx, result.State, code, result.Reason); err != nil {
		return withReason(ReasonGenerationFailed, ErrRemediationInconclusive, "source verification result could not be persisted")
	}
	switch result.State {
	case actionverify.StateUnresolved:
		return nil
	case actionverify.StateAlreadyPresent:
		return withReason(ReasonAlreadyPresent, ErrRemediationAlreadyPresent, result.Reason)
	default:
		return withReason(ReasonSourceVerificationInconclusive, ErrRemediationInconclusive, result.Reason)
	}
}

// buildFixManager builds the fix-PR manager for the source repo using
// userToken. It does not resolve a failure; callers that need the pattern look
// it up separately.
func (s *Service) buildFixManager(ctx context.Context, userToken string) (*fixpr.Manager, error) {
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return nil, err
	}
	return s.buildFixManagerFor(ctx, userToken, destination)
}

func (s *Service) buildFixManagerFor(ctx context.Context, userToken string, destination project.FixDestination) (*fixpr.Manager, error) {
	return s.buildFixManagerForRepositoryAccess(ctx, userToken, destination, true)
}

func (s *Service) buildFixManagerForRepositoryAccess(
	ctx context.Context, userToken string, destination project.FixDestination, allowRepositoryToken bool,
) (*fixpr.Manager, error) {
	eff := s.cfg.EffectiveFixPRs()
	if destination.Repo.Owner == "" || destination.Repo.Name == "" {
		return nil, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	aiClient := s.aiClient()
	ar := eff.AgentRuntime
	if ar.Type == "opencode" && s.ai.API == ai.APIResponses {
		return nil, fmt.Errorf("local fix runtime requires chat_completions; select a remote runtime to avoid the shared model endpoint")
	}
	if aiClient == nil && ar.Type == "opencode" {
		return nil, fmt.Errorf("AI is not configured on the server; cannot draft a local fix")
	}

	critique, critiqueRetries, err := fixruntime.Critique(aiClient, eff.CritiqueRetries)
	if err != nil {
		return nil, err
	}

	var prFiller fixpr.PRBodyFiller
	if aiClient != nil {
		prFiller = repotemplate.NewPRFiller(userToken, aiClient, destination.Repo.Owner, destination.Repo.Name)
	}
	prClient := fixpr.NewClients(userToken)
	opts := fixpr.Options{
		SourceOwner:     destination.Repo.Owner,
		SourceName:      destination.Repo.Name,
		Fork:            destination.Fork,
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
	if eff.Verify != nil && eff.Verify.Enabled && ar.Type != "agent-sandbox" {
		trusted, err := runtime.TrustedLocalRuntimeEnabled()
		if err != nil {
			return nil, err
		}
		if !trusted {
			return nil, fmt.Errorf("local Fix PR verification requires %s=true on a trusted development or CI host", runtime.TrustedLocalRuntimeEnv)
		}
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
	commands, err := ar.RuntimeCommands(ar.ParsedTimeout())
	if len(destination.AllowedCommands) > 0 {
		copyRuntime := *ar
		copyRuntime.AllowedCommands = destination.AllowedCommands
		commands, err = copyRuntime.RuntimeCommands(copyRuntime.ParsedTimeout())
	}
	if err != nil {
		return nil, fmt.Errorf("agent command policy: %w", err)
	}
	model := ar.Model
	if model == "" {
		model = s.ai.Model
	}
	gitToken := previewRepositoryToken(ar.Type, userToken, allowRepositoryToken)
	opts.Agent = &fixpr.AgentConfig{
		Runtime:               agentRuntime,
		API:                   s.ai.API,
		SharedModelEndpoint:   ar.Type == "opencode",
		Model:                 model,
		Endpoint:              s.ai.Endpoint,
		ModelToken:            s.ai.Token,
		MaxTurns:              ar.MaxTurns,
		MaxFiles:              eff.MaxFiles,
		ModelProvider:         ar.ModelProvider.RuntimeConfig(),
		OutputLimitBytes:      ar.OutputLimitBytes,
		AllowBash:             allowBash,
		NetworkDomains:        ar.NetworkDomains,
		CommandPolicy:         runtime.CommandPolicy{AllowShell: allowBash, Commands: commands},
		RequireCommandResults: ar.Type == "agent-sandbox",
		Timeout:               ar.ParsedTimeout(),
		GitToken:              gitToken,
		ExecutionID:           actionRequestID(ctx),
	}
	if opts.Agent.ExecutionID != "" {
		opts.Agent.WorkObserver = s.observeRuntimeWork(opts.Agent.ExecutionID)
	}
	opts.PatchToken = userToken
	mgr := fixpr.NewManager(prClient,
		fixStateFile(s.dataDir, eff, destination), opts)
	return mgr, nil
}

func fixStateFile(dataDir string, eff project.FixPRs, destination project.FixDestination) string {
	if eff.Repo != nil && strings.EqualFold(destination.Repo.Owner, eff.Repo.Owner) && strings.EqualFold(destination.Repo.Name, eff.Repo.Name) {
		return filepath.Join(dataDir, "fix_pr_state.json")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(destination.Repo.Owner + "/" + destination.Repo.Name)))
	return filepath.Join(dataDir, ".fix-pr-state", hex.EncodeToString(sum[:8])+".json")
}

func repositoryToken(runtimeType, token string) string {
	if runtimeType == "agent-sandbox" {
		return ""
	}
	return token
}

func previewRepositoryToken(runtimeType, token string, allow bool) string {
	if !allow {
		return ""
	}
	return repositoryToken(runtimeType, token)
}

func actionUsageOutcome(err error) aiusage.Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return aiusage.OutcomeCancelled
	}
	if err != nil {
		return aiusage.OutcomeError
	}
	return aiusage.OutcomeSuccess
}

// generateIssuePreview renders an issue draft without caching or posting it.
func (s *Service) generateIssuePreview(ctx context.Context, failureID, userToken, instruction string, baseIssue *issues.IssueSpec, baseTargetRepo, basePatternHash string) (_ PreviewResult, _ *previewEntry, resultErr error) {
	logicalID := actionRequestID(ctx)
	if logicalID == "" {
		logicalID = failureID
	}
	ctx, usageOperation := aiusage.Begin(ctx, s.ai.UsageRecorder, aiusage.Metadata{
		LogicalID: logicalID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureIssueDraft,
	})
	defer func() { usageOperation.Finish(actionUsageOutcome(resultErr)) }()
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.verifyRemediation(ctx, subject); err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.verifyOptionalRemediation(ctx, subject, instruction); err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
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
	entry := &previewEntry{failureID: subject.ID, patternHash: subject.ContentHash, kind: "issue", targetRepo: targetRepo, verificationVersion: sourceVerificationVersion, spec: final}
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
	if err := s.verifyOptionalRemediation(ctx, subject, final.Title+"\n"+final.Body); err != nil {
		return PreviewResult{}, nil, err
	}
	return preview, entry, nil
}

// PreviewIssue renders the exact issue that would be filed for the failure,
// without filing it, and caches it for confirmation.
func (s *Service) PreviewIssue(ctx context.Context, failureID, owner, writeToken, instruction string) (PreviewResult, error) {
	preview, entry, err := s.generateIssuePreview(ctx, failureID, writeToken, instruction, nil, "", "")
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
	}
	token, err := s.stash(owner, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// generateFixPreview creates a fix draft without caching or opening it.
func (s *Service) generateFixPreview(ctx context.Context, failureID, userToken, instruction string) (_ PreviewResult, _ *previewEntry, resultErr error) {
	logicalID := actionRequestID(ctx)
	if logicalID == "" {
		logicalID = failureID
	}
	ctx, usageOperation := aiusage.Begin(ctx, s.ai.UsageRecorder, aiusage.Metadata{
		LogicalID: logicalID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureFixPreview,
	})
	defer func() { usageOperation.Finish(actionUsageOutcome(resultErr)) }()
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.verifyRemediation(ctx, subject); err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.verifyOptionalRemediation(ctx, subject, instruction); err != nil {
		return PreviewResult{}, nil, err
	}
	if subject.Kind == actionSubjectPattern {
		return s.generateFixPreviewForPattern(ctx, *subject.Pattern, userToken, instruction, nil)
	}
	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
		return PreviewResult{}, nil, err
	}
	eff := s.cfg.EffectiveFixPRs()
	if eff.Repo == nil {
		return PreviewResult{}, nil, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	sourceFiles := verifiedBuildSourceFiles(subject.Build, eff.Repo.Owner, eff.Repo.Name)
	if len(sourceFiles) == 0 {
		return PreviewResult{}, nil, fmt.Errorf("%w: repository source investigation did not identify a verified local path; create an issue or investigate source before proposing a fix", ErrPreviewRejected)
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return PreviewResult{}, nil, err
	}
	mgr, err := s.buildFixManager(ctx, userToken)
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
	if err := s.validateFixFiles(destination, gf.Preview.Files); err != nil {
		return PreviewResult{}, nil, fmt.Errorf("%w: %v", ErrPreviewRejected, err)
	}
	if err := s.verifyOptionalRemediation(ctx, subject, gf.Title+"\n"+gf.Description); err != nil {
		return PreviewResult{}, nil, err
	}
	return PreviewResult{Kind: gfKind, Title: gf.Title, Body: gf.Description, Diff: gf.Preview.Diff,
			VerifyStatus: string(gf.Preview.Verify.Status), VerifySummary: gf.Preview.Verify.Summary, VerifyOutput: gf.Preview.Verify.Output},
		&previewEntry{failureID: subject.ID, patternHash: subject.ContentHash, kind: gfKind, targetRepo: eff.Repo.Owner + "/" + eff.Repo.Name, targetConfig: fixTargetFingerprint(eff), verificationVersion: sourceVerificationVersion, fix: gf}, nil
}

func (s *Service) generateFixPreviewForPattern(
	ctx context.Context, pattern models.PatternAnalysis, userToken, instruction string,
	generationContext *fixpr.GenerationContext,
) (PreviewResult, *previewEntry, error) {
	verificationPattern := pattern
	sourceFiles := []string(nil)
	sourceRepository := ""
	if generationContext != nil {
		if generationContext.ProposedRevision != nil {
			verificationPattern.SuggestedFix = generationContext.ProposedRevision.SuggestedFix
			if strings.TrimSpace(verificationPattern.SuggestedFix) != strings.TrimSpace(pattern.SuggestedFix) {
				verificationPattern.RemediationTargets = []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}}
			}
		}
		if generationContext.Source != nil {
			sourceRepository = generationContext.Source.Repository
			verificationPattern.RemediationTargets = []models.RemediationTarget{generationContext.Source.Target}
			verificationPattern.SourceRef = generationContext.Source.Repository + "@" + generationContext.Source.Revision
			verificationPattern.RelevantFiles = nil
			verificationPattern.FileLinks = nil
			for _, citation := range generationContext.Source.Citations {
				sourceFiles = append(sourceFiles, citation.Path)
			}
		}
	}
	destination, targetRevision, err := s.fixDestinationForPattern(verificationPattern)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	if sourceRepository != "" && !strings.EqualFold(sourceRepository, destination.Repo.Owner+"/"+destination.Repo.Name) {
		return PreviewResult{}, nil, fmt.Errorf("%w: investigated repository does not match the configured fix target", ErrPreviewRejected)
	}
	policyText := ""
	if generationContext != nil {
		parts := []string{generationContext.AssistantAnswer}
		if generationContext.ProposedRevision != nil {
			parts = append(parts, generationContext.ProposedRevision.RootCause, generationContext.ProposedRevision.SuggestedFix)
		}
		if generationContext.Source != nil {
			parts = append(parts, generationContext.Source.Finding)
		}
		policyText = strings.Join(parts, "\n")
	}
	subject := &ActionSubject{
		Kind: actionSubjectPattern, ID: pattern.ID, ContentHash: pattern.ContentHash, Pattern: &verificationPattern, SourceFiles: sourceFiles, PolicyText: policyText,
	}
	if err := s.verifyRemediation(ctx, subject); err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.verifyOptionalRemediation(ctx, subject, instruction); err != nil {
		return PreviewResult{}, nil, err
	}
	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
		return PreviewResult{}, nil, err
	}
	generationPattern := verificationPattern
	if targetRevision != "" {
		generationPattern.SourceRef = destination.Repo.Owner + "/" + destination.Repo.Name + "@" + targetRevision
		generationPattern.RelevantFiles = nil
		generationPattern.FileLinks = nil
		for _, target := range verificationPattern.RemediationTargets {
			generationPattern.RelevantFiles = append(generationPattern.RelevantFiles, target.Path)
		}
	}
	mgr, err := s.buildFixManagerFor(ctx, userToken, destination)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	var gf *fixpr.GeneratedFix
	if generationContext == nil {
		gf, err = mgr.GeneratePreview(ctx, generationPattern, instruction)
	} else {
		gf, err = mgr.GeneratePreviewWithContext(ctx, generationPattern, instruction, *generationContext)
	}
	if err != nil {
		return PreviewResult{}, nil, safeFixPreviewError(err)
	}
	if err := s.validateFixFiles(destination, gf.Preview.Files); err != nil {
		return PreviewResult{}, nil, fmt.Errorf("%w: %v", ErrPreviewRejected, err)
	}
	if err := s.verifyOptionalRemediation(ctx, subject, gf.Title+"\n"+gf.Description); err != nil {
		return PreviewResult{}, nil, err
	}
	return PreviewResult{
		Kind: gfKind, Title: gf.Title, Body: gf.Description, Diff: gf.Preview.Diff,
		VerifyStatus: string(gf.Preview.Verify.Status), VerifySummary: gf.Preview.Verify.Summary,
		VerifyOutput: gf.Preview.Verify.Output,
	}, s.patternFixPreviewEntry(pattern, gf, destination), nil
}

func (s *Service) patternFixPreviewEntry(pattern models.PatternAnalysis, fix *fixpr.GeneratedFix, destinations ...project.FixDestination) *previewEntry {
	eff := s.cfg.EffectiveFixPRs()
	destination := project.FixDestination{Fork: eff.Fork == nil || *eff.Fork}
	if eff.Repo != nil {
		destination.Repo = *eff.Repo
	}
	if len(destinations) > 0 {
		destination = destinations[0]
	}
	return &previewEntry{
		failureID: pattern.ID, patternHash: pattern.ContentHash, kind: gfKind,
		targetRepo: destination.Repo.Owner + "/" + destination.Repo.Name, targetConfig: fixDestinationFingerprint(eff, destination),
		verificationVersion: sourceVerificationVersion, fix: fix,
	}
}

func safeFixPreviewError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrPreviewRejected, safeReason(err.Error()))
}

const gfKind = "fix"

// PreviewFix generates the exact fix PR preview and caches it for confirmation.
func (s *Service) PreviewFix(ctx context.Context, failureID, owner, writeToken, instruction string) (PreviewResult, error) {
	if err := s.requireFixActions(); err != nil {
		return PreviewResult{}, err
	}
	preview, entry, err := s.generateFixPreview(ctx, failureID, writeToken, instruction)
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
	}
	token, err := s.stash(owner, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// PreviewFixWithContext generates a fix from one validated selected chat response.
func (s *Service) PreviewFixWithContext(
	ctx context.Context, pattern models.PatternAnalysis, owner, writeToken, instruction string, target FixTarget, generationContext fixpr.GenerationContext,
) (_ PreviewResult, resultErr error) {
	if err := s.requireFixActions(); err != nil {
		return PreviewResult{}, err
	}
	logicalID := actionRequestID(ctx)
	if logicalID == "" {
		logicalID = pattern.ID
	}
	ctx, usageOperation := aiusage.Begin(ctx, s.ai.UsageRecorder, aiusage.Metadata{
		LogicalID: logicalID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureFixPreview,
	})
	defer func() { usageOperation.Finish(actionUsageOutcome(resultErr)) }()
	if strings.TrimSpace(target.JobID) == "" || strings.TrimSpace(target.BuildID) == "" ||
		pattern.JobID != target.JobID || !slices.Contains(pattern.SharedBuilds, target.BuildID) {
		return PreviewResult{}, ErrPatternMismatch
	}
	preview, entry, err := s.generateFixPreviewForPattern(ctx, pattern, writeToken, instruction, &generationContext)
	if err != nil {
		return PreviewResult{}, err
	}
	preview, err = validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
	}
	token, err := s.stash(owner, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// Confirm files the issue or opens the PR previously cached under token.
func (s *Service) Confirm(ctx context.Context, token, owner, writeToken string) (string, error) {
	lease := s.requestTimeout + 30*time.Second
	entry, resultURL, attemptID, reconcile, err := s.beginConfirm(owner, token, lease)
	if err != nil || resultURL != "" {
		return resultURL, err
	}
	if !reconcile && entry.kind == gfKind && entry.verificationVersion != sourceVerificationVersion {
		_ = s.previewStore.discard(owner, token, attemptID)
		return "", ErrPreviewTargetChanged
	}
	if !reconcile && entry.kind == gfKind && (entry.failureID == "" || entry.patternHash == "") {
		_ = s.previewStore.discard(owner, token, attemptID)
		return "", ErrPreviewTargetChanged
	}
	if !reconcile {
		if _, validateErr := validatedPreviewEntry(entry); validateErr != nil {
			_ = s.previewStore.discard(owner, token, attemptID)
			return "", withReason(previewValidationReasonCode(validateErr), ErrPreviewRejected, "")
		}
	}
	if !reconcile && entry.analysisBinding != nil {
		if err := s.validateAnalysisPreview(ctx, owner, *entry.analysisBinding); err != nil {
			_ = s.previewStore.discard(owner, token, attemptID)
			return "", ErrPreviewTargetChanged
		}
	} else if !reconcile && (entry.failureID != "" || entry.patternHash != "") {
		if err := s.validateSubjectSnapshot(entry.failureID, entry.patternHash, entry.kind); err != nil {
			_ = s.previewStore.discard(owner, token, attemptID)
			return "", err
		}
	}
	if reconcile {
		resultURL, found, reconcileErr := s.reconcileEntry(ctx, entry, writeToken)
		if reconcileErr != nil {
			if errors.Is(reconcileErr, ErrPreviewTargetChanged) {
				_ = s.previewStore.discard(owner, token, attemptID)
				return "", reconcileErr
			}
			_ = s.finishConfirm(owner, token, attemptID, "", reconcileErr)
			return "", reconcileErr
		}
		if !found {
			_ = s.finishConfirm(owner, token, attemptID, "", ErrPreviewOutcomeUnknown)
			return "", ErrPreviewOutcomeUnknown
		}
		if err := s.recordBotWrite(token, owner, entry, resultURL, botWriteReconciled); err != nil {
			_ = s.finishConfirm(owner, token, attemptID, "", err)
			return resultURL, err
		}
		if err := s.finishConfirm(owner, token, attemptID, resultURL, nil); err != nil {
			return resultURL, err
		}
		return resultURL, nil
	}
	resultURL, confirmErr := s.confirmEntry(ctx, entry, writeToken)
	if errors.Is(confirmErr, ErrPreviewTargetChanged) {
		_ = s.previewStore.discard(owner, token, attemptID)
		return "", confirmErr
	}
	finishInputErr := confirmErr
	if resultURL != "" {
		if err := s.recordBotWrite(token, owner, entry, resultURL, botWriteConfirmed); err != nil {
			_ = s.finishConfirm(owner, token, attemptID, "", err)
			return resultURL, err
		}
		finishInputErr = nil
	}
	finishErr := s.finishConfirm(owner, token, attemptID, resultURL, finishInputErr)
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

func (s *Service) beginConfirm(owner, token string, lease time.Duration) (*previewEntry, string, string, bool, error) {
	return s.previewStore.begin(owner, token, lease)
}

func (s *Service) finishConfirm(owner, token, attemptID, resultURL string, confirmErr error) error {
	return s.previewStore.finish(owner, token, attemptID, resultURL, confirmErr)
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
		if err := s.requireFixActions(); err != nil {
			return "", false, err
		}
		eff := s.cfg.EffectiveFixPRs()
		if entry.fix == nil {
			return "", false, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
		}
		destination, err := s.fixDestinationForEntry(entry)
		if err != nil || entry.targetRepo != destination.Repo.Owner+"/"+destination.Repo.Name {
			return "", false, ErrPreviewTargetChanged
		}
		if entry.targetConfig != fixDestinationFingerprint(eff, destination) {
			return "", false, ErrPreviewTargetChanged
		}
		key := entry.fix.Snapshot().Key
		client := fixpr.NewClients(userToken)
		if strings.HasPrefix(entry.failureID, "build::") || entry.analysisBinding != nil {
			_, url, found, err := client.SearchAnyPR(ctx, destination.Repo.Owner, destination.Repo.Name, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
			return url, found, err
		}
		_, url, found, err := client.SearchOpenPR(ctx, destination.Repo.Owner, destination.Repo.Name, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
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
	if entry != nil && entry.failureID != "" && entry.analysisBinding == nil {
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
		if strings.HasPrefix(entry.failureID, "build::") || entry.analysisBinding != nil {
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
		if err := s.requireFixActions(); err != nil {
			return "", err
		}
		eff := s.cfg.EffectiveFixPRs()
		destination, err := s.fixDestinationForEntry(entry)
		if err != nil || entry.targetRepo != destination.Repo.Owner+"/"+destination.Repo.Name {
			return "", ErrPreviewTargetChanged
		}
		if entry.targetConfig != fixDestinationFingerprint(eff, destination) {
			return "", ErrPreviewTargetChanged
		}
		mgr, err := s.buildFixManagerFor(ctx, userToken, destination)
		if err != nil {
			return "", err
		}
		url, err := mgr.OpenFromPreview(ctx, entry.fix)
		if err != nil {
			if errors.Is(err, fixpr.ErrPreviewBaseChanged) {
				return "", ErrPreviewTargetChanged
			}
			if errors.Is(err, fixpr.ErrWriteOutcomeUnknown) {
				return "", fmt.Errorf("%w: opening fix: %s", ErrPreviewOutcomeUnknown, safeReason(err.Error()))
			}
			return "", fmt.Errorf("%s", safeReason(err.Error()))
		}
		if strings.HasPrefix(entry.failureID, "build::") || entry.analysisBinding != nil {
			mgr.Forget(entry.fix.Snapshot().Key)
		}
		if err := mgr.SaveState(); err != nil {
			if strings.HasPrefix(entry.failureID, "build::") || entry.analysisBinding != nil {
				log.Printf("Warning: single-analysis fix state cleanup failed after opening %s: %v", url, err)
				return url, nil
			}
			return url, fmt.Errorf("saving fix-PR state: %w", err)
		}
		return url, nil
	}
	return "", ErrPreviewNotFound
}

func fixTargetFingerprint(eff project.FixPRs) string {
	destination := project.FixDestination{Fork: eff.Fork == nil || *eff.Fork}
	if eff.Repo != nil {
		destination.Repo = *eff.Repo
	}
	return fixDestinationFingerprint(eff, destination)
}

func fixDestinationFingerprint(eff project.FixPRs, destination project.FixDestination) string {
	payload, _ := json.Marshal(struct {
		Repository  string                    `json:"repository"`
		Fork        bool                      `json:"fork"`
		AuthorName  string                    `json:"author_name"`
		AuthorEmail string                    `json:"author_email"`
		Labels      []string                  `json:"labels"`
		Commands    []project.FixAgentCommand `json:"commands,omitempty"`
	}{destination.Repo.Owner + "/" + destination.Repo.Name, destination.Fork, eff.AuthorName, eff.AuthorEmail, eff.Labels, destination.AllowedCommands})
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
	if !models.PatternAllowsActions(*pa) {
		return withReason(ReasonContractGenerationFailed, ErrRemediationInconclusive, "This causal-group result is analysis-only and cannot start an action.")
	}
	if !pa.Systemic {
		return fmt.Errorf("only systemic recurring patterns can be resolved")
	}
	if !models.PatternIsActive(*pa) {
		return fmt.Errorf("inactive recurring patterns cannot be manually resolved")
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
	pattern, err := s.findPattern(failureID)
	if err != nil {
		return err
	}
	if !models.PatternAllowsActions(*pattern) {
		return withReason(ReasonContractGenerationFailed, ErrRemediationInconclusive, "This causal-group result is analysis-only and cannot start an action.")
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
func (s *Service) stash(owner string, entry *previewEntry) (string, error) {
	return s.previewStore.stash(owner, entry)
}

// take removes one persisted preview for compatibility with direct callers.
func (s *Service) take(owner, token string) (*previewEntry, error) {
	return s.previewStore.take(owner, token)
}

// tokenHash binds a preview to the admin who generated it without retaining the
// raw owner identifier.
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
