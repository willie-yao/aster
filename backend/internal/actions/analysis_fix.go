package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/buildsource"
	fixpr "github.com/willie-yao/aster/backend/internal/fix/pr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/remediationpolicy"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	maxAnalysisSourceFiles      = 16
	maxAnalysisFixCitations     = 16
	maxAnalysisFailureTextBytes = 8 << 10
	analysisFixHandoffVersion   = 6
)

// FixBranchTarget selects the branch an exact-analysis Fix patches and targets.
type FixBranchTarget string

const (
	// FixBranchTested generates against and targets the failure's tested branch.
	FixBranchTested FixBranchTarget = "tested"
	// FixBranchDefault generates against and targets the repository's default branch.
	FixBranchDefault FixBranchTarget = "default"
)

// Valid reports whether t is a supported choice.
func (t FixBranchTarget) Valid() bool {
	return t == FixBranchTested || t == FixBranchDefault
}

// ParseFixBranchTarget reads a client choice. An omitted choice is the tested branch.
func ParseFixBranchTarget(value string) (FixBranchTarget, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return FixBranchTested, nil
	}
	if target := FixBranchTarget(value); target.Valid() {
		return target, nil
	}
	return "", fmt.Errorf("unsupported Fix branch target %q", value)
}

// AnalysisFixBranches names the branches an exact-analysis Fix can target.
type AnalysisFixBranches struct {
	Tested  string `json:"tested_branch,omitempty"`
	Default string `json:"default_branch,omitempty"`
}

// AnalysisIdentity identifies one exact published failure analysis.
type AnalysisIdentity struct {
	Project             string `json:"project"`
	JobID               string `json:"job_id"`
	BuildID             string `json:"build_id"`
	TestName            string `json:"test_name"`
	Source              string `json:"source,omitempty"`
	SuiteName           string `json:"suite_name,omitempty"`
	ClassName           string `json:"class_name,omitempty"`
	JUnitFile           string `json:"junit_file,omitempty"`
	AnalysisGeneratedAt string `json:"analysis_generated_at"`
}

// AnalysisActionSubject is one current failed test or build analysis.
type AnalysisActionSubject struct {
	ID                  string
	ContentHash         string
	AnalysisContentHash string
	Identity            AnalysisIdentity
	JobName             string
	Build               models.BuildInfo
	Failure             models.TestCase
	SourceRepository    sourceinvestigation.Repository
	SourceHints         []string
}

type analysisTargetSnapshot struct {
	GeneratedAt    string `json:"generated_at"`
	RootCause      string `json:"root_cause"`
	Severity       string `json:"severity"`
	SuggestedFix   string `json:"suggested_fix"`
	CritiquePassed bool   `json:"critique_passed"`
}

// AnalysisFixInput is one owner-bound chat finding selected for fix generation.
type AnalysisFixInput struct {
	Origin                    analysischat.FixOrigin
	Version                   int
	Owner                     string
	Instruction               string
	TargetConfig              string
	HandoffHash               string
	Identity                  AnalysisIdentity
	ChatSessionID             string
	ChatRequestID             string
	ChatResponseHash          string
	PreviewRequestHash        string
	AnalysisContentHash       string
	FailureContentHash        string
	SourceHints               []string
	TargetAnalysis            analysisTargetSnapshot
	SourceRepository          sourceinvestigation.Repository
	FailureRevision           string
	GenerationBaseRevision    string
	SourceBranch              string
	BranchTarget              FixBranchTarget
	TargetBranch              string
	AssistantAnswer           string
	AssistantUnverified       bool
	AssistantUnverifiedReason string
	ProposedRevision          *fixpr.RevisionContext
	ArtifactCitations         []fixpr.Evidence
	EvidenceWarnings          []string
}

// AnalysisPreviewBinding retains the immutable action-owned finding.
type AnalysisPreviewBinding struct {
	Handoff *AnalysisFixInput `json:"handoff"`
}

type analysisSourceRevisionClient interface {
	ResolveBase(context.Context, string, string, string) (ghpr.Base, error)
	CompareCommits(context.Context, string, string, string, string) (bool, string, error)
}

type analysisSourceCompatibility struct {
	TargetBranch           string
	GenerationBaseRevision string
}

var errAnalysisSourceUnavailable = errors.New("GitHub source check unavailable; retry the action")

