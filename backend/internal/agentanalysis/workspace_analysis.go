package agentanalysis

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

//go:embed skill/workspace-analysis.md
var workspaceAnalysisSkill string

//go:embed skill/analysis-agent.md
var workspaceAnalysisAgent string

//go:embed skill/analysis-finalizer.md
var workspaceAnalysisFinalizer string

// WorkspaceSkillHash returns the file-backed analyzer prompt fingerprint.
func WorkspaceSkillHash() string {
	return hashString(workspaceAnalysisSkill + "\n" + workspaceAnalysisAgent + "\n" + workspaceAnalysisFinalizer)
}

// WorkspaceAgentPrompt returns the static read-only evidence-agent guidance.
func WorkspaceAgentPrompt() string { return strings.TrimSpace(workspaceAnalysisAgent) }

// WorkspaceFinalizerPrompt returns the static StructuredOutput-only guidance.
func WorkspaceFinalizerPrompt() string { return strings.TrimSpace(workspaceAnalysisFinalizer) }

// WorkspaceFinalizationInstruction requests the authoritative result after evidence succeeds.
func WorkspaceFinalizationInstruction() string {
	return "Finalize the analysis now from evidence already inspected in this session. Use StructuredOutput exactly once. Include source citations or relevant files only if source evidence was successfully read or grepped during the evidence phase."
}

// WorkspaceCitationReference is a model-authored path and exact line range.
type WorkspaceCitationReference struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// WorkspaceAnalysis is one validated file-backed OpenCode result.
type WorkspaceAnalysis struct {
	Summary           string                         `json:"summary"`
	IsTransient       bool                           `json:"is_transient"`
	RootCause         string                         `json:"root_cause"`
	Severity          string                         `json:"severity"`
	SuggestedFix      string                         `json:"suggested_fix"`
	RelevantFiles     []string                       `json:"relevant_files,omitempty"`
	EvidenceCitations []models.EvidenceCitation      `json:"evidence_citations"`
	SourceCitations   []sourceinvestigation.Citation `json:"source_citations,omitempty"`
	UnresolvedDetails []string                       `json:"unresolved_details,omitempty"`
}

const (
	WorkspaceTelemetryAvailable   = "available"
	WorkspaceTelemetryUnavailable = "unavailable"
	WorkspaceTelemetryMalformed   = "malformed"
	WorkspaceTelemetryTruncated   = "truncated"
)

