package remediationinvestigation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type RevisionVerification struct {
	Repository sourceinvestigation.Repository `json:"repository"`
	State      string                         `json:"state"`
	Reason     string                         `json:"reason"`
	BuildIDs   []string                       `json:"build_ids,omitempty"`
}

// VerifiedResult is the deterministic terminal interpretation of a private
// model result. Only ClassificationActionable may carry a verified proposal.
type VerifiedResult struct {
	VerificationVersion int                    `json:"verification_version"`
	Classification      Classification         `json:"classification"`
	Reason              string                 `json:"reason"`
	Proposal            *ActionableProposal    `json:"proposal,omitempty"`
	CurrentSource       *RevisionVerification  `json:"current_source,omitempty"`
	FailureSources      []RevisionVerification `json:"failure_sources,omitempty"`
}

type Verifier struct {
	source sourceinvestigation.TreeReader
}

func NewVerifier(source sourceinvestigation.TreeReader) (*Verifier, error) {
	if source == nil {
		return nil, fmt.Errorf("remediation verification source reader is required")
	}
	return &Verifier{source: source}, nil
}

func (v *Verifier) Verify(ctx context.Context, input FrozenInput, entry CacheEntry, browser artifacts.Browser) (VerifiedResult, error) {
	if browser == nil {
		return VerifiedResult{}, fmt.Errorf("remediation verification artifact browser is required")
	}
	if err := ValidateFrozenInput(input); err != nil {
		return VerifiedResult{}, err
	}
	key, err := CacheKey(input)
	if err != nil {
		return VerifiedResult{}, err
	}
	if entry.Key != key || entry.ResultDigest != ResultDigest(entry.Result) || entry.Provenance.InputDigest != FrozenInputDigest(input) ||
		entry.Provenance.ProviderFingerprint != input.ProviderFingerprint || entry.Provenance.Versions != CurrentVersions() ||
		entry.Provenance.PolicyHash != HashPolicy(input.DestinationPolicy) || entry.Provenance.Source != input.InvestigationSource {
		return VerifiedResult{}, fmt.Errorf("cached remediation investigation provenance does not match the frozen input")
	}
	if err := ValidateResult(entry.Result); err != nil {
		return VerifiedResult{}, err
	}
	if err := verifyCachedEvidence(ctx, v.source, browser, input, entry.Result); err != nil {
		return insufficientVerification("cached investigation evidence could not be reverified"), nil
	}

	result := entry.Result
	if result.Classification != ClassificationActionable {
		switch result.Classification {
		case ClassificationAlreadyFixed:
			return insufficientVerification("already-fixed classification lacks a typed target for deterministic current-source verification"), nil
		case ClassificationExternalDependency:
			return insufficientVerification("external-dependency classification lacks a typed ownership identity for deterministic repository verification"), nil
		}
		return VerifiedResult{
			VerificationVersion: VerificationVersion,
			Classification:      result.Classification,
			Reason:              result.Reason,
		}, nil
	}
	if result.Proposal == nil {
		return insufficientVerification("actionable classification has no typed proposal"), nil
	}
	if err := bindProposalToFrozenInput(result.Proposal, input); err != nil {
		return insufficientVerification("proposal does not match the frozen repository or destination policy"), nil
	}
	if !deterministicallyVerifiableTargetKind(result.Proposal.TargetKind) {
		return insufficientVerification("the typed target kind lacks a deterministic present-or-missing predicate"), nil
	}
	if suspiciousRepositoryPath(result.Proposal.Target.Path) {
		return insufficientVerification("module-cache or workspace paths cannot identify a destination-repository target"), nil
	}
	if result.Proposal.Target.Intent == models.RemediationIntentSetJobEnvironment {
		if err := v.verifyFrozenProwJobIdentity(ctx, input, result.Proposal.Target); err != nil {
			return insufficientVerification("the prow target does not match the exact frozen job identity"), nil
		}
	}
	if err := verifyStructuralRelationship(input, result); err != nil {
		return insufficientVerification(err.Error()), nil
	}

	currentState, err := sourceinvestigation.VerifyTargetState(ctx, v.source, input.InvestigationSource, targetForRepository(result.Proposal.Target, input.InvestigationSource))
	if err != nil {
		return insufficientVerification("current-source target verification was inconclusive"), nil
	}
	current := RevisionVerification{
		Repository: input.InvestigationSource,
		State:      currentState.State, Reason: currentState.Reason,
	}
	if currentState.State == actionverify.StateAlreadyPresent {
		return VerifiedResult{
			VerificationVersion: VerificationVersion,
			Classification:      ClassificationAlreadyFixed,
			Reason:              "Current source already contains the deterministically verified remediation target.",
			CurrentSource:       &current,
		}, nil
	}
	if currentState.State != actionverify.StateUnresolved {
		return insufficientVerification("current source does not prove one unresolved remediation target"), nil
	}

	failureStates, ok := v.verifyFailureSources(ctx, input, result.Proposal.Target)
	if !ok {
		return insufficientVerification("failure revisions do not prove that the target was unresolved for every causal-group build"), nil
	}
	proposal := policyDerivedProposal(input, *result.Proposal)
	return VerifiedResult{
		VerificationVersion: VerificationVersion,
		Classification:      ClassificationActionable,
		Reason:              "The typed target is absent from current source and every available failure revision, with bounded evidence linking it to the recurring cause.",
		Proposal:            &proposal,
		CurrentSource:       &current,
		FailureSources:      failureStates,
	}, nil
}