func analysisSourceCheckError(operation string, repo sourceinvestigation.Repository, err error) error {
	cause := fmt.Errorf("%w: %s in %s/%s: %w", errAnalysisSourceUnavailable, operation, repo.Owner, repo.Name, err)
	return withReason(ReasonGenerationFailed, cause, cause.Error())
}

const (
	analysisWarningCritique            = "The original analysis critique did not pass."
	analysisWarningSuggestedFix        = "The original analysis has no suggested fix."
	analysisWarningRootCause           = "The original analysis root cause is incomplete."
	analysisWarningTransient           = "The original analysis is marked transient."
	analysisWarningProse               = "The model-authored remediation prose is incomplete."
	analysisWarningPolicy              = "The model-authored prose triggered a text-only remediation-policy concern."
	analysisWarningEvidenceQualified   = "The selected chat finding has evidence qualification warnings; treat warned claims as hypotheses."
	analysisWarningAssistantUnverified = "The selected chat answer is unverified; treat it as an investigation hypothesis."
	analysisWarningNoCitations         = "The selected chat answer has no retained artifact citations; treat it as an investigation hypothesis."
	analysisWarningNoSourceHints       = "The published analysis has no source hints; the coding agent must investigate the repository."
)

// ResolveAnalysisActionSubject resolves one current failed test or build analysis.
func (s *Service) ResolveAnalysisActionSubject(identity AnalysisIdentity) (*AnalysisActionSubject, error) {
	return s.resolveAnalysisActionSubject(identity, true)
}

func (s *Service) resolveAnalysisActionSubject(identity AnalysisIdentity, requireAnalysis bool) (*AnalysisActionSubject, error) {
	identity = normalizeAnalysisIdentity(identity)
	if err := validateAnalysisIdentity(identity); err != nil {
		return nil, err
	}
	if s.cfg == nil || strings.TrimSpace(s.cfg.Name) == "" || identity.Project != strings.TrimSpace(s.cfg.Name) {
		return nil, ErrNotFound
	}
	data, err := osReadFile(s.dataDir, identity.JobID)
	if err != nil {
		return nil, ErrNotFound
	}
	var detail models.JobDetail
	if json.Unmarshal(data, &detail) != nil || detail.JobID != "" && detail.JobID != identity.JobID {
		return nil, ErrNotFound
	}
	var matches []struct {
		run      models.BuildInfo
		jobName  string
		testCase models.TestCase
	}
	for _, run := range detail.Runs {
		if run.BuildID != identity.BuildID {
			continue
		}
		for _, testCase := range run.TestCases {
			if strings.TrimSpace(testCase.Name) != identity.TestName || strings.TrimSpace(testCase.Source) != identity.Source ||
				strings.TrimSpace(testCase.SuiteName) != identity.SuiteName || strings.TrimSpace(testCase.ClassName) != identity.ClassName ||
				strings.TrimSpace(testCase.JUnitFile) != identity.JUnitFile {
				continue
			}
			matches = append(matches, struct {
				run      models.BuildInfo
				jobName  string
				testCase models.TestCase
			}{run.BuildInfo, detail.Name, testCase})
		}
	}
	if len(matches) != 1 {
		return nil, ErrNotFound
	}
	match := matches[0]
	analysis := match.testCase.AIAnalysis
	if match.run.Passed || match.testCase.Status != "failed" ||
		requireAnalysis && (analysis == nil || analysis.GeneratedAt != identity.AnalysisGeneratedAt) {
		return nil, fmt.Errorf("analysis is not a current failed-test or build action target")
	}
	analysisRepo := s.cfg.EffectiveAnalysisSourceRepo()
	source, ok := ai.ResolveBuildSource(match.run, analysisRepo.Owner, analysisRepo.Name)
	if !ok {
		return nil, fmt.Errorf("%w: analysis build source repository revision could not be resolved", ErrPreviewRejected)
	}
	repository := sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return nil, fmt.Errorf("%w: analysis immutable source identity is unavailable", ErrPreviewRejected)
	}
	var sourceHints []string
	if analysis != nil {
		sourceHints = verifiedSourceFiles(analysis.FileLinks, repository.Owner, repository.Name, repository.Revision)
		slices.Sort(sourceHints)
		sourceHints = slices.Compact(sourceHints)
		if len(sourceHints) > maxAnalysisSourceFiles {
			sourceHints = sourceHints[:maxAnalysisSourceFiles]
		}
	}
	subject := &AnalysisActionSubject{
		Identity: identity, JobName: match.jobName, Build: match.run, Failure: match.testCase,
		SourceRepository: repository, SourceHints: sourceHints,
	}
	if requireAnalysis {
		subject.AnalysisContentHash = models.TestAnalysisContentHash(match.testCase)
	}
	subject.ID = analysisActionID(identity)
	subject.ContentHash = analysisActionHash(subject)
	return subject, nil
}

