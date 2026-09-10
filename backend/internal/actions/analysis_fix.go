package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/willie-yao/aster/backend/internal/fixpr"
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
	analysisFixHandoffVersion   = 4
)

// AnalysisIdentity identifies one exact published JUnit analysis.
type AnalysisIdentity struct {
	Project             string `json:"project"`
	JobID               string `json:"job_id"`
	BuildID             string `json:"build_id"`
	TestName            string `json:"test_name"`
	Source              string `json:"source,omitempty"`
	SuiteName           string `json:"suite_name,omitempty"`
	ClassName           string `json:"class_name,omitempty"`
	JUnitFile           string `json:"junit_file"`
	AnalysisGeneratedAt string `json:"analysis_generated_at"`
}

// AnalysisActionSubject is one current failed JUnit analysis.
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
	SourceRepository          sourceinvestigation.Repository
	FailureRevision           string
	GenerationBaseRevision    string
	SourceBranch              string
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
	GenerationBaseRevision string
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

// ResolveAnalysisActionSubject resolves and validates one current failed JUnit analysis.
func (s *Service) ResolveAnalysisActionSubject(identity AnalysisIdentity) (*AnalysisActionSubject, error) {
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
	if match.run.Passed || match.testCase.Status != "failed" || match.testCase.Source == models.TestCaseSourceBuild || match.testCase.JUnitFile == "" ||
		analysis == nil || analysis.GeneratedAt != identity.AnalysisGeneratedAt {
		return nil, fmt.Errorf("JUnit analysis is not a current failed-test action target")
	}
	analysisRepo := s.cfg.EffectiveAnalysisSourceRepo()
	source, ok := ai.ResolveBuildSource(match.run, analysisRepo.Owner, analysisRepo.Name)
	if !ok {
		return nil, fmt.Errorf("%w: JUnit analysis build source repository revision could not be resolved", ErrPreviewRejected)
	}
	repository := sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return nil, fmt.Errorf("%w: JUnit analysis immutable source identity is unavailable", ErrPreviewRejected)
	}
	sourceHints := verifiedSourceFiles(analysis.FileLinks, repository.Owner, repository.Name, repository.Revision)
	slices.Sort(sourceHints)
	sourceHints = slices.Compact(sourceHints)
	if len(sourceHints) > maxAnalysisSourceFiles {
		sourceHints = sourceHints[:maxAnalysisSourceFiles]
	}
	subject := &AnalysisActionSubject{
		Identity: identity, JobName: match.jobName, Build: match.run, Failure: match.testCase,
		SourceRepository: repository, SourceHints: sourceHints,
	}
	subject.AnalysisContentHash = models.TestAnalysisContentHash(match.testCase)
	subject.ID = analysisActionID(identity)
	subject.ContentHash = analysisActionHash(subject)
	return subject, nil
}

func osReadFile(dataDir, jobID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID)))
}

// PreflightAnalysisFixSource binds a fix request to the current branch base.
func (s *Service) PreflightAnalysisFixSource(
	ctx context.Context, repo sourceinvestigation.Repository, targetBranch string,
) (string, error) {
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repo, targetBranch)
	if err != nil {
		if code, ok := ReasonCodeFrom(err); ok {
			return "", withReason(code, ErrPreviewRejected, err.Error())
		}
		return "", fmt.Errorf("%w: exact JUnit Fix repository/base preflight: %w", ErrPreviewRejected, err)
	}
	return compatibility.GenerationBaseRevision, nil
}

