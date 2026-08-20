package analysisexecutor

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type providerFreeIntegritySummary struct {
	Before                   agentanalysis.SourceIntegritySnapshot `json:"before"`
	After                    agentanalysis.SourceIntegritySnapshot `json:"after"`
	AfterCategory            string                                `json:"after_category,omitempty"`
	TerminalState            string                                `json:"terminal_state"`
	FailureCode              string                                `json:"failure_code,omitempty"`
	FailureClassification    string                                `json:"failure_classification,omitempty"`
	FakeGatewayRequests      int                                   `json:"fake_gateway_requests"`
	ProviderRequests         int                                   `json:"provider_requests"`
	Steps                    int                                   `json:"steps"`
	ArtifactEvidenceCalls    int                                   `json:"artifact_evidence_calls"`
	SourceEvidenceCalls      int                                   `json:"source_evidence_calls"`
	StructuredOutputCalls    int                                   `json:"structured_output_calls"`
	StructuredResult         bool                                  `json:"structured_result"`
	ResultValidationStatus   string                                `json:"result_validation_status,omitempty"`
	ExpectedSourcePathSHA256 string                                `json:"expected_source_path_sha256"`
}

func TestProviderFreeSourceIntegrityHarness(t *testing.T) {
	if os.Getenv("RUN_PROVIDER_FREE_SOURCE_INTEGRITY") != "1" {
		t.Skip("set RUN_PROVIDER_FREE_SOURCE_INTEGRITY=1")
	}
	workspaceRoot := requiredHarnessEnv(t, "PROVIDER_FREE_WORKSPACE_ROOT")
	summaryPath := requiredHarnessEnv(t, "PROVIDER_FREE_SUMMARY_JSON")
	expectedSource := requiredHarnessEnv(t, "PROVIDER_FREE_EXPECTED_SOURCE")
	artifactPath := requiredHarnessEnv(t, "PROVIDER_FREE_ARTIFACT")
	grepPattern := requiredHarnessEnv(t, "PROVIDER_FREE_GREP_PATTERN")
	revision := requiredHarnessEnv(t, "PROVIDER_FREE_SOURCE_REVISION")
	modePolicy := agentanalysis.WorkspaceSourceModePolicy(requiredHarnessEnv(t, "PROVIDER_FREE_SOURCE_MODE_POLICY"))

	sourceRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourcesDir, "primary")
	artifactRoot := filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(ai.FailureAnalysisRequest{
		JobID: "periodic::provider-free-integrity", BuildPrefix: "logs/provider-free/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "provider-free-integrity", RepoRefs: map[string]string{"example/source": revision}},
		TestCase: models.TestCase{Name: "Provider-free source integrity", Status: "failed", FailureMessage: "deterministic source integrity check"},
	}, sourceinvestigation.Repository{Owner: "example", Name: "source", Revision: revision}, "Inspect the deterministic artifact and source evidence.", files)
	if err != nil {
		t.Fatal(err)
	}
	before, err := agentanalysis.InspectPreparedSourceIntegrity(t.Context(), sourceRoot, revision, modePolicy)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			writeSyntheticOpenAIStream(t, w, "read", map[string]any{"filePath": filepath.ToSlash(filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir, artifactPath)), "offset": 1, "limit": 80})
		case 2:
			writeSyntheticOpenAIStream(t, w, "read", map[string]any{"filePath": filepath.ToSlash(filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourcesDir, "primary", expectedSource)), "offset": 630, "limit": 80})
		case 3:
			writeSyntheticOpenAIStream(t, w, "grep", map[string]any{"pattern": grepPattern, "path": filepath.ToSlash(filepath.Dir(filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourcesDir, "primary", expectedSource))), "include": filepath.Base(expectedSource)})
		case 4:
			writeSyntheticOpenAIText(t, w, "Evidence inspected.")
		case 5:
			writeSyntheticOpenAIStream(t, w, "StructuredOutput", map[string]any{
				"version": 1, "contract_version": agentanalysis.WorkspaceContractVersion,
				"summary":      "The provider-free integrity harness inspected artifact and source evidence.",
				"is_transient": false,
				"root_cause":   "The deterministic source selection logic and artifact marker were inspected without modifying the workspace.",
				"severity":     "Low", "suggested_fix": "",
				"relevant_file_ids":     []string{"source-001"},
				"artifact_evidence_ids": []string{"artifact-001"},
				"source_evidence_ids":   []string{"source-001"},
				"unresolved_details":    []string{},
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
		CredentialMode: modelprovider.CredentialModeGateway,
		API:            modelprovider.APIChatCompletions, Endpoint: server.URL + "/v1/chat/completions",
		Model: "provider-free-integrity", Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
	request, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceModePolicy(
		manifest, modePolicy, provider, 5*time.Minute, 20, 200000, 8192, 256<<10,
	)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{WorkspaceRoot: workspaceRoot, TempRoot: filepath.Dir(caPath)}
	if os.Getenv("PROVIDER_FREE_SKIP_MOUNT_VERIFY") == "1" {
		opts.MountVerifier = func(string, string) error { return nil }
	}
	result := Execute(context.Background(), request, opts)
	after, inspectErr := agentanalysis.InspectPreparedSourceIntegrity(t.Context(), sourceRoot, revision, modePolicy)
	afterCategory := agentanalysis.SourceIntegrityCategory(inspectErr)
	if inspectErr != nil && afterCategory == "" {
		t.Fatal(inspectErr)
	}
	pathDigest := sha256.Sum256([]byte(filepath.ToSlash(expectedSource)))
	summary := providerFreeIntegritySummary{
		Before: before, After: after, AfterCategory: afterCategory,
		TerminalState: string(result.TerminalState), FailureCode: result.OpenCodeTelemetry.FailureCode,
		FailureClassification: providerFreeFailureClassification(result), FakeGatewayRequests: calls,
		ProviderRequests: result.OpenCodeTelemetry.ProviderRequests,
		Steps:            result.OpenCodeTelemetry.StepsUsed, ArtifactEvidenceCalls: result.OpenCodeTelemetry.ArtifactEvidenceToolCalls,
		SourceEvidenceCalls:   result.OpenCodeTelemetry.SourceEvidenceToolCalls,
		StructuredOutputCalls: result.OpenCodeTelemetry.StructuredOutputToolCalls,
		StructuredResult:      result.Analysis != nil, ResultValidationStatus: result.ResultValidation.Status,
		ExpectedSourcePathSHA256: hex.EncodeToString(pathDigest[:]),
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(summaryPath), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("terminal=%s failure_code=%s failure_classification=%s fake_gateway_requests=%d", summary.TerminalState, summary.FailureCode, summary.FailureClassification, calls)
}

func providerFreeFailureClassification(result agentanalysis.WorkspaceExecutionResult) string {
	if result.OpenCodeTelemetry.FailureCode != "" {
		return result.OpenCodeTelemetry.FailureCode
	}
	reason := strings.ToLower(result.FailureReason)
	switch {
	case strings.Contains(reason, "prepared mount") || strings.Contains(reason, "mount identity"):
		return "prepared_mounts"
	case strings.Contains(reason, "model provider") || strings.Contains(reason, "gateway"):
		return "provider_configuration"
	case strings.Contains(reason, "result root") || strings.Contains(reason, "result directory"):
		return "result_workspace"
	case reason == "":
		return "none"
	default:
		return "unknown"
	}
}

func requiredHarnessEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func TestProviderFreeIntegritySummaryOmitsContent(t *testing.T) {
	data, err := json.Marshal(providerFreeIntegritySummary{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"prompt", "source_content", "artifact_content", "model_output", "tool_arguments"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("summary contains %q: %s", forbidden, data)
		}
	}
	if fmt.Sprint(providerFreeIntegritySummary{}) == "" {
		t.Fatal("summary formatting is unavailable")
	}
}
