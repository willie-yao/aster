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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
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
}

// AnalysisFixInput is one owner-bound chat finding selected for fix generation.
type AnalysisFixInput struct {
	Identity            AnalysisIdentity
	ChatSessionID       string
	ChatRequestID       string
	ChatResponseHash    string
	PreviewRequestHash  string
	AnalysisContentHash string
	SourceRepository    sourceinvestigation.Repository
	AssistantAnswer     string
	ProposedRevision    *fixpr.RevisionContext
	ArtifactCitations   []fixpr.Evidence
}

// AnalysisPreviewBinding preserves the exact analysis, chat, and source identities.
type AnalysisPreviewBinding struct {
	Identity            AnalysisIdentity               `json:"identity"`
	AnalysisID          string                         `json:"analysis_id"`
	AnalysisHash        string                         `json:"analysis_hash"`
	AnalysisContentHash string                         `json:"analysis_content_hash"`
	ChatSessionID       string                         `json:"chat_session_id"`
	ChatRequestID       string                         `json:"chat_request_id"`
	ChatResponseHash    string                         `json:"chat_response_hash"`
	PreviewRequestHash  string                         `json:"preview_request_hash"`
	SourceRepository    sourceinvestigation.Repository `json:"source_repository"`
	SourceFiles         []string                       `json:"source_files"`
	SourceVerification  string                         `json:"source_verification"`
	FindingText         string                         `json:"finding_text"`
	FindingVerification string                         `json:"finding_verification"`
	VerificationVersion int                            `json:"verification_version"`
}

// AnalysisPreviewValidator revalidates an owner-bound chat response before confirmation.
type AnalysisPreviewValidator interface {
	ValidateAnalysisPreview(context.Context, string, AnalysisPreviewBinding) error
}

type sourceSnapshotReader interface {
	ReadFile(context.Context, string) (string, bool, error)
}

type sourceSnapshotReaderFactory func(sourceinvestigation.Repository) sourceSnapshotReader

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
		analysis == nil || analysis.GeneratedAt != identity.AnalysisGeneratedAt || !ai.MeetsCurrentCritiqueContract(analysis) ||
		strings.TrimSpace(analysis.RootCause) == "" || strings.TrimSpace(analysis.SuggestedFix) == "" ||
		strings.EqualFold(strings.TrimSpace(analysis.Severity), "Transient-Ignore") {
		return nil, fmt.Errorf("JUnit analysis does not pass current action quality gates")
	}
	subject := &AnalysisActionSubject{Identity: identity, JobName: match.jobName, Build: match.run, Failure: match.testCase}
	subject.AnalysisContentHash = models.TestAnalysisContentHash(match.testCase)
	subject.ID = analysisActionID(identity)
	subject.ContentHash = analysisActionHash(subject)
	return subject, nil
}

