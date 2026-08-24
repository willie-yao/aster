// Package remediationinvestigation defines the private, read-only investigation
// that turns one frozen causal group into a typed remediation classification.
package remediationinvestigation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	PromptVersion                  = 8
	SchemaVersion                  = 4
	VerificationVersion            = 6
	ResultVersion                  = 4
	TargetExtractionVersion        = 1
	NonActionableAssessmentVersion = 1
	EvidenceCatalogVersion         = 3
)

type Versions struct {
	Prompt       int `json:"prompt"`
	Schema       int `json:"schema"`
	Verification int `json:"verification"`
}

func CurrentVersions() Versions {
	return Versions{Prompt: PromptVersion, Schema: SchemaVersion, Verification: VerificationVersion}
}

type AnalysisReference struct {
	BuildID          string                          `json:"build_id"`
	TestName         string                          `json:"test_name"`
	GeneratedAt      string                          `json:"generated_at"`
	RootCause        string                          `json:"root_cause"`
	Severity         string                          `json:"severity"`
	RelevantFiles    []string                        `json:"relevant_files,omitempty"`
	Evidence         []models.EvidenceCitation       `json:"evidence_citations,omitempty"`
	SourceRepository *sourceinvestigation.Repository `json:"source_repository,omitempty"`
}

type BuildReference struct {
	BuildID     string                          `json:"build_id"`
	BuildPrefix string                          `json:"build_prefix"`
	ProwURL     string                          `json:"prow_url,omitempty"`
	WebURL      string                          `json:"web_url,omitempty"`
	Source      *sourceinvestigation.Repository `json:"source,omitempty"`
}

type ValidationCommand struct {
	Argv    []string `json:"argv"`
	Timeout string   `json:"timeout"`
}

type RepositoryPolicy struct {
	Repository      string              `json:"repository"`
	AllowedPaths    []string            `json:"allowed_paths"`
	AllowedCommands []ValidationCommand `json:"allowed_commands"`
}

type DestinationPolicy struct {
	Project      string             `json:"project"`
	Repositories []RepositoryPolicy `json:"repositories"`
}

// FrozenInput binds an investigation to one exact published causal group and
// the private policy and provenance that affect its meaning.
type FrozenInput struct {
	PatternID           string                         `json:"pattern_id"`
	PatternHash         string                         `json:"pattern_hash"`
	CausalGroupID       string                         `json:"causal_group_id"`
	CausalGroupHash     string                         `json:"causal_group_hash"`
	JobID               string                         `json:"job_id"`
	JobName             string                         `json:"job_name"`
	Recurrence          models.PatternRecurrence       `json:"recurrence_classification"`
	Group               models.PatternCausalGroup      `json:"causal_group"`
	Builds              []BuildReference               `json:"builds"`
	Analyses            []AnalysisReference            `json:"analyses"`
	RelevantFiles       []string                       `json:"relevant_files,omitempty"`
	InvestigationSource sourceinvestigation.Repository `json:"investigation_source"`
	DestinationPolicy   DestinationPolicy              `json:"destination_policy"`
	ConsumerPrompt      string                         `json:"consumer_prompt,omitempty"`
	ConsumerPromptHash  string                         `json:"consumer_prompt_hash"`
	SkillHash           string                         `json:"skill_hash,omitempty"`
	ProviderFingerprint string                         `json:"provider_fingerprint"`
	Versions            Versions                       `json:"versions"`
}

func HashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func FrozenInputDigest(input FrozenInput) string {
	input = canonicalFrozenInput(input)
	encoded, _ := json.Marshal(input)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func canonicalFrozenInput(input FrozenInput) FrozenInput {
	input.Group.Builds = slices.Clone(input.Group.Builds)
	slices.Sort(input.Group.Builds)
	input.Builds = slices.Clone(input.Builds)
	sort.Slice(input.Builds, func(i, j int) bool { return input.Builds[i].BuildID < input.Builds[j].BuildID })
	input.Analyses = slices.Clone(input.Analyses)
	for index := range input.Analyses {
		input.Analyses[index].RelevantFiles = slices.Clone(input.Analyses[index].RelevantFiles)
		slices.Sort(input.Analyses[index].RelevantFiles)
		input.Analyses[index].Evidence = slices.Clone(input.Analyses[index].Evidence)
	}
	sort.Slice(input.Analyses, func(i, j int) bool { return input.Analyses[i].BuildID < input.Analyses[j].BuildID })
	input.RelevantFiles = slices.Clone(input.RelevantFiles)
	slices.Sort(input.RelevantFiles)
	input.DestinationPolicy.Repositories = slices.Clone(input.DestinationPolicy.Repositories)
	for index := range input.DestinationPolicy.Repositories {
		repository := &input.DestinationPolicy.Repositories[index]
		repository.AllowedPaths = slices.Clone(repository.AllowedPaths)
		slices.Sort(repository.AllowedPaths)
		repository.AllowedCommands = cloneValidationCommands(repository.AllowedCommands)
		sort.Slice(repository.AllowedCommands, func(i, j int) bool {
			return validationCommandKey(repository.AllowedCommands[i]) < validationCommandKey(repository.AllowedCommands[j])
		})
	}
	sort.Slice(input.DestinationPolicy.Repositories, func(i, j int) bool {
		return input.DestinationPolicy.Repositories[i].Repository < input.DestinationPolicy.Repositories[j].Repository
	})
	return input
}

type Classification string

const (
	ClassificationActionable                  Classification = "actionable"
	ClassificationAlreadyFixed                Classification = "already_fixed"
	ClassificationExternalDependency          Classification = "external_dependency"
	ClassificationEnvironmentOrInfrastructure Classification = "environment_or_infrastructure"
	ClassificationMitigationOnly              Classification = "mitigation_only"
	ClassificationInsufficientEvidence        Classification = "insufficient_evidence"
	ClassificationAmbiguous                   Classification = "ambiguous"
)

type CauseAssessment string

const (
	CauseSupports     CauseAssessment = "supports"
	CauseRefines      CauseAssessment = "refines"
	CauseContradicts  CauseAssessment = "contradicts"
	CauseInconclusive CauseAssessment = "inconclusive"
)

type NonActionableReason string

const (
	NonActionableEnvironmentOrInfrastructure   NonActionableReason = "environment_or_infrastructure"
	NonActionableMitigationOnly                NonActionableReason = "mitigation_only"
	NonActionableInsufficientEvidence          NonActionableReason = "insufficient_evidence"
	NonActionableDependencyOwnershipUnverified NonActionableReason = "dependency_ownership_unverified"
)

type CandidateKind string

const (
	CandidateRequiredCall         CandidateKind = "required_call"
	CandidateSymbolAddition       CandidateKind = "symbol_addition"
	CandidateProwEnvironmentEntry CandidateKind = "prow_environment_entry"
	CandidateConfigurationField   CandidateKind = "configuration_field"
)

// CandidateTarget is a model-authored discriminated target. Each concrete
// variant contains only fields that are relevant to that target kind.
type CandidateTarget interface {
	candidateKind() CandidateKind
}

type RequiredCallCandidate struct {
	Kind             CandidateKind `json:"kind"`
	Path             string        `json:"path"`
	ContainingSymbol string        `json:"containing_symbol"`
	RequiredCall     string        `json:"required_call"`
}

func (*RequiredCallCandidate) candidateKind() CandidateKind { return CandidateRequiredCall }

type SymbolAdditionCandidate struct {
	Kind   CandidateKind `json:"kind"`
	Path   string        `json:"path"`
	Symbol string        `json:"symbol"`
}

func (*SymbolAdditionCandidate) candidateKind() CandidateKind { return CandidateSymbolAddition }

type ProwEnvironmentEntryCandidate struct {
	Kind       CandidateKind `json:"kind"`
	ConfigPath string        `json:"config_path"`
	Job        string        `json:"job"`
	Container  string        `json:"container"`
	Name       string        `json:"name"`
	Value      string        `json:"value"`
}

func (*ProwEnvironmentEntryCandidate) candidateKind() CandidateKind {
	return CandidateProwEnvironmentEntry
}

type ConfigurationFieldCandidate struct {
	Kind      CandidateKind `json:"kind"`
	Path      string        `json:"path"`
	FieldPath []string      `json:"field_path"`
	Value     string        `json:"value"`
}

func (*ConfigurationFieldCandidate) candidateKind() CandidateKind { return CandidateConfigurationField }

type EvidenceKind string

const (
	EvidenceSource     EvidenceKind = "source"
	EvidenceSourceGrep EvidenceKind = "source_grep"
	EvidenceAnalysis   EvidenceKind = "analysis"
	EvidenceArtifact   EvidenceKind = "artifact"
)

type SourceEvidenceIdentity struct {
	Repository    sourceinvestigation.Repository `json:"repository"`
	Path          string                         `json:"path"`
	ContentDigest string                         `json:"content_digest"`
}

type SourceGrepEvidenceIdentity struct {
	Repository    sourceinvestigation.Repository `json:"repository"`
	Path          string                         `json:"path"`
	LineStart     int                            `json:"line_start"`
	LineEnd       int                            `json:"line_end"`
	ContentDigest string                         `json:"content_digest"`
	Match         string                         `json:"match"`
}

type AnalysisEvidenceIdentity struct {
	BuildID         string `json:"build_id"`
	GeneratedAt     string `json:"generated_at"`
	RootCauseDigest string `json:"root_cause_digest"`
}

type ArtifactEvidenceIdentity struct {
	BuildID string `json:"build_id"`
	Path    string `json:"path"`
	// LineStart and LineEnd bound the region this record covers, and
	// ContentDigest covers exactly that region. Verification re-reads the same
	// lines, so a citation into a large artifact stays checkable without ever
	// holding the whole file.
	LineStart     int    `json:"line_start"`
	LineEnd       int    `json:"line_end"`
	ContentDigest string `json:"content_digest"`
}

// EvidenceRecord is private engine-issued evidence identity. The model cites
// only ID values and cannot author paths, revisions, build IDs, or excerpts.
type EvidenceRecord struct {
	ID         string                      `json:"id"`
	Kind       EvidenceKind                `json:"kind"`
	Source     *SourceEvidenceIdentity     `json:"source,omitempty"`
	SourceGrep *SourceGrepEvidenceIdentity `json:"source_grep,omitempty"`
	Analysis   *AnalysisEvidenceIdentity   `json:"analysis,omitempty"`
	Artifact   *ArtifactEvidenceIdentity   `json:"artifact,omitempty"`
}

type EvidenceCatalog struct {
	Version int              `json:"version"`
	Records []EvidenceRecord `json:"records"`
}

type TargetKind string

const (
	TargetAddSymbol             TargetKind = "add_symbol"
	TargetAddRequiredCall       TargetKind = "add_required_call"
	TargetSetJobEnvironment     TargetKind = "set_job_environment"
	TargetSetConfigurationField TargetKind = "set_configuration_field"
)

// ActionableProposal contains only engine-derived, deterministically verified
// action inputs. It is never decoded from a model response.
type ActionableProposal struct {
	TargetKind                TargetKind                     `json:"target_kind"`
	Repository                sourceinvestigation.Repository `json:"repository"`
	Target                    models.RemediationTarget       `json:"target"`
	ExpectedBehavior          string                         `json:"expected_behavior"`
	EvidenceIDs               []string                       `json:"evidence_ids"`
	VerificationRequirements  []string                       `json:"verification_requirements"`
	AllowedChangedPaths       []string                       `json:"allowed_changed_paths"`
	AllowedValidationCommands []ValidationCommand            `json:"allowed_validation_commands"`
}

// TargetHypothesis is a model-authored verification subject. It does not carry
// repository, revision, lifecycle, policy, command, or action eligibility.
type TargetHypothesis struct {
	Target             CandidateTarget `json:"target"`
	EvidenceIDs        []string        `json:"evidence_ids"`
	RelationshipReason string          `json:"relationship_reason"`
}

// TargetExtraction is the first bounded structured model stage.
type TargetExtraction struct {
	Version    int                `json:"version"`
	Hypotheses []TargetHypothesis `json:"hypotheses"`
}

// NonActionableAssessment is requested only when no target hypothesis verifies.
type NonActionableAssessment struct {
	Version             int                 `json:"version"`
	CauseAssessment     CauseAssessment     `json:"cause_assessment"`
	Reason              string              `json:"reason"`
	EvidenceIDs         []string            `json:"evidence_ids"`
	NonActionableReason NonActionableReason `json:"non_actionable_reason"`
}

// Result is the final private engine-owned combination of the two model stages.
// Dashboard code owns repository, revision, source state, lifecycle, and action eligibility.
type Result struct {
	Version       int                      `json:"version"`
	Hypotheses    []TargetHypothesis       `json:"hypotheses"`
	NonActionable *NonActionableAssessment `json:"non_actionable,omitempty"`
}

type EvidenceStats struct {
	ToolCalls         int `json:"tool_calls"`
	SourceLists       int `json:"source_lists,omitempty"`
	SourceGreps       int `json:"source_greps,omitempty"`
	SourceReads       int `json:"source_reads,omitempty"`
	SourceReadBytes   int `json:"source_read_bytes,omitempty"`
	ArtifactLists     int `json:"artifact_lists,omitempty"`
	ArtifactGreps     int `json:"artifact_greps,omitempty"`
	ArtifactReads     int `json:"artifact_reads,omitempty"`
	ArtifactReadBytes int `json:"artifact_read_bytes,omitempty"`
	ToolErrors        int `json:"tool_errors,omitempty"`
}

type Metrics struct {
	ElapsedMs                        int    `json:"elapsed_ms"`
	ModelRequests                    int    `json:"model_requests,omitempty"`
	ReportedRequests                 int    `json:"reported_requests,omitempty"`
	UnreportedRequests               int    `json:"unreported_requests,omitempty"`
	CoverageCountsKnown              bool   `json:"coverage_counts_known,omitempty"`
	UsageInvalid                     bool   `json:"usage_invalid,omitempty"`
	Currency                         string `json:"currency,omitempty"`
	PricingHash                      string `json:"pricing_hash,omitempty"`
	InputTokens                      int64  `json:"input_tokens,omitempty"`
	CachedInputTokens                int64  `json:"cached_input_tokens,omitempty"`
	OutputTokens                     int64  `json:"output_tokens,omitempty"`
	ReasoningTokens                  int64  `json:"reasoning_tokens,omitempty"`
	EstimatedCostNanos               int64  `json:"estimated_cost_nanos,omitempty"`
	RepairCount                      int    `json:"repair_count,omitempty"`
	EvidenceRetryCount               int    `json:"evidence_retry_count,omitempty"`
	TargetExtractionModelRequests    *int   `json:"target_extraction_model_requests,omitempty"`
	TargetExtractionProviderAttempts *int   `json:"target_extraction_provider_attempts,omitempty"`
	TargetExtractionRepairCount      *int   `json:"target_extraction_repair_count,omitempty"`
	TargetExtractionFinalAttempt     string `json:"target_extraction_final_attempt,omitempty"`
}

type Provenance struct {
	InputDigest         string                         `json:"input_digest"`
	ProviderFingerprint string                         `json:"provider_fingerprint"`
	Model               string                         `json:"model,omitempty"`
	APIMode             string                         `json:"api_mode,omitempty"`
	ReasoningEffort     string                         `json:"reasoning_effort,omitempty"`
	Source              sourceinvestigation.Repository `json:"source"`
	Versions            Versions                       `json:"versions"`
	ConsumerPromptHash  string                         `json:"consumer_prompt_hash"`
	SkillHash           string                         `json:"skill_hash,omitempty"`
	PolicyHash          string                         `json:"policy_hash"`
	Evidence            EvidenceStats                  `json:"evidence"`
	Metrics             Metrics                        `json:"metrics"`
	CompletedAt         string                         `json:"completed_at"`
}

func NewProvenance(input FrozenInput, model, apiMode, reasoningEffort string, evidence EvidenceStats, metrics Metrics, completed time.Time) Provenance {
	return Provenance{
		InputDigest: FrozenInputDigest(input), ProviderFingerprint: input.ProviderFingerprint,
		Model: model, APIMode: apiMode, ReasoningEffort: reasoningEffort, Source: input.InvestigationSource,
		Versions: input.Versions, ConsumerPromptHash: input.ConsumerPromptHash,
		SkillHash: input.SkillHash, PolicyHash: HashPolicy(input.DestinationPolicy),
		Evidence: evidence, Metrics: metrics, CompletedAt: completed.UTC().Format(time.RFC3339),
	}
}

func cloneValidationCommands(commands []ValidationCommand) []ValidationCommand {
	out := slices.Clone(commands)
	for index := range out {
		out[index].Argv = slices.Clone(out[index].Argv)
	}
	return out
}

func validationCommandKey(command ValidationCommand) string {
	encoded, _ := json.Marshal(command)
	return string(encoded)
}

func ResultDigest(result Result) string {
	encoded, _ := json.Marshal(result)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func EvidenceCatalogDigest(catalog EvidenceCatalog) string {
	catalog = canonicalEvidenceCatalog(catalog)
	encoded, _ := json.Marshal(catalog)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func canonicalEvidenceCatalog(catalog EvidenceCatalog) EvidenceCatalog {
	catalog.Records = slices.Clone(catalog.Records)
	sort.Slice(catalog.Records, func(i, j int) bool { return catalog.Records[i].ID < catalog.Records[j].ID })
	return catalog
}

func HashPolicy(policy DestinationPolicy) string {
	canonical := canonicalFrozenInput(FrozenInput{DestinationPolicy: policy}).DestinationPolicy
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