func (v *Verifier) verifyFailureSources(ctx context.Context, input FrozenInput, target models.RemediationTarget) ([]RevisionVerification, bool) {
	type revisionBuilds struct {
		repository sourceinvestigation.Repository
		builds     []string
	}
	byRevision := map[string]*revisionBuilds{}
	for _, build := range input.Builds {
		if build.Source == nil || !sameRepository(*build.Source, input.InvestigationSource) {
			return nil, false
		}
		key := strings.ToLower(build.Source.Owner + "/" + build.Source.Name + "@" + build.Source.Revision)
		entry := byRevision[key]
		if entry == nil {
			entry = &revisionBuilds{repository: *build.Source}
			byRevision[key] = entry
		}
		entry.builds = append(entry.builds, build.BuildID)
	}
	keys := make([]string, 0, len(byRevision))
	for key := range byRevision {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]RevisionVerification, 0, len(keys))
	for _, key := range keys {
		item := byRevision[key]
		verification, err := sourceinvestigation.VerifyTargetState(ctx, v.source, item.repository, targetForRepository(target, item.repository))
		if err != nil || verification.State != actionverify.StateUnresolved {
			return nil, false
		}
		slices.Sort(item.builds)
		out = append(out, RevisionVerification{
			Repository: item.repository, State: verification.State, Reason: verification.Reason,
			BuildIDs: slices.Clone(item.builds),
		})
	}
	return out, len(out) > 0
}

func verifyCachedEvidence(ctx context.Context, source sourceinvestigation.TreeReader, browser artifacts.Browser, input FrozenInput, result Result) error {
	var sourceCitations []sourceinvestigation.Citation
	coveredBuilds := map[string]bool{}
	for _, citation := range result.Evidence {
		switch citation.Kind {
		case EvidenceSource:
			sourceCitations = append(sourceCitations, sourceinvestigation.Citation{
				Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Quote: citation.Quote,
			})
		case EvidenceArtifact:
			if err := verifyArtifactCitation(ctx, browser, citation); err != nil {
				return err
			}
			coveredBuilds[citation.BuildID] = true
		case EvidenceAnalysis:
			if !analysisCitationMatches(citation, input.Analyses) {
				return fmt.Errorf("analysis citation does not match frozen evidence")
			}
			coveredBuilds[citation.BuildID] = true
		}
	}
	if len(sourceCitations) > 0 {
		if _, err := sourceinvestigation.VerifyCitations(ctx, source, input.InvestigationSource, sourceCitations); err != nil {
			return err
		}
	}
	for _, buildID := range input.Group.Builds {
		if !coveredBuilds[buildID] {
			return fmt.Errorf("build %s lacks verified recurring evidence", buildID)
		}
	}
	return nil
}