func osReadFile(dataDir, jobID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID)))
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
	ctx, usageOperation := aiusage.Begin(ctx, s.ai.UsageRecorder, aiusage.Metadata{
		LogicalID: input.ChatRequestID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureFixPreview,
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
	source, ok := ai.ResolveBuildSource(subject.Build, analysisRepo.Owner, analysisRepo.Name)
	if !ok {
		return PreviewResult{}, fmt.Errorf("%w: build source repository revision could not be resolved", ErrPreviewRejected)
	}
	repository := sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}
	if input.SourceRepository != repository {
		return PreviewResult{}, ErrPreviewTargetChanged
	}
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return PreviewResult{}, fmt.Errorf("%w: immutable source identity is unavailable", ErrPreviewRejected)
	}
	sourceFiles := verifiedSourceFiles(subject.Failure.AIAnalysis.FileLinks, repository.Owner, repository.Name, repository.Revision)
	if len(sourceFiles) == 0 || len(sourceFiles) > maxAnalysisSourceFiles {
		return PreviewResult{}, fmt.Errorf("%w: exact analysis has no bounded verified source paths", ErrPreviewRejected)
	}
	verification, err := s.verifyAnalysisSourceSnapshot(ctx, repository, sourceFiles)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: immutable source investigation failed", ErrPreviewRejected)
	}
	findingText := input.AssistantAnswer
	if input.ProposedRevision != nil {
		findingText += "\n" + input.ProposedRevision.RootCause + "\n" + input.ProposedRevision.SuggestedFix
	}
	policyText := findingText + "\n" + instruction
	if remediationpolicy.Reason(policyText, nil) != "" {
		return PreviewResult{}, withReason(ReasonUnsafeRemediation, ErrPreviewRejected, "")
	}
	findingVerification, err := s.verifyAnalysisFinding(ctx, repository, sourceFiles, findingText)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("%w: selected source finding could not be verified as unresolved", ErrPreviewRejected)
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return PreviewResult{}, err
	}
	if !strings.EqualFold(destination.Repo.Owner, repository.Owner) || !strings.EqualFold(destination.Repo.Name, repository.Name) {
		return PreviewResult{}, fmt.Errorf("%w: verified source does not match the configured fix destination", ErrPreviewRejected)
	}
	targetConfig := fixDestinationFingerprint(eff, destination)
	generationHash := analysisPreviewGenerationHash(
		subject, input.PreviewRequestHash, repository, verification, findingVerification, targetConfig,
	)
	token, existing, acquired, err := s.previewStore.reserveIdempotent(
		owner, input.PreviewRequestHash, generationHash, s.requestTimeout+30*time.Second,
	)
	if err != nil {
		return PreviewResult{}, err
	}
	if !acquired {
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
		SourceRevision: repository.Revision, SourceFiles: slices.Clone(sourceFiles), SourceVerification: verification,
		FindingVerification: findingVerification,
	}, instruction)
	if err != nil {
		return PreviewResult{}, safeFixPreviewError(err)
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
			FindingText: findingText, FindingVerification: findingVerification,
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
	sourceVerification, findingVerification, targetConfig string,
) string {
	payload, _ := json.Marshal(struct {
		AnalysisID, AnalysisHash, AnalysisContentHash, RequestHash string
		Repository                                                 sourceinvestigation.Repository
		SourceVerification, FindingVerification, TargetConfig      string
	}{
		subject.ID, subject.ContentHash, subject.AnalysisContentHash, requestHash,
		repo, sourceVerification, findingVerification, targetConfig,
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
	source, ok := ai.ResolveBuildSource(subject.Build, binding.SourceRepository.Owner, binding.SourceRepository.Name)
	if !ok || !strings.EqualFold(source.Revision, binding.SourceRepository.Revision) {
		return ErrPreviewTargetChanged
	}
	files := verifiedSourceFiles(subject.Failure.AIAnalysis.FileLinks, binding.SourceRepository.Owner, binding.SourceRepository.Name, binding.SourceRepository.Revision)
	if !slices.Equal(files, binding.SourceFiles) {
		return ErrPreviewTargetChanged
	}
	verification, err := s.verifyAnalysisSourceSnapshot(ctx, binding.SourceRepository, files)
	if err != nil || verification != binding.SourceVerification {
		return ErrPreviewTargetChanged
	}
	findingVerification, err := s.verifyAnalysisFinding(ctx, binding.SourceRepository, files, binding.FindingText)
	if err != nil || findingVerification != binding.FindingVerification {
		return ErrPreviewTargetChanged
	}
	return nil
}

func (s *Service) verifyAnalysisFinding(
	ctx context.Context, repo sourceinvestigation.Repository, files []string, finding string,
) (string, error) {
	reader := s.analysisSourceReader(repo)
	archiveReader, ok := reader.(actionverify.Reader)
	if !ok {
		return "", fmt.Errorf("pinned source archive reader is unavailable")
	}
	result, err := actionverify.VerifyFindingSource(ctx, archiveReader, finding, files)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(struct {
		Version    int
		Repository sourceinvestigation.Repository
		Files      []string
		Finding    string
		Result     actionverify.FindingResult
	}{analysisSourceVerificationVersion, repo, slices.Clone(files), finding, result})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) verifyAnalysisSourceSnapshot(ctx context.Context, repo sourceinvestigation.Repository, files []string) (string, error) {
	if err := sourceinvestigation.ValidateRepository(repo); err != nil {
		return "", err
	}
	if len(files) == 0 || len(files) > maxAnalysisSourceFiles {
		return "", fmt.Errorf("verified source paths must contain 1-%d entries", maxAnalysisSourceFiles)
	}
	reader := s.analysisSourceReader(repo)
	if reader == nil {
		return "", fmt.Errorf("source reader is unavailable")
	}
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	hash := sha256.New()
	_, _ = hash.Write([]byte(fmt.Sprintf("v%d\x00%s", analysisSourceVerificationVersion, strings.ToLower(repo.Owner+"/"+repo.Name+"@"+repo.Revision))))
	for _, file := range files {
		clean := path.Clean(strings.TrimSpace(file))
		if clean == "." || clean == ".." || clean != file || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return "", fmt.Errorf("source path is unsafe")
		}
		content, found, err := reader.ReadFile(ctx, clean)
		if err != nil || !found {
			return "", fmt.Errorf("verified source path is unavailable")
		}
		contentHash := sha256.Sum256([]byte(content))
		_, _ = hash.Write([]byte("\x00" + clean + "\x00" + hex.EncodeToString(contentHash[:])))
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

func cloneAnalysisPreviewBinding(binding *AnalysisPreviewBinding) *AnalysisPreviewBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	copy.SourceFiles = slices.Clone(binding.SourceFiles)
	return &copy
}