func osReadFile(dataDir, jobID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID)))
}

// preflightAnalysisFixSource binds a fix request to the current base of the selected branch.
func (s *Service) preflightAnalysisFixSource(
	ctx context.Context, repo sourceinvestigation.Repository, testedBranch string, target FixBranchTarget,
) (analysisSourceCompatibility, error) {
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repo, testedBranch, target)
	if err != nil {
		if _, ok := ReasonCodeFrom(err); ok {
			return analysisSourceCompatibility{}, err
		}
		return analysisSourceCompatibility{}, fmt.Errorf("%w: exact analysis Fix repository/base preflight: %w", ErrPreviewRejected, err)
	}
	return compatibility, nil
}

// ResolveAnalysisFixBranches reports the tested branch and the Fix repository's
// default branch. The default branch is omitted when the tested branch is unknown.
func (s *Service) ResolveAnalysisFixBranches(
	ctx context.Context, repo sourceinvestigation.Repository, testedBranch string,
) (AnalysisFixBranches, error) {
	if err := s.requireFixActions(); err != nil {
		return AnalysisFixBranches{}, err
	}
	branches := AnalysisFixBranches{Tested: strings.TrimSpace(testedBranch)}
	if branches.Tested == "" {
		return branches, nil
	}
	if err := sourceinvestigation.ValidateRepository(repo); err != nil {
		return AnalysisFixBranches{}, err
	}
	base, err := s.analysisRevisionClient().ResolveBase(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return AnalysisFixBranches{}, analysisSourceCheckError("resolving the default branch", repo, err)
	}
	branches.Default = base.Branch
	return branches, nil
}

func (s *Service) analysisRevisionClient() analysisSourceRevisionClient {
	if s.sourceRevisionClient != nil {
		return s.sourceRevisionClient
	}
	return ghpr.NewClient(nil, s.ai.SourceToken)
}

func (s *Service) verifyAnalysisSourceCompatibility(
	ctx context.Context, failureRepo sourceinvestigation.Repository, testedBranch string, target FixBranchTarget,
) (analysisSourceCompatibility, error) {
	if err := sourceinvestigation.ValidateRepository(failureRepo); err != nil {
		return analysisSourceCompatibility{}, err
	}
	testedBranch = strings.TrimSpace(testedBranch)
	if testedBranch == "" {
		return analysisSourceCompatibility{}, withReason(ReasonSourceBranchUnknown, ErrPreviewRejected, "")
	}
	branch := testedBranch
	switch target {
	case FixBranchTested:
	case FixBranchDefault:
		// The tested branch is not resolved here but is still named in the PR.
		if !ghpr.ValidBranchName(testedBranch) {
			return analysisSourceCompatibility{}, withReason(ReasonSourceBranchUnknown, ErrPreviewRejected, "")
		}
		branch = ""
	default:
		return analysisSourceCompatibility{}, fmt.Errorf("unsupported Fix branch target %q", target)
	}
	if s.cfg == nil {
		return analysisSourceCompatibility{}, fmt.Errorf("project configuration is unavailable")
	}
	analysisRepo := s.cfg.EffectiveAnalysisSourceRepo()
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return analysisSourceCompatibility{}, err
	}
	if !strings.EqualFold(failureRepo.Owner, analysisRepo.Owner) || !strings.EqualFold(failureRepo.Name, analysisRepo.Name) ||
		!strings.EqualFold(failureRepo.Owner, destination.Repo.Owner) || !strings.EqualFold(failureRepo.Name, destination.Repo.Name) {
		return analysisSourceCompatibility{}, fmt.Errorf("analysis source and fix repositories do not match")
	}
	failureRevision, ok := buildsource.NormalizeRevision(failureRepo.Revision)
	if !ok {
		return analysisSourceCompatibility{}, fmt.Errorf("failure revision is not an immutable full commit")
	}
	client := s.analysisRevisionClient()
	base, err := client.ResolveBase(ctx, failureRepo.Owner, failureRepo.Name, branch)
	if err != nil {
		return analysisSourceCompatibility{}, analysisSourceCheckError("resolving current generation base", failureRepo, err)
	}
	switch {
	case target == FixBranchTested && base.Branch != testedBranch:
		return analysisSourceCompatibility{}, fmt.Errorf("resolved generation base branch does not match the failure branch")
	case target == FixBranchDefault && (base.Branch == "" || base.Branch == testedBranch):
		return analysisSourceCompatibility{}, fmt.Errorf("the default branch is the tested branch; target the tested branch instead")
	}
	generationBase, ok := buildsource.NormalizeRevision(base.HeadSHA)
	if !ok {
		return analysisSourceCompatibility{}, fmt.Errorf("generation base is not an immutable full commit")
	}
	// Another branch has its own history, so ancestry only applies to the tested branch.
	if target == FixBranchTested && !strings.EqualFold(failureRevision, generationBase) {
		contains, _, err := client.CompareCommits(ctx, failureRepo.Owner, failureRepo.Name, failureRevision, generationBase)
		if err != nil {
			return analysisSourceCompatibility{}, analysisSourceCheckError("checking failure revision ancestry", failureRepo, err)
		}
		if !contains {
			return analysisSourceCompatibility{}, withReason(ReasonSourceRevisionDiverged, ErrPreviewRejected, "")
		}
	}
	return analysisSourceCompatibility{TargetBranch: base.Branch, GenerationBaseRevision: generationBase}, nil
}

