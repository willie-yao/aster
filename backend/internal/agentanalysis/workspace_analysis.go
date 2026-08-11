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

// WorkspaceSkillHash returns the file-backed analyzer prompt fingerprint.
func WorkspaceSkillHash() string { return hashString(workspaceAnalysisSkill) }

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

// WorkspaceUsage records provider telemetry when the runtime exposes it.
type WorkspaceUsage struct {
	Available         bool   `json:"available"`
	ModelRequests     int    `json:"model_requests,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
	CostUSD           string `json:"cost_usd,omitempty"`
}

// WorkspaceExecutionResult is the single executor result read from Pod logs.
type WorkspaceExecutionResult struct {
	Version         int                         `json:"version"`
	ContractVersion string                      `json:"contract_version"`
	RequestHash     string                      `json:"request_hash"`
	TerminalState   engineruntime.TerminalState `json:"terminal_state"`
	FailureReason   string                      `json:"failure_reason,omitempty"`
	Analysis        *WorkspaceAnalysis          `json:"analysis,omitempty"`
	DurationMs      int64                       `json:"duration_ms"`
	Usage           WorkspaceUsage              `json:"usage"`
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
	switch result.TerminalState {
	case engineruntime.TerminalSucceeded:
		if strings.TrimSpace(result.FailureReason) != "" || result.Analysis == nil {
			return result, fmt.Errorf("successful workspace execution must contain only an analysis")
		}
		if err := VerifySourceWorkspace(context.Background(), sourceRoot, request.Manifest.Source.Revision); err != nil {
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
	if !usage.Available && (usage.ModelRequests != 0 || usage.InputTokens != 0 || usage.CachedInputTokens != 0 || usage.OutputTokens != 0 || usage.CostUSD != "") {
		return fmt.Errorf("unavailable workspace usage must not contain inferred values")
	}
	return nil
}
