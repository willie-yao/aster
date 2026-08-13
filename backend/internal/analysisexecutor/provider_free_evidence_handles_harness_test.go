package analysisexecutor

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const providerFreeOverflowGreps = 12

type providerFreeEvidenceHandleSummary struct {
	TerminalState          string                                           `json:"terminal_state"`
	FailureCode            string                                           `json:"failure_code,omitempty"`
	FailureClassification  string                                           `json:"failure_classification,omitempty"`
	FakeGatewayRequests    int                                              `json:"fake_gateway_requests"`
	UsageStatus            string                                           `json:"usage_status"`
	UsageAvailable         bool                                             `json:"usage_available"`
	ProviderRequests       int                                              `json:"provider_requests"`
	ProviderRequestsKnown  bool                                             `json:"provider_requests_known"`
	Steps                  int                                              `json:"steps"`
	ArtifactEvidenceCalls  int                                              `json:"artifact_evidence_calls"`
	SourceEvidenceCalls    int                                              `json:"source_evidence_calls"`
	SourceEvidenceStatus   string                                           `json:"source_evidence_status,omitempty"`
	SourceCorrectiveTurn   bool                                             `json:"source_corrective_turn,omitempty"`
	StructuredOutputCalls  int                                              `json:"structured_output_calls"`
	EvidencePhaseCompleted bool                                             `json:"evidence_phase_completed"`
	FinalizationCompleted  bool                                             `json:"finalization_completed"`
	StructuredResult       bool                                             `json:"structured_result"`
	ResultValidationStatus string                                           `json:"result_validation_status,omitempty"`
	EvidenceHandles        agentanalysis.WorkspaceEvidenceHandleDiagnostics `json:"evidence_handles"`
	OpenCodeVersion        string                                           `json:"opencode_version"`
	SourceRevision         string                                           `json:"source_revision"`
	WorkspaceIdentity      string                                           `json:"workspace_identity_sha256"`
}