// PreviewAnalysisFix creates a confirmable preview for one exact selected chat answer.
func (s *Service) PreviewAnalysisFix(
	ctx context.Context, input AnalysisFixInput, owner, writeToken, instruction string,
) (_ PreviewResult, resultErr error) {
	if err := s.requireFixActions(); err != nil {
		return PreviewResult{}, err
	}
	owner = normalizeActionOwner(owner)
	if owner == "" {
		return PreviewResult{}, fmt.Errorf("preview owner is required")
	}
	if s.cfg != nil {
		input.Identity.Project = strings.TrimSpace(s.cfg.Name)
	}
	if err := validateAnalysisFixHandoff(input); err != nil {
		return PreviewResult{}, err
	}
	if owner != input.Owner || strings.TrimSpace(instruction) != input.Instruction {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	subject, err := s.resolveAnalysisFixTarget(input, true)
	if err != nil {
		return PreviewResult{}, err
	}
	logicalID := actionRequestID(ctx)
	if logicalID == "" {
		logicalID = input.ChatRequestID
	}
	ctx, usageOperation := aiusage.Begin(ctx, s.ai.UsageRecorder, aiusage.Metadata{
		LogicalID: logicalID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureFixPreview,
		Correlation: aiusage.Correlation{JobID: input.Identity.JobID, BuildID: input.Identity.BuildID, TestName: input.Identity.TestName},
	})
	defer func() { usageOperation.Finish(actionUsageOutcome(resultErr)) }()

	eff := s.cfg.EffectiveFixPRs()
	if !eff.Enabled || eff.AgentRuntime == nil || eff.AgentRuntime.Type != "agent-sandbox" {
		return PreviewResult{}, fmt.Errorf("%w: exact analysis fix preview requires the Agent Sandbox Fix runtime", ErrPreviewRejected)
	}
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return PreviewResult{}, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	analysisRepo := s.cfg.EffectiveAnalysisSourceRepo()
	if analysisRepo.Owner == "" || analysisRepo.Name == "" ||
		!strings.EqualFold(analysisRepo.Owner, eff.Repo.Owner) || !strings.EqualFold(analysisRepo.Name, eff.Repo.Name) {
		return PreviewResult{}, fmt.Errorf("%w: analysis source and fix repositories must match", ErrPreviewRejected)
	}
	repository := subject.SourceRepository
	if !strings.EqualFold(repository.Owner, analysisRepo.Owner) || !strings.EqualFold(repository.Name, analysisRepo.Name) ||
		input.SourceRepository != repository {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return PreviewResult{}, fmt.Errorf("%w: immutable source identity is unavailable", ErrPreviewRejected)
	}
	findingText := input.AssistantAnswer
	if input.ProposedRevision != nil {
		findingText += "\n" + input.ProposedRevision.RootCause + "\n" + input.ProposedRevision.SuggestedFix
	}
	if remediationpolicy.RelationshipTextWarning(instruction) != "" {
		return PreviewResult{}, withReason(ReasonUnsafeRemediation, ErrPreviewRejected, "")
	}
	warnings := analysisQualityWarnings(input)
	if remediationpolicy.RelationshipTextWarning(findingText) != "" {
		warnings = append(warnings, ActionWarning{Message: analysisWarningPolicy})
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return PreviewResult{}, err
	}
	if !strings.EqualFold(destination.Repo.Owner, repository.Owner) || !strings.EqualFold(destination.Repo.Name, repository.Name) {
		return PreviewResult{}, fmt.Errorf("%w: failure source does not match the configured fix destination", ErrPreviewRejected)
	}
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repository, input.SourceBranch, input.BranchTarget)
	if err != nil {
		if _, ok := ReasonCodeFrom(err); ok {
			return PreviewResult{}, err
		}
		return PreviewResult{}, fmt.Errorf("%w: checking exact analysis Fix repository/base: %w", ErrPreviewRejected, err)
	}
	if !strings.EqualFold(input.FailureRevision, repository.Revision) || compatibility.TargetBranch != input.TargetBranch ||
		!strings.EqualFold(input.GenerationBaseRevision, compatibility.GenerationBaseRevision) {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	if err := s.setRequestWarnings(ctx, warnings...); err != nil {
		return PreviewResult{}, err
	}

	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
		return PreviewResult{}, err
	}
	targetConfig := fixDestinationFingerprint(eff, destination)
	if targetConfig != input.TargetConfig {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	generationHash := analysisPreviewGenerationHash(subject, input, targetConfig)
	token, existing, acquired, err := s.previewStore.reserveIdempotent(
		owner, input.PreviewRequestHash, generationHash, s.analysisPreviewLease(),
	)
	if err != nil {
		return PreviewResult{}, err
	}
	if !acquired {
		if existing != nil && existing.fix != nil {
			if err := s.setRequestWarnings(ctx, plainWarnings(existing.fix.Warnings...)...); err != nil {
				return PreviewResult{}, err
			}
		}
		preview, err := validatedPreviewEntry(existing)
		if err != nil {
			return PreviewResult{}, classifiedAnalysisPreviewValidationError(err)
		}
		preview.Token = token
		return preview, nil
	}
	reserved := true
	defer func() {
		if reserved && resultErr != nil {
			_ = s.previewStore.cancelIdempotent(owner, token, input.PreviewRequestHash, generationHash)
		}
	}()
	mgr, err := s.buildFixManagerForRepositoryAccess(ctx, writeToken, destination, false)
	if err != nil {
		return PreviewResult{}, err
	}
	failure := analysisFailureForGeneration(subject, input)
	gf, err := mgr.GenerateAnalysisPreview(ctx, failure, instruction)
	if err != nil {
		return PreviewResult{}, safeAnalysisFixPreviewError(err)
	}
	if err := s.setRequestWarnings(ctx, plainWarnings(gf.Warnings...)...); err != nil {
		return PreviewResult{}, err
	}
	if err := s.validateFixFiles(destination, gf.Preview.Files); err != nil {
		return PreviewResult{}, classifiedAnalysisFixFailure(
			ReasonUnsafeRemediation, AnalysisFixFailureSafetyIntegrity, "", fmt.Errorf("%w: %v", ErrPreviewRejected, err),
		)
	}
	entry := &previewEntry{
		failureID: subject.ID, patternHash: subject.ContentHash, kind: gfKind,
		targetRepo: destination.Repo.Owner + "/" + destination.Repo.Name, targetConfig: targetConfig,
		verificationVersion: sourceVerificationVersion, fix: gf,
		analysisBinding: &AnalysisPreviewBinding{Handoff: cloneAnalysisFixInput(input)},
	}
	preview, err := validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, classifiedAnalysisPreviewValidationError(err)
	}
	if err := s.validateAnalysisPreview(ctx, owner, *entry.analysisBinding); err != nil {
		return PreviewResult{}, err
	}
	if err := s.previewStore.completeIdempotent(owner, token, input.PreviewRequestHash, generationHash, entry); err != nil {
		return PreviewResult{}, err
	}
	reserved = false
	preview.Token = token
	return preview, nil
}

