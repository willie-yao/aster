package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/buildsource"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	maxAnalysisSourceFiles            = 16
	maxAnalysisFixCitations           = 16
	analysisSourceVerificationVersion = 1
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

// AnalysisActionSubject is one currently eligible failed JUnit analysis.
type AnalysisActionSubject struct {
	ID                  string
	ContentHash         string
	AnalysisContentHash string
	Identity            AnalysisIdentity
	JobName             string
	Build               models.BuildInfo
	Failure             models.TestCase
	SourceRepository    sourceinvestigation.Repository
	SourceFiles         []string
}

// AnalysisFixInput is one owner-bound chat finding selected for fix generation.
type AnalysisFixInput struct {
	Identity                 AnalysisIdentity
	ChatSessionID            string
	ChatRequestID            string
	ChatResponseHash         string
	PreviewRequestHash       string
	AnalysisContentHash      string
	SourceRepository         sourceinvestigation.Repository
	FailureRevision          string
	GenerationBaseRevision   string
	VerifiedSourceFileHashes map[string]string
	SourceBranch             string
	AssistantAnswer          string
	ProposedRevision         *fixpr.RevisionContext
	ArtifactCitations        []fixpr.Evidence
}

// AnalysisPreviewBinding preserves the exact analysis, chat, and source identities.
type AnalysisPreviewBinding struct {
	Identity                 AnalysisIdentity               `json:"identity"`
	AnalysisID               string                         `json:"analysis_id"`
	AnalysisHash             string                         `json:"analysis_hash"`
	AnalysisContentHash      string                         `json:"analysis_content_hash"`
	ChatSessionID            string                         `json:"chat_session_id"`
	ChatRequestID            string                         `json:"chat_request_id"`
	ChatResponseHash         string                         `json:"chat_response_hash"`
	PreviewRequestHash       string                         `json:"preview_request_hash"`
	SourceRepository         sourceinvestigation.Repository `json:"source_repository"`
	SourceFiles              []string                       `json:"source_files"`
	SourceVerification       string                         `json:"source_verification"`
	FailureRevision          string                         `json:"failure_revision"`
	GenerationBaseRevision   string                         `json:"generation_base_revision"`
	VerifiedSourceFileHashes map[string]string              `json:"verified_source_file_hashes"`
	FindingText              string                         `json:"finding_text"`
	FindingVerification      string                         `json:"finding_verification"`
	VerificationVersion      int                            `json:"verification_version"`
}

// AnalysisPreviewValidator revalidates an owner-bound chat response before confirmation.
type AnalysisPreviewValidator interface {
	ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error
}

type sourceSnapshotReader interface {
	ReadFile(context.Context, string) (string, bool, error)
}

type sourceSnapshotReaderFactory func(sourceinvestigation.Repository) sourceSnapshotReader

type analysisSourceRevisionClient interface {
	ResolveBase(context.Context, string, string) (ghpr.Base, error)
	CompareCommits(context.Context, string, string, string, string) (bool, string, error)
}

type analysisSourceCompatibility struct {
	GenerationBaseRevision   string
	VerifiedSourceFileHashes map[string]string
	FindingVerification      string
	Warnings                 []string
}

const (
	analysisWarningCritique     = "The original analysis critique did not pass."
	analysisWarningSuggestedFix = "The original analysis has no suggested fix."
	analysisWarningRootCause    = "The original analysis root cause is incomplete."
	analysisWarningTransient    = "The original analysis is marked transient."
	analysisWarningProse        = "The model-authored remediation prose is incomplete."
	analysisWarningPolicy       = "The model-authored prose triggered a text-only remediation-policy concern."
)

// ConfigureAnalysisPreviewValidator binds exact chat state to later confirmation.
func (s *Service) ConfigureAnalysisPreviewValidator(validator AnalysisPreviewValidator) {
	s.analysisPreviewValidator = validator
}

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
		analysis == nil || analysis.GeneratedAt != identity.AnalysisGeneratedAt || analysis.Mode != ai.AgenticMode {
		return nil, fmt.Errorf("JUnit analysis does not pass current action quality gates")
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
	sourceFiles := verifiedSourceFiles(analysis.FileLinks, repository.Owner, repository.Name, repository.Revision)
	if len(sourceFiles) == 0 || len(sourceFiles) > maxAnalysisSourceFiles {
		return nil, fmt.Errorf("%w: JUnit analysis has no bounded verified source paths", ErrPreviewRejected)
	}
	subject := &AnalysisActionSubject{
		Identity: identity, JobName: match.jobName, Build: match.run, Failure: match.testCase,
		SourceRepository: repository, SourceFiles: sourceFiles,
	}
	subject.AnalysisContentHash = models.TestAnalysisContentHash(match.testCase)
	subject.ID = analysisActionID(identity)
	subject.ContentHash = analysisActionHash(subject)
	return subject, nil
}