func TestProviderFreeEvidenceHandleScaleHarness(t *testing.T) {
	if os.Getenv("RUN_PROVIDER_FREE_EVIDENCE_HANDLES") != "1" {
		t.Skip("set RUN_PROVIDER_FREE_EVIDENCE_HANDLES=1")
	}
	workspaceRoot := requiredHarnessEnv(t, "PROVIDER_FREE_EVIDENCE_WORKSPACE_ROOT")
	summaryPath := requiredHarnessEnv(t, "PROVIDER_FREE_EVIDENCE_SUMMARY_JSON")
	revision := requiredHarnessEnv(t, "PROVIDER_FREE_EVIDENCE_SOURCE_REVISION")
	sourcePath := requiredHarnessEnv(t, "PROVIDER_FREE_EVIDENCE_SOURCE_PATH")
	opencodeBin := requiredHarnessEnv(t, "OPENCODE_1_18_2_BIN")

	artifactRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(ai.FailureAnalysisRequest{
		JobID: "periodic::provider-free-evidence-handles", BuildPrefix: "logs/provider-free/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "provider-free-evidence-handles", RepoRefs: map[string]string{"example/source": revision}},
		TestCase: models.TestCase{Name: "Provider-free evidence handle scale", Status: "failed", FailureMessage: "deterministic high-cardinality evidence"},
	}, sourceinvestigation.Repository{Owner: "example", Name: "source", Revision: revision}, "Inspect broad artifact evidence without changing the workspace.", files)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case calls <= providerFreeOverflowGreps:
			writeSyntheticOpenAIStream(t, w, "grep", map[string]any{
				"pattern": ".", "path": filepath.ToSlash(artifactRoot), "include": "*.log",
			})
		case calls == providerFreeOverflowGreps+1:
			writeSyntheticOpenAIStream(t, w, "read", map[string]any{
				"filePath": filepath.ToSlash(filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir, sourcePath)), "offset": 1, "limit": 80,
			})
		case calls == providerFreeOverflowGreps+2:
			writeSyntheticOpenAIText(t, w, "Evidence inspected.")
		case calls == providerFreeOverflowGreps+3:
			writeSyntheticOpenAIStream(t, w, "StructuredOutput", map[string]any{
				"version": 1, "contract_version": agentanalysis.WorkspaceContractVersion,
				"summary": "The deterministic artifact evidence was inspected.", "is_transient": false,
				"root_cause": "The provider-free harness exercised bounded high-cardinality evidence handling.",
				"severity":   "Low", "suggested_fix": "",
				"relevant_file_ids": []string{"source-001"}, "artifact_evidence_ids": []string{"artifact-001"},
				"source_evidence_ids": []string{"source-001"}, "unresolved_details": []string{},
			})
		default:
			t.Fatalf("unexpected fake gateway request %d", calls)
		}
	}))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "fake-gateway-ca.pem")
	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", caPath)
	provider := modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway, API: modelprovider.APIChatCompletions,
		Endpoint: server.URL + "/v1/chat/completions", Model: "provider-free-evidence-handles", Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
	request, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceEvidence(
		manifest, agentanalysis.WorkspaceSourceModePreserve, true, provider, 10*time.Minute, 20, 200000, 8192, 256<<10,
	)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{WorkspaceRoot: workspaceRoot, TempRoot: t.TempDir(), OpenCodeBin: opencodeBin}
	if os.Getenv("PROVIDER_FREE_SKIP_MOUNT_VERIFY") == "1" {
		opts.MountVerifier = func(string, string) error { return nil }
	}
	result := Execute(context.Background(), request, opts)
	identity := sha256.Sum256([]byte(strings.Join([]string{revision, manifest.Hash, request.Hash}, "\x00")))
	summary := providerFreeEvidenceHandleSummary{
		TerminalState: string(result.TerminalState), FailureCode: result.OpenCodeTelemetry.FailureCode,
		FailureClassification: providerFreeFailureClassification(result), FakeGatewayRequests: calls,
		UsageStatus: result.Usage.Status, UsageAvailable: result.Usage.Available,
		ProviderRequests: result.OpenCodeTelemetry.ProviderRequests, ProviderRequestsKnown: result.OpenCodeTelemetry.ProviderRequestsKnown,
		Steps: result.OpenCodeTelemetry.StepsUsed, ArtifactEvidenceCalls: result.OpenCodeTelemetry.ArtifactEvidenceToolCalls,
		SourceEvidenceCalls: result.OpenCodeTelemetry.SourceEvidenceToolCalls, SourceEvidenceStatus: result.OpenCodeTelemetry.SourceEvidenceStatus, SourceCorrectiveTurn: result.OpenCodeTelemetry.SourceEvidenceCorrectiveTurn, StructuredOutputCalls: result.OpenCodeTelemetry.StructuredOutputToolCalls,
		EvidencePhaseCompleted: result.OpenCodeTelemetry.EvidencePhaseCompleted, FinalizationCompleted: result.OpenCodeTelemetry.FinalizationPhaseCompleted,
		StructuredResult: result.Analysis != nil, ResultValidationStatus: result.ResultValidation.Status,
		EvidenceHandles: result.OpenCodeTelemetry.EvidenceHandles, OpenCodeVersion: result.OpenCodeTelemetry.RequestShape.OpenCodeVersion,
		SourceRevision: revision, WorkspaceIdentity: hex.EncodeToString(identity[:]),
	}
	wantCodes := []string{
		agentanalysis.WorkspaceEvidenceHandleDuplicate,
		agentanalysis.WorkspaceEvidenceHandleTruncated,
		agentanalysis.WorkspaceEvidenceRangeLineInvalid,
		agentanalysis.WorkspaceEvidenceRangeOverflow,
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(summaryPath), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if result.TerminalState != "succeeded" || result.Analysis == nil || !result.Usage.Available || result.Usage.Status != agentanalysis.WorkspaceTelemetryAvailable || !result.OpenCodeTelemetry.ProviderRequestsKnown || result.OpenCodeTelemetry.ProviderRequests != providerFreeOverflowGreps+3 || result.OpenCodeTelemetry.StepsUsed != providerFreeOverflowGreps+3 || result.OpenCodeTelemetry.ArtifactEvidenceToolCalls != providerFreeOverflowGreps || result.OpenCodeTelemetry.SourceEvidenceToolCalls != 1 || result.OpenCodeTelemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceEvidenceAccepted || result.OpenCodeTelemetry.SourceEvidenceCorrectiveTurn || result.OpenCodeTelemetry.StructuredOutputToolCalls != 1 || !result.OpenCodeTelemetry.EvidencePhaseCompleted || !result.OpenCodeTelemetry.FinalizationPhaseCompleted || result.ResultValidation.Status != agentanalysis.WorkspaceResultAccepted || result.OpenCodeTelemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesAcceptedWithWarnings || result.OpenCodeTelemetry.EvidenceHandles.AcceptedArtifactHandleCount != 64 || result.OpenCodeTelemetry.EvidenceHandles.AcceptedSourceHandleCount < 1 || !result.OpenCodeTelemetry.EvidenceHandles.Truncated || !slices.Equal(result.OpenCodeTelemetry.EvidenceHandles.Codes, wantCodes) || result.OpenCodeTelemetry.RequestShape.OpenCodeVersion != "1.18.2" || calls != providerFreeOverflowGreps+3 {
		t.Fatalf("corrected evidence lifecycle failed: result=%+v calls=%d", result, calls)
	}
	t.Logf("terminal=%s failure=%s requests=%d evidence_status=%s evidence_codes=%v", summary.TerminalState, summary.FailureCode, calls, summary.EvidenceHandles.Status, summary.EvidenceHandles.Codes)
}

func TestProviderFreeEvidenceHandleSummaryOmitsContent(t *testing.T) {
	data, err := json.Marshal(providerFreeEvidenceHandleSummary{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"path", "line", "quote", "prompt", "model_output", "tool_arguments", "raw"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("summary contains %q: %s", forbidden, data)
		}
	}
}