func (s *Service) analysisPreviewLease() time.Duration {
	return s.requestTimeout + 30*time.Second
}

func analysisFailureForGeneration(subject *AnalysisActionSubject, input AnalysisFixInput) fixpr.AnalysisFailure {
	return fixpr.AnalysisFailure{
		ID: subject.ID, Project: subject.Identity.Project, JobID: subject.Identity.JobID, JobName: subject.JobName,
		BuildID: subject.Identity.BuildID, TestName: subject.Identity.TestName, Source: subject.Identity.Source,
		ProwURL:             subject.Build.ProwURL,
		AnalysisGeneratedAt: input.TargetAnalysis.GeneratedAt,
		AnalysisHash:        subject.ContentHash, RootCause: input.TargetAnalysis.RootCause, SuggestedFix: input.TargetAnalysis.SuggestedFix,
		FailureMessage:  boundedAnalysisFailureText(subject.Failure.FailureMessage),
		FailureBody:     boundedAnalysisFailureText(subject.Failure.FailureBody),
		AssistantAnswer: input.AssistantAnswer, AssistantUnverified: input.AssistantUnverified,
		AssistantUnverifiedReason: input.AssistantUnverifiedReason,
		ChatResponseHash:          input.ChatResponseHash, PreviewRequestHash: input.PreviewRequestHash,
		ProposedRevision:  input.ProposedRevision,
		ArtifactCitations: slices.Clone(input.ArtifactCitations), EvidenceWarnings: slices.Clone(input.EvidenceWarnings),
		SourceRepository: subject.SourceRepository.Owner + "/" + subject.SourceRepository.Name,
		SourceBranch:     input.SourceBranch, TargetBranch: input.TargetBranch,
		FailureRevision: subject.SourceRepository.Revision, GenerationBaseRevision: input.GenerationBaseRevision,
		SourceHints: slices.Clone(input.SourceHints),
	}
}

