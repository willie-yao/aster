package agentanalysis

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestParseWorkspaceAnalysisAcceptsNoVerifiedFixOrSourceClaims(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		value["suggested_fix"] = ""
		value["source_evidence_ids"] = []any{}
		value["relevant_file_ids"] = []any{}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || len(validation.Codes) != 0 || analysis.SuggestedFix != "" || len(analysis.SourceCitations) != 0 || len(analysis.RelevantFiles) != 0 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisCanonicalizesNonFatalWarnings(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	sourceID := workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		value["summary"] = "  summary  "
		value["root_cause"] = "  cause  "
		value["suggested_fix"] = ""
		value["severity"] = "Transient-Ignore"
		value["is_transient"] = false
		value["unresolved_details"] = []any{"", "  bounded detail  "}
		value["artifact_evidence_ids"] = []any{workspaceCitationSelection(artifactID), workspaceCitationSelection(artifactID)}
		value["source_evidence_ids"] = []any{workspaceCitationSelection(sourceID), workspaceCitationSelection(sourceID)}
		value["relevant_file_ids"] = []any{sourceID, sourceID}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
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
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		value["artifact_evidence_ids"] = []any{
			workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 3)),
			workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 1)),
		}
		value["source_evidence_ids"] = []any{
			workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)),
			workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 1)),
		}
		value["relevant_file_ids"] = []any{workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || len(validation.Codes) != 0 || analysis.EvidenceCitations[0].LineStart != 1 || analysis.SourceCitations[0].LineStart != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisWarnsOnRelevantFileWithoutSourceEvidence(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		value["source_evidence_ids"] = []any{}
		value["relevant_file_ids"] = []any{workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
	wantCodes := []string{WorkspaceInvalidRelevantFile, WorkspaceInvalidSourcePath}
	if err != nil || validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, wantCodes) || len(analysis.RelevantFiles) != 0 {
		t.Fatalf("err=%v validation=%+v", err, validation)
	}
}

func TestParseWorkspaceAnalysisDropsUnknownOptionalEvidenceIDs(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	sourceID := workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		value["artifact_evidence_ids"] = []any{artifactID, "artifact-999"}
		value["source_evidence_ids"] = []any{sourceID, "source-999"}
		value["relevant_file_ids"] = []any{sourceID, "source-999"}
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantCodes := []string{WorkspaceInvalidArtifactPath, WorkspaceInvalidRelevantFile, WorkspaceInvalidSourcePath}
	if validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, wantCodes) || len(analysis.EvidenceCitations) != 1 || len(analysis.SourceCitations) != 1 || len(analysis.RelevantFiles) != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisInspectsUnknownIDsBeyondRetainedBounds(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	raw := workspaceValidationJSON(t, handles, func(value map[string]any) {
		ids := make([]any, maxEvidenceCitations+1)
		for index := range maxEvidenceCitations {
			ids[index] = artifactID
		}
		ids[maxEvidenceCitations] = "artifact-999"
		value["artifact_evidence_ids"] = ids
	})
	analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantCodes := []string{WorkspaceInvalidArtifactOverlap, WorkspaceInvalidArtifactPath}
	if validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, wantCodes) || len(analysis.EvidenceCitations) != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestParseWorkspaceAnalysisRejectsHardFailuresWithCodes(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	tests := []struct {
		name string
		code string
		raw  func() string
	}{
		{name: "json", code: WorkspaceInvalidResultJSON, raw: func() string { return `{"version":1` }},
		{name: "version", code: WorkspaceInvalidResultVersion, raw: func() string {
			return workspaceValidationJSON(t, handles, func(value map[string]any) { value["version"] = 2 })
		}},
		{name: "analysis text", code: WorkspaceInvalidAnalysisText, raw: func() string {
			return workspaceValidationJSON(t, handles, func(value map[string]any) { value["summary"] = "" })
		}},
		{name: "classification", code: WorkspaceInvalidClassification, raw: func() string {
			return workspaceValidationJSON(t, handles, func(value map[string]any) { value["severity"] = "Unknown" })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			analysis, validation, err := ParseWorkspaceAnalysis(test.raw(), handles, manifest, artifactRoot, sourceRoot)
			if !errors.Is(err, ErrInvalidResult) || WorkspaceInvalidResultCode(err) != test.code || validation.Status != WorkspaceResultRejected || !slices.Equal(validation.Codes, []string{test.code}) || analysis.Summary != "" || len(analysis.EvidenceCitations) != 0 {
				t.Fatalf("analysis=%+v validation=%+v code=%q err=%v", analysis, validation, WorkspaceInvalidResultCode(err), err)
			}
			if strings.Contains(err.Error(), "artifact-999") || strings.Contains(err.Error(), "source-999") {
				t.Fatalf("validator error retained model evidence ID: %q", err)
			}
		})
	}
}

func TestParseWorkspaceAnalysisWarnsOnMissingArtifactGrounding(t *testing.T) {
	sourceRoot, artifactRoot, manifest, handles := workspaceValidationFixture(t)
	sourceID := workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)
	for _, test := range []struct {
		name  string
		value []any
		codes []string
	}{
		{name: "empty", value: []any{}, codes: []string{WorkspaceInvalidArtifactCount}},
		{name: "unknown", value: []any{"artifact-999"}, codes: []string{WorkspaceInvalidArtifactCount, WorkspaceInvalidArtifactPath}},
		{name: "wrong root", value: []any{sourceID}, codes: []string{WorkspaceInvalidArtifactCount, WorkspaceInvalidArtifactPath}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := workspaceValidationJSON(t, handles, func(value map[string]any) { value["artifact_evidence_ids"] = test.value })
			analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, sourceRoot)
			if err != nil || validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, test.codes) || len(analysis.EvidenceCitations) != 0 {
				t.Fatalf("analysis=%+v validation=%+v err=%v", analysis, validation, err)
			}
		})
	}
}

