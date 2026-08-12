package agentanalysis

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func TestParseWorkspaceAnalysisAcceptsNoVerifiedFixOrSourceClaims(t *testing.T) {
	sourceRoot, artifactRoot, manifest := workspaceValidationManifest(t)
	raw := workspaceValidationJSON(t, func(value map[string]any) {
		value["suggested_fix"] = ""
		value["source_citations"] = []any{}
		value["relevant_files"] = []any{}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || len(validation.Codes) != 0 || analysis.SuggestedFix != "" || len(analysis.SourceCitations) != 0 || len(analysis.RelevantFiles) != 0 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisCanonicalizesNonFatalWarnings(t *testing.T) {
	sourceRoot, artifactRoot, manifest := workspaceValidationManifest(t)
	raw := workspaceValidationJSON(t, func(value map[string]any) {
		value["summary"] = "  summary  "
		value["root_cause"] = "  cause  "
		value["suggested_fix"] = ""
		value["severity"] = "Transient-Ignore"
		value["is_transient"] = false
		value["unresolved_details"] = []any{"", "  bounded detail  "}
		value["evidence_citations"] = []any{
			map[string]any{"path": " logs/build.log ", "line_start": 2, "line_end": 2},
			map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2},
		}
		value["source_citations"] = []any{
			map[string]any{"path": " pkg/controller.go ", "line_start": 3, "line_end": 3},
			map[string]any{"path": "pkg/controller.go", "line_start": 3, "line_end": 3},
		}
		value["relevant_files"] = []any{" pkg/controller.go ", "pkg/controller.go"}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantCodes := []string{
		WorkspaceInvalidAnalysisText,
		WorkspaceInvalidArtifactOverlap,
		WorkspaceInvalidClassification,
		WorkspaceInvalidRelevantFile,
		WorkspaceInvalidSourceOverlap,
	}
	if validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, wantCodes) {
		t.Fatalf("validation=%+v want=%v", validation, wantCodes)
	}
	if analysis.Summary != "summary" || analysis.RootCause != "cause" || !analysis.IsTransient || analysis.Severity != "Transient-Ignore" || analysis.SuggestedFix != "" {
		t.Fatalf("analysis text=%+v", analysis)
	}
	if !slices.Equal(analysis.UnresolvedDetails, []string{"bounded detail"}) || len(analysis.EvidenceCitations) != 1 || len(analysis.SourceCitations) != 1 || !slices.Equal(analysis.RelevantFiles, []string{"pkg/controller.go"}) {
		t.Fatalf("canonical analysis=%+v", analysis)
	}
}

func TestParseWorkspaceAnalysisOrdersCitationsWithoutWarning(t *testing.T) {
	sourceRoot, artifactRoot, manifest := workspaceValidationManifest(t)
	raw := workspaceValidationJSON(t, func(value map[string]any) {
		value["evidence_citations"] = []any{
			map[string]any{"path": "logs/build.log", "line_start": 3, "line_end": 3},
			map[string]any{"path": "logs/build.log", "line_start": 1, "line_end": 1},
		}
		value["source_citations"] = []any{
			map[string]any{"path": "pkg/controller.go", "line_start": 3, "line_end": 3},
			map[string]any{"path": "pkg/controller.go", "line_start": 1, "line_end": 1},
		}
		value["relevant_files"] = []any{"pkg/controller.go"}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || len(validation.Codes) != 0 || analysis.EvidenceCitations[0].LineStart != 1 || analysis.SourceCitations[0].LineStart != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisDropsUncitedRelevantFile(t *testing.T) {
	sourceRoot, artifactRoot, manifest := workspaceValidationManifest(t)
	raw := workspaceValidationJSON(t, func(value map[string]any) {
		value["source_citations"] = []any{}
		value["relevant_files"] = []any{"pkg/controller.go"}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, []string{WorkspaceInvalidRelevantFile}) || len(analysis.RelevantFiles) != 0 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisRejectsHardFailuresWithCodes(t *testing.T) {
	sourceRoot, artifactRoot, manifest := workspaceValidationManifest(t)
	tests := []struct {
		name string
		code string
		raw  func() string
	}{
		{name: "json", code: WorkspaceInvalidResultJSON, raw: func() string { return `{"version":1` }},
		{name: "version", code: WorkspaceInvalidResultVersion, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) { value["version"] = 2 })
		}},
		{name: "analysis text", code: WorkspaceInvalidAnalysisText, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) { value["summary"] = "" })
		}},
		{name: "artifact count", code: WorkspaceInvalidArtifactCount, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) { value["evidence_citations"] = []any{} })
		}},
		{name: "artifact path", code: WorkspaceInvalidArtifactPath, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				value["evidence_citations"] = []any{map[string]any{"path": "missing.log", "line_start": 1, "line_end": 1}}
			})
		}},
		{name: "artifact line range", code: WorkspaceInvalidArtifactLineRange, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				value["evidence_citations"] = []any{map[string]any{"path": "logs/build.log", "line_start": 1, "line_end": 99}}
			})
		}},
		{name: "source path", code: WorkspaceInvalidSourcePath, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				value["source_citations"] = []any{map[string]any{"path": "../outside.go", "line_start": 1, "line_end": 1}}
			})
		}},
		{name: "source line range", code: WorkspaceInvalidSourceLineRange, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				value["source_citations"] = []any{map[string]any{"path": "pkg/controller.go", "line_start": 1, "line_end": 99}}
			})
		}},
		{name: "relevant file", code: WorkspaceInvalidRelevantFile, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) { value["relevant_files"] = []any{"pkg/missing.go"} })
		}},
		{name: "classification", code: WorkspaceInvalidClassification, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) { value["severity"] = "Unknown" })
		}},
		{name: "artifact path beyond retained bound", code: WorkspaceInvalidArtifactPath, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				citations := make([]any, maxEvidenceCitations+1)
				for index := range maxEvidenceCitations {
					citations[index] = map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}
				}
				citations[maxEvidenceCitations] = map[string]any{"path": "missing.log", "line_start": 1, "line_end": 1}
				value["evidence_citations"] = citations
			})
		}},
		{name: "source path beyond retained bound", code: WorkspaceInvalidSourcePath, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				citations := make([]any, maxSourceCitations+1)
				for index := range maxSourceCitations {
					citations[index] = map[string]any{"path": "pkg/controller.go", "line_start": 3, "line_end": 3}
				}
				citations[maxSourceCitations] = map[string]any{"path": "../outside.go", "line_start": 1, "line_end": 1}
				value["source_citations"] = citations
			})
		}},
		{name: "relevant path beyond retained bound", code: WorkspaceInvalidRelevantFile, raw: func() string {
			return workspaceValidationJSON(t, func(value map[string]any) {
				files := make([]any, maxRelevantFiles+1)
				for index := range maxRelevantFiles {
					files[index] = "pkg/controller.go"
				}
				files[maxRelevantFiles] = "pkg/missing.go"
				value["relevant_files"] = files
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			analysis, validation, err := ParseWorkspaceAnalysis(test.raw(), manifest, artifactRoot, sourceRoot)
			if !errors.Is(err, ErrInvalidResult) || WorkspaceInvalidResultCode(err) != test.code || validation.Status != WorkspaceResultRejected || !slices.Equal(validation.Codes, []string{test.code}) || analysis.Summary != "" || len(analysis.EvidenceCitations) != 0 {
				t.Fatalf("analysis=%+v validation=%+v code=%q err=%v", analysis, validation, WorkspaceInvalidResultCode(err), err)
			}
			if strings.Contains(err.Error(), "missing.log") || strings.Contains(err.Error(), "outside.go") || strings.Contains(err.Error(), "pkg/missing.go") {
				t.Fatalf("validator error retained model path: %q", err)
			}
		})
	}
}

