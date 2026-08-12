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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
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
	VerificationVersion int                      `json:"verification_version"`
	Classification      Classification           `json:"classification"`
	Reason              string                   `json:"reason"`
	Proposal            *ActionableProposal      `json:"proposal,omitempty"`
	CurrentSource       *RevisionVerification    `json:"current_source,omitempty"`
	FailureSources      []RevisionVerification   `json:"failure_sources,omitempty"`
	PolicyRuleID        remediationpolicy.RuleID `json:"policy_rule_id,omitempty"`
	PolicyWarningRuleID remediationpolicy.RuleID `json:"policy_warning_rule_id,omitempty"`
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
	if entry.Key != key || entry.ResultDigest != ResultDigest(entry.Result) ||
		entry.EvidenceCatalogDigest != EvidenceCatalogDigest(entry.EvidenceCatalog) ||
		entry.Provenance.InputDigest != FrozenInputDigest(input) ||
		entry.Provenance.ProviderFingerprint != input.ProviderFingerprint || entry.Provenance.Versions != CurrentVersions() ||
		entry.Provenance.PolicyHash != HashPolicy(input.DestinationPolicy) || entry.Provenance.Source != input.InvestigationSource {
		return VerifiedResult{}, fmt.Errorf("cached remediation investigation provenance does not match the frozen input")
	}
	if err := ValidateResult(entry.Result); err != nil {
		return VerifiedResult{}, err
	}
	if err := ValidateEvidenceCatalog(entry.EvidenceCatalog); err != nil {
		return VerifiedResult{}, err
	}
	verified, err := v.VerifyHypotheses(ctx, input, entry.Result.Hypotheses, entry.EvidenceCatalog, browser)
	if err != nil {
		return VerifiedResult{}, err
	}
	accepted := acceptedHypothesisResults(verified)
	switch len(accepted) {
	case 1:
		return accepted[0], nil
	case 0:
		if entry.Result.NonActionable == nil {
			result := insufficientVerification("no target hypothesis passed deterministic verification")
			result.PolicyRuleID = sharedPolicyRuleID(verified)
			return result, nil
		}
		assessment := *entry.Result.NonActionable
		if err := verifyCachedEvidence(ctx, v.source, browser, input, assessment.EvidenceIDs, entry.EvidenceCatalog); err != nil {
			return insufficientVerification("cached non-actionable evidence could not be reverified"), nil
		}
		return VerifiedResult{
			VerificationVersion: VerificationVersion,
			Classification:      classificationForNonActionable(&assessment.NonActionableReason),
			Reason:              assessment.Reason,
			PolicyRuleID:        sharedPolicyRuleID(verified),
		}, nil
	default:
		return VerifiedResult{
			VerificationVersion: VerificationVersion,
			Classification:      ClassificationAmbiguous,
			Reason:              "Multiple distinct target hypotheses passed deterministic verification.",
		}, nil
	}
}

// VerifyHypotheses independently verifies every model-authored target identity.
func (v *Verifier) VerifyHypotheses(ctx context.Context, input FrozenInput, hypotheses []TargetHypothesis, catalog EvidenceCatalog, browser artifacts.Browser) ([]VerifiedResult, error) {
	if browser == nil {
		return nil, fmt.Errorf("remediation verification artifact browser is required")
	}
	if err := ValidateFrozenInput(input); err != nil {
		return nil, err
	}
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		return nil, err
	}
	if err := ValidateTargetExtraction(TargetExtraction{Version: TargetExtractionVersion, Hypotheses: hypotheses}); err != nil {
		return nil, err
	}
	results := make([]VerifiedResult, 0, len(hypotheses))
	for _, hypothesis := range hypotheses {
		results = append(results, v.verifyHypothesis(ctx, input, hypothesis, catalog, browser))
	}
	return results, nil
}

func sharedPolicyRuleID(results []VerifiedResult) remediationpolicy.RuleID {
	if len(results) == 0 {
		return ""
	}
	shared := results[0].PolicyRuleID
	if shared == "" {
		return ""
	}
	for _, result := range results[1:] {
		if result.PolicyRuleID != shared {
			return ""
		}
	}
	return shared
}