func TestWorkspaceAnalysisDispositionSeparatesWarningsFromRejection(t *testing.T) {
	analysis := WorkspaceAnalysis{
		Summary: "summary", RootCause: "cause", Severity: "High",
		EvidenceCitations: []models.EvidenceCitation{{Path: "logs/build.log", LineStart: 1, LineEnd: 1, Quote: "failure"}},
	}
	disposition, warnings := WorkspaceAnalysisDisposition(analysis, WorkspaceResultValidation{Status: WorkspaceResultAccepted}, false)
	if disposition != models.AnalysisDispositionGrounded || len(warnings) != 0 {
		t.Fatalf("grounded disposition=%q warnings=%v", disposition, warnings)
	}
	analysis.UnresolvedDetails = []string{"remediation pin was not verified"}
	disposition, warnings = WorkspaceAnalysisDisposition(analysis, WorkspaceResultValidation{Status: WorkspaceResultAccepted}, false)
	if disposition != models.AnalysisDispositionGrounded || !slices.Equal(warnings, []string{models.AnalysisWarningInvestigation}) {
		t.Fatalf("advisory disposition=%q warnings=%v", disposition, warnings)
	}
	analysis.UnresolvedDetails = nil
	disposition, warnings = WorkspaceAnalysisDisposition(analysis, WorkspaceResultValidation{Status: WorkspaceResultAcceptedWithWarnings, Codes: []string{WorkspaceInvalidRelevantFile}}, true)
	if disposition != models.AnalysisDispositionPreliminary || !slices.Equal(warnings, []string{models.AnalysisWarningSourceGrounding}) {
		t.Fatalf("preliminary disposition=%q warnings=%v", disposition, warnings)
	}
	if disposition, _ := WorkspaceAnalysisDisposition(analysis, WorkspaceResultValidation{Status: WorkspaceResultRejected, Codes: []string{WorkspaceInvalidResultJSON}}, false); disposition != "" {
		t.Fatalf("rejected disposition=%q", disposition)
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

func workspaceValidationFixture(t *testing.T) (string, string, WorkspaceManifest, []WorkspaceEvidenceHandle) {
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
	return sourceRoot, artifactRoot, manifest, workspaceDefaultHandles(t, sourceRoot, artifactRoot)
}

func workspaceValidationJSON(t *testing.T, handles []WorkspaceEvidenceHandle, mutate func(map[string]any)) string {
	t.Helper()
	var value map[string]any
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	if err := json.Unmarshal([]byte(workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection(artifactID)}, nil)), &value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidateWorkspaceExecutionResultRequiresSourceEvidenceFloor(t *testing.T) {
	_, base := workspaceSandboxFixture(t)
	request, err := NewWorkspaceExecutionRequestWithSourceEvidence(base.Request.Manifest, base.Request.SourceModePolicy, true, base.Request.ModelProvider, time.Minute, base.Request.MaxSteps, base.Request.ModelContextTokens, base.Request.ModelOutputTokens, base.Request.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	result := validWorkspaceExecution(request)
	result.OpenCodeTelemetry.SourceEvidenceStatus = WorkspaceSourceEvidenceAccepted
	result.OpenCodeTelemetry.EvidenceHandles = WorkspaceEvidenceHandleDiagnostics{
		Status: WorkspaceEvidenceHandlesAccepted, ObservedRangeCount: 2, AcceptedArtifactHandleCount: 1, AcceptedSourceHandleCount: 1,
	}
	if _, err := ValidateWorkspaceExecutionResult(result, request, base.ArtifactRoot, base.SourceRoot); err != nil {
		t.Fatalf("valid source floor was rejected: %v", err)
	}
	for _, mutate := range []func(*WorkspaceExecutionResult){
		func(value *WorkspaceExecutionResult) {
			value.OpenCodeTelemetry.SourceEvidenceStatus = WorkspaceSourceToolSkipped
		},
		func(value *WorkspaceExecutionResult) { value.OpenCodeTelemetry.SourceEvidenceToolCalls = 0 },
		func(value *WorkspaceExecutionResult) {
			value.OpenCodeTelemetry.EvidenceHandles.AcceptedSourceHandleCount = 0
		},
	} {
		changed := result
		mutate(&changed)
		if _, err := ValidateWorkspaceExecutionResult(changed, request, base.ArtifactRoot, base.SourceRoot); err == nil {
			t.Fatalf("missing source floor was accepted: %+v", changed.OpenCodeTelemetry)
		}
	}
}

func TestValidateWorkspaceExecutionResultAcceptsRejectedCorrectiveSourceHandle(t *testing.T) {
	_, base := workspaceSandboxFixture(t)
	request, err := NewWorkspaceExecutionRequestWithSourceEvidence(base.Request.Manifest, base.Request.SourceModePolicy, true, base.Request.ModelProvider, time.Minute, base.Request.MaxSteps, base.Request.ModelContextTokens, base.Request.ModelOutputTokens, base.Request.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	result := validWorkspaceExecution(request)
	result.TerminalState = engineruntime.TerminalFailed
	result.FailureReason = "required source evidence is missing"
	result.Analysis = nil
	result.OpenCodeTelemetry.ProviderRequests = 2
	result.OpenCodeTelemetry.StepsUsed = 2
	result.OpenCodeTelemetry.EvidencePhaseSteps = 2
	result.OpenCodeTelemetry.EvidencePhaseRequests = 2
	result.OpenCodeTelemetry.FinalizationPhaseCompleted = false
	result.OpenCodeTelemetry.FinalizationPhaseSteps = 0
	result.OpenCodeTelemetry.FinalizationPhaseRequests = 0
	result.OpenCodeTelemetry.StructuredOutputToolCalls = 0
	result.OpenCodeTelemetry.SourceEvidenceStatus = WorkspaceSourceEvidenceUnusable
	result.OpenCodeTelemetry.SourceEvidenceCorrectiveTurn = true
	result.OpenCodeTelemetry.SourceEvidenceCorrectionReason = WorkspaceSourceToolSkipped
	result.OpenCodeTelemetry.EvidenceHandles = WorkspaceEvidenceHandleDiagnostics{
		Status: WorkspaceEvidenceHandlesAccepted, ObservedRangeCount: 2, AcceptedArtifactHandleCount: 1, AcceptedSourceHandleCount: 1,
	}
	result.OpenCodeTelemetry.FailureCode = "source_evidence_missing"
	if _, err := ValidateWorkspaceExecutionResult(result, request, base.ArtifactRoot, base.SourceRoot); err != nil {
		t.Fatalf("failed corrective result was rejected: %v", err)
	}
}

func TestValidateWorkspaceExecutionResultAllowsPostModelGrace(t *testing.T) {
	_, spec := workspaceSandboxFixture(t)
	result := validWorkspaceExecution(spec.Request)
	result.TerminalState = engineruntime.TerminalFailed
	result.FailureReason = "source verification completed"
	result.Analysis = nil
	result.DurationMs = spec.Request.TimeoutSeconds*1000 + WorkspacePostModelGrace.Milliseconds()
	if _, err := ValidateWorkspaceExecutionResult(result, spec.Request, spec.ArtifactRoot, spec.SourceRoot); err != nil {
		t.Fatalf("duration at post-model grace was rejected: %v", err)
	}
	result.DurationMs++
	if _, err := ValidateWorkspaceExecutionResult(result, spec.Request, spec.ArtifactRoot, spec.SourceRoot); err == nil {
		t.Fatal("duration beyond post-model grace was accepted")
	}
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