func TestWorkspaceResultValidationRejectsUnsafeTelemetryShapes(t *testing.T) {
	for _, validation := range []WorkspaceResultValidation{
		{Status: WorkspaceResultAccepted, Codes: []string{WorkspaceInvalidAnalysisText}},
		{Status: WorkspaceResultAcceptedWithWarnings},
		{Status: WorkspaceResultRejected, Codes: []string{WorkspaceInvalidArtifactPath, WorkspaceInvalidSourcePath}},
		{Status: WorkspaceResultRejected, Codes: []string{"model_path"}},
	} {
		if err := validateWorkspaceResultValidation(validation, false); err == nil {
			t.Fatalf("validation was accepted: %+v", validation)
		}
	}
}

func workspaceValidationManifest(t *testing.T) (string, string, WorkspaceManifest) {
	t.Helper()
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	return sourceRoot, artifactRoot, manifest
}

func workspaceValidationJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}}, nil)), &value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidateWorkspaceExecutionResultPreservesPostModelValidation(t *testing.T) {
	_, spec := workspaceSandboxFixture(t)
	result := validWorkspaceExecution(spec.Request)
	result.TerminalState = engineruntime.TerminalFailed
	result.FailureReason = "source evidence unavailable"
	result.Analysis = nil
	validated, err := ValidateWorkspaceExecutionResult(result, spec.Request, spec.ArtifactRoot, spec.SourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ResultValidation.Status != WorkspaceResultAccepted {
		t.Fatalf("validation=%+v", validated.ResultValidation)
	}

	result.ResultValidation = WorkspaceResultValidation{Status: WorkspaceResultRejected, Codes: []string{WorkspaceInvalidArtifactPath}}
	result.FailureReason = WorkspaceResultRejectedReason
	validated, err = ValidateWorkspaceExecutionResult(result, spec.Request, spec.ArtifactRoot, spec.SourceRoot)
	if err != nil || validated.ResultValidation.Status != WorkspaceResultRejected {
		t.Fatalf("validated=%+v err=%v", validated, err)
	}
	result.FailureReason = "private/model/path.log"
	if _, err := ValidateWorkspaceExecutionResult(result, spec.Request, spec.ArtifactRoot, spec.SourceRoot); err == nil {
		t.Fatal("rejected result with non-generic failure reason was accepted")
	}
}