func verifyStructuralRelationship(input FrozenInput, result Result) error {
	proposal := result.Proposal
	if proposal == nil {
		return fmt.Errorf("typed proposal is missing")
	}
	targetPath := proposal.Target.Path
	pathCited := false
	for _, citation := range result.Evidence {
		if citation.Kind == EvidenceSource && citation.Path == targetPath {
			pathCited = true
			break
		}
	}
	if !pathCited {
		return fmt.Errorf("the exact target path lacks a verified source citation")
	}
	pathReferenced := slices.Contains(input.RelevantFiles, targetPath)
	for _, analysis := range input.Analyses {
		pathReferenced = pathReferenced || slices.Contains(analysis.RelevantFiles, targetPath)
	}
	if !pathReferenced {
		return fmt.Errorf("the typed target is not connected to any referenced per-build analysis")
	}
	if proposal.Target.Intent == models.RemediationIntentSetJobEnvironment && proposal.Target.Job != input.JobName {
		return fmt.Errorf("the Prow environment target does not match the exact frozen job name")
	}
	identifiers := targetEvidenceIdentifiers(*proposal)
	if len(identifiers) == 0 {
		return fmt.Errorf("the typed target has no exact evidence identifier")
	}
	buildsWithIdentifier := map[string]bool{}
	for _, citation := range result.Evidence {
		if citation.Kind != EvidenceArtifact && citation.Kind != EvidenceAnalysis {
			continue
		}
		quote := strings.ToLower(citation.Quote)
		for _, identifier := range identifiers {
			if strings.Contains(quote, strings.ToLower(identifier)) {
				buildsWithIdentifier[citation.BuildID] = true
				break
			}
		}
	}
	if len(buildsWithIdentifier) < 2 {
		return fmt.Errorf("fewer than two causal-group builds contain the exact target identifier")
	}
	return nil
}

func targetEvidenceIdentifiers(proposal ActionableProposal) []string {
	target := proposal.Target
	switch proposal.TargetKind {
	case TargetAddSymbol:
		return []string{target.Symbol}
	case TargetAddRequiredCall:
		_, name, ok := actionverify.RequiredCallParts(target.RequiredCall)
		if !ok {
			return nil
		}
		return []string{target.Symbol, name}
	case TargetSetJobEnvironment:
		return []string{target.Name}
	default:
		return nil
	}
}

func deterministicallyVerifiableTargetKind(kind TargetKind) bool {
	switch kind {
	case TargetAddSymbol, TargetAddRequiredCall, TargetSetJobEnvironment:
		return true
	default:
		return false
	}
}

func suspiciousRepositoryPath(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "/pkg/mod/") || strings.HasPrefix(normalized, "pkg/mod/") ||
		strings.Contains(normalized, "\\pkg\\mod\\") || strings.HasPrefix(normalized, ".cache/") || strings.Contains(normalized, "/.cache/") ||
		strings.Contains(normalized, "@v")
}

func (v *Verifier) verifyFrozenProwJobIdentity(ctx context.Context, input FrozenInput, target models.RemediationTarget) error {
	if !strings.EqualFold(target.Repository, "kubernetes/test-infra") || target.Revision != input.InvestigationSource.Revision {
		return fmt.Errorf("prow target repository or revision is not frozen")
	}
	content, err := v.source.ReadFile(ctx, input.InvestigationSource, target.Path)
	if err != nil {
		return err
	}
	definitions, err := jobconfig.ParseCatalog([]byte(content), target.Path)
	if err != nil {
		return err
	}
	matches := 0
	for _, definition := range definitions {
		if definition.Name == input.JobName && definition.ID() == input.JobID && definition.ConfigFile == target.Path {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("frozen job identity matched %d definitions", matches)
	}
	return nil
}

func policyDerivedProposal(input FrozenInput, proposal ActionableProposal) ActionableProposal {
	proposal = cloneProposal(proposal)
	proposal.AllowedChangedPaths = []string{proposal.Target.Path}
	repositoryName := strings.ToLower(proposal.Repository.Owner + "/" + proposal.Repository.Name)
	for _, policy := range input.DestinationPolicy.Repositories {
		if strings.ToLower(strings.TrimSpace(policy.Repository)) == repositoryName {
			proposal.AllowedValidationCommands = cloneValidationCommands(policy.AllowedCommands)
			break
		}
	}
	proposal.CurrentSource = CurrentSourceAbsent
	return proposal
}

func targetForRepository(target models.RemediationTarget, repository sourceinvestigation.Repository) models.RemediationTarget {
	if target.Intent == models.RemediationIntentSetJobEnvironment {
		target.Repository = repository.Owner + "/" + repository.Name
		target.Revision = repository.Revision
	}
	return target
}

func insufficientVerification(reason string) VerifiedResult {
	return VerifiedResult{
		VerificationVersion: VerificationVersion,
		Classification:      ClassificationInsufficientEvidence,
		Reason:              reason,
	}
}

func sameRepository(left, right sourceinvestigation.Repository) bool {
	return strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Name, right.Name)
}

func cloneProposal(proposal ActionableProposal) ActionableProposal {
	proposal.VerificationRequirements = slices.Clone(proposal.VerificationRequirements)
	proposal.AllowedChangedPaths = slices.Clone(proposal.AllowedChangedPaths)
	proposal.AllowedValidationCommands = slices.Clone(proposal.AllowedValidationCommands)
	return proposal
}