// WorkspaceUsage records provider telemetry when the runtime exposes it.
type WorkspaceUsage struct {
	Available         bool   `json:"available"`
	Status            string `json:"status"`
	ModelRequests     int    `json:"model_requests,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
	CostAvailable     bool   `json:"cost_available"`
	CostUSD           string `json:"cost_usd,omitempty"`
}

// WorkspaceToolTelemetry is one sanitized OpenCode tool aggregate.
type WorkspaceToolTelemetry struct {
	Name     string `json:"name"`
	Count    int    `json:"count"`
	Failures int    `json:"failures,omitempty"`
	Denied   int    `json:"denied,omitempty"`
}

// WorkspaceOpenCodeRequestShape records bounded engine-owned request facts.
type WorkspaceOpenCodeRequestShape struct {
	Available                  bool   `json:"available"`
	StreamingMode              string `json:"streaming_mode,omitempty"`
	ModelID                    string `json:"model_id,omitempty"`
	SystemPromptBytesAvailable bool   `json:"system_prompt_bytes_available"`
	SystemPromptBytes          int    `json:"system_prompt_bytes,omitempty"`
	UserPromptBytes            int    `json:"user_prompt_bytes,omitempty"`
	ToolSchemaAvailable        bool   `json:"tool_schema_available"`
	ToolCount                  int    `json:"tool_count,omitempty"`
	ToolSchemaSHA256           string `json:"tool_schema_sha256,omitempty"`
	ResponseSchemaSHA256       string `json:"response_schema_sha256,omitempty"`
	ToolChoiceMode             string `json:"tool_choice_mode,omitempty"`
	ContextLimit               int    `json:"context_limit,omitempty"`
	OutputTokenLimit           int    `json:"output_token_limit,omitempty"`
	OpenCodeVersion            string `json:"opencode_version,omitempty"`
}

// WorkspaceOpenCodeErrorTelemetry contains only allowlisted failure facts.
type WorkspaceOpenCodeErrorTelemetry struct {
	Available                  bool   `json:"available"`
	Name                       string `json:"name,omitempty"`
	HTTPStatusCode             int    `json:"http_status_code,omitempty"`
	RetryableKnown             bool   `json:"retryable_known"`
	Retryable                  bool   `json:"retryable"`
	Classification             string `json:"classification,omitempty"`
	MetadataCode               string `json:"metadata_code,omitempty"`
	HeaderTimeout              bool   `json:"header_timeout,omitempty"`
	ResponseStreamError        bool   `json:"response_stream_error,omitempty"`
	ContextOverflow            bool   `json:"context_overflow,omitempty"`
	ResponseContentTypePresent bool   `json:"response_content_type_present,omitempty"`
	ResponseBodyPresent        bool   `json:"response_body_present,omitempty"`
	ResponseBodyBytesBounded   int    `json:"response_body_bytes_bounded,omitempty"`
	ResponseBodySHA256         string `json:"response_body_sha256,omitempty"`
}

// WorkspaceOpenCodeTelemetry contains no prompts, responses, evidence, or raw events.
type WorkspaceOpenCodeTelemetry struct {
	Available                    bool                            `json:"available"`
	Status                       string                          `json:"status"`
	EventCount                   int                             `json:"event_count,omitempty"`
	ProviderRequests             int                             `json:"provider_requests,omitempty"`
	RequestShape                 WorkspaceOpenCodeRequestShape   `json:"request_shape"`
	Error                        WorkspaceOpenCodeErrorTelemetry `json:"error"`
	Tools                        []WorkspaceToolTelemetry        `json:"tools,omitempty"`
	DeniedToolCount              int                             `json:"denied_tool_count,omitempty"`
	ToolFailureCount             int                             `json:"tool_failure_count,omitempty"`
	StepsUsed                    int                             `json:"steps_used,omitempty"`
	StructuredOutputRetriesKnown bool                            `json:"structured_output_retries_known"`
	StructuredOutputRetries      int                             `json:"structured_output_retries,omitempty"`
	StructuredOutputErrors       int                             `json:"structured_output_errors,omitempty"`
	EvidencePhaseCompleted       bool                            `json:"evidence_phase_completed,omitempty"`
	EvidencePhaseSteps           int                             `json:"evidence_phase_steps,omitempty"`
	EvidencePhaseRequests        int                             `json:"evidence_phase_requests,omitempty"`
	ArtifactEvidenceToolCalls    int                             `json:"artifact_evidence_tool_calls,omitempty"`
	SourceEvidenceToolCalls      int                             `json:"source_evidence_tool_calls,omitempty"`
	FinalizationPhaseCompleted   bool                            `json:"finalization_phase_completed,omitempty"`
	FinalizationPhaseSteps       int                             `json:"finalization_phase_steps,omitempty"`
	FinalizationPhaseRequests    int                             `json:"finalization_phase_requests,omitempty"`
	StructuredOutputToolCalls    int                             `json:"structured_output_tool_calls,omitempty"`
	ContextLimit                 bool                            `json:"context_limit,omitempty"`
	TimedOut                     bool                            `json:"timed_out,omitempty"`
	FailureCode                  string                          `json:"failure_code,omitempty"`
	StdoutTruncated              bool                            `json:"stdout_truncated,omitempty"`
	StderrTruncated              bool                            `json:"stderr_truncated,omitempty"`
}

// WorkspaceExecutionResult is the single executor result read from Pod logs.
type WorkspaceExecutionResult struct {
	Version           int                         `json:"version"`
	ContractVersion   string                      `json:"contract_version"`
	RequestHash       string                      `json:"request_hash"`
	TerminalState     engineruntime.TerminalState `json:"terminal_state"`
	FailureReason     string                      `json:"failure_reason,omitempty"`
	Analysis          *WorkspaceAnalysis          `json:"analysis,omitempty"`
	DurationMs        int64                       `json:"duration_ms"`
	Usage             WorkspaceUsage              `json:"usage"`
	OpenCodeTelemetry WorkspaceOpenCodeTelemetry  `json:"opencode_telemetry"`
}

type workspaceAnalysisEnvelope struct {
	Version           int                          `json:"version"`
	ContractVersion   string                       `json:"contract_version"`
	Summary           string                       `json:"summary"`
	IsTransient       *bool                        `json:"is_transient"`
	RootCause         string                       `json:"root_cause"`
	Severity          string                       `json:"severity"`
	SuggestedFix      string                       `json:"suggested_fix"`
	RelevantFiles     []string                     `json:"relevant_files"`
	EvidenceCitations []WorkspaceCitationReference `json:"evidence_citations"`
	SourceCitations   []WorkspaceCitationReference `json:"source_citations"`
	UnresolvedDetails []string                     `json:"unresolved_details"`
}

// WorkspaceResultSchema returns the exact schema passed to OpenCode.
func WorkspaceResultSchema() map[string]any {
	citation := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"path":       map[string]any{"type": "string"},
			"line_start": map[string]any{"type": "integer", "minimum": 1},
			"line_end":   map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"path", "line_start", "line_end"},
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object", "additionalProperties": false,
		"properties": map[string]any{
			"version":            map[string]any{"type": "integer", "const": WorkspaceResultVersion},
			"contract_version":   map[string]any{"type": "string", "const": WorkspaceContractVersion},
			"summary":            map[string]any{"type": "string"},
			"is_transient":       map[string]any{"type": "boolean"},
			"root_cause":         map[string]any{"type": "string"},
			"severity":           map[string]any{"type": "string", "enum": []string{"Critical", "High", "Medium", "Low", "Transient-Ignore"}},
			"suggested_fix":      map[string]any{"type": "string"},
			"relevant_files":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxRelevantFiles},
			"evidence_citations": map[string]any{"type": "array", "items": citation, "minItems": 1, "maxItems": maxEvidenceCitations},
			"source_citations":   map[string]any{"type": "array", "items": citation, "maxItems": maxSourceCitations},
			"unresolved_details": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxUnresolvedDetails},
		},
		"required": []string{"version", "contract_version", "summary", "is_transient", "root_cause", "severity", "suggested_fix", "relevant_files", "evidence_citations", "source_citations", "unresolved_details"},
	}
}

// WorkspaceResultSchemaHash returns the schema fingerprint sealed into each request.
func WorkspaceResultSchemaHash() string {
	data, err := json.Marshal(WorkspaceResultSchema())
	if err != nil {
		panic(err)
	}
	return hashString(string(data))
}

// ParseWorkspaceAnalysis validates one schema-constrained result against mounted files.
func ParseWorkspaceAnalysis(raw string, manifest WorkspaceManifest, artifactRoot, sourceRoot string) (WorkspaceAnalysis, error) {
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return WorkspaceAnalysis{}, err
	}
	if raw == "" || len(raw) > maxResultBytes || !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return WorkspaceAnalysis{}, fmt.Errorf("%w: workspace analysis output is empty, invalid, or oversized", ErrInvalidResult)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return WorkspaceAnalysis{}, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(raw), maxResultBytes+1))
	decoder.DisallowUnknownFields()
	var parsed workspaceAnalysisEnvelope
	if err := decoder.Decode(&parsed); err != nil {
		return WorkspaceAnalysis{}, fmt.Errorf("%w: decode workspace analysis: %v", ErrInvalidResult, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return WorkspaceAnalysis{}, fmt.Errorf("%w: workspace analysis contains trailing data", ErrInvalidResult)
	}
	if parsed.Version != WorkspaceResultVersion || parsed.ContractVersion != WorkspaceContractVersion || parsed.IsTransient == nil {
		return WorkspaceAnalysis{}, fmt.Errorf("%w: workspace analysis version or transient classification is invalid", ErrInvalidResult)
	}
	analysis := WorkspaceAnalysis{
		Summary: strings.TrimSpace(parsed.Summary), IsTransient: *parsed.IsTransient,
		RootCause: strings.TrimSpace(parsed.RootCause), Severity: strings.TrimSpace(parsed.Severity),
		SuggestedFix: strings.TrimSpace(parsed.SuggestedFix), RelevantFiles: slices.Clone(parsed.RelevantFiles),
		UnresolvedDetails: slices.Clone(parsed.UnresolvedDetails),
	}
	for index := range analysis.UnresolvedDetails {
		analysis.UnresolvedDetails[index] = strings.TrimSpace(analysis.UnresolvedDetails[index])
	}
	for _, citation := range parsed.EvidenceCitations {
		analysis.EvidenceCitations = append(analysis.EvidenceCitations, models.EvidenceCitation{Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd})
	}
	for _, citation := range parsed.SourceCitations {
		analysis.SourceCitations = append(analysis.SourceCitations, sourceinvestigation.Citation{Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd})
	}
	return canonicalizeWorkspaceAnalysis(analysis, manifest, artifactRoot, sourceRoot, false)
}

// ValidateWorkspaceAnalysis rechecks canonical output against the sealed workspace.
func ValidateWorkspaceAnalysis(analysis WorkspaceAnalysis, manifest WorkspaceManifest, artifactRoot, sourceRoot string) (WorkspaceAnalysis, error) {
	return canonicalizeWorkspaceAnalysis(analysis, manifest, artifactRoot, sourceRoot, true)
}

func canonicalizeWorkspaceAnalysis(analysis WorkspaceAnalysis, manifest WorkspaceManifest, artifactRoot, sourceRoot string, requireCanonical bool) (WorkspaceAnalysis, error) {
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return WorkspaceAnalysis{}, err
	}
	text := Analysis{Summary: analysis.Summary, IsTransient: analysis.IsTransient, RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix, UnresolvedDetails: slices.Clone(analysis.UnresolvedDetails)}
	if err := validateAnalysisText(text); err != nil {
		return WorkspaceAnalysis{}, err
	}
	artifactCitations, err := verifyWorkspaceArtifactCitations(analysis.EvidenceCitations, manifest, artifactRoot, requireCanonical)
	if err != nil {
		return WorkspaceAnalysis{}, err
	}
	sourceCitations, err := verifyWorkspaceSourceCitations(analysis.SourceCitations, sourceRoot, requireCanonical)
	if err != nil {
		return WorkspaceAnalysis{}, err
	}
	relevantFiles, err := workspaceRelevantFiles(analysis.RelevantFiles, sourceCitations)
	if err != nil {
		return WorkspaceAnalysis{}, err
	}
	analysis.Summary = strings.TrimSpace(analysis.Summary)
	analysis.RootCause = strings.TrimSpace(analysis.RootCause)
	analysis.Severity = strings.TrimSpace(analysis.Severity)
	analysis.SuggestedFix = strings.TrimSpace(analysis.SuggestedFix)
	analysis.EvidenceCitations = artifactCitations
	analysis.SourceCitations = sourceCitations
	analysis.RelevantFiles = relevantFiles
	return analysis, nil
}

// MarshalWorkspaceAnalysis encodes one canonical result file.
func MarshalWorkspaceAnalysis(analysis WorkspaceAnalysis) ([]byte, error) {
	envelope := struct {
		Version         int    `json:"version"`
		ContractVersion string `json:"contract_version"`
		WorkspaceAnalysis
	}{Version: WorkspaceResultVersion, ContractVersion: WorkspaceContractVersion, WorkspaceAnalysis: analysis}
	return json.Marshal(envelope)
}

// DecodeWorkspaceExecutionResult decodes one strict executor log result.
func DecodeWorkspaceExecutionResult(raw string) (WorkspaceExecutionResult, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 1<<20 || !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return WorkspaceExecutionResult{}, fmt.Errorf("workspace execution result is empty, invalid, or oversized")
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return WorkspaceExecutionResult{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result WorkspaceExecutionResult
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return result, fmt.Errorf("workspace execution result contains trailing data")
	}
	return result, nil
}

// ValidateWorkspaceExecutionResult validates identity, lifecycle, and citations.
func ValidateWorkspaceExecutionResult(result WorkspaceExecutionResult, request WorkspaceExecutionRequest, artifactRoot, sourceRoot string) (WorkspaceExecutionResult, error) {
	if err := ValidateWorkspaceExecutionRequest(request); err != nil {
		return result, err
	}
	if result.Version != WorkspaceResultVersion || result.ContractVersion != WorkspaceContractVersion || result.RequestHash != request.Hash {
		return result, fmt.Errorf("workspace execution result identity mismatch")
	}
	if result.DurationMs < 0 || result.DurationMs > request.TimeoutSeconds*1000+30_000 {
		return result, fmt.Errorf("workspace execution duration is outside the request bound")
	}
	if err := validateWorkspaceUsage(result.Usage); err != nil {
		return result, err
	}
	if err := validateWorkspaceOpenCodeTelemetry(result.OpenCodeTelemetry); err != nil {
		return result, err
	}
	switch result.TerminalState {
	case engineruntime.TerminalSucceeded:
		if strings.TrimSpace(result.FailureReason) != "" || result.Analysis == nil {
			return result, fmt.Errorf("successful workspace execution must contain only an analysis")
		}
		if !result.OpenCodeTelemetry.EvidencePhaseCompleted || !result.OpenCodeTelemetry.FinalizationPhaseCompleted || result.OpenCodeTelemetry.ArtifactEvidenceToolCalls < 1 || result.OpenCodeTelemetry.StructuredOutputToolCalls != 1 {
			return result, fmt.Errorf("successful workspace execution is missing required phase telemetry")
		}
		if (len(result.Analysis.SourceCitations) > 0 || len(result.Analysis.RelevantFiles) > 0) && result.OpenCodeTelemetry.SourceEvidenceToolCalls < 1 {
			return result, fmt.Errorf("successful workspace execution contains source claims without source evidence telemetry")
		}
		if err := VerifyPreparedSourceWorkspace(context.Background(), sourceRoot, request.Manifest.Source.Revision, request.SourceModePolicy); err != nil {
			return result, err
		}
		if err := VerifyArtifactWorkspace(artifactRoot, request.Manifest); err != nil {
			return result, err
		}
		analysis, err := ValidateWorkspaceAnalysis(*result.Analysis, request.Manifest, artifactRoot, sourceRoot)
		if err != nil {
			return result, err
		}
		result.Analysis = &analysis
	case engineruntime.TerminalFailed, engineruntime.TerminalTimedOut, engineruntime.TerminalCancelled:
		if strings.TrimSpace(result.FailureReason) == "" || result.Analysis != nil {
			return result, fmt.Errorf("failed workspace execution has an invalid result shape")
		}
	default:
		return result, fmt.Errorf("workspace execution terminal state is invalid")
	}
	return result, nil
}

// WorkspaceInstruction builds the one-session OpenCode instruction.
func WorkspaceInstruction(request WorkspaceExecutionRequest, workspaceRoot string) (string, error) {
	if err := ValidateWorkspaceExecutionRequest(request); err != nil {
		return "", err
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	failure, err := json.MarshalIndent(request.Manifest.Request, "", "  ")
	if err != nil {
		return "", err
	}
	var artifactPaths []string
	for _, file := range request.Manifest.Artifacts {
		artifactPaths = append(artifactPaths, file.Path)
	}
	paths, err := json.MarshalIndent(artifactPaths, "", "  ")
	if err != nil {
		return "", err
	}
	instruction := fmt.Sprintf(`%s

Workspace root: %s
Source revision: %s
Artifact manifest hash: %s

Failure metadata:
%s

Consumer guidance:
<consumer-guidance>
%s
</consumer-guidance>

Available artifact paths:
%s
`, strings.TrimSpace(workspaceAnalysisSkill), workspaceRoot, request.Manifest.Source.Revision, request.Manifest.Hash, failure, request.Manifest.ConsumerPrompt, paths)
	if len(instruction) > maxAgentPromptBytes || !utf8.ValidString(instruction) {
		return "", fmt.Errorf("workspace analysis instruction exceeds %d bytes", maxAgentPromptBytes)
	}
	return instruction, nil
}

// FailureAnalysisResult maps a validated private result to the authoritative wire shape.
func (analysis WorkspaceAnalysis) FailureAnalysisResult(generatedAt, model string, durationMs int64, usage WorkspaceUsage) ai.FailureAnalysisResult {
	return ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: generatedAt, Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		Analysis: &models.AIAnalysis{
			GeneratedAt: generatedAt, Model: model, RootCause: analysis.RootCause, Severity: analysis.Severity,
			SuggestedFix: analysis.SuggestedFix, RelevantFiles: slices.Clone(analysis.RelevantFiles),
			EvidenceCitations: slices.Clone(analysis.EvidenceCitations), Mode: "agent-sandbox-opencode",
			ModelRequests: usage.ModelRequests, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			ElapsedMs: int(durationMs),
		},
	}
}

func verifyWorkspaceArtifactCitations(citations []models.EvidenceCitation, manifest WorkspaceManifest, root string, requireCanonical bool) ([]models.EvidenceCitation, error) {
	if len(citations) < 1 || len(citations) > maxEvidenceCitations {
		return nil, fmt.Errorf("%w: artifact citations must contain 1-%d entries", ErrInvalidResult, maxEvidenceCitations)
	}
	known := make(map[string]WorkspaceFile, len(manifest.Artifacts))
	for _, file := range manifest.Artifacts {
		known[file.Path] = file
	}
	out := slices.Clone(citations)
	seen := map[string][][2]int{}
	for index := range out {
		citation := &out[index]
		file, ok := known[citation.Path]
		if !ok || strings.TrimSpace(citation.Path) != citation.Path {
			return nil, fmt.Errorf("%w: artifact citation %d references an unknown path", ErrInvalidResult, index)
		}
		content, err := readWorkspaceText(root, citation.Path, file.Size)
		if err != nil {
			return nil, fmt.Errorf("%w: artifact citation %d: %v", ErrInvalidResult, index, err)
		}
		quote, err := canonicalWorkspaceQuote(content, citation.LineStart, citation.LineEnd)
		if err != nil {
			return nil, fmt.Errorf("%w: artifact citation %d: %v", ErrInvalidResult, index, err)
		}
		if requireCanonical && citation.Quote != quote {
			return nil, fmt.Errorf("%w: artifact citation %d quote is not canonical", ErrInvalidResult, index)
		}
		if overlapsWorkspaceCitation(seen[citation.Path], citation.LineStart, citation.LineEnd) {
			return nil, fmt.Errorf("%w: artifact citation %d duplicates or overlaps another citation", ErrInvalidResult, index)
		}
		seen[citation.Path] = append(seen[citation.Path], [2]int{citation.LineStart, citation.LineEnd})
		citation.Quote = quote
	}
	return out, nil
}

func verifyWorkspaceSourceCitations(citations []sourceinvestigation.Citation, root string, requireCanonical bool) ([]sourceinvestigation.Citation, error) {
	if len(citations) > maxSourceCitations {
		return nil, fmt.Errorf("%w: source citations exceed %d", ErrInvalidResult, maxSourceCitations)
	}
	out := slices.Clone(citations)
	seen := map[string][][2]int{}
	for index := range out {
		citation := &out[index]
		if !safeWorkspaceSourcePath(citation.Path) || strings.TrimSpace(citation.Path) != citation.Path {
			return nil, fmt.Errorf("%w: source citation %d path is unsafe", ErrInvalidResult, index)
		}
		content, err := readWorkspaceText(root, citation.Path, maxWorkspaceFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: source citation %d: %v", ErrInvalidResult, index, err)
		}
		identity, err := resolvedWorkspaceIdentity(root, citation.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: source citation %d: %v", ErrInvalidResult, index, err)
		}
		quote, err := canonicalWorkspaceQuote(content, citation.LineStart, citation.LineEnd)
		if err != nil {
			return nil, fmt.Errorf("%w: source citation %d: %v", ErrInvalidResult, index, err)
		}
		if requireCanonical && (!citation.Verified || citation.Quote != quote) {
			return nil, fmt.Errorf("%w: source citation %d is not canonical", ErrInvalidResult, index)
		}
		if overlapsWorkspaceCitation(seen[identity], citation.LineStart, citation.LineEnd) {
			return nil, fmt.Errorf("%w: source citation %d duplicates or overlaps another citation", ErrInvalidResult, index)
		}
		seen[identity] = append(seen[identity], [2]int{citation.LineStart, citation.LineEnd})
		citation.Quote = quote
		citation.Verified = true
	}
	return out, nil
}

func canonicalWorkspaceQuote(content string, lineStart, lineEnd int) (string, error) {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if lineStart < 1 || lineEnd < lineStart || lineEnd-lineStart+1 > maxCitationLines || lineEnd > len(lines) {
		return "", fmt.Errorf("citation has invalid line range")
	}
	quote := strings.Join(lines[lineStart-1:lineEnd], "\n")
	if strings.TrimSpace(quote) == "" || len(quote) > maxCitationQuoteBytes {
		return "", fmt.Errorf("citation range is empty or oversized")
	}
	return quote, nil
}

func overlapsWorkspaceCitation(ranges [][2]int, lineStart, lineEnd int) bool {
	for _, current := range ranges {
		if lineStart <= current[1] && current[0] <= lineEnd {
			return true
		}
	}
	return false
}

func workspaceRelevantFiles(files []string, citations []sourceinvestigation.Citation) ([]string, error) {
	if len(files) > maxRelevantFiles {
		return nil, fmt.Errorf("%w: relevant files exceed %d", ErrInvalidResult, maxRelevantFiles)
	}
	grounded := map[string]bool{}
	for _, citation := range citations {
		grounded[citation.Path] = citation.Verified
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for index, file := range files {
		clean, err := artifacts.SafePath(file)
		if err != nil || clean != file || !safeWorkspaceSourcePath(file) || seen[file] || !grounded[file] {
			return nil, fmt.Errorf("%w: relevant file %d is unsafe, duplicated, or uncited", ErrInvalidResult, index)
		}
		seen[file] = true
		out = append(out, file)
	}
	sort.Strings(out)
	return out, nil
}

func safeWorkspaceSourcePath(value string) bool {
	clean, err := artifacts.SafePath(value)
	parts := strings.Split(clean, "/")
	return err == nil && clean == value && len(parts) > 0 && !strings.EqualFold(parts[0], ".git")
}

func resolvedWorkspaceIdentity(root, relative string) (string, error) {
	clean, err := artifacts.SafePath(relative)
	if err != nil || clean != relative {
		return "", fmt.Errorf("workspace path is unsafe")
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace path escapes the root")
	}
	return filepath.ToSlash(rel), nil
}

func readWorkspaceText(root, relative string, expectedMax int64) (string, error) {
	clean, err := artifacts.SafePath(relative)
	if err != nil || clean != relative {
		return "", fmt.Errorf("workspace path is unsafe")
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace path escapes the root")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("workspace path is not a regular file")
	}
	limit := expectedMax
	if limit <= 0 || limit > maxWorkspaceFileBytes {
		limit = maxWorkspaceFileBytes
	}
	if info.Size() > limit {
		return "", fmt.Errorf("workspace file exceeds the expected bound")
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("workspace file is not valid text")
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

func validateWorkspaceUsage(usage WorkspaceUsage) error {
	if usage.ModelRequests < 0 || usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.OutputTokens < 0 || len(usage.CostUSD) > 64 {
		return fmt.Errorf("workspace execution usage is invalid")
	}
	if usage.Available {
		if usage.Status != WorkspaceTelemetryAvailable || usage.ModelRequests < 1 {
			return fmt.Errorf("available workspace usage is invalid")
		}
		if usage.CostAvailable != (usage.CostUSD != "") {
			return fmt.Errorf("workspace execution cost availability is invalid")
		}
		return nil
	}
	if usage.Status != WorkspaceTelemetryUnavailable && usage.Status != WorkspaceTelemetryMalformed && usage.Status != WorkspaceTelemetryTruncated {
		return fmt.Errorf("unavailable workspace usage status is invalid")
	}
	if usage.ModelRequests != 0 || usage.InputTokens != 0 || usage.CachedInputTokens != 0 || usage.OutputTokens != 0 || usage.CostAvailable || usage.CostUSD != "" {
		return fmt.Errorf("unavailable workspace usage must not contain inferred values")
	}
	return nil
}

func validateWorkspaceOpenCodeTelemetry(telemetry WorkspaceOpenCodeTelemetry) error {
	if telemetry.EventCount < 0 || telemetry.ProviderRequests < 0 || telemetry.DeniedToolCount < 0 || telemetry.ToolFailureCount < 0 || telemetry.StepsUsed < 0 || telemetry.StructuredOutputRetries < 0 || telemetry.StructuredOutputErrors < 0 || telemetry.EvidencePhaseSteps < 0 || telemetry.EvidencePhaseRequests < 0 || telemetry.ArtifactEvidenceToolCalls < 0 || telemetry.SourceEvidenceToolCalls < 0 || telemetry.FinalizationPhaseSteps < 0 || telemetry.FinalizationPhaseRequests < 0 || telemetry.StructuredOutputToolCalls < 0 || !validWorkspaceFailureCode(telemetry.FailureCode) {
		return fmt.Errorf("workspace OpenCode telemetry is invalid")
	}
	if err := validateWorkspaceOpenCodeRequestShape(telemetry.RequestShape); err != nil {
		return err
	}
	if err := validateWorkspaceOpenCodeErrorTelemetry(telemetry.Error); err != nil {
		return err
	}
	if telemetry.Available {
		if telemetry.Status != WorkspaceTelemetryAvailable || telemetry.EventCount < 1 || !telemetry.StructuredOutputRetriesKnown {
			return fmt.Errorf("available workspace OpenCode telemetry is invalid")
		}
		if telemetry.StepsUsed == 0 && !telemetry.Error.Available {
			return fmt.Errorf("zero-step workspace OpenCode telemetry requires an error")
		}
	} else if telemetry.Status != WorkspaceTelemetryUnavailable && telemetry.Status != WorkspaceTelemetryMalformed && telemetry.Status != WorkspaceTelemetryTruncated {
		return fmt.Errorf("unavailable workspace OpenCode telemetry status is invalid")
	}
	seen := map[string]bool{}
	toolFailures, toolDenied := 0, 0
	for _, tool := range telemetry.Tools {
		if tool.Name == "" || len(tool.Name) > 64 || tool.Count < 1 || tool.Failures < 0 || tool.Denied < 0 || tool.Failures > tool.Count || tool.Denied > tool.Failures || seen[tool.Name] {
			return fmt.Errorf("workspace OpenCode tool telemetry is invalid")
		}
		seen[tool.Name] = true
		toolFailures += tool.Failures
		toolDenied += tool.Denied
	}
	if toolFailures != telemetry.ToolFailureCount || toolDenied != telemetry.DeniedToolCount {
		return fmt.Errorf("workspace OpenCode tool telemetry totals are invalid")
	}
	if telemetry.Error.ContextOverflow && !telemetry.ContextLimit {
		return fmt.Errorf("workspace OpenCode context telemetry is inconsistent")
	}
	if telemetry.ProviderRequests < telemetry.StepsUsed {
		return fmt.Errorf("workspace OpenCode provider request telemetry is inconsistent")
	}
	if telemetry.EvidencePhaseCompleted {
		if telemetry.EvidencePhaseSteps < 1 || telemetry.EvidencePhaseRequests < 1 || telemetry.ArtifactEvidenceToolCalls < 1 {
			return fmt.Errorf("workspace OpenCode evidence phase telemetry is invalid")
		}
	} else if telemetry.EvidencePhaseSteps != 0 || telemetry.EvidencePhaseRequests != 0 || telemetry.ArtifactEvidenceToolCalls != 0 || telemetry.SourceEvidenceToolCalls != 0 {
		return fmt.Errorf("workspace OpenCode evidence phase telemetry is inconsistent")
	}
	if telemetry.FinalizationPhaseCompleted {
		if !telemetry.EvidencePhaseCompleted || telemetry.FinalizationPhaseSteps < 1 || telemetry.FinalizationPhaseRequests < 1 {
			return fmt.Errorf("workspace OpenCode finalization phase telemetry is invalid")
		}
	} else if telemetry.FinalizationPhaseSteps != 0 || telemetry.FinalizationPhaseRequests != 0 || telemetry.StructuredOutputToolCalls != 0 {
		return fmt.Errorf("workspace OpenCode finalization phase telemetry is inconsistent")
	}
	if telemetry.EvidencePhaseSteps+telemetry.FinalizationPhaseSteps > telemetry.StepsUsed || telemetry.EvidencePhaseRequests+telemetry.FinalizationPhaseRequests > telemetry.ProviderRequests {
		return fmt.Errorf("workspace OpenCode phase telemetry exceeds totals")
	}
	if !telemetry.Available && (telemetry.EventCount != 0 || len(telemetry.Tools) != 0 || telemetry.DeniedToolCount != 0 || telemetry.ToolFailureCount != 0 || telemetry.StepsUsed != 0 || telemetry.StructuredOutputRetries != 0 || telemetry.StructuredOutputErrors != 0 || telemetry.EvidencePhaseCompleted || telemetry.EvidencePhaseSteps != 0 || telemetry.EvidencePhaseRequests != 0 || telemetry.ArtifactEvidenceToolCalls != 0 || telemetry.SourceEvidenceToolCalls != 0 || telemetry.FinalizationPhaseCompleted || telemetry.FinalizationPhaseSteps != 0 || telemetry.FinalizationPhaseRequests != 0 || telemetry.StructuredOutputToolCalls != 0) {
		return fmt.Errorf("unavailable workspace OpenCode telemetry must not contain event-derived values")
	}
	return nil
}

func validateWorkspaceOpenCodeRequestShape(shape WorkspaceOpenCodeRequestShape) error {
	if !shape.Available {
		if shape.StreamingMode != "" || shape.ModelID != "" || shape.SystemPromptBytesAvailable || shape.SystemPromptBytes != 0 || shape.UserPromptBytes != 0 || shape.ToolSchemaAvailable || shape.ToolCount != 0 || shape.ToolSchemaSHA256 != "" || shape.ResponseSchemaSHA256 != "" || shape.ToolChoiceMode != "" || shape.ContextLimit != 0 || shape.OutputTokenLimit != 0 || shape.OpenCodeVersion != "" {
			return fmt.Errorf("unavailable workspace OpenCode request shape must be empty")
		}
		return nil
	}
	if shape.StreamingMode != "streaming" || strings.TrimSpace(shape.ModelID) == "" || len(shape.ModelID) > 256 || shape.UserPromptBytes < 1 || shape.UserPromptBytes > 1<<20 || (shape.ToolChoiceMode != "auto" && shape.ToolChoiceMode != "required") || shape.ContextLimit < 1 || shape.OutputTokenLimit < 1 || strings.TrimSpace(shape.OpenCodeVersion) == "" || len(shape.OpenCodeVersion) > 64 {
		return fmt.Errorf("workspace OpenCode request shape is invalid")
	}
	if shape.ToolChoiceMode == "required" {
		if !validSHA256(shape.ResponseSchemaSHA256) {
			return fmt.Errorf("workspace OpenCode response schema telemetry is invalid")
		}
	} else if shape.ResponseSchemaSHA256 != "" {
		return fmt.Errorf("workspace OpenCode evidence request must not contain a response schema")
	}
	if shape.SystemPromptBytesAvailable {
		if shape.SystemPromptBytes < 1 || shape.SystemPromptBytes > 1<<20 {
			return fmt.Errorf("workspace OpenCode system prompt telemetry is invalid")
		}
	} else if shape.SystemPromptBytes != 0 {
		return fmt.Errorf("unavailable workspace OpenCode system prompt telemetry must be empty")
	}
	if shape.ToolSchemaAvailable {
		if shape.ToolCount < 1 || shape.ToolCount > 128 || !validSHA256(shape.ToolSchemaSHA256) {
			return fmt.Errorf("workspace OpenCode tool schema telemetry is invalid")
		}
	} else if shape.ToolCount != 0 || shape.ToolSchemaSHA256 != "" {
		return fmt.Errorf("unavailable workspace OpenCode tool schema telemetry must be empty")
	}
	return nil
}

func validateWorkspaceOpenCodeErrorTelemetry(value WorkspaceOpenCodeErrorTelemetry) error {
	if !value.Available {
		if value.Name != "" || value.HTTPStatusCode != 0 || value.RetryableKnown || value.Retryable || value.Classification != "" || value.MetadataCode != "" || value.HeaderTimeout || value.ResponseStreamError || value.ContextOverflow || value.ResponseContentTypePresent || value.ResponseBodyPresent || value.ResponseBodyBytesBounded != 0 || value.ResponseBodySHA256 != "" {
			return fmt.Errorf("unavailable workspace OpenCode error telemetry must be empty")
		}
		return nil
	}
	if !validOpenCodeErrorName(value.Name) || !validOpenCodeErrorClassification(value.Classification) || !validOpenCodeMetadataCode(value.MetadataCode) {
		return fmt.Errorf("workspace OpenCode error telemetry is invalid")
	}
	if value.HTTPStatusCode != 0 && (value.HTTPStatusCode < 100 || value.HTTPStatusCode > 599) {
		return fmt.Errorf("workspace OpenCode HTTP status is invalid")
	}
	if !value.RetryableKnown && value.Retryable {
		return fmt.Errorf("workspace OpenCode retryability is invalid")
	}
	if value.ResponseBodyBytesBounded < 0 || value.ResponseBodyBytesBounded > 1<<20 {
		return fmt.Errorf("workspace OpenCode response body telemetry is invalid")
	}
	if value.ResponseBodyPresent != (value.ResponseBodySHA256 != "") || value.ResponseBodyPresent != (value.ResponseBodyBytesBounded > 0) {
		return fmt.Errorf("workspace OpenCode response body telemetry is inconsistent")
	}
	if value.ResponseBodySHA256 != "" && !validSHA256(value.ResponseBodySHA256) {
		return fmt.Errorf("workspace OpenCode response body digest is invalid")
	}
	if value.HeaderTimeout != (value.Classification == "header_timeout") || value.ResponseStreamError != (value.Classification == "response_stream") || value.ContextOverflow != (value.Classification == "context_overflow") {
		return fmt.Errorf("workspace OpenCode error classification is inconsistent")
	}
	return nil
}

func validOpenCodeErrorName(value string) bool {
	switch value {
	case "ProviderAuthError", "UnknownError", "MessageOutputLengthError", "MessageAbortedError", "StructuredOutputError", "ContextOverflowError", "ContentFilterError", "APIError":
		return true
	default:
		return false
	}
}

func validOpenCodeErrorClassification(value string) bool {
	switch value {
	case "api_bad_request", "api_unauthorized", "api_forbidden", "api_timeout", "api_request_too_large", "api_rate_limited", "api_server_error", "api_error", "malformed_error", "header_timeout", "response_stream", "context_overflow", "structured_output", "provider_auth", "output_length", "aborted", "content_filter", "unknown":
		return true
	default:
		return false
	}
}

func validOpenCodeMetadataCode(value string) bool {
	switch value {
	case "", "ProviderHeaderTimeoutError", "ProviderResponseStreamError", "ECONNRESET", "ZlibError":
		return true
	default:
		return false
	}
}

func validWorkspaceFailureCode(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}