func osReadFile(dataDir, jobID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID)))
}

// PreflightAnalysisFixSource checks relevant source drift before a Fix-intended provider turn.
func (s *Service) PreflightAnalysisFixSource(
	ctx context.Context, repo sourceinvestigation.Repository, targetBranch string, files []string,
) (string, map[string]string, error) {
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repo, targetBranch, files, "")
	if err != nil {
		return "", nil, fmt.Errorf("%w: exact JUnit Fix relevant source changed", ErrPreviewRejected)
	}
	return compatibility.GenerationBaseRevision, cloneStringMap(compatibility.VerifiedSourceFileHashes), nil
}

func (s *Service) verifyAnalysisSourceCompatibility(
	ctx context.Context, failureRepo sourceinvestigation.Repository, targetBranch string, files []string, finding string,
) (analysisSourceCompatibility, error) {
	if err := sourceinvestigation.ValidateRepository(failureRepo); err != nil {
		return analysisSourceCompatibility{}, err
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
	base, err := client.ResolveBase(ctx, failureRepo.Owner, failureRepo.Name)
	if err != nil {
		return analysisSourceCompatibility{}, fmt.Errorf("resolving current generation base: %w", err)
	}
	generationBase, ok := buildsource.NormalizeRevision(base.HeadSHA)
	if !ok {
		return analysisSourceCompatibility{}, fmt.Errorf("generation base is not an immutable full commit")
	}
	if !strings.EqualFold(failureRevision, generationBase) {
		if strings.TrimSpace(targetBranch) == "" || base.Branch != strings.TrimSpace(targetBranch) {
			return analysisSourceCompatibility{}, fmt.Errorf("failure revision target branch does not match the generation base branch")
		}
		contains, _, err := client.CompareCommits(ctx, failureRepo.Owner, failureRepo.Name, failureRevision, generationBase)
		if err != nil {
			return analysisSourceCompatibility{}, fmt.Errorf("checking failure revision ancestry: %w", err)
		}
		if !contains {
			return analysisSourceCompatibility{}, fmt.Errorf("failure revision is not an ancestor of the generation base")
		}
	}
	files, err = normalizeAnalysisSourceFiles(files)
	if err != nil {
		return analysisSourceCompatibility{}, err
	}
	failureRepo.Revision = failureRevision
	generationRepo := failureRepo
	generationRepo.Revision = generationBase
	failureReader := s.analysisSourceReader(failureRepo)
	if failureReader == nil {
		return analysisSourceCompatibility{}, fmt.Errorf("failure source reader is unavailable")
	}
	sameRevision := strings.EqualFold(failureRevision, generationBase)
	generationReader := failureReader
	if !sameRevision {
		generationReader = s.analysisSourceReader(generationRepo)
		if generationReader == nil {
			return analysisSourceCompatibility{}, fmt.Errorf("generation source reader is unavailable")
		}
	}
	hashes := make(map[string]string, len(files))
	contents := make(map[string]string, len(files))
	for _, file := range files {
		failureContent, found, err := failureReader.ReadFile(ctx, file)
		if err != nil || !found {
			return analysisSourceCompatibility{}, fmt.Errorf("verified source path is unavailable at the failure revision")
		}
		generationContent := failureContent
		if !sameRevision {
			generationContent, found, err = generationReader.ReadFile(ctx, file)
			if err != nil || !found {
				return analysisSourceCompatibility{}, fmt.Errorf("verified source path is unavailable at the generation base")
			}
		}
		if failureContent != generationContent {
			return analysisSourceCompatibility{}, fmt.Errorf("verified source path changed at the generation base")
		}
		sum := sha256.Sum256([]byte(failureContent))
		hashes[file] = hex.EncodeToString(sum[:])
		contents[file] = generationContent
	}
	findingVerification := ""
	var warnings []string
	if strings.TrimSpace(finding) != "" {
		findingVerification, warnings, err = s.verifyAnalysisFinding(generationRepo, files, finding, contents)
		if err != nil {
			return analysisSourceCompatibility{}, err
		}
	}
	return analysisSourceCompatibility{
		GenerationBaseRevision: generationBase, VerifiedSourceFileHashes: hashes,
		FindingVerification: findingVerification, Warnings: warnings,
	}, nil
}

func normalizeAnalysisSourceFiles(files []string) ([]string, error) {
	if len(files) == 0 || len(files) > maxAnalysisSourceFiles {
		return nil, fmt.Errorf("verified source paths must contain 1-%d entries", maxAnalysisSourceFiles)
	}
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	for _, file := range files {
		clean := path.Clean(strings.TrimSpace(file))
		if clean == "." || clean == ".." || clean != file || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return nil, fmt.Errorf("source path is unsafe")
		}
	}
	return files, nil
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
	if err := validateAnalysisFixInput(input); err != nil {
		return PreviewResult{}, err
	}
	subject, err := s.ResolveAnalysisActionSubject(input.Identity)
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
	sourceFiles := slices.Clone(subject.SourceFiles)
	verification, err := s.verifyAnalysisSourceSnapshot(ctx, repository, sourceFiles)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: immutable source investigation failed", ErrPreviewRejected)
	}
	findingText := input.AssistantAnswer
	if input.ProposedRevision != nil {
		findingText += "\n" + input.ProposedRevision.RootCause + "\n" + input.ProposedRevision.SuggestedFix
	}
	if remediationpolicy.RelationshipTextWarning(instruction) != "" {
		return PreviewResult{}, withReason(ReasonUnsafeRemediation, ErrPreviewRejected, "")
	}
	warnings := analysisQualityWarnings(subject.Failure.AIAnalysis, input)
	if remediationpolicy.RelationshipTextWarning(findingText) != "" {
		warnings = append(warnings, analysisWarningPolicy)
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return PreviewResult{}, err
	}
	if !strings.EqualFold(destination.Repo.Owner, repository.Owner) || !strings.EqualFold(destination.Repo.Name, repository.Name) {
		return PreviewResult{}, fmt.Errorf("%w: verified source does not match the configured fix destination", ErrPreviewRejected)
	}
	targetBranch, _ := buildsource.Branch(subject.Build, repository.Owner, repository.Name)
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, repository, targetBranch, sourceFiles, findingText)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: relevant source or selected symbol changed", ErrPreviewRejected)
	}
	if input.GenerationBaseRevision != "" &&
		(!strings.EqualFold(input.FailureRevision, repository.Revision) ||
			!strings.EqualFold(input.GenerationBaseRevision, compatibility.GenerationBaseRevision) ||
			!stringMapsEqual(input.VerifiedSourceFileHashes, compatibility.VerifiedSourceFileHashes)) {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	warnings = append(warnings, compatibility.Warnings...)
	if err := s.setRequestWarning(ctx, warnings...); err != nil {
		return PreviewResult{}, err
	}
	if input.GenerationBaseRevision != "" && !strings.EqualFold(input.GenerationBaseRevision, input.FailureRevision) &&
		(strings.TrimSpace(input.SourceBranch) == "" || input.SourceBranch != targetBranch) {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	if input.GenerationBaseRevision == "" && !strings.EqualFold(repository.Revision, compatibility.GenerationBaseRevision) {
		return PreviewResult{}, fmt.Errorf("%w: branch advancement requires a Fix-intended source preflight", ErrPreviewRejected)
	}
	if err := s.setRequestStage(ctx, RequestStageDrafting); err != nil {
		return PreviewResult{}, err
	}
	targetConfig := fixDestinationFingerprint(eff, destination)
	generationHash := analysisPreviewGenerationHash(
		subject, input.PreviewRequestHash, repository, verification, compatibility, targetConfig,
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
			return PreviewResult{}, withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
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
	analysis := subject.Failure.AIAnalysis
	gf, err := mgr.GenerateAnalysisPreview(ctx, fixpr.AnalysisFailure{
		ID: subject.ID, Project: subject.Identity.Project, JobID: subject.Identity.JobID, JobName: subject.JobName,
		BuildID: subject.Identity.BuildID, TestName: subject.Identity.TestName, AnalysisGeneratedAt: subject.Identity.AnalysisGeneratedAt,
		AnalysisHash: subject.ContentHash, RootCause: analysis.RootCause, SuggestedFix: analysis.SuggestedFix,
		AssistantAnswer: input.AssistantAnswer, ChatResponseHash: input.ChatResponseHash, PreviewRequestHash: input.PreviewRequestHash,
		ProposedRevision:  input.ProposedRevision,
		ArtifactCitations: slices.Clone(input.ArtifactCitations), SourceRepository: repository.Owner + "/" + repository.Name,
		FailureRevision: repository.Revision, GenerationBaseRevision: compatibility.GenerationBaseRevision,
		VerifiedSourceFileHashes: cloneStringMap(compatibility.VerifiedSourceFileHashes),
		SourceFiles:              slices.Clone(sourceFiles), SourceVerification: verification,
		FindingVerification: compatibility.FindingVerification,
	}, instruction)
	if err != nil {
		return PreviewResult{}, safeFixPreviewError(err)
	}
	if err := s.setRequestWarning(ctx, gf.Warnings...); err != nil {
		return PreviewResult{}, err
	}
	if err := s.validateFixFiles(destination, gf.Preview.Files); err != nil {
		return PreviewResult{}, fmt.Errorf("%w: %v", ErrPreviewRejected, err)
	}
	entry := &previewEntry{
		failureID: subject.ID, patternHash: subject.ContentHash, kind: gfKind,
		targetRepo: destination.Repo.Owner + "/" + destination.Repo.Name, targetConfig: targetConfig,
		verificationVersion: sourceVerificationVersion, fix: gf,
		analysisBinding: &AnalysisPreviewBinding{
			Identity: subject.Identity, AnalysisID: subject.ID, AnalysisHash: subject.ContentHash,
			AnalysisContentHash: subject.AnalysisContentHash,
			ChatSessionID:       input.ChatSessionID, ChatRequestID: input.ChatRequestID, ChatResponseHash: input.ChatResponseHash,
			PreviewRequestHash: input.PreviewRequestHash,
			SourceRepository:   repository, SourceFiles: slices.Clone(sourceFiles), SourceVerification: verification,
			FailureRevision: repository.Revision, GenerationBaseRevision: compatibility.GenerationBaseRevision,
			VerifiedSourceFileHashes: cloneStringMap(compatibility.VerifiedSourceFileHashes),
			FindingText:              findingText, FindingVerification: compatibility.FindingVerification,
			VerificationVersion: analysisSourceVerificationVersion,
		},
	}
	preview, err := validatedPreviewEntry(entry)
	if err != nil {
		return PreviewResult{}, withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
	}
	if err := s.previewStore.completeIdempotent(owner, token, input.PreviewRequestHash, generationHash, entry); err != nil {
		return PreviewResult{}, err
	}
	reserved = false
	preview.Token = token
	return preview, nil
}

func analysisPreviewGenerationHash(
	subject *AnalysisActionSubject,
	requestHash string,
	repo sourceinvestigation.Repository,
	sourceVerification string,
	compatibility analysisSourceCompatibility,
	targetConfig string,
) string {
	payload, _ := json.Marshal(struct {
		AnalysisID, AnalysisHash, AnalysisContentHash, RequestHash      string
		Repository                                                      sourceinvestigation.Repository
		SourceVerification, FindingVerification, GenerationBaseRevision string
		VerifiedSourceFileHashes                                        map[string]string
		TargetConfig                                                    string
	}{
		subject.ID, subject.ContentHash, subject.AnalysisContentHash, requestHash,
		repo, sourceVerification, compatibility.FindingVerification, compatibility.GenerationBaseRevision,
		compatibility.VerifiedSourceFileHashes, targetConfig,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Service) validateAnalysisPreview(ctx context.Context, owner string, binding AnalysisPreviewBinding) error {
	if s.analysisPreviewValidator == nil {
		return ErrPreviewTargetChanged
	}
	if binding.VerificationVersion != analysisSourceVerificationVersion {
		return ErrPreviewTargetChanged
	}
	if err := s.analysisPreviewValidator.ValidateAnalysisPreview(ctx, owner, binding); err != nil {
		return err
	}
	subject, err := s.ResolveAnalysisActionSubject(binding.Identity)
	if err != nil || subject.ID != binding.AnalysisID || subject.ContentHash != binding.AnalysisHash ||
		subject.AnalysisContentHash != binding.AnalysisContentHash {
		return ErrPreviewTargetChanged
	}
	if subject.SourceRepository != binding.SourceRepository || !slices.Equal(subject.SourceFiles, binding.SourceFiles) {
		return ErrPreviewTargetChanged
	}
	if !strings.EqualFold(binding.FailureRevision, binding.SourceRepository.Revision) || binding.GenerationBaseRevision == "" || len(binding.VerifiedSourceFileHashes) == 0 {
		return ErrPreviewTargetChanged
	}
	files := subject.SourceFiles
	verification, err := s.verifyAnalysisSourceSnapshot(ctx, binding.SourceRepository, files)
	if err != nil || verification != binding.SourceVerification {
		return ErrPreviewTargetChanged
	}
	targetBranch, _ := buildsource.Branch(subject.Build, binding.SourceRepository.Owner, binding.SourceRepository.Name)
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, binding.SourceRepository, targetBranch, files, binding.FindingText)
	if err != nil || !strings.EqualFold(compatibility.GenerationBaseRevision, binding.GenerationBaseRevision) ||
		!stringMapsEqual(compatibility.VerifiedSourceFileHashes, binding.VerifiedSourceFileHashes) ||
		compatibility.FindingVerification != binding.FindingVerification {
		return ErrPreviewTargetChanged
	}
	return nil
}

func (s *Service) verifyAnalysisFinding(
	repo sourceinvestigation.Repository, files []string, finding string, contents map[string]string,
) (string, []string, error) {
	result, err := actionverify.VerifyFindingSource(finding, files, contents)
	if err != nil {
		return "", nil, err
	}
	payload, _ := json.Marshal(struct {
		Version    int
		Repository sourceinvestigation.Repository
		Files      []string
		Finding    string
		Result     actionverify.FindingResult
	}{analysisSourceVerificationVersion, repo, slices.Clone(files), finding, result})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), slices.Clone(result.Warnings), nil
}

func analysisQualityWarnings(analysis *models.AIAnalysis, input AnalysisFixInput) []string {
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
	return warnings
}

func (s *Service) verifyAnalysisSourceSnapshot(ctx context.Context, repo sourceinvestigation.Repository, files []string) (string, error) {
	if err := sourceinvestigation.ValidateRepository(repo); err != nil {
		return "", err
	}
	var err error
	files, err = normalizeAnalysisSourceFiles(files)
	if err != nil {
		return "", err
	}
	reader := s.analysisSourceReader(repo)
	if reader == nil {
		return "", fmt.Errorf("source reader is unavailable")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(fmt.Sprintf("v%d\x00%s", analysisSourceVerificationVersion, strings.ToLower(repo.Owner+"/"+repo.Name+"@"+repo.Revision))))
	for _, file := range files {
		content, found, err := reader.ReadFile(ctx, file)
		if err != nil || !found {
			return "", fmt.Errorf("verified source path is unavailable")
		}
		contentHash := sha256.Sum256([]byte(content))
		_, _ = hash.Write([]byte("\x00" + file + "\x00" + hex.EncodeToString(contentHash[:])))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Service) analysisSourceReader(repo sourceinvestigation.Repository) sourceSnapshotReader {
	if s.sourceReaderFactory != nil {
		return s.sourceReaderFactory(repo)
	}
	reader, _ := ai.NewGitHubRepoReader(repo.Owner, repo.Name, repo.Revision, s.ai.SourceToken).(sourceSnapshotReader)
	return reader
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
		len(input.ArtifactCitations) == 0 || len(input.ArtifactCitations) > maxAnalysisFixCitations {
		return fmt.Errorf("invalid exact analysis fix request")
	}
	hasPreflight := strings.TrimSpace(input.FailureRevision) != "" || strings.TrimSpace(input.GenerationBaseRevision) != "" || len(input.VerifiedSourceFileHashes) != 0
	if hasPreflight && (strings.TrimSpace(input.FailureRevision) == "" || strings.TrimSpace(input.GenerationBaseRevision) == "" || len(input.VerifiedSourceFileHashes) == 0) {
		return fmt.Errorf("invalid exact analysis Fix source binding")
	}
	return nil
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

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func cloneAnalysisPreviewBinding(binding *AnalysisPreviewBinding) *AnalysisPreviewBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	copy.SourceFiles = slices.Clone(binding.SourceFiles)
	copy.VerifiedSourceFileHashes = cloneStringMap(binding.VerifiedSourceFileHashes)
	return &copy
}
