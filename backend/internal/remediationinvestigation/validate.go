package remediationinvestigation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{16,128}$`)

func ValidateFrozenInput(input FrozenInput) error {
	for name, value := range map[string]string{
		"pattern ID": input.PatternID, "pattern hash": input.PatternHash,
		"causal group ID": input.CausalGroupID, "causal group hash": input.CausalGroupHash,
		"job ID": input.JobID, "job name": input.JobName, "provider fingerprint": input.ProviderFingerprint,
	} {
		if strings.TrimSpace(value) == "" || len(value) > 512 {
			return fmt.Errorf("%s is required and must be bounded", name)
		}
	}
	if !hexDigest.MatchString(strings.ToLower(input.PatternHash)) || !hexDigest.MatchString(strings.ToLower(input.CausalGroupHash)) {
		return fmt.Errorf("pattern and causal-group hashes must be hexadecimal digests")
	}
	if input.Recurrence != models.PatternRecurrenceSharedCause && input.Recurrence != models.PatternRecurrenceMixedCauses {
		return fmt.Errorf("recurrence %q does not identify a recurring causal group", input.Recurrence)
	}
	if len(input.Group.Builds) < 2 || len(input.Group.Builds) > 50 {
		return fmt.Errorf("causal group must contain 2-50 builds")
	}
	if input.Group.ID != input.CausalGroupID || input.Group.ContentHash != input.CausalGroupHash {
		return fmt.Errorf("causal-group identity does not match the frozen subject")
	}
	if models.PatternCausalGroupHash(input.Group) != input.CausalGroupHash {
		return fmt.Errorf("causal-group content hash does not match the frozen content")
	}
	if models.PatternCausalGroupID(input.PatternID, input.Group) != input.CausalGroupID {
		return fmt.Errorf("causal-group ID does not match the frozen pattern and content")
	}
	if input.Versions != CurrentVersions() {
		return fmt.Errorf("investigation versions are not current")
	}
	if input.ConsumerPromptHash != HashText(input.ConsumerPrompt) {
		return fmt.Errorf("consumer prompt hash does not match the frozen prompt")
	}
	if input.SkillHash != "" && !hexDigest.MatchString(strings.ToLower(input.SkillHash)) {
		return fmt.Errorf("skill hash must be a hexadecimal digest")
	}
	if err := sourceinvestigation.ValidateRepository(input.InvestigationSource); err != nil {
		return fmt.Errorf("investigation source: %w", err)
	}
	if err := validateDestinationPolicy(input.DestinationPolicy); err != nil {
		return err
	}
	if len(input.RelevantFiles) > 100 {
		return fmt.Errorf("relevant files exceed 100")
	}
	if err := validateUniquePaths(input.RelevantFiles, true); err != nil {
		return fmt.Errorf("relevant files: %w", err)
	}
	buildIDs := make([]string, 0, len(input.Builds))
	seenBuilds := map[string]bool{}
	for _, build := range input.Builds {
		buildID := strings.TrimSpace(build.BuildID)
		if buildID == "" || len(buildID) > 256 || seenBuilds[buildID] {
			return fmt.Errorf("build references must have unique bounded IDs")
		}
		seenBuilds[buildID] = true
		buildIDs = append(buildIDs, buildID)
		if build.Source != nil {
			if err := sourceinvestigation.ValidateRepository(*build.Source); err != nil {
				return fmt.Errorf("build %s source: %w", buildID, err)
			}
		}
		if len(build.BuildPrefix) > 2048 || len(build.ProwURL) > 4096 || len(build.WebURL) > 4096 {
			return fmt.Errorf("build %s metadata is too large", buildID)
		}
	}
	if !sameStringSet(buildIDs, input.Group.Builds) {
		return fmt.Errorf("build references do not exactly match the causal group")
	}
	analysisBuilds := make([]string, 0, len(input.Analyses))
	seenAnalyses := map[string]bool{}
	for _, analysis := range input.Analyses {
		if strings.TrimSpace(analysis.BuildID) == "" || seenAnalyses[analysis.BuildID] {
			return fmt.Errorf("analyses must have unique build IDs")
		}
		seenAnalyses[analysis.BuildID] = true
		analysisBuilds = append(analysisBuilds, analysis.BuildID)
		if strings.TrimSpace(analysis.RootCause) == "" || len(analysis.RootCause) > 32<<10 {
			return fmt.Errorf("analysis %s has an invalid root cause", analysis.BuildID)
		}
		if strings.TrimSpace(analysis.GeneratedAt) == "" || len(analysis.TestName) > 4096 || len(analysis.GeneratedAt) > 128 || len(analysis.Severity) > 64 {
			return fmt.Errorf("analysis %s metadata is incomplete or too large", analysis.BuildID)
		}
		if err := validateUniquePaths(analysis.RelevantFiles, true); err != nil {
			return fmt.Errorf("analysis %s relevant files: %w", analysis.BuildID, err)
		}
		if len(analysis.Evidence) > 20 {
			return fmt.Errorf("analysis %s has too many evidence citations", analysis.BuildID)
		}
		if analysis.SourceRepository != nil {
			if err := sourceinvestigation.ValidateRepository(*analysis.SourceRepository); err != nil {
				return fmt.Errorf("analysis %s source: %w", analysis.BuildID, err)
			}
		}
	}
	if !sameStringSet(analysisBuilds, input.Group.Builds) {
		return fmt.Errorf("referenced analyses do not exactly cover the causal group")
	}
	return nil
}

func validateDestinationPolicy(policy DestinationPolicy) error {
	if strings.TrimSpace(policy.Project) == "" || len(policy.Project) > 512 {
		return fmt.Errorf("destination policy project is required")
	}
	if len(policy.Repositories) == 0 || len(policy.Repositories) > 20 {
		return fmt.Errorf("destination policy must contain 1-20 repositories")
	}
	seen := map[string]bool{}
	for _, repository := range policy.Repositories {
		name := strings.ToLower(strings.TrimSpace(repository.Repository))
		if name == "" || strings.Count(name, "/") != 1 || seen[name] {
			return fmt.Errorf("destination repositories must be unique owner/name values")
		}
		seen[name] = true
		if len(repository.AllowedPaths) == 0 {
			return fmt.Errorf("destination repository %s has no allowed paths", name)
		}
		if err := validateUniquePaths(repository.AllowedPaths, false); err != nil {
			return fmt.Errorf("destination repository %s paths: %w", name, err)
		}
		if len(repository.AllowedCommands) > 20 {
			return fmt.Errorf("destination repository %s has too many commands", name)
		}
		seenCommands := map[string]bool{}
		for _, command := range repository.AllowedCommands {
			command = strings.TrimSpace(command)
			if command == "" || len(command) > 1024 || seenCommands[command] {
				return fmt.Errorf("destination repository %s commands must be unique and bounded", name)
			}
			seenCommands[command] = true
		}
	}
	return nil
}

func validateUniquePaths(paths []string, allowFiles bool) error {
	seen := map[string]bool{}
	for _, value := range paths {
		clean, err := artifacts.SafePath(strings.TrimSpace(value))
		if err != nil || clean == "" || len(clean) > 1024 || seen[clean] {
			return fmt.Errorf("paths must be unique safe relative paths")
		}
		if !allowFiles && strings.HasSuffix(value, "//") {
			return fmt.Errorf("path prefixes must be canonical")
		}
		seen[clean] = true
	}
	return nil
}

func sameStringSet(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func CacheKey(input FrozenInput) (string, error) {
	if err := ValidateFrozenInput(input); err != nil {
		return "", err
	}
	return cacheKeyForDigest(FrozenInputDigest(input)), nil
}

func cacheKeyForDigest(inputDigest string) string {
	sum := sha256Digest("remediation-investigation\x00" + inputDigest)
	return "remediation-investigation:" + sum
}

func sha256Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func DecodeResult(raw json.RawMessage) (Result, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return Result{}, err
	}
	if err := validateResultObjectKeys(raw); err != nil {
		return Result{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode remediation investigation result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("decode remediation investigation result: trailing data")
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return fmt.Errorf("decode remediation investigation result: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode remediation investigation result: trailing data")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[key] = true
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func validateResultObjectKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode remediation investigation result: %w", err)
	}
	if field := firstUnknownField(value, map[string]bool{
		"version": true, "classification": true, "reason": true, "cause_assessment": true,
		"cause_assessment_reason": true, "proposal": true, "evidence": true,
	}); field != "" {
		return fmt.Errorf("unknown field path %s", field)
	}
	version, ok := value["version"]
	if !ok {
		return fmt.Errorf("result version is missing")
	}
	number, ok := version.(json.Number)
	if !ok {
		return fmt.Errorf("result version must be the integer 1")
	}
	parsedVersion, err := number.Int64()
	if err != nil || parsedVersion != ResultVersion {
		return fmt.Errorf("result version must be the integer 1")
	}
	if proposal, ok := value["proposal"].(map[string]any); ok {
		if field := firstUnknownField(proposal, map[string]bool{
			"target_kind": true, "repository": true, "target": true, "expected_behavior": true,
			"relationship_proof": true, "current_source": true, "verification_requirements": true,
			"allowed_changed_paths": true, "allowed_validation_commands": true,
		}); field != "" {
			return fmt.Errorf("unknown field path proposal.%s", field)
		}
		if repository, ok := proposal["repository"].(map[string]any); ok {
			if field := firstUnknownField(repository, map[string]bool{"owner": true, "name": true, "revision": true}); field != "" {
				return fmt.Errorf("unknown field path proposal.repository.%s", field)
			}
		}
		if target, ok := proposal["target"].(map[string]any); ok {
			if field := firstUnknownField(target, map[string]bool{
				"intent": true, "symbol": true, "required_call": true, "path": true, "value": true,
				"repository": true, "revision": true, "job": true, "container": true, "name": true,
			}); field != "" {
				return fmt.Errorf("unknown field path proposal.target.%s", field)
			}
		}
	}
	if evidence, ok := value["evidence"].([]any); ok {
		for index, item := range evidence {
			citation, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if field := firstUnknownField(citation, map[string]bool{
				"kind": true, "build_id": true, "path": true, "line_start": true,
				"line_end": true, "quote": true, "analysis_generated_at": true,
			}); field != "" {
				return fmt.Errorf("unknown field path evidence[%d].%s", index, field)
			}
		}
	}
	return nil
}

func firstUnknownField(value map[string]any, allowed map[string]bool) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !allowed[key] {
			return key
		}
	}
	return ""
}

func ValidateResult(result Result) error {
	if result.Version != ResultVersion {
		return fmt.Errorf("result version %d is not current", result.Version)
	}
	if !validClassification(result.Classification) {
		return fmt.Errorf("classification %q is invalid", result.Classification)
	}
	if !validCauseAssessment(result.CauseAssessment) {
		return fmt.Errorf("cause assessment %q is invalid", result.CauseAssessment)
	}
	if err := boundedText("reason", result.Reason, 16<<10); err != nil {
		return err
	}
	if err := boundedText("cause assessment reason", result.CauseAssessmentReason, 16<<10); err != nil {
		return err
	}
	if len(result.Evidence) == 0 || len(result.Evidence) > 16 {
		return fmt.Errorf("result must contain 1-16 evidence citations")
	}
	sourceEvidence := false
	seen := map[string]bool{}
	for index, citation := range result.Evidence {
		if err := validateEvidenceCitation(citation); err != nil {
			return fmt.Errorf("evidence %d: %w", index, err)
		}
		encoded, _ := json.Marshal(citation)
		key := string(encoded)
		if seen[key] {
			return fmt.Errorf("duplicate evidence citation %d", index)
		}
		seen[key] = true
		sourceEvidence = sourceEvidence || citation.Kind == EvidenceSource
	}
	if result.Classification != ClassificationActionable {
		if result.Proposal != nil {
			return fmt.Errorf("non-actionable classification must not include a proposal")
		}
		return nil
	}
	if result.Proposal == nil {
		return fmt.Errorf("actionable classification requires a proposal")
	}
	if result.CauseAssessment != CauseSupports && result.CauseAssessment != CauseRefines {
		return fmt.Errorf("actionable proposal must support or refine the claimed cause")
	}
	if !sourceEvidence {
		return fmt.Errorf("actionable proposal requires source evidence")
	}
	if err := validateProposal(*result.Proposal); err != nil {
		return err
	}
	policyText := strings.Join([]string{result.Reason, result.CauseAssessmentReason, result.Proposal.ExpectedBehavior, result.Proposal.RelationshipProof}, "\n")
	if reason := remediationpolicy.Reason(policyText, []models.RemediationTarget{result.Proposal.Target}); reason != "" {
		return fmt.Errorf("actionable proposal violates remediation safety policy: %s", reason)
	}
	return nil
}

func validateProposal(proposal ActionableProposal) error {
	if err := sourceinvestigation.ValidateRepository(proposal.Repository); err != nil {
		return fmt.Errorf("proposal repository: %w", err)
	}
	if reason := actionverify.InvalidTargetReason(proposal.Target); reason != "" {
		return fmt.Errorf("proposal target: %s", reason)
	}
	if !targetKindMatches(proposal.TargetKind, proposal.Target) {
		return fmt.Errorf("proposal target kind does not match the typed remediation target")
	}
	if proposal.Target.Intent == models.RemediationIntentInvestigate {
		return fmt.Errorf("actionable proposal cannot use an investigation target")
	}
	repository := strings.TrimSpace(proposal.Repository.Owner + "/" + proposal.Repository.Name)
	if proposal.Target.Intent == models.RemediationIntentSetJobEnvironment {
		if !strings.EqualFold(strings.TrimSpace(proposal.Target.Repository), repository) || !strings.EqualFold(strings.TrimSpace(proposal.Target.Revision), proposal.Repository.Revision) {
			return fmt.Errorf("prow target repository and revision must match the pinned repository")
		}
	} else if proposal.Target.Repository != "" || proposal.Target.Revision != "" {
		return fmt.Errorf("code and configuration targets inherit repository identity from the proposal")
	}
	if proposal.CurrentSource != CurrentSourceAbsent {
		return fmt.Errorf("actionable proposal must report the target absent from current source")
	}
	if err := boundedText("expected behavior", proposal.ExpectedBehavior, 16<<10); err != nil {
		return err
	}
	if err := boundedText("relationship proof", proposal.RelationshipProof, 16<<10); err != nil {
		return err
	}
	if len(proposal.VerificationRequirements) == 0 || len(proposal.VerificationRequirements) > 20 {
		return fmt.Errorf("proposal must contain 1-20 verification requirements")
	}
	for _, requirement := range proposal.VerificationRequirements {
		if err := boundedText("verification requirement", requirement, 2048); err != nil {
			return err
		}
	}
	if len(proposal.AllowedChangedPaths) == 0 || len(proposal.AllowedChangedPaths) > 20 {
		return fmt.Errorf("proposal must contain 1-20 allowed changed paths")
	}
	if err := validateUniquePaths(proposal.AllowedChangedPaths, true); err != nil {
		return fmt.Errorf("proposal allowed changed paths: %w", err)
	}
	if proposal.Target.Path != "" && !slices.Contains(proposal.AllowedChangedPaths, path.Clean(proposal.Target.Path)) {
		return fmt.Errorf("proposal target path is not in allowed changed paths")
	}
	if len(proposal.AllowedValidationCommands) > 20 {
		return fmt.Errorf("proposal has too many validation commands")
	}
	seenCommands := map[string]bool{}
	for _, command := range proposal.AllowedValidationCommands {
		if err := boundedText("validation command", command, 1024); err != nil {
			return err
		}
		if seenCommands[command] {
			return fmt.Errorf("proposal validation commands must be unique")
		}
		seenCommands[command] = true
	}
	return nil
}

func targetKindMatches(kind TargetKind, target models.RemediationTarget) bool {
	switch kind {
	case TargetAddSymbol:
		return target.Intent == models.RemediationIntentAddSymbol
	case TargetModifySymbol:
		return target.Intent == models.RemediationIntentModifySymbol && target.RequiredCall == ""
	case TargetAddRequiredCall:
		return target.Intent == models.RemediationIntentModifySymbol && target.RequiredCall != ""
	case TargetSetConfiguration:
		return target.Intent == models.RemediationIntentSetConfiguration
	case TargetRemoveConfiguration:
		return target.Intent == models.RemediationIntentRemoveConfiguration
	case TargetSetJobEnvironment:
		return target.Intent == models.RemediationIntentSetJobEnvironment
	default:
		return false
	}
}

func validateEvidenceCitation(citation EvidenceCitation) error {
	if err := boundedText("evidence quote", citation.Quote, 4<<10); err != nil {
		return err
	}
	switch citation.Kind {
	case EvidenceSource:
		if citation.BuildID != "" || citation.AnalysisGeneratedAt != "" {
			return fmt.Errorf("source evidence cannot carry build or analysis identity")
		}
		if err := validateEvidencePathAndLines(citation); err != nil {
			return err
		}
	case EvidenceArtifact:
		if strings.TrimSpace(citation.BuildID) == "" || citation.AnalysisGeneratedAt != "" {
			return fmt.Errorf("artifact evidence requires only a build ID")
		}
		if err := validateEvidencePathAndLines(citation); err != nil {
			return err
		}
	case EvidenceAnalysis:
		if strings.TrimSpace(citation.BuildID) == "" || strings.TrimSpace(citation.AnalysisGeneratedAt) == "" || citation.Path != "" || citation.LineStart != 0 || citation.LineEnd != 0 {
			return fmt.Errorf("analysis evidence requires build and generated-at identity only")
		}
	default:
		return fmt.Errorf("evidence kind %q is invalid", citation.Kind)
	}
	return nil
}

func validateEvidencePathAndLines(citation EvidenceCitation) error {
	clean, err := artifacts.SafePath(citation.Path)
	if err != nil || clean == "" || clean != citation.Path {
		return fmt.Errorf("evidence path must be a canonical safe relative path")
	}
	if citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd-citation.LineStart+1 > 200 {
		return fmt.Errorf("evidence line range is invalid")
	}
	return nil
}

func boundedText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || len(value) > limit || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must be non-empty, trimmed, and at most %d bytes", name, limit)
	}
	return nil
}

func validClassification(value Classification) bool {
	switch value {
	case ClassificationActionable, ClassificationAlreadyFixed, ClassificationExternalDependency,
		ClassificationEnvironmentOrInfrastructure, ClassificationMitigationOnly, ClassificationInsufficientEvidence:
		return true
	default:
		return false
	}
}

func validCauseAssessment(value CauseAssessment) bool {
	switch value {
	case CauseSupports, CauseRefines, CauseContradicts, CauseInconclusive:
		return true
	default:
		return false
	}
}