func analysisPreviewGenerationHash(subject *AnalysisActionSubject, input AnalysisFixInput, targetConfig string) string {
	payload, _ := json.Marshal(struct {
		AnalysisID, AnalysisHash, AnalysisContentHash, RequestHash       string
		Repository                                                       sourceinvestigation.Repository
		SourceBranch, TargetBranch, GenerationBaseRevision, TargetConfig string
	}{
		subject.ID, subject.ContentHash, subject.AnalysisContentHash, input.HandoffHash,
		input.SourceRepository, input.SourceBranch, input.TargetBranch, input.GenerationBaseRevision, targetConfig,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func analysisFixReplacementHash(input AnalysisFixInput) string {
	input.Instruction, input.PreviewRequestHash = "", ""
	return analysisFixHandoffHash(input)
}

func analysisQualityWarnings(input AnalysisFixInput) []ActionWarning {
	analysis := input.TargetAnalysis
	var messages []string
	if !analysis.CritiquePassed {
		messages = append(messages, analysisWarningCritique)
	}
	if strings.TrimSpace(analysis.SuggestedFix) == "" {
		messages = append(messages, analysisWarningSuggestedFix)
	}
	if strings.TrimSpace(analysis.RootCause) == "" && strings.TrimSpace(input.AssistantAnswer) != "" {
		messages = append(messages, analysisWarningRootCause)
	}
	if strings.EqualFold(strings.TrimSpace(analysis.Severity), "Transient-Ignore") {
		messages = append(messages, analysisWarningTransient)
	}
	if input.ProposedRevision != nil && (strings.TrimSpace(input.ProposedRevision.RootCause) == "" || strings.TrimSpace(input.ProposedRevision.SuggestedFix) == "") {
		messages = append(messages, analysisWarningProse)
	}
	warnings := plainWarnings(messages...)
	if len(input.EvidenceWarnings) > 0 {
		warnings = append(warnings, ActionWarning{Code: WarningEvidenceQualified, Message: analysisWarningEvidenceQualified})
	}
	if input.AssistantUnverified {
		warnings = append(warnings, ActionWarning{Code: WarningAssistantUnverified, Message: analysisWarningAssistantUnverified})
	}
	if len(input.ArtifactCitations) == 0 {
		warnings = append(warnings, ActionWarning{Code: WarningNoCitations, Message: analysisWarningNoCitations})
	}
	if len(input.SourceHints) == 0 {
		warnings = append(warnings, ActionWarning{Message: analysisWarningNoSourceHints})
	}
	return warnings
}

func normalizeAnalysisIdentity(identity AnalysisIdentity) AnalysisIdentity {
	identity.Project = strings.TrimSpace(identity.Project)
	identity.JobID = strings.TrimSpace(identity.JobID)
	identity.BuildID = strings.TrimSpace(identity.BuildID)
	identity.TestName = strings.TrimSpace(identity.TestName)
	identity.Source = strings.TrimSpace(identity.Source)
	identity.SuiteName = strings.TrimSpace(identity.SuiteName)
	identity.ClassName = strings.TrimSpace(identity.ClassName)
	identity.JUnitFile = strings.TrimSpace(identity.JUnitFile)
	identity.AnalysisGeneratedAt = strings.TrimSpace(identity.AnalysisGeneratedAt)
	return identity
}

func validateAnalysisIdentity(identity AnalysisIdentity) error {
	if identity.Project == "" || identity.JobID == "" || identity.BuildID == "" || identity.TestName == "" ||
		identity.AnalysisGeneratedAt == "" {
		return fmt.Errorf("%w: exact analysis identity is incomplete", ErrNotFound)
	}
	switch identity.Source {
	case models.TestCaseSourceBuild:
		if identity.JUnitFile != "" {
			return fmt.Errorf("%w: build analysis identity must not include junit_file", ErrNotFound)
		}
	case "":
		if identity.JUnitFile == "" {
			return fmt.Errorf("%w: JUnit analysis identity requires junit_file", ErrNotFound)
		}
	default:
		return fmt.Errorf("%w: unsupported analysis source", ErrNotFound)
	}
	return nil
}

func validateAnalysisFixCandidateInput(input AnalysisFixInput) error {
	input.Identity = normalizeAnalysisIdentity(input.Identity)
	if err := validateAnalysisIdentity(input.Identity); err != nil {
		return err
	}
	if strings.TrimSpace(input.ChatSessionID) == "" || strings.TrimSpace(input.ChatRequestID) == "" ||
		strings.TrimSpace(input.ChatResponseHash) == "" || strings.TrimSpace(input.PreviewRequestHash) == "" || strings.TrimSpace(input.AnalysisContentHash) == "" ||
		strings.TrimSpace(input.AssistantAnswer) == "" || sourceinvestigation.ValidateRepository(input.SourceRepository) != nil ||
		!input.BranchTarget.Valid() ||
		len(input.ArtifactCitations) > maxAnalysisFixCitations || len(input.AssistantUnverifiedReason) > 512 || len(input.EvidenceWarnings) > 20 {
		return fmt.Errorf("invalid exact analysis fix request")
	}
	for _, warning := range input.EvidenceWarnings {
		if strings.TrimSpace(warning) == "" || len(warning) > 512 {
			return fmt.Errorf("invalid exact analysis Fix evidence warning")
		}
	}
	return analysisFixContext(input).Validate()
}

func validateAnalysisFixInput(input AnalysisFixInput) error {
	if err := validateAnalysisFixCandidateInput(input); err != nil {
		return err
	}
	if input.FailureContentHash == "" || input.TargetAnalysis.GeneratedAt != input.Identity.AnalysisGeneratedAt ||
		len(input.SourceHints) > maxAnalysisSourceFiles || len(input.TargetAnalysis.RootCause) > 32<<10 ||
		len(input.TargetAnalysis.SuggestedFix) > 16<<10 {
		return ErrPreviewTargetChanged
	}
	return nil
}

func boundedAnalysisFailureText(value string) string {
	if len(value) <= maxAnalysisFailureTextBytes {
		return value
	}
	return textutil.Truncate(value, maxAnalysisFailureTextBytes-len("…"))
}

func analysisActionID(identity AnalysisIdentity) string {
	data, _ := json.Marshal(identity)
	sum := sha256.Sum256(data)
	return "analysis::" + hex.EncodeToString(sum[:])
}

func analysisActionHash(subject *AnalysisActionSubject) string {
	payload, _ := json.Marshal(struct {
		Identity            AnalysisIdentity
		AnalysisContentHash string
	}{subject.Identity, subject.AnalysisContentHash})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func cloneAnalysisPreviewBinding(binding *AnalysisPreviewBinding) *AnalysisPreviewBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	if binding.Handoff != nil {
		copy.Handoff = cloneAnalysisFixInput(*binding.Handoff)
	}
	return &copy
}
