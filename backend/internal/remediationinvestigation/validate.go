package remediationinvestigation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actionverify"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

var (
	hexDigest       = regexp.MustCompile(`^[0-9a-f]{16,128}$`)
	fullHexDigest   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	evidenceIDShape = regexp.MustCompile(`^(source|source_grep|analysis|artifact):[0-9a-f]{64}$`)
	fieldSegment    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

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
			if err := validateValidationCommand(command); err != nil {
				return fmt.Errorf("destination repository %s command: %w", name, err)
			}
			key := validationCommandKey(command)
			if seenCommands[key] {
				return fmt.Errorf("destination repository %s commands must be unique", name)
			}
			seenCommands[key] = true
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

func DecodeTargetExtraction(raw json.RawMessage) (TargetExtraction, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return TargetExtraction{}, err
	}
	if err := validateTargetExtractionObjectKeys(raw); err != nil {
		return TargetExtraction{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result TargetExtraction
	if err := decoder.Decode(&result); err != nil {
		return TargetExtraction{}, fmt.Errorf("decode remediation target extraction: %w", err)
	}
	if err := ValidateTargetExtraction(result); err != nil {
		return TargetExtraction{}, err
	}
	return result, nil
}

func DecodeNonActionableAssessment(raw json.RawMessage) (NonActionableAssessment, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return NonActionableAssessment{}, err
	}
	if err := validateNonActionableObjectKeys(raw); err != nil {
		return NonActionableAssessment{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result NonActionableAssessment
	if err := decoder.Decode(&result); err != nil {
		return NonActionableAssessment{}, fmt.Errorf("decode remediation non-actionable assessment: %w", err)
	}
	if err := ValidateNonActionableAssessment(result); err != nil {
		return NonActionableAssessment{}, err
	}
	return result, nil
}

func (h *TargetHypothesis) UnmarshalJSON(data []byte) error {
	type wireHypothesis struct {
		Target             json.RawMessage `json:"target"`
		EvidenceIDs        []string        `json:"evidence_ids"`
		RelationshipReason string          `json:"relationship_reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireHypothesis
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	target, err := decodeCandidate(wire.Target)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("target hypothesis must contain one typed target")
	}
	*h = TargetHypothesis{Target: target, EvidenceIDs: wire.EvidenceIDs, RelationshipReason: wire.RelationshipReason}
	return nil
}

func (r *Result) UnmarshalJSON(data []byte) error {
	type wireResult struct {
		Version       int                      `json:"version"`
		Hypotheses    []TargetHypothesis       `json:"hypotheses"`
		NonActionable *NonActionableAssessment `json:"non_actionable"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireResult
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*r = Result(wire)
	return nil
}

func decodeCandidate(raw json.RawMessage) (CandidateTarget, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var header struct {
		Kind CandidateKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("decode candidate target: %w", err)
	}
	var candidate CandidateTarget
	switch header.Kind {
	case CandidateRequiredCall:
		candidate = &RequiredCallCandidate{}
	case CandidateSymbolAddition:
		candidate = &SymbolAdditionCandidate{}
	case CandidateProwEnvironmentEntry:
		candidate = &ProwEnvironmentEntryCandidate{}
	case CandidateConfigurationField:
		candidate = &ConfigurationFieldCandidate{}
	default:
		return nil, fmt.Errorf("candidate kind %q is invalid", header.Kind)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(candidate); err != nil {
		return nil, fmt.Errorf("decode candidate target: %w", err)
	}
	return candidate, nil
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

func validateTargetExtractionObjectKeys(raw []byte) error {
	value, err := decodeJSONObject(raw, "remediation target extraction")
	if err != nil {
		return err
	}
	allowed := map[string]bool{"version": true, "hypotheses": true}
	if field := firstUnknownField(value, allowed); field != "" {
		return fmt.Errorf("unknown field path %s", field)
	}
	for field := range allowed {
		if _, ok := value[field]; !ok {
			return fmt.Errorf("target extraction field %s is missing", field)
		}
	}
	return validateWireVersion(value["version"], TargetExtractionVersion, "target extraction")
}

func validateNonActionableObjectKeys(raw []byte) error {
	value, err := decodeJSONObject(raw, "remediation non-actionable assessment")
	if err != nil {
		return err
	}
	allowed := map[string]bool{"version": true, "cause_assessment": true, "reason": true, "evidence_ids": true, "non_actionable_reason": true}
	if field := firstUnknownField(value, allowed); field != "" {
		return fmt.Errorf("unknown field path %s", field)
	}
	for field := range allowed {
		if _, ok := value[field]; !ok {
			return fmt.Errorf("non-actionable assessment field %s is missing", field)
		}
	}
	return validateWireVersion(value["version"], NonActionableAssessmentVersion, "non-actionable assessment")
}

func decodeJSONObject(raw []byte, name string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	return value, nil
}

func validateWireVersion(value any, expected int, name string) error {
	version, ok := value.(json.Number)
	if !ok {
		return fmt.Errorf("%s version must be the integer %d", name, expected)
	}
	parsed, err := version.Int64()
	if err != nil || parsed != int64(expected) {
		return fmt.Errorf("%s version must be the integer %d", name, expected)
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

func ValidateTargetExtraction(result TargetExtraction) error {
	if result.Version != TargetExtractionVersion {
		return fmt.Errorf("target extraction version %d is not current", result.Version)
	}
	if len(result.Hypotheses) > 3 {
		return fmt.Errorf("target extraction must contain at most three hypotheses")
	}
	seen := map[string]bool{}
	for index, hypothesis := range result.Hypotheses {
		if err := validateTargetHypothesis(hypothesis); err != nil {
			return fmt.Errorf("target hypothesis %d: %w", index, err)
		}
		key := candidateIdentityKey(hypothesis.Target)
		if seen[key] {
			return fmt.Errorf("target hypothesis %d duplicates another target identity", index)
		}
		seen[key] = true
	}
	return nil
}

func ValidateNonActionableAssessment(result NonActionableAssessment) error {
	if result.Version != NonActionableAssessmentVersion {
		return fmt.Errorf("non-actionable assessment version %d is not current", result.Version)
	}
	if !validCauseAssessment(result.CauseAssessment) {
		return fmt.Errorf("cause assessment %q is invalid", result.CauseAssessment)
	}
	if err := boundedText("reason", result.Reason, 16<<10); err != nil {
		return err
	}
	if !validNonActionableReason(result.NonActionableReason) {
		return fmt.Errorf("non-actionable reason %q is invalid", result.NonActionableReason)
	}
	return validateEvidenceIDs(result.EvidenceIDs)
}

func ValidateResult(result Result) error {
	if result.Version != ResultVersion {
		return fmt.Errorf("result version %d is not current", result.Version)
	}
	if err := ValidateTargetExtraction(TargetExtraction{Version: TargetExtractionVersion, Hypotheses: result.Hypotheses}); err != nil {
		return err
	}
	if result.NonActionable != nil {
		if err := ValidateNonActionableAssessment(*result.NonActionable); err != nil {
			return err
		}
	}
	if len(result.Hypotheses) == 0 && result.NonActionable == nil {
		return fmt.Errorf("result requires target hypotheses or a non-actionable assessment")
	}
	return nil
}

func validateTargetHypothesis(hypothesis TargetHypothesis) error {
	if hypothesis.Target == nil {
		return fmt.Errorf("typed target is required")
	}
	if err := boundedText("relationship reason", hypothesis.RelationshipReason, 16<<10); err != nil {
		return err
	}
	if err := validateEvidenceIDs(hypothesis.EvidenceIDs); err != nil {
		return err
	}
	return validateCandidate(hypothesis.Target)
}

func validateEvidenceIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > 128 {
		return fmt.Errorf("1-128 evidence IDs are required")
	}
	seen := map[string]bool{}
	for index, id := range ids {
		if !evidenceIDShape.MatchString(id) {
			return fmt.Errorf("evidence ID %d is invalid", index)
		}
		if seen[id] {
			return fmt.Errorf("duplicate evidence ID %d", index)
		}
		seen[id] = true
	}
	return nil
}

func candidateIdentityKey(candidate CandidateTarget) string {
	encoded, _ := json.Marshal(candidate)
	return string(encoded)
}

func resultEvidenceIDs(result Result) []string {
	seen := map[string]bool{}
	var ids []string
	for _, hypothesis := range result.Hypotheses {
		for _, id := range hypothesis.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if result.NonActionable != nil {
		for _, id := range result.NonActionable.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func validateCandidate(candidate CandidateTarget) error {
	switch value := candidate.(type) {
	case *RequiredCallCandidate:
		if value == nil || value.Kind != CandidateRequiredCall {
			return fmt.Errorf("required-call candidate kind is invalid")
		}
		target := models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Symbol: value.ContainingSymbol, RequiredCall: value.RequiredCall, Path: value.Path}
		if reason := actionverify.InvalidTargetReason(target); reason != "" {
			return fmt.Errorf("required-call candidate: %s", reason)
		}
	case *SymbolAdditionCandidate:
		if value == nil || value.Kind != CandidateSymbolAddition {
			return fmt.Errorf("symbol-addition candidate kind is invalid")
		}
		target := models.RemediationTarget{Intent: models.RemediationIntentAddSymbol, Symbol: value.Symbol, Path: value.Path}
		if reason := actionverify.InvalidTargetReason(target); reason != "" {
			return fmt.Errorf("symbol-addition candidate: %s", reason)
		}
	case *ProwEnvironmentEntryCandidate:
		if value == nil || value.Kind != CandidateProwEnvironmentEntry {
			return fmt.Errorf("prow environment candidate kind is invalid")
		}
		target := models.RemediationTarget{
			Intent: models.RemediationIntentSetJobEnvironment, Path: value.ConfigPath,
			Repository: "kubernetes/test-infra", Revision: strings.Repeat("0", 40),
			Job: value.Job, Container: value.Container, Name: value.Name, Value: value.Value,
		}
		if reason := actionverify.InvalidTargetReason(target); reason != "" {
			return fmt.Errorf("prow environment candidate: %s", reason)
		}
	case *ConfigurationFieldCandidate:
		if value == nil || value.Kind != CandidateConfigurationField {
			return fmt.Errorf("configuration-field candidate kind is invalid")
		}
		if err := validateCandidatePath(value.Path); err != nil {
			return err
		}
		if len(value.FieldPath) == 0 || len(value.FieldPath) > 16 {
			return fmt.Errorf("configuration field path must contain 1-16 segments")
		}
		for _, segment := range value.FieldPath {
			if segment == "" || segment != strings.TrimSpace(segment) || len(segment) > 128 || !fieldSegment.MatchString(segment) {
				return fmt.Errorf("configuration field path segments must be bounded identifiers")
			}
		}
		if err := boundedText("configuration field value", value.Value, 512); err != nil {
			return err
		}
	default:
		return fmt.Errorf("candidate target type is unsupported")
	}
	return nil
}

func validateCandidatePath(value string) error {
	clean, err := artifacts.SafePath(value)
	if err != nil || clean == "" || clean != value || len(value) > 1024 {
		return fmt.Errorf("candidate path must be a canonical safe relative path")
	}
	return nil
}

func ValidateEvidenceCatalog(catalog EvidenceCatalog) error {
	if catalog.Version != EvidenceCatalogVersion {
		return fmt.Errorf("evidence catalog version %d is not current", catalog.Version)
	}
	if len(catalog.Records) == 0 || len(catalog.Records) > 256 {
		return fmt.Errorf("evidence catalog must contain 1-256 records")
	}
	seen := map[string]bool{}
	for index, record := range catalog.Records {
		if !evidenceIDShape.MatchString(record.ID) || seen[record.ID] {
			return fmt.Errorf("evidence catalog record %d has an invalid or duplicate ID", index)
		}
		seen[record.ID] = true
		if err := validateEvidenceRecord(record); err != nil {
			return fmt.Errorf("evidence catalog record %d: %w", index, err)
		}
		if record.ID != evidenceRecordID(record) {
			return fmt.Errorf("evidence catalog record %d ID does not match its engine-issued identity", index)
		}
	}
	return nil
}

func validateEvidenceRecord(record EvidenceRecord) error {
	only := func(kind EvidenceKind) bool {
		return (kind == EvidenceSource) == (record.Source != nil) &&
			(kind == EvidenceSourceGrep) == (record.SourceGrep != nil) &&
			(kind == EvidenceAnalysis) == (record.Analysis != nil) &&
			(kind == EvidenceArtifact) == (record.Artifact != nil)
	}
	if !only(record.Kind) {
		return fmt.Errorf("evidence identity does not match kind %q", record.Kind)
	}
	switch record.Kind {
	case EvidenceSource:
		if err := sourceinvestigation.ValidateRepository(record.Source.Repository); err != nil {
			return err
		}
		if err := validateCandidatePath(record.Source.Path); err != nil {
			return err
		}
		if !fullHexDigest.MatchString(record.Source.ContentDigest) {
			return fmt.Errorf("source content digest is invalid")
		}
	case EvidenceSourceGrep:
		if err := sourceinvestigation.ValidateRepository(record.SourceGrep.Repository); err != nil {
			return err
		}
		if err := validateCandidatePath(record.SourceGrep.Path); err != nil {
			return err
		}
		if record.SourceGrep.LineStart < 1 || record.SourceGrep.LineEnd < record.SourceGrep.LineStart || record.SourceGrep.LineEnd-record.SourceGrep.LineStart > 10 {
			return fmt.Errorf("source grep line range is invalid")
		}
		if !fullHexDigest.MatchString(record.SourceGrep.ContentDigest) {
			return fmt.Errorf("source grep content digest is invalid")
		}
		if strings.TrimSpace(record.SourceGrep.Match) == "" || len(record.SourceGrep.Match) > maxSourceGrepMatchBytes || strings.ContainsRune(record.SourceGrep.Match, '\x00') {
			return fmt.Errorf("source grep match must be non-empty and bounded")
		}
	case EvidenceAnalysis:
		if strings.TrimSpace(record.Analysis.BuildID) == "" || len(record.Analysis.BuildID) > 256 || strings.TrimSpace(record.Analysis.GeneratedAt) == "" || len(record.Analysis.GeneratedAt) > 128 || !fullHexDigest.MatchString(record.Analysis.RootCauseDigest) {
			return fmt.Errorf("analysis evidence identity is invalid")
		}
	case EvidenceArtifact:
		if strings.TrimSpace(record.Artifact.BuildID) == "" || len(record.Artifact.BuildID) > 256 {
			return fmt.Errorf("artifact build ID is invalid")
		}
		if err := validateCandidatePath(record.Artifact.Path); err != nil {
			return err
		}
		if !fullHexDigest.MatchString(record.Artifact.ContentDigest) {
			return fmt.Errorf("artifact content digest is invalid")
		}
	default:
		return fmt.Errorf("evidence kind %q is invalid", record.Kind)
	}
	return nil
}

func evidenceRecordID(record EvidenceRecord) string {
	copy := record
	copy.ID = ""
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	return string(record.Kind) + ":" + fmt.Sprintf("%x", sum)
}

func validateValidationCommand(command ValidationCommand) error {
	if len(command.Argv) == 0 || len(command.Argv) > 32 {
		return fmt.Errorf("argv must contain 1-32 entries")
	}
	for _, argument := range command.Argv {
		if argument == "" || argument != strings.TrimSpace(argument) || len(argument) > 1024 || strings.ContainsAny(argument, "\r\n\x00") {
			return fmt.Errorf("argv entries must be trimmed and bounded")
		}
	}
	if command.Timeout != "" {
		duration, err := time.ParseDuration(command.Timeout)
		if err != nil || duration <= 0 || duration > 2*time.Hour {
			return fmt.Errorf("timeout must be a positive duration of at most 2h")
		}
	}
	return nil
}

func boundedText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || len(value) > limit || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must be non-empty, trimmed, and at most %d bytes", name, limit)
	}
	return nil
}

func validCauseAssessment(value CauseAssessment) bool {
	switch value {
	case CauseSupports, CauseRefines, CauseContradicts, CauseInconclusive:
		return true
	default:
		return false
	}
}

func validNonActionableReason(value NonActionableReason) bool {
	switch value {
	case NonActionableEnvironmentOrInfrastructure, NonActionableMitigationOnly,
		NonActionableInsufficientEvidence, NonActionableDependencyOwnershipUnverified:
		return true
	default:
		return false
	}
}

func pathAllowedByPolicy(target string, allowed []string) bool {
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(candidate)
		if strings.HasSuffix(candidate, "/") {
			if strings.HasPrefix(target, candidate) {
				return true
			}
			continue
		}
		if target == candidate {
			return true
		}
	}
	return false
}

func candidatePath(candidate CandidateTarget) string {
	switch value := candidate.(type) {
	case *RequiredCallCandidate:
		return value.Path
	case *SymbolAdditionCandidate:
		return value.Path
	case *ProwEnvironmentEntryCandidate:
		return value.ConfigPath
	case *ConfigurationFieldCandidate:
		return value.Path
	default:
		return ""
	}
}

func candidateToTarget(candidate CandidateTarget, repository sourceinvestigation.Repository) (TargetKind, models.RemediationTarget, bool) {
	switch value := candidate.(type) {
	case *RequiredCallCandidate:
		return TargetAddRequiredCall, models.RemediationTarget{
			Intent: models.RemediationIntentModifySymbol, Symbol: value.ContainingSymbol,
			RequiredCall: value.RequiredCall, Path: value.Path,
		}, true
	case *SymbolAdditionCandidate:
		return TargetAddSymbol, models.RemediationTarget{
			Intent: models.RemediationIntentAddSymbol, Symbol: value.Symbol, Path: value.Path,
		}, true
	case *ProwEnvironmentEntryCandidate:
		return TargetSetJobEnvironment, models.RemediationTarget{
			Intent: models.RemediationIntentSetJobEnvironment, Path: value.ConfigPath,
			Repository: repository.Owner + "/" + repository.Name, Revision: repository.Revision,
			Job: value.Job, Container: value.Container, Name: value.Name, Value: value.Value,
		}, true
	case *ConfigurationFieldCandidate:
		return TargetSetConfigurationField, models.RemediationTarget{
			Intent: models.RemediationIntentSetConfiguration, Path: value.Path,
			Value: strings.Join(value.FieldPath, ".") + "=" + value.Value,
		}, true
	default:
		return "", models.RemediationTarget{}, false
	}
}

func candidateExpectedBehavior(candidate CandidateTarget) string {
	switch value := candidate.(type) {
	case *RequiredCallCandidate:
		return fmt.Sprintf("Ensure %s invokes %s in %s.", value.ContainingSymbol, value.RequiredCall, value.Path)
	case *SymbolAdditionCandidate:
		return fmt.Sprintf("Add symbol %s in %s.", value.Symbol, value.Path)
	case *ProwEnvironmentEntryCandidate:
		return fmt.Sprintf("Set %s=%s for container %s in Prow job %s.", value.Name, value.Value, value.Container, value.Job)
	case *ConfigurationFieldCandidate:
		return fmt.Sprintf("Set configuration field %s in %s.", strings.Join(value.FieldPath, "."), value.Path)
	default:
		return ""
	}
}

func cloneCandidate(candidate CandidateTarget) CandidateTarget {
	switch value := candidate.(type) {
	case *RequiredCallCandidate:
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	case *SymbolAdditionCandidate:
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	case *ProwEnvironmentEntryCandidate:
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	case *ConfigurationFieldCandidate:
		if value == nil {
			return nil
		}
		copy := *value
		copy.FieldPath = slices.Clone(value.FieldPath)
		return &copy
	default:
		return nil
	}
}