func (s *Service) verifyAnalysisSourceCompatibility(
	ctx context.Context, failureRepo sourceinvestigation.Repository, targetBranch string,
) (analysisSourceCompatibility, error) {
	if err := sourceinvestigation.ValidateRepository(failureRepo); err != nil {
		return analysisSourceCompatibility{}, err
	}
	targetBranch = strings.TrimSpace(targetBranch)
	if targetBranch == "" {
		return analysisSourceCompatibility{}, withReason(ReasonSourceBranchUnknown, ErrPreviewRejected, "")
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
	client := s.sourceRevisionClient
	if client == nil {
		client = ghpr.NewClient(nil, s.ai.SourceToken)
	}
	base, err := client.ResolveBase(ctx, failureRepo.Owner, failureRepo.Name, targetBranch)
	if err != nil {
		return analysisSourceCompatibility{}, fmt.Errorf("resolving current generation base: %w", err)
	}
	if base.Branch != targetBranch {
		return analysisSourceCompatibility{}, fmt.Errorf("resolved generation base branch does not match the failure branch")
	}
	generationBase, ok := buildsource.NormalizeRevision(base.HeadSHA)
	if !ok {
		return analysisSourceCompatibility{}, fmt.Errorf("generation base is not an immutable full commit")
	}
	if !strings.EqualFold(failureRevision, generationBase) {
		contains, _, err := client.CompareCommits(ctx, failureRepo.Owner, failureRepo.Name, failureRevision, generationBase)
		if err != nil {
			return analysisSourceCompatibility{}, fmt.Errorf("checking failure revision ancestry: %w", err)
		}
		if !contains {
			return analysisSourceCompatibility{}, withReason(ReasonSourceRevisionDiverged, ErrPreviewRejected, "")
		}
	}
	return analysisSourceCompatibility{GenerationBaseRevision: generationBase}, nil
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
	subject, err := s.resolveAnalysisFixTarget(input)
	if err != nil {
		return PreviewResult{}, err
	}
	if input.AnalysisContentHash != subject.AnalysisContentHash {
		return PreviewResult{}, ErrPreviewTargetChanged
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
		return PreviewResult{}, fmt.Errorf("%w: exact JUnit fix preview requires the Agent Sandbox Fix runtime", ErrPreviewRejected)
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
	sourceHints := slices.Clone(subject.SourceHints)
	findingText := input.AssistantAnswer
	if input.ProposedRevision != nil {
		findingText += "\n" + input.ProposedRevision.RootCause + "\n" + input.ProposedRevision.SuggestedFix
	}
	if remediationpolicy.RelationshipTextWarning(instruction) != "" {
		return PreviewResult{}, withReason(ReasonUnsafeRemediation, ErrPreviewRejected, "")
	}
	warnings := analysisQualityWarnings(subject.Failure.AIAnalysis, input, sourceHints)
	if remediationpolicy.RelationshipTextWarning(findingText) != "" {
		warnings = append(warnings, analysisWarningPolicy)
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return PreviewResult{}, err
	}
	if !strings.EqualFold(destination.Repo.Owner, repository.Owner) || !strings.EqualFold(destination.Repo.Name, repository.Name) {
		return PreviewResult{}, fmt.Errorf("%w: failure source does not match the configured fix destination", ErrPreviewRejected)
	}
	targetBranch, _ := buildsource.Branch(subject.Build, repository.Owner, repository.Name)
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repository, targetBranch)
	if err != nil {
		if code, ok := ReasonCodeFrom(err); ok {
			return PreviewResult{}, withReason(code, ErrPreviewRejected, err.Error())
		}
		return PreviewResult{}, fmt.Errorf("%w: checking exact JUnit Fix repository/base: %w", ErrPreviewRejected, err)
	}
	if !strings.EqualFold(input.FailureRevision, repository.Revision) ||
		!strings.EqualFold(input.GenerationBaseRevision, compatibility.GenerationBaseRevision) {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	if err := s.setRequestWarning(ctx, warnings...); err != nil {
		return PreviewResult{}, err
	}

	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
		return PreviewResult{}, err
	}
	targetConfig := fixDestinationFingerprint(eff, destination)
	if targetConfig != input.TargetConfig {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	generationHash := analysisPreviewGenerationHash(
		subject, input.HandoffHash, repository, targetBranch, compatibility.GenerationBaseRevision, targetConfig,
	)
	token, existing, acquired, err := s.previewStore.reserveIdempotent(
		owner, input.PreviewRequestHash, generationHash, s.requestTimeout+30*time.Second,
	)
	if err != nil {
		return PreviewResult{}, err
	}
	if !acquired {
		if existing != nil && existing.fix != nil {
			if err := s.setRequestWarning(ctx, existing.fix.Warnings...); err != nil {
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
	failure := analysisFailureForGeneration(subject, input, targetBranch, compatibility.GenerationBaseRevision)
	gf, err := mgr.GenerateAnalysisPreview(ctx, failure, instruction)
	if err != nil {
		return PreviewResult{}, safeAnalysisFixPreviewError(err)
	}
	if err := s.setRequestWarning(ctx, gf.Warnings...); err != nil {
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

func analysisFailureForGeneration(
	subject *AnalysisActionSubject, input AnalysisFixInput, targetBranch, generationBaseRevision string,
) fixpr.AnalysisFailure {
	analysis := subject.Failure.AIAnalysis
	return fixpr.AnalysisFailure{
		ID: subject.ID, Project: subject.Identity.Project, JobID: subject.Identity.JobID, JobName: subject.JobName,
		BuildID: subject.Identity.BuildID, TestName: subject.Identity.TestName, AnalysisGeneratedAt: subject.Identity.AnalysisGeneratedAt,
		AnalysisHash: subject.ContentHash, RootCause: analysis.RootCause, SuggestedFix: analysis.SuggestedFix,
		FailureMessage:  boundedAnalysisFailureText(subject.Failure.FailureMessage),
		FailureBody:     boundedAnalysisFailureText(subject.Failure.FailureBody),
		AssistantAnswer: input.AssistantAnswer, AssistantUnverified: input.AssistantUnverified,
		AssistantUnverifiedReason: input.AssistantUnverifiedReason,
		ChatResponseHash:          input.ChatResponseHash, PreviewRequestHash: input.PreviewRequestHash,
		ProposedRevision:  input.ProposedRevision,
		ArtifactCitations: slices.Clone(input.ArtifactCitations), EvidenceWarnings: slices.Clone(input.EvidenceWarnings),
		SourceRepository: subject.SourceRepository.Owner + "/" + subject.SourceRepository.Name,
		SourceBranch:     targetBranch,
		FailureRevision:  subject.SourceRepository.Revision, GenerationBaseRevision: generationBaseRevision,
		SourceHints: slices.Clone(subject.SourceHints),
	}
}

func analysisPreviewGenerationHash(
	subject *AnalysisActionSubject,
	requestHash string,
	repo sourceinvestigation.Repository,
	sourceBranch string,
	generationBaseRevision string,
	targetConfig string,
) string {
	payload, _ := json.Marshal(struct {
		AnalysisID, AnalysisHash, AnalysisContentHash, RequestHash string
		Repository                                                 sourceinvestigation.Repository
		SourceBranch, GenerationBaseRevision, TargetConfig         string
	}{
		subject.ID, subject.ContentHash, subject.AnalysisContentHash, requestHash,
		repo, sourceBranch, generationBaseRevision, targetConfig,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func analysisFixReplacementHash(input AnalysisFixInput) string {
	input.Instruction, input.PreviewRequestHash = "", ""
	return analysisFixHandoffHash(input)
}

func analysisQualityWarnings(analysis *models.AIAnalysis, input AnalysisFixInput, sourceHints []string) []string {
	if analysis == nil {
		return nil
	}
	warnings := make([]string, 0, 6)
	if !analysis.CritiquePassed {
		warnings = append(warnings, analysisWarningCritique)
	}
	if strings.TrimSpace(analysis.SuggestedFix) == "" {
		warnings = append(warnings, analysisWarningSuggestedFix)
	}
	if strings.TrimSpace(analysis.RootCause) == "" && strings.TrimSpace(input.AssistantAnswer) != "" {
		warnings = append(warnings, analysisWarningRootCause)
	}
	if strings.EqualFold(strings.TrimSpace(analysis.Severity), "Transient-Ignore") {
		warnings = append(warnings, analysisWarningTransient)
	}
	if input.ProposedRevision != nil && (strings.TrimSpace(input.ProposedRevision.RootCause) == "" || strings.TrimSpace(input.ProposedRevision.SuggestedFix) == "") {
		warnings = append(warnings, analysisWarningProse)
	}
	if len(input.EvidenceWarnings) > 0 {
		warnings = append(warnings, analysisWarningEvidenceQualified)
	}
	if input.AssistantUnverified {
		warnings = append(warnings, analysisWarningAssistantUnverified)
	}
	if len(input.ArtifactCitations) == 0 {
		warnings = append(warnings, analysisWarningNoCitations)
	}
	if len(sourceHints) == 0 {
		warnings = append(warnings, analysisWarningNoSourceHints)
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
		identity.Source == models.TestCaseSourceBuild || identity.JUnitFile == "" || identity.AnalysisGeneratedAt == "" {
		return fmt.Errorf("%w: exact JUnit analysis identity is incomplete", ErrNotFound)
	}
	return nil
}

func validateAnalysisFixInput(input AnalysisFixInput) error {
	input.Identity = normalizeAnalysisIdentity(input.Identity)
	if err := validateAnalysisIdentity(input.Identity); err != nil {
		return err
	}
	if strings.TrimSpace(input.ChatSessionID) == "" || strings.TrimSpace(input.ChatRequestID) == "" ||
		strings.TrimSpace(input.ChatResponseHash) == "" || strings.TrimSpace(input.PreviewRequestHash) == "" || strings.TrimSpace(input.AnalysisContentHash) == "" ||
		strings.TrimSpace(input.AssistantAnswer) == "" || sourceinvestigation.ValidateRepository(input.SourceRepository) != nil ||
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