func acceptedHypothesisResults(results []VerifiedResult) []VerifiedResult {
	accepted := make([]VerifiedResult, 0, len(results))
	for _, result := range results {
		if result.Classification == ClassificationActionable && result.Proposal != nil {
			accepted = append(accepted, result)
		} else if result.Classification == ClassificationAlreadyFixed && result.Proposal == nil {
			accepted = append(accepted, result)
		}
	}
	return accepted
}

func (v *Verifier) verifyHypothesis(ctx context.Context, input FrozenInput, hypothesis TargetHypothesis, catalog EvidenceCatalog, browser artifacts.Browser) VerifiedResult {
	if err := verifyCachedEvidence(ctx, v.source, browser, input, hypothesis.EvidenceIDs, catalog); err != nil {
		return insufficientVerification("target hypothesis evidence could not be reverified")
	}
	kind, target, ok := candidateToTarget(hypothesis.Target, input.InvestigationSource)
	if !ok {
		return insufficientVerification("target hypothesis could not be converted to a typed remediation target")
	}
	policy := destinationPolicyForSource(input)
	if policy == nil {
		return insufficientVerification("the frozen source repository is not an allowed destination repository")
	}
	if !pathAllowedByPolicy(target.Path, policy.AllowedPaths) {
		return insufficientVerification("target hypothesis path is outside the frozen destination policy")
	}
	if reason := actionverify.InvalidTargetReason(target); reason != "" {
		return insufficientVerification("target hypothesis failed typed remediation validation")
	}
	if !deterministicallyVerifiableTargetKind(kind) {
		return insufficientVerification("the typed target kind lacks a deterministic present-or-missing predicate")
	}
	if suspiciousRepositoryPath(target.Path) {
		return insufficientVerification("module-cache or workspace paths cannot identify a destination-repository target")
	}
	policyEvaluation := remediationpolicy.Evaluate(candidateExpectedBehavior(hypothesis.Target), []models.RemediationTarget{target})
	if policyEvaluation.Blocked() {
		return insufficientPolicyVerification("target hypothesis violates deterministic remediation safety policy", policyEvaluation.RuleID)
	}
	policyWarning := remediationpolicy.RelationshipTextWarning(hypothesis.RelationshipReason)
	if target.Intent == models.RemediationIntentSetJobEnvironment {
		if err := v.verifyFrozenProwJobIdentity(ctx, input, target); err != nil {
			return insufficientVerification("the prow target does not match the exact frozen job identity")
		}
	}
	proposal := policyDerivedProposal(input, hypothesis, kind, target, *policy)
	if err := verifyStructuralRelationship(ctx, v.source, browser, input, hypothesis, catalog, proposal); err != nil {
		return insufficientVerification(err.Error())
	}

	currentState, err := sourceinvestigation.VerifyTargetState(ctx, v.source, input.InvestigationSource, targetForRepository(target, input.InvestigationSource))
	if err != nil {
		return insufficientVerification("current-source target verification was inconclusive")
	}
	current := RevisionVerification{Repository: input.InvestigationSource, State: currentState.State, Reason: currentState.Reason}
	if currentState.State == actionverify.StateAlreadyPresent {
		return VerifiedResult{
			VerificationVersion: VerificationVersion,
			Classification:      ClassificationAlreadyFixed,
			Reason:              "Current source already contains the deterministically verified remediation target.",
			CurrentSource:       &current,
			PolicyWarningRuleID: policyWarning,
		}
	}
	if currentState.State != actionverify.StateUnresolved {
		return insufficientVerification("current source does not prove one unresolved remediation target")
	}
	failureStates, ok := v.verifyFailureSources(ctx, input, target)
	if !ok {
		return insufficientVerification("failure revisions do not prove that the target was unresolved for every causal-group build")
	}
	return VerifiedResult{
		VerificationVersion: VerificationVersion,
		Classification:      ClassificationActionable,
		Reason:              "The typed target is absent from current source and every available failure revision, with bounded evidence linking it to the recurring cause.",
		Proposal:            &proposal,
		CurrentSource:       &current,
		FailureSources:      failureStates,
		PolicyWarningRuleID: policyWarning,
	}
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

func verifyCachedEvidence(ctx context.Context, source sourceinvestigation.TreeReader, browser artifacts.Browser, input FrozenInput, evidenceIDs []string, catalog EvidenceCatalog) error {
	records, err := selectedEvidenceRecords(evidenceIDs, catalog)
	if err != nil {
		return err
	}
	coveredBuilds := map[string]bool{}
	for _, record := range records {
		switch record.Kind {
		case EvidenceSource:
			if record.Source == nil || record.Source.Repository != input.InvestigationSource {
				return fmt.Errorf("source evidence does not match frozen source identity")
			}
			content, readErr := source.ReadFile(ctx, input.InvestigationSource, record.Source.Path)
			if readErr != nil || HashText(content) != record.Source.ContentDigest {
				return fmt.Errorf("source evidence content does not match frozen identity")
			}
		case EvidenceSourceGrep:
			if err := verifySourceGrepEvidence(ctx, source, input.InvestigationSource, record); err != nil {
				return err
			}
		case EvidenceArtifact:
			if record.Artifact == nil {
				return fmt.Errorf("artifact evidence identity is missing")
			}
			buildID, ok := artifactBuildID(record.Artifact.Path, input.Group.Builds)
			if !ok || buildID != record.Artifact.BuildID {
				return fmt.Errorf("artifact evidence does not match a frozen causal-group build")
			}
			if err := verifyArtifactEvidence(ctx, browser, record); err != nil {
				return err
			}
			coveredBuilds[record.Artifact.BuildID] = true
		case EvidenceAnalysis:
			if !analysisEvidenceMatches(record, input.Analyses) {
				return fmt.Errorf("analysis evidence does not match frozen evidence")
			}
			coveredBuilds[record.Analysis.BuildID] = true
		}
	}
	for _, buildID := range input.Group.Builds {
		if !coveredBuilds[buildID] {
			return fmt.Errorf("build %s lacks verified recurring evidence", buildID)
		}
	}
	return nil
}

func verifyStructuralRelationship(ctx context.Context, source sourceinvestigation.TreeReader, browser artifacts.Browser, input FrozenInput, hypothesis TargetHypothesis, catalog EvidenceCatalog, proposal ActionableProposal) error {
	records, err := selectedEvidenceRecords(hypothesis.EvidenceIDs, catalog)
	if err != nil {
		return err
	}
	targetPath := proposal.Target.Path
	pathCited := false
	for _, record := range records {
		path, repository, ok := sourceEvidenceIdentity(record)
		if ok && path == targetPath && repository == input.InvestigationSource {
			pathCited = true
			break
		}
	}
	if !pathCited {
		return fmt.Errorf("the exact target path lacks an engine-issued source evidence ID")
	}
	pathReferenced := slices.Contains(input.RelevantFiles, targetPath)
	for _, analysis := range input.Analyses {
		pathReferenced = pathReferenced || slices.Contains(analysis.RelevantFiles, targetPath)
	}
	if !pathReferenced {
		return fmt.Errorf("the typed target is not connected to any referenced per-build analysis")
	}
	if proposal.Target.Intent == models.RemediationIntentSetJobEnvironment {
		if proposal.Target.Job != input.JobName {
			return fmt.Errorf("the Prow environment target does not match the exact frozen job name")
		}
		if !prowEnvironmentValueGrounded(ctx, source, input.InvestigationSource, records, proposal.Target) {
			return fmt.Errorf("the exact Prow environment value lacks selected source evidence")
		}
	}
	identifiers := targetEvidenceIdentifiers(proposal)
	if len(identifiers) == 0 {
		return fmt.Errorf("the typed target has no exact evidence identifier")
	}
	buildsWithIdentifier := map[string]bool{}
	for _, record := range records {
		var buildID, evidenceText string
		switch record.Kind {
		case EvidenceArtifact:
			if record.Artifact != nil {
				buildID = record.Artifact.BuildID
				content, readErr := readArtifactEvidence(ctx, browser, record.Artifact.Path)
				if readErr == nil && HashText(content) == record.Artifact.ContentDigest {
					evidenceText = content
				}
			}
		case EvidenceAnalysis:
			if record.Analysis != nil {
				buildID = record.Analysis.BuildID
				for _, analysis := range input.Analyses {
					if analysis.BuildID == record.Analysis.BuildID && analysis.GeneratedAt == record.Analysis.GeneratedAt {
						evidenceText = analysis.RootCause
						break
					}
				}
			}
		}
		for _, identifier := range identifiers {
			if strings.Contains(strings.ToLower(evidenceText), strings.ToLower(identifier)) {
				buildsWithIdentifier[buildID] = true
				break
			}
		}
	}
	if len(buildsWithIdentifier) < 2 {
		return fmt.Errorf("fewer than two causal-group builds contain the exact target identifier")
	}
	return nil
}

func prowEnvironmentValueGrounded(ctx context.Context, source sourceinvestigation.TreeReader, repository sourceinvestigation.Repository, records []EvidenceRecord, target models.RemediationTarget) bool {
	name := strings.ToLower(target.Name)
	value := strings.ToLower(target.Value)
	for _, record := range records {
		var text string
		switch record.Kind {
		case EvidenceSource:
			if record.Source == nil || record.Source.Repository != repository {
				continue
			}
			content, err := source.ReadFile(ctx, repository, record.Source.Path)
			if err == nil && HashText(content) == record.Source.ContentDigest {
				text = content
			}
		case EvidenceSourceGrep:
			if record.SourceGrep != nil && record.SourceGrep.Repository == repository {
				text = record.SourceGrep.Match
			}
		}
		lower := strings.ToLower(text)
		if strings.Contains(lower, name) && strings.Contains(lower, value) {
			return true
		}
	}
	return false
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
	case TargetAddRequiredCall, TargetSetJobEnvironment:
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

func destinationPolicyForSource(input FrozenInput) *RepositoryPolicy {
	repositoryName := strings.ToLower(input.InvestigationSource.Owner + "/" + input.InvestigationSource.Name)
	for index := range input.DestinationPolicy.Repositories {
		candidate := &input.DestinationPolicy.Repositories[index]
		if strings.ToLower(strings.TrimSpace(candidate.Repository)) == repositoryName {
			return candidate
		}
	}
	return nil
}

func policyDerivedProposal(input FrozenInput, hypothesis TargetHypothesis, kind TargetKind, target models.RemediationTarget, policy RepositoryPolicy) ActionableProposal {
	return ActionableProposal{
		TargetKind: kind, Repository: input.InvestigationSource, Target: target,
		ExpectedBehavior:          candidateExpectedBehavior(hypothesis.Target),
		EvidenceIDs:               slices.Clone(hypothesis.EvidenceIDs),
		VerificationRequirements:  verificationRequirements(kind),
		AllowedChangedPaths:       []string{target.Path},
		AllowedValidationCommands: cloneValidationCommands(policy.AllowedCommands),
	}
}

func verificationRequirements(kind TargetKind) []string {
	switch kind {
	case TargetAddRequiredCall:
		return []string{
			"Verify the exact required call is missing from the containing symbol at the pinned revision.",
			"Verify the exact required call is missing from every failure revision and absent from current source.",
		}
	case TargetAddSymbol:
		return []string{
			"Verify the exact symbol is missing from the target package at the pinned revision.",
			"Verify the exact symbol is missing from every failure revision and absent from current source.",
		}
	case TargetSetJobEnvironment:
		return []string{
			"Resolve the exact Prow job, container, environment name, and desired value uniquely.",
			"Verify the desired environment entry is missing from every failure revision and current source.",
		}
	default:
		return nil
	}
}

func classificationForNonActionable(reason *NonActionableReason) Classification {
	if reason == nil {
		return ClassificationInsufficientEvidence
	}
	switch *reason {
	case NonActionableEnvironmentOrInfrastructure:
		return ClassificationEnvironmentOrInfrastructure
	case NonActionableMitigationOnly:
		return ClassificationMitigationOnly
	case NonActionableDependencyOwnershipUnverified, NonActionableInsufficientEvidence:
		return ClassificationInsufficientEvidence
	default:
		return ClassificationInsufficientEvidence
	}
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

func insufficientPolicyVerification(reason string, ruleID remediationpolicy.RuleID) VerifiedResult {
	result := insufficientVerification(reason)
	result.PolicyRuleID = ruleID
	return result
}

func sameRepository(left, right sourceinvestigation.Repository) bool {
	return strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Name, right.Name)
}
