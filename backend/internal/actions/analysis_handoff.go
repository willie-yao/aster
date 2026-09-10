package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/fixpr"
)

// AnalysisFixRequestHash identifies one explicit selection and instruction.
func AnalysisFixRequestHash(sessionID, requestID, instruction string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(sessionID), strings.TrimSpace(requestID), strings.TrimSpace(instruction),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func analysisFixHandoffHash(input AnalysisFixInput) string {
	input.HandoffHash = ""
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func analysisFixTargetRef(identity AnalysisIdentity) analysischat.AnalysisRef {
	return analysischat.AnalysisRef{
		Scope: analysischat.ScopeTest, JobID: identity.JobID, BuildID: identity.BuildID, TestName: identity.TestName,
		Source: identity.Source, SuiteName: identity.SuiteName, ClassName: identity.ClassName,
		JUnitFile: identity.JUnitFile, AnalysisGeneratedAt: identity.AnalysisGeneratedAt,
	}
}

func (s *Service) resolveAnalysisFixTarget(input AnalysisFixInput) (*AnalysisActionSubject, error) {
	if err := validateAnalysisFixInput(input); err != nil {
		return nil, err
	}
	if input.Origin.FixTarget != analysisFixTargetRef(input.Identity) {
		return nil, ErrPreviewTargetChanged
	}
	if err := analysischat.ValidateFixOrigin(s.dataDir, input.Origin); err != nil {
		return nil, ErrPreviewTargetChanged
	}
	subject, err := s.ResolveAnalysisActionSubject(input.Identity)
	if err != nil {
		return nil, err
	}
	if input.AnalysisContentHash != subject.AnalysisContentHash || input.SourceRepository != subject.SourceRepository {
		return nil, ErrPreviewTargetChanged
	}
	branch, _ := buildsource.Branch(subject.Build, subject.SourceRepository.Owner, subject.SourceRepository.Name)
	if branch != input.SourceBranch {
		return nil, ErrPreviewTargetChanged
	}
	return subject, nil
}

func (s *Service) prepareAnalysisFix(ctx context.Context, input AnalysisFixInput, owner, instruction string) (AnalysisFixInput, error) {
	input = *cloneAnalysisFixInput(input)
	input.Owner, input.Instruction = owner, instruction
	input.PreviewRequestHash = AnalysisFixRequestHash(input.ChatSessionID, input.ChatRequestID, instruction)
	// Only actions select the generation base and destination contract.
	input.FailureRevision, input.GenerationBaseRevision = "", ""
	subject, err := s.resolveAnalysisFixTarget(input)
	if err != nil {
		return AnalysisFixInput{}, err
	}
	base, err := s.PreflightAnalysisFixSource(ctx, subject.SourceRepository, input.SourceBranch)
	if err != nil {
		return AnalysisFixInput{}, err
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil {
		return AnalysisFixInput{}, err
	}
	input.Version = analysisFixHandoffVersion
	input.FailureRevision = subject.SourceRepository.Revision
	input.GenerationBaseRevision = base
	input.TargetConfig = fixDestinationFingerprint(s.cfg.EffectiveFixPRs(), destination)
	input.HandoffHash = analysisFixHandoffHash(input)
	if err := validateAnalysisFixHandoff(input); err != nil {
		return AnalysisFixInput{}, err
	}
	// Source preflight can outlive a publication refresh.
	if _, err := s.resolveAnalysisFixTarget(input); err != nil {
		return AnalysisFixInput{}, err
	}
	return input, nil
}

func validateAnalysisFixHandoff(input AnalysisFixInput) error {
	if input.Version != analysisFixHandoffVersion || input.Owner == "" || input.Owner != normalizeActionOwner(input.Owner) ||
		input.HandoffHash == "" || input.HandoffHash != analysisFixHandoffHash(input) || input.TargetConfig == "" ||
		input.GenerationBaseRevision == "" || input.FailureRevision == "" || input.SourceBranch == "" ||
		input.PreviewRequestHash != AnalysisFixRequestHash(input.ChatSessionID, input.ChatRequestID, input.Instruction) ||
		len(input.Instruction) > 4096 || input.Origin.FixTarget != analysisFixTargetRef(input.Identity) {
		return ErrPreviewTargetChanged
	}
	failure, failureOK := buildsource.NormalizeRevision(input.FailureRevision)
	_, baseOK := buildsource.NormalizeRevision(input.GenerationBaseRevision)
	if !failureOK || !baseOK || !strings.EqualFold(failure, input.SourceRepository.Revision) {
		return ErrPreviewTargetChanged
	}
	return validateAnalysisFixInput(input)
}

func validateAnalysisPreviewBinding(binding *AnalysisPreviewBinding) error {
	if binding == nil || binding.Handoff == nil {
		return ErrPreviewTargetChanged
	}
	return validateAnalysisFixHandoff(*binding.Handoff)
}

func (s *Service) validateAnalysisPreview(ctx context.Context, owner string, binding AnalysisPreviewBinding) error {
	if err := validateAnalysisPreviewBinding(&binding); err != nil {
		return err
	}
	input := *binding.Handoff
	if input.Owner != normalizeActionOwner(owner) {
		return ErrPreviewTargetChanged
	}
	if _, err := s.resolveAnalysisFixTarget(input); err != nil {
		return ErrPreviewTargetChanged
	}
	destination, err := s.cfg.ResolveFixDestination("", "")
	if err != nil || input.TargetConfig != fixDestinationFingerprint(s.cfg.EffectiveFixPRs(), destination) {
		return ErrPreviewTargetChanged
	}
	compatibility, err := s.verifyAnalysisSourceCompatibility(ctx, input.SourceRepository, input.SourceBranch)
	if err != nil || !strings.EqualFold(compatibility.GenerationBaseRevision, input.GenerationBaseRevision) {
		return ErrPreviewTargetChanged
	}
	return nil
}

func analysisFixContext(input AnalysisFixInput) fixpr.GenerationContext {
	return fixpr.GenerationContext{
		AssistantAnswer: input.AssistantAnswer, AssistantUnverified: input.AssistantUnverified,
		AssistantUnverifiedReason: input.AssistantUnverifiedReason, ProposedRevision: input.ProposedRevision,
		ArtifactCitations: input.ArtifactCitations, EvidenceWarnings: input.EvidenceWarnings,
	}
}

// FindAnalysisFixRequest reconnects an admitted request without reading chat.
func (s *Service) FindAnalysisFixRequest(sessionID, requestID, owner, instruction string) (ActionRequestView, bool, error) {
	owner = normalizeActionOwner(owner)
	if owner == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(requestID) == "" {
		return ActionRequestView{}, false, ErrRequestNotFound
	}
	hash := AnalysisFixRequestHash(sessionID, requestID, instruction)
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			return ActionRequestView{}, false, err
		}
	}
	for _, request := range s.requests.Requests {
		if request != nil && request.Kind == requestKindAnalysisFix && request.Owner == owner && request.RequestHash == hash && analysisFixRequestReusable(request) {
			return request.ActionRequestView, true, nil
		}
	}
	return ActionRequestView{}, false, nil
}

func analysisFixRequestReusable(request *actionRequest) bool {
	return request.Status == RequestPending || request.Status == RequestReady || request.Status == RequestCancelling ||
		request.Status == RequestUnknown || request.Status == RequestConfirmed
}

func isAnalysisPreview(entry *previewEntry) bool {
	return entry != nil && (entry.analysisBinding != nil || strings.HasPrefix(entry.failureID, "analysis::"))
}

func validateAnalysisPreviewEntry(entry *previewEntry) error {
	if entry == nil || validateAnalysisPreviewBinding(entry.analysisBinding) != nil || entry.fix == nil {
		return ErrPreviewTargetChanged
	}
	input := *entry.analysisBinding.Handoff
	subject := &AnalysisActionSubject{Identity: input.Identity, AnalysisContentHash: input.AnalysisContentHash}
	fix := entry.fix.Snapshot()
	if entry.failureID != analysisActionID(input.Identity) || entry.patternHash != analysisActionHash(subject) ||
		entry.targetRepo != input.SourceRepository.Owner+"/"+input.SourceRepository.Name || entry.targetConfig != input.TargetConfig ||
		fix.Base.Branch != input.SourceBranch || !strings.EqualFold(fix.Base.HeadSHA, input.GenerationBaseRevision) || !fix.RequireBaseCurrent {
		return ErrPreviewTargetChanged
	}
	return nil
}
