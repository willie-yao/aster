package analysisexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestExecuteRunsOneNativeSessionAndReturnsAnalysis(t *testing.T) {
	root, request := executorTestFixture(t)
	calls := 0
	times := []time.Time{time.Unix(100, 0), time.Unix(102, 0)}
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		Now: func() time.Time {
			value := times[min(calls, len(times)-1)]
			return value
		},
		RunOpenCode: func(_ context.Context, spec OpenCodeSpec) (OpenCodeRunResult, error) {
			calls++
			if spec.WorkDir != root || spec.MaxSteps != request.MaxSteps || !strings.Contains(spec.Prompt, "logs/build.log") || strings.Contains(spec.Prompt, "artifact-only-marker") {
				t.Fatalf("spec=%+v", spec)
			}
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || result.ResultValidation.Status != agentanalysis.WorkspaceResultAccepted || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
	if result.Analysis.EvidenceCitations[0].Path != "logs/build.log" || result.Analysis.EvidenceCitations[0].Quote != "artifact-only-marker specific failure" || !result.Usage.Available || result.Usage.ModelRequests != 2 {
		t.Fatalf("result=%+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("artifact-002")) || bytes.Contains(encoded, []byte("source-003")) {
		t.Fatalf("execution result retained evidence handles: %s", encoded)
	}
	data, err := os.ReadFile(filepath.Join(root, agentanalysis.WorkspaceResultDir, agentanalysis.WorkspaceResultFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"quote":"artifact-only-marker specific failure"`) || !strings.Contains(string(data), `"verified":true`) {
		t.Fatalf("canonical result = %s", data)
	}
}

func TestExecuteRejectsRequiredSourceFloorMissingFromRunner(t *testing.T) {
	root, base := executorTestFixture(t)
	request, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceEvidence(base.Manifest, base.SourceModePolicy, true, base.ModelProvider, 5*time.Minute, base.MaxSteps, base.ModelContextTokens, base.ModelOutputTokens, base.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.Telemetry.SourceEvidenceToolCalls = 0
			value.Telemetry.SourceEvidenceStatus = agentanalysis.WorkspaceSourceToolSkipped
			value.Telemetry.EvidenceHandles = agentanalysis.WorkspaceEvidenceHandleDiagnostics{Status: agentanalysis.WorkspaceEvidenceHandlesAccepted, ObservedRangeCount: 1, AcceptedArtifactHandleCount: 1}
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Analysis != nil || result.OpenCodeTelemetry.FailureCode != "source_evidence_missing" {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteAcceptsGroundedAnalysisWithoutSuggestedFix(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.Structured = bytes.Replace(value.Structured, []byte(`"suggested_fix": "Correct the request before retrying."`), []byte(`"suggested_fix": ""`), 1)
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || result.Analysis.SuggestedFix != "" || result.ResultValidation.Status != agentanalysis.WorkspaceResultAccepted {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecutePublishesCanonicalAnalysisWithValidationWarnings(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			var structured map[string]any
			if err := json.Unmarshal(value.Structured, &structured); err != nil {
				t.Fatal(err)
			}
			structured["suggested_fix"] = ""
			structured["severity"] = "Transient-Ignore"
			structured["is_transient"] = false
			structured["source_evidence_ids"] = []any{"source-003"}
			structured["relevant_file_ids"] = []any{"source-999"}
			structured["artifact_evidence_ids"] = []any{"artifact-002", "artifact-002"}
			value.Structured, _ = json.Marshal(structured)
			return value, nil
		},
	})
	wantCodes := []string{agentanalysis.WorkspaceInvalidArtifactOverlap, agentanalysis.WorkspaceInvalidClassification, agentanalysis.WorkspaceInvalidRelevantFile}
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || result.ResultValidation.Status != agentanalysis.WorkspaceResultAcceptedWithWarnings || !slices.Equal(result.ResultValidation.Codes, wantCodes) {
		t.Fatalf("result=%+v wantCodes=%v", result, wantCodes)
	}
	if !result.Analysis.IsTransient || result.Analysis.Severity != "Transient-Ignore" || result.Analysis.SuggestedFix != "" || len(result.Analysis.EvidenceCitations) != 1 || len(result.Analysis.SourceCitations) != 1 || len(result.Analysis.RelevantFiles) != 0 {
		t.Fatalf("canonical analysis=%+v", result.Analysis)
	}
}

func TestExecuteRejectsUnsafeAnalysisWithPrivacySafeCode(t *testing.T) {
	root, request := executorTestFixture(t)
	const modelEvidenceID = "artifact-999"
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.Structured = bytes.Replace(value.Structured, []byte("artifact-002"), []byte(modelEvidenceID), 1)
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Analysis != nil || result.FailureReason != agentanalysis.WorkspaceResultRejectedReason || result.OpenCodeTelemetry.FailureCode != "analysis_result_invalid" || result.ResultValidation.Status != agentanalysis.WorkspaceResultRejected || !slices.Equal(result.ResultValidation.Codes, []string{agentanalysis.WorkspaceInvalidArtifactPath}) {
		t.Fatalf("result=%+v", result)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(modelEvidenceID)) || bytes.Contains(data, []byte("artifact-only-marker")) {
		t.Fatalf("rejected result retained model or evidence content: %s", data)
	}
}

func TestExecuteRejectsInvalidEngineEvidenceHandlesSeparately(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.EvidenceHandles[0], value.EvidenceHandles[len(value.EvidenceHandles)-1] = value.EvidenceHandles[len(value.EvidenceHandles)-1], value.EvidenceHandles[0]
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Analysis != nil || result.FailureReason != "workspace evidence handles are invalid" || result.OpenCodeTelemetry.FailureCode != "evidence_handle_invalid" || result.ResultValidation.Status != "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteVerifiesReadOnlyPreparedSource(t *testing.T) {
	root, request := executorTestFixture(t)
	sourceRoot := filepath.Join(root, agentanalysis.WorkspaceSourceDir)
	restore := makeExecutorTreeReadOnly(t, sourceRoot)
	defer restore()
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsWorkspaceMutation(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			if err := os.WriteFile(filepath.Join(root, agentanalysis.WorkspaceArtifactsDir, "logs", "build.log"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return OpenCodeRunResult{}, errors.New("agent failed after mutation")
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, "workspace changed") {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsSourceMutation(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			if err := os.WriteFile(filepath.Join(root, agentanalysis.WorkspaceSourceDir, "pkg", "controller.go"), []byte("package changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureReason != "workspace changed during analysis: "+agentanalysis.SourceWorktreeContentChanged || result.OpenCodeTelemetry.FailureCode != agentanalysis.SourceWorktreeContentChanged {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsSourceModeMutation(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			if err := os.Chmod(filepath.Join(root, agentanalysis.WorkspaceSourceDir, "pkg", "controller.go"), 0o700); err != nil {
				t.Fatal(err)
			}
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureReason != "workspace changed during analysis: "+agentanalysis.SourceWorktreeModeChanged || result.OpenCodeTelemetry.FailureCode != agentanalysis.SourceWorktreeModeChanged {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsExtraResultFile(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			if err := os.WriteFile(filepath.Join(root, agentanalysis.WorkspaceResultDir, "extra.txt"), []byte("extra\n"), 0o600); err != nil {
				return OpenCodeRunResult{}, err
			}
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, "modified") {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsSymlinkResult(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			target := filepath.Join(root, agentanalysis.WorkspaceArtifactsDir, "logs", "build.log")
			path := filepath.Join(root, agentanalysis.WorkspaceResultDir, agentanalysis.WorkspaceResultFile)
			if err := os.Symlink(target, path); err != nil {
				return OpenCodeRunResult{}, err
			}
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Analysis != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteReportsCancellationWithoutSecondRun(t *testing.T) {
	root, request := executorTestFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	calls := 0
	result := Execute(parent, request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (OpenCodeRunResult, error) {
			calls++
			cancel()
			<-ctx.Done()
			return OpenCodeRunResult{}, ctx.Err()
		},
	})
	if result.TerminalState != engineruntime.TerminalCancelled || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}

func TestPromptOpenCodeEvidenceHasNoStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["agent"] != openCodeEvidenceAgent || payload["parts"].([]any)[0].(map[string]any)["text"] != "investigate" {
			t.Fatalf("payload=%v", payload)
		}
		if _, ok := payload["format"]; ok {
			t.Fatalf("evidence phase exposed StructuredOutput: %v", payload)
		}
		if _, ok := payload["tools"]; ok {
			t.Fatalf("evidence phase installed session permissions: %v", payload)
		}
		fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: "/workspace", Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate"}
	if err := promptOpenCodeEvidence(t.Context(), server.Client(), server.URL, "session-1", spec); err != nil {
		t.Fatal(err)
	}
}

func TestPromptOpenCodeUsesStructuredOutputSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/session-1/message" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["agent"] != openCodeFinalizationAgent {
			t.Fatalf("agent = %v", payload["agent"])
		}
		if _, ok := payload["tools"]; ok {
			t.Fatalf("message request must not install session permissions: %v", payload["tools"])
		}
		format := payload["format"].(map[string]any)
		if format["type"] != "json_schema" {
			t.Fatalf("format = %v", format)
		}
		if _, ok := format["retryCount"]; ok {
			t.Fatal("OpenCode 1.18.2 does not implement structured-output retries")
		}
		schema := format["schema"].(map[string]any)
		citations := schema["properties"].(map[string]any)["artifact_evidence_ids"].(map[string]any)
		item := citations["items"].(map[string]any)
		if item["type"] != "string" || item["pattern"] != "^artifact-[0-9]{3}$" {
			t.Fatalf("citation schema = %v", item)
		}
		parts := payload["parts"].([]any)
		prompt := parts[0].(map[string]any)["text"].(string)
		if !strings.Contains(prompt, "artifact-001") || strings.Contains(prompt, "artifact-only-marker") {
			t.Fatalf("finalization prompt = %q", prompt)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"info":{"id":"message-1","role":"assistant","structured":%s},"parts":[]}`, executorAnalysisJSON())
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: "/workspace", Provider: testOpenCodeProvider("", "test-model"), Prompt: "analyze"}
	got, err := promptOpenCode(t.Context(), server.Client(), server.URL, "session-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"artifact_evidence_ids"`) {
		t.Fatalf("structured = %s", got)
	}
}

func TestOpenCodeJSONRejectsTrailingData(t *testing.T) {
	for _, body := range []string{`{"id":"ok"}{"other":true}`, `{"id":"ok"} trailing`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			var target struct {
				ID string `json:"id"`
			}
			if err := openCodeJSON(t.Context(), server.Client(), http.MethodGet, server.URL, nil, &target); err == nil {
				t.Fatal("trailing API data was accepted")
			}
		})
	}
}

func TestWriteOpenCodeConfigSeparatesEvidenceAndFinalizationPermissions(t *testing.T) {
	home := t.TempDir()
	gateway := testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1/chat/completions", "test-model")
	if err := writeOpenCodeConfig(home, gateway, 2, 200000, 8192, false); err == nil {
		t.Fatal("two-step OpenCode analysis was accepted")
	}
	if err := writeOpenCodeConfig(home, gateway, 20, 200000, 8192, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	modelConfig := config["provider"].(map[string]any)["engine"].(map[string]any)["models"].(map[string]any)["test-model"].(map[string]any)
	if _, ok := modelConfig["options"]; ok {
		t.Fatalf("empty reasoning effort changed model config: %v", modelConfig)
	}
	if config["default_agent"] != openCodeEvidenceAgent || config["permission"].(map[string]any)["*"] != "deny" {
		t.Fatalf("config=%v", config)
	}
	agents := config["agent"].(map[string]any)
	if len(agents) != 2 {
		t.Fatalf("agents=%v", agents)
	}
	evidence := agents[openCodeEvidenceAgent].(map[string]any)
	finalize := agents[openCodeFinalizationAgent].(map[string]any)
	if evidence["steps"].(float64) != 18 || finalize["steps"].(float64) != 2 {
		t.Fatalf("evidence=%v finalize=%v", evidence, finalize)
	}
	evidencePermissions := evidence["permission"].(map[string]any)
	if evidencePermissions["*"] != "deny" || evidencePermissions["glob"] != "allow" || evidencePermissions["grep"] != "allow" || evidencePermissions["StructuredOutput"] != "deny" {
		t.Fatalf("evidence permissions=%v", evidencePermissions)
	}
	read := evidencePermissions["read"].(map[string]any)
	if read["*"] != "deny" || read["artifacts/*"] != "allow" || read["source/*"] != "allow" || read["*/artifacts/*"] != "allow" || read["*/source/*"] != "allow" || len(read) != 5 {
		t.Fatalf("read permissions=%v", read)
	}
	bash := evidencePermissions["bash"].(map[string]any)
	if bash["*"] != "deny" || bash["git status --short"] != "allow" || bash["git log -1 --oneline"] != "allow" || bash["git diff --no-ext-diff --stat"] != "allow" || len(bash) != 4 {
		t.Fatalf("bash permissions=%v", bash)
	}
	finalPermissions := finalize["permission"].(map[string]any)
	if finalPermissions["*"] != "deny" || finalPermissions["StructuredOutput"] != "allow" {
		t.Fatalf("final permissions=%v", finalPermissions)
	}
	for _, denied := range []string{"read", "glob", "grep", "bash", "edit", "write", "apply_patch", "webfetch", "websearch", "task", "skill", "external_directory"} {
		if finalPermissions[denied] != "deny" || evidencePermissions[denied] != "deny" && denied != "read" && denied != "glob" && denied != "grep" && denied != "bash" {
			t.Fatalf("permission %s evidence=%v final=%v", denied, evidencePermissions[denied], finalPermissions[denied])
		}
	}
	if strings.Contains(strings.ToLower(string(data)), "token") || strings.Contains(string(data), "/chat/completions") {
		t.Fatalf("config contains credential or non-base endpoint: %s", data)
	}
}

func TestWriteOpenCodeConfigAddsCopilotIntegrationHeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "copilot", endpoint: "https://api.githubcopilot.com/chat/completions", want: true},
		{name: "copilot port", endpoint: "https://api.githubcopilot.com:443/chat/completions", want: true},
		{name: "other provider", endpoint: "https://provider.example/v1/chat/completions"},
		{name: "lookalike", endpoint: "https://notapi.githubcopilot.com/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			provider := testDirectBearerProvider(tc.endpoint, "fixture-model")
			if err := writeOpenCodeConfig(home, provider, 20, 200000, 8192, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			options := config["provider"].(map[string]any)["engine"].(map[string]any)["options"].(map[string]any)
			headers, present := options["headers"].(map[string]any)
			if present != tc.want {
				t.Fatalf("headers present = %t, want %t: %v", present, tc.want, options)
			}
			if tc.want && (len(headers) != 1 || headers["Copilot-Integration-Id"] != "copilot-developer-cli") {
				t.Fatalf("headers = %v", headers)
			}
		})
	}
}

func TestWriteOpenCodeConfigReservesSourceCorrectionAgent(t *testing.T) {
	home := t.TempDir()
	provider := testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1/chat/completions", "test-model")
	if err := writeOpenCodeConfig(home, provider, 20, 200000, 8192, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	agents := config["agent"].(map[string]any)
	if len(agents) != 3 {
		t.Fatalf("agents=%v", agents)
	}
	evidence := agents[openCodeEvidenceAgent].(map[string]any)
	source := agents[openCodeSourceEvidenceAgent].(map[string]any)
	finalize := agents[openCodeFinalizationAgent].(map[string]any)
	if evidence["steps"].(float64) != 16 || source["steps"].(float64) != 2 || finalize["steps"].(float64) != 2 {
		t.Fatalf("evidence=%v source=%v finalize=%v", evidence, source, finalize)
	}
	permissions := source["permission"].(map[string]any)
	read := permissions["read"].(map[string]any)
	if permissions["*"] != "deny" || permissions["grep"] != "allow" || permissions["StructuredOutput"] != "deny" || permissions["glob"] != "deny" || read["*"] != "deny" || read["source/*"] != "allow" || read["*/source/*"] != "allow" || len(read) != 3 {
		t.Fatalf("source permissions=%v", permissions)
	}
	for _, denied := range []string{"bash", "edit", "write", "apply_patch", "webfetch", "websearch", "task", "skill", "external_directory"} {
		if permissions[denied] != "deny" {
			t.Fatalf("source permission %s=%v", denied, permissions[denied])
		}
	}
}

func executorTestFixture(t *testing.T) (string, agentanalysis.WorkspaceExecutionRequest) {
	t.Helper()
	root := t.TempDir()
	sourceRoot := filepath.Join(root, agentanalysis.WorkspaceSourceDir)
	artifactRoot := filepath.Join(root, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(filepath.Join(sourceRoot, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "pkg", "controller.go"), []byte("package controller\n\nfunc reconcile() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runExecutorGit(t, sourceRoot, "init", "-q")
	runExecutorGit(t, sourceRoot, "config", "user.name", "Test")
	runExecutorGit(t, sourceRoot, "config", "user.email", "test@example.com")
	runExecutorGit(t, sourceRoot, "config", "commit.gpgsign", "false")
	runExecutorGit(t, sourceRoot, "add", ".")
	runExecutorGit(t, sourceRoot, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runExecutorGit(t, sourceRoot, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(artifactRoot, "logs", "build.log"), []byte("setup\nartifact-only-marker specific failure\ncleanup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	request := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "specific failure"},
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	gateway := testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1", "test-model")
	execution, err := agentanalysis.NewWorkspaceExecutionRequest(manifest, gateway, 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	return root, execution
}

func testOpenCodeResult() OpenCodeRunResult {
	return OpenCodeRunResult{
		Structured: executorAnalysisJSON(),
		EvidenceHandles: []agentanalysis.WorkspaceEvidenceHandle{
			{ID: "artifact-001", Root: agentanalysis.WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 1, LineEnd: 1},
			{ID: "artifact-002", Root: agentanalysis.WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
			{ID: "artifact-003", Root: agentanalysis.WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 3, LineEnd: 3},
			{ID: "source-001", Root: agentanalysis.WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 1, LineEnd: 1},
			{ID: "source-002", Root: agentanalysis.WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 2, LineEnd: 2},
			{ID: "source-003", Root: agentanalysis.WorkspaceSourceDir, Path: "pkg/controller.go", LineStart: 3, LineEnd: 3},
		},
		Usage: agentanalysis.WorkspaceUsage{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, ModelRequests: 2, InputTokens: 10, OutputTokens: 5, CostAvailable: true, CostUSD: "0.01000000"},
		Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
			Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, EventCount: 4, ProviderRequests: 2, ProviderRequestsKnown: true, StepsUsed: 2, StructuredOutputRetriesKnown: true,
			EvidencePhaseCompleted: true, EvidencePhaseSteps: 1, EvidencePhaseRequests: 1, ArtifactEvidenceToolCalls: 1, SourceEvidenceToolCalls: 1,
			FinalizationPhaseCompleted: true, FinalizationPhaseSteps: 1, FinalizationPhaseRequests: 1, StructuredOutputToolCalls: 1,
		},
	}
}

func executorAnalysisJSON() []byte {
	return []byte(`{
  "version": 1,
  "contract_version": "agent-analysis-workspace-v8",
  "summary": "The controller rejected the request.",
  "is_transient": false,
  "root_cause": "The specific failure occurred before cleanup.",
  "severity": "High",
  "suggested_fix": "Correct the request before retrying.",
  "relevant_file_ids": ["source-003"],
  "artifact_evidence_ids": ["artifact-002"],
  "source_evidence_ids": ["source-003"],
  "unresolved_details": []
}`)
}

func runExecutorGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func TestSourceVerificationTimeoutCoversBoundedNetworkStorage(t *testing.T) {
	if agentanalysis.WorkspaceSourceVerificationTimeout != 30*time.Second {
		t.Fatalf("source verification timeout = %s", agentanalysis.WorkspaceSourceVerificationTimeout)
	}
	if agentanalysis.WorkspacePostModelGrace < 2*agentanalysis.WorkspaceSourceVerificationTimeout {
		t.Fatalf("post-model grace = %s", agentanalysis.WorkspacePostModelGrace)
	}
}

func TestExecuteReportsTimeoutTelemetry(t *testing.T) {
	root, request := executorTestFixture(t)
	request.TimeoutSeconds = 1
	request.Hash = ""
	data, _ := json.Marshal(request)
	_ = data
	// Rebuild through the constructor so the request hash remains canonical.
	request, err := agentanalysis.NewWorkspaceExecutionRequest(request.Manifest, request.ModelProvider, time.Second, request.MaxSteps, request.ModelContextTokens, request.ModelOutputTokens, request.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (OpenCodeRunResult, error) {
			<-ctx.Done()
			return OpenCodeRunResult{}, ctx.Err()
		},
	})
	if result.TerminalState != engineruntime.TerminalTimedOut || !result.OpenCodeTelemetry.TimedOut || result.OpenCodeTelemetry.FailureCode != "timeout" || result.Usage.Status != agentanalysis.WorkspaceTelemetryUnavailable {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteRejectsResultReturnedAfterDeadline(t *testing.T) {
	root, base := executorTestFixture(t)
	request, err := agentanalysis.NewWorkspaceExecutionRequest(base.Manifest, base.ModelProvider, time.Second, base.MaxSteps, base.ModelContextTokens, base.ModelOutputTokens, base.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(ctx context.Context, _ OpenCodeSpec) (OpenCodeRunResult, error) {
			<-ctx.Done()
			return testOpenCodeResult(), nil
		},
	})
	if result.TerminalState != engineruntime.TerminalTimedOut || result.Analysis != nil || !result.OpenCodeTelemetry.TimedOut || result.OpenCodeTelemetry.FailureCode != "timeout" {
		t.Fatalf("result=%+v", result)
	}
}

func TestOpenCodeFailureCodePrefersContextLimit(t *testing.T) {
	if got := openCodeFailureCode(errors.New("OpenCode structured output failed: ContextOverflowError")); got != "context_limit" {
		t.Fatalf("failure code = %q", got)
	}
}

func TestVerifyPreparedMountInfoRequiresExactReadOnlyManifestPaths(t *testing.T) {
	hash := strings.Repeat("a", 64)
	valid := "36 25 0:32 /" + hash + "/source /workspace/source ro,relatime - ext4 /dev/sda ro\n" +
		"37 25 0:32 /" + hash + "/artifacts /workspace/artifacts ro,relatime - ext4 /dev/sda ro\n"
	if err := verifyPreparedMountInfo(valid, "/workspace", hash); err != nil {
		t.Fatal(err)
	}
	kata := "129 120 0:40 / /workspace/source ro,relatime - virtiofs none rw\n" +
		"131 120 0:41 / /workspace/artifacts ro,relatime - virtiofs none rw\n"
	if err := verifyPreparedMountInfo(kata, "/workspace", hash); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"writable":   strings.Replace(valid, "ro,relatime", "rw,relatime", 1),
		"wrong hash": strings.Replace(valid, hash+"/artifacts", strings.Repeat("b", 64)+"/artifacts", 1),
		"missing":    strings.Split(valid, "\n")[0] + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyPreparedMountInfo(raw, "/workspace", hash); err == nil {
				t.Fatal("unsafe mountinfo was accepted")
			}
		})
	}
}

func TestVerifyStagedMountInfoRequiresReadOnlyWorkspaceAndWritableResult(t *testing.T) {
	valid := "36 25 0:32 / /workspace ro,relatime - tmpfs tmpfs rw\n" +
		"37 25 0:33 / /workspace/result rw,relatime - tmpfs tmpfs rw\n"
	if err := verifyStagedMountInfo(valid, "/workspace"); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"writable workspace": strings.Replace(valid, "/workspace ro,", "/workspace rw,", 1),
		"read-only result":   strings.Replace(valid, "/workspace/result rw,", "/workspace/result ro,", 1),
		"missing result":     strings.Split(valid, "\n")[0] + "\n",
		"writable source":    valid + "38 25 0:34 / /workspace/source rw,relatime - tmpfs tmpfs rw\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyStagedMountInfo(raw, "/workspace"); err == nil {
				t.Fatal("unsafe staged mountinfo was accepted")
			}
		})
	}
}

type pinnedPermissionRule struct {
	permission string
	action     string
}

func pinnedOpenCodePermission(permission string, rules ...pinnedPermissionRule) string {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].permission == permission || rules[i].permission == "*" {
			return rules[i].action
		}
	}
	return "ask"
}

func TestPinnedOpenCodeSessionPermissionPrecedence(t *testing.T) {
	agent := []pinnedPermissionRule{{permission: "*", action: "deny"}, {permission: "read", action: "allow"}, {permission: "bash", action: "deny"}}
	denySession := []pinnedPermissionRule{{permission: "read", action: "deny"}}
	if got := pinnedOpenCodePermission("read", append(agent, denySession...)...); got != "deny" {
		t.Fatalf("session denial did not override agent allow: %s", got)
	}
	allowSession := []pinnedPermissionRule{{permission: "bash", action: "allow"}}
	if got := pinnedOpenCodePermission("bash", append(agent, allowSession...)...); got != "allow" {
		t.Fatalf("session allow did not broaden agent denial: %s", got)
	}
	if got := pinnedOpenCodePermission("read", agent...); got != "allow" {
		t.Fatalf("agent permission changed without a session override: %s", got)
	}
}

func TestVerifyReadSafeWorkspaceRejectsInstructionFiles(t *testing.T) {
	root, _ := executorTestFixture(t)
	sourceRoot := filepath.Join(root, agentanalysis.WorkspaceSourceDir)
	for _, name := range []string{"AGENTS.md", "nested/CLAUDE.md", "logs/CONTEXT.md"} {
		if err := verifyReadSafeWorkspace(t.Context(), sourceRoot, []agentanalysis.WorkspaceFile{{Path: name}}); err == nil {
			t.Fatalf("artifact instruction file was accepted: %s", name)
		}
	}
	if err := verifyReadSafeWorkspace(t.Context(), sourceRoot, []agentanalysis.WorkspaceFile{{Path: "nested/instructions.md"}}); err != nil {
		t.Fatalf("benign similarly named file was rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "AGENTS.md"), []byte("untrusted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runExecutorGit(t, sourceRoot, "add", "AGENTS.md")
	if err := verifyReadSafeWorkspace(t.Context(), sourceRoot, []agentanalysis.WorkspaceFile{{Path: "logs/build.log"}}); err == nil {
		t.Fatal("tracked source instruction file was accepted")
	}
}

func TestExecutePreservesSanitizedFailureTelemetryWithoutUsage(t *testing.T) {
	root, request := executorTestFixture(t)
	errorTelemetry := agentanalysis.WorkspaceOpenCodeErrorTelemetry{
		Available: true, Name: "APIError", HTTPStatusCode: 429, RetryableKnown: true, Retryable: true,
		Classification: "api_rate_limited",
	}
	shape := newOpenCodeRequestShape(OpenCodeSpec{
		Provider: request.ModelProvider, Prompt: "synthetic", ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens,
	}, "1.18.2", "finalize")
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root,
		TempRoot:      t.TempDir(),
		MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			return OpenCodeRunResult{
				Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
				Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
					Status: agentanalysis.WorkspaceTelemetryUnavailable, ProviderRequests: 1, ProviderRequestsKnown: true, RequestShape: shape,
					Error: errorTelemetry, StructuredOutputRetriesKnown: true,
				},
			}, &openCodePromptError{name: "APIError", telemetry: errorTelemetry}
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Usage.Available || result.Usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || result.OpenCodeTelemetry.ProviderRequests != 1 || result.OpenCodeTelemetry.Error != errorTelemetry || result.OpenCodeTelemetry.FailureCode != "api_rate_limited" || !result.OpenCodeTelemetry.RequestShape.Available {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecutePreservesUnknownErrorWithUnknownProviderStage(t *testing.T) {
	root, request := executorTestFixture(t)
	errorTelemetry := agentanalysis.WorkspaceOpenCodeErrorTelemetry{
		Available: true, Name: "UnknownError", Classification: "database",
		MessagePresent: true, MessageBytes: 20, RedactedMessageSHA256: strings.Repeat("a", 64),
	}
	shape := newOpenCodeEvidenceRequestShape(OpenCodeSpec{
		Provider: request.ModelProvider, Prompt: "synthetic", ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens,
	}, "1.18.2")
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root,
		TempRoot:      t.TempDir(),
		MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			return OpenCodeRunResult{
				Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
				Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
					Status: agentanalysis.WorkspaceTelemetryUnavailable, RequestShape: shape,
					Error: errorTelemetry, StructuredOutputRetriesKnown: true,
				},
			}, &openCodePromptError{name: "UnknownError", telemetry: errorTelemetry}
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.Usage.Available || result.OpenCodeTelemetry.ProviderRequestsKnown || result.OpenCodeTelemetry.ProviderRequests != 0 || result.OpenCodeTelemetry.Error != errorTelemetry || result.OpenCodeTelemetry.FailureCode != "database" {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecutePreservesObservedProviderRequestLowerBound(t *testing.T) {
	root, request := executorTestFixture(t)
	beforeProvider := false
	beforeTool := false
	errorTelemetry := agentanalysis.WorkspaceOpenCodeErrorTelemetry{
		Available: true, Name: "UnknownError", Classification: "dns", CauseCode: "ENOTFOUND",
		MessagePresent: true, MessageBytes: 24, RedactedMessageSHA256: strings.Repeat("a", 64),
		BeforeProviderRequest: &beforeProvider, BeforeFirstTool: &beforeTool,
	}
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root,
		TempRoot:      t.TempDir(),
		MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			return OpenCodeRunResult{
				Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
				Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
					Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, EventCount: 4,
					ProviderRequests: 1, ProviderRequestsKnown: false, StepsUsed: 1,
					Error: errorTelemetry, Tools: []agentanalysis.WorkspaceToolTelemetry{{Name: "read", Count: 1}},
					StructuredOutputRetriesKnown: true,
				},
			}, &openCodePromptError{name: "UnknownError", telemetry: errorTelemetry}
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.OpenCodeTelemetry.ProviderRequests != 1 || result.OpenCodeTelemetry.ProviderRequestsKnown || result.OpenCodeTelemetry.Error.Classification != "dns" {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteGenericFailureLowerBoundSatisfiesResultContract(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root,
		TempRoot:      t.TempDir(),
		MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			return OpenCodeRunResult{
				Usage: agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable},
				Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
					Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, EventCount: 4,
					ProviderRequests: 1, ProviderRequestsKnown: false, StepsUsed: 1,
					Tools:                        []agentanalysis.WorkspaceToolTelemetry{{Name: "read", Count: 1}},
					StructuredOutputRetriesKnown: true, FailureCode: "http_error",
				},
			}, fmt.Errorf("OpenCode API returned HTTP 502")
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.OpenCodeTelemetry.FailureCode != "http_error" || result.OpenCodeTelemetry.Error.Available || result.OpenCodeTelemetry.ProviderRequestsKnown {
		t.Fatalf("result=%+v", result)
	}
	validated, err := agentanalysis.ValidateWorkspaceExecutionResult(result, request, filepath.Join(root, agentanalysis.WorkspaceArtifactsDir), filepath.Join(root, agentanalysis.WorkspaceSourceDir))
	if err != nil {
		t.Fatalf("generic lower-bound result failed validation: %v", err)
	}
	if validated.OpenCodeTelemetry.ProviderRequests != 1 || validated.OpenCodeTelemetry.ProviderRequestsKnown {
		t.Fatalf("validated=%+v", validated.OpenCodeTelemetry)
	}
}

func TestApplyOpenCodePromptErrorReplacesStaleErrorForGenericFailure(t *testing.T) {
	result := OpenCodeRunResult{Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
		ProviderRequests: 1, ProviderRequestsKnown: true,
		Error: agentanalysis.WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "APIError", Classification: "api_rate_limited",
			HTTPStatusCode: 429, RetryableKnown: true, Retryable: true,
		},
		Tools: []agentanalysis.WorkspaceToolTelemetry{{Name: "read", Count: 1}},
	}}
	applyOpenCodePromptError(&result, fmt.Errorf("OpenCode API returned HTTP 502"), 1, true, true)
	if result.Telemetry.Error.Available || result.Telemetry.FailureCode != "http_error" || result.Telemetry.ProviderRequests != 1 || result.Telemetry.ProviderRequestsKnown {
		t.Fatalf("stale error was retained: %+v", result.Telemetry)
	}
}

func TestApplyOpenCodePromptErrorPreservesCompleteSessionLifecycle(t *testing.T) {
	beforeFirstTool := false
	promptBeforeFirstTool := true
	result := OpenCodeRunResult{Telemetry: agentanalysis.WorkspaceOpenCodeTelemetry{
		ProviderRequests: 2, ProviderRequestsKnown: true,
		Error: agentanalysis.WorkspaceOpenCodeErrorTelemetry{
			Available: true, Name: "UnknownError", Classification: "unknown",
			MessagePresent: true, RedactedMessageSHA256: strings.Repeat("a", 64),
			BeforeFirstTool: &beforeFirstTool,
		},
		Tools: []agentanalysis.WorkspaceToolTelemetry{{Name: "read", Count: 1}},
	}}
	promptTelemetry := agentanalysis.WorkspaceOpenCodeErrorTelemetry{
		Available: true, Name: "UnknownError", Classification: "response_stream",
		MessagePresent: true, RedactedMessageSHA256: strings.Repeat("b", 64),
		BeforeFirstTool: &promptBeforeFirstTool,
	}
	applyOpenCodePromptError(&result, &openCodePromptError{name: "UnknownError", telemetry: promptTelemetry}, 1, true, false)
	if result.Telemetry.Error.BeforeFirstTool == nil || *result.Telemetry.Error.BeforeFirstTool || result.Telemetry.Error.Classification != "unknown" || result.Telemetry.FailureCode != "unknown" {
		t.Fatalf("complete session telemetry was overwritten: %+v", result.Telemetry)
	}
}

func makeExecutorTreeReadOnly(t *testing.T, root string) func() {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return os.Chmod(path, 0o600)
		})
	}
}

func TestRunOpenCodePhasesUsesOneSessionAndGatesFinalization(t *testing.T) {
	workDir := t.TempDir()
	for _, dir := range []string{agentanalysis.WorkspaceSourceDir, agentanalysis.WorkspaceArtifactsDir} {
		if err := os.Mkdir(filepath.Join(workDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	artifactPath := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir, "failure.log")
	sourcePath := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir, "main.go")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceTelemetry := fmt.Sprintf(`[{
		"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[2]q},"metadata":{"display":{"type":"file","path":%[2]q,"lineStart":1,"lineEnd":1}}}},
		{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}
	]}]`, artifactPath, sourcePath)
	finalTelemetry := strings.TrimSuffix(evidenceTelemetry, "]") + fmt.Sprintf(`,{"info":{"role":"assistant","structured":%s},"parts":[{"type":"step-start"},{"type":"tool","tool":"StructuredOutput","state":{"status":"completed","input":{}}},{"type":"step-finish","cost":0.2,"tokens":{"input":20,"output":4,"cache":{"read":2}}}]}]`, executorAnalysisJSON())
	posts := 0
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/session-1/message" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPost:
			posts++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if posts == 1 {
				if payload["agent"] != openCodeEvidenceAgent {
					t.Fatalf("evidence payload=%v", payload)
				}
				if _, ok := payload["format"]; ok {
					t.Fatalf("evidence payload exposed format=%v", payload)
				}
				fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
				return
			}
			if payload["agent"] != openCodeFinalizationAgent || payload["format"].(map[string]any)["type"] != "json_schema" {
				t.Fatalf("final payload=%v", payload)
			}
			fmt.Fprintf(w, `{"info":{"role":"assistant","structured":%s},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"running","input":{"filePath":"unpersisted"}}}]}`, executorAnalysisJSON())
		case http.MethodGet:
			gets++
			w.Header().Set("Content-Type", "application/json")
			if gets == 1 {
				fmt.Fprint(w, evidenceTelemetry)
				return
			}
			fmt.Fprint(w, finalTelemetry)
		default:
			t.Fatalf("method=%s", r.Method)
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err != nil {
		t.Fatal(err)
	}
	if posts != 2 || gets != 2 || len(result.Structured) == 0 || !result.Telemetry.EvidencePhaseCompleted || !result.Telemetry.FinalizationPhaseCompleted || result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceEvidenceAccepted || result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.StepsUsed != 2 || result.Usage.ModelRequests != 2 {
		t.Fatalf("posts=%d gets=%d result=%+v", posts, gets, result)
	}
	for _, tool := range result.Telemetry.Tools {
		if tool.Name == "read" && tool.Count != 2 {
			t.Fatalf("unpersisted response parts were counted: %+v", result.Telemetry.Tools)
		}
	}
}

func TestRunOpenCodePhasesArtifactOnlySkipsSourceCorrection(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, agentanalysis.WorkspaceSourceDir), 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
	final := strings.TrimSuffix(initial, "]") + fmt.Sprintf(`,{"info":{"role":"assistant","structured":%s},"parts":[{"type":"step-start"},{"type":"tool","tool":"StructuredOutput","state":{"status":"completed","input":{}}},{"type":"step-finish","cost":0.2,"tokens":{"input":20,"output":4,"cache":{"read":2}}}]}]`, executorAnalysisJSON())
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if posts == 1 {
				if payload["agent"] != openCodeEvidenceAgent {
					t.Fatalf("evidence payload=%v", payload)
				}
				fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
				return
			}
			if posts != 2 || payload["agent"] != openCodeFinalizationAgent {
				t.Fatalf("unexpected payload=%v posts=%d", payload, posts)
			}
			fmt.Fprintf(w, `{"info":{"role":"assistant","structured":%s},"parts":[]}`, executorAnalysisJSON())
		case http.MethodGet:
			gets++
			if gets == 1 {
				fmt.Fprint(w, initial)
			} else {
				fmt.Fprint(w, final)
			}
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err != nil || posts != 2 || gets != 2 || result.Telemetry.SourceEvidenceStatus != "" || result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 {
		t.Fatalf("posts=%d gets=%d result=%+v err=%v", posts, gets, result, err)
	}
}

func TestRunOpenCodePhasesInterceptsArtifactOnlyFinalizationForSourceEvidence(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
	for _, dir := range []string{artifactDir, sourceDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
	corrected := strings.TrimSuffix(initial, "]") + fmt.Sprintf(`,{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, sourcePath)
	final := strings.TrimSuffix(corrected, "]") + fmt.Sprintf(`,{"info":{"role":"assistant","structured":%s},"parts":[{"type":"step-start"},{"type":"tool","tool":"StructuredOutput","state":{"status":"completed","input":{}}},{"type":"step-finish","cost":0.2,"tokens":{"input":20,"output":4,"cache":{"read":2}}}]}]`, executorAnalysisJSON())
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			switch posts {
			case 1:
				if payload["agent"] != openCodeEvidenceAgent {
					t.Fatalf("initial payload=%v", payload)
				}
				fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
			case 2:
				if payload["agent"] != openCodeSourceEvidenceAgent {
					t.Fatalf("correction payload=%v", payload)
				}
				if _, ok := payload["format"]; ok {
					t.Fatalf("source correction exposed StructuredOutput: %v", payload)
				}
				prompt := payload["parts"].([]any)[0].(map[string]any)["text"].(string)
				for _, forbidden := range []string{"test/k8s-integration/main.go", "Windows snapshot", "expected diagnosis", "expected signal"} {
					if strings.Contains(prompt, forbidden) {
						t.Fatalf("correction leaked %q: %s", forbidden, prompt)
					}
				}
				fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
			case 3:
				if payload["agent"] != openCodeFinalizationAgent {
					t.Fatalf("final payload=%v", payload)
				}
				fmt.Fprintf(w, `{"info":{"role":"assistant","structured":%s},"parts":[]}`, executorAnalysisJSON())
			default:
				t.Fatalf("unexpected post %d", posts)
			}
		case http.MethodGet:
			gets++
			switch gets {
			case 1:
				fmt.Fprint(w, initial)
			case 2:
				fmt.Fprint(w, corrected)
			case 3:
				fmt.Fprint(w, final)
			default:
				t.Fatalf("unexpected get %d", gets)
			}
		default:
			t.Fatalf("method=%s", r.Method)
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err != nil {
		t.Fatal(err)
	}
	if posts != 3 || gets != 3 || result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceEvidenceAccepted || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.SourceEvidenceCorrectionReason != agentanalysis.WorkspaceSourceToolSkipped || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.EvidenceHandles.AcceptedSourceHandleCount != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.EvidencePhaseSteps != 2 || result.Telemetry.FinalizationPhaseSteps != 1 {
		t.Fatalf("posts=%d gets=%d result=%+v", posts, gets, result)
	}
}

func TestRunOpenCodePhasesRejectsUnusableCorrectiveSourceEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		tool       func(string, string) string
		wantStatus string
	}{
		{name: "empty grep", wantStatus: agentanalysis.WorkspaceSourceEvidenceUnusable, tool: func(_, sourceDir string) string {
			return fmt.Sprintf(`{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"metadata":{"matches":0}}}`, sourceDir)
		}},
		{name: "invalid line", wantStatus: agentanalysis.WorkspaceSourceEvidenceUnusable, tool: func(sourcePath, _ string) string {
			return fmt.Sprintf(`{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":2,"lineEnd":2}}}}`, sourcePath)
		}},
		{name: "failed read", wantStatus: agentanalysis.WorkspaceSourceToolFailed, tool: func(sourcePath, _ string) string {
			return fmt.Sprintf(`{"type":"tool","tool":"read","state":{"status":"error","error":"read failed","input":{"filePath":%q}}}`, sourcePath)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
			sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
			for _, dir := range []string{artifactDir, sourceDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			artifactPath := filepath.Join(artifactDir, "failure.log")
			sourcePath := filepath.Join(sourceDir, "main.go")
			if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
			corrected := strings.TrimSuffix(initial, "]") + fmt.Sprintf(`,{"info":{"role":"assistant"},"parts":[{"type":"step-start"},%s,{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, test.tool(sourcePath, sourceDir))
			posts, gets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					posts++
					if posts > 2 {
						t.Fatalf("finalization ran after unusable source evidence")
					}
					fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
				case http.MethodGet:
					gets++
					if gets == 1 {
						fmt.Fprint(w, initial)
					} else {
						fmt.Fprint(w, corrected)
					}
				}
			}))
			defer server.Close()
			spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
			result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
			if err == nil || posts != 2 || gets != 2 || result.Telemetry.FailureCode != "source_evidence_missing" || result.Telemetry.SourceEvidenceStatus != test.wantStatus || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.StructuredOutputToolCalls != 0 {
				t.Fatalf("posts=%d gets=%d result=%+v err=%v", posts, gets, result, err)
			}
		})
	}
}

func TestRunOpenCodePhasesRejectsArtifactAccessDuringSourceCorrection(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
	for _, dir := range []string{artifactDir, sourceDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
	grepOutput := fmt.Sprintf("%s:\n  Line 1: failure\n", artifactPath)
	corrected := strings.TrimSuffix(initial, "]") + fmt.Sprintf(`,{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%[1]q},"output":%[2]q,"metadata":{"matches":1}}},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[3]q},"metadata":{"display":{"type":"file","path":%[3]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactDir, grepOutput, sourcePath)
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			if posts > 2 {
				t.Fatalf("finalization ran after corrective artifact access")
			}
			fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
		case http.MethodGet:
			gets++
			if gets == 1 {
				fmt.Fprint(w, initial)
			} else {
				fmt.Fprint(w, corrected)
			}
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err == nil || posts != 2 || gets != 2 || result.Telemetry.FailureCode != "source_evidence_missing" || result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceEvidenceUnusable || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.StructuredOutputToolCalls != 0 {
		t.Fatalf("posts=%d gets=%d result=%+v err=%v", posts, gets, result, err)
	}
}

func TestRunOpenCodePhasesPreservesCorrectionTelemetryWhenTranscriptUnavailable(t *testing.T) {
	for _, test := range []struct {
		name string
		get  func(http.ResponseWriter)
	}{
		{name: "malformed", get: func(w http.ResponseWriter) { fmt.Fprint(w, `[{`) }},
		{name: "unavailable", get: func(w http.ResponseWriter) { http.Error(w, "unavailable", http.StatusServiceUnavailable) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
			if err := os.MkdirAll(artifactDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(workDir, agentanalysis.WorkspaceSourceDir), 0o700); err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(artifactDir, "failure.log")
			if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
			posts, gets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					posts++
					fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
				case http.MethodGet:
					gets++
					if gets == 1 {
						fmt.Fprint(w, initial)
					} else {
						test.get(w)
					}
				}
			}))
			defer server.Close()
			spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
			result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
			if err == nil || posts != 2 || gets != 2 || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.SourceEvidenceCorrectionReason != agentanalysis.WorkspaceSourceToolSkipped || result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceToolSkipped || result.Telemetry.FailureCode != "telemetry_unavailable" {
				t.Fatalf("posts=%d gets=%d result=%+v err=%v", posts, gets, result, err)
			}
		})
	}
}

func TestRunOpenCodePhasesBoundsSourceCorrectionToOneTurn(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, agentanalysis.WorkspaceSourceDir), 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
	corrected := strings.TrimSuffix(initial, "]") + `,{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			if posts > 2 {
				t.Fatalf("more than one source correction was attempted")
			}
			fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
		case http.MethodGet:
			gets++
			if gets == 1 {
				fmt.Fprint(w, initial)
			} else {
				fmt.Fprint(w, corrected)
			}
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192, RequireSourceEvidence: true}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err == nil || posts != 2 || gets != 2 || result.Telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceToolSkipped || result.Telemetry.FailureCode != "source_evidence_missing" || !result.Telemetry.SourceEvidenceCorrectiveTurn || result.Telemetry.StructuredOutputToolCalls != 0 {
		t.Fatalf("posts=%d gets=%d result=%+v err=%v", posts, gets, result, err)
	}
}

func TestApplyOpenCodeNativeToolSchemaClassifiesRequiredSourceToolAbsence(t *testing.T) {
	shape := agentanalysis.WorkspaceOpenCodeRequestShape{}
	telemetry := agentanalysis.WorkspaceOpenCodeTelemetry{}
	err := applyOpenCodeNativeToolSchema(&shape, &telemetry, true, false, 0, "", errors.New("read tool missing"))
	if err == nil || telemetry.FailureCode != agentanalysis.WorkspaceSourceToolUnavailable || telemetry.SourceEvidenceStatus != agentanalysis.WorkspaceSourceToolUnavailable || shape.ToolSchemaAvailable {
		t.Fatalf("shape=%+v telemetry=%+v err=%v", shape, telemetry, err)
	}
	if err := applyOpenCodeNativeToolSchema(&shape, &telemetry, false, false, 0, "", errors.New("read tool missing")); err != nil {
		t.Fatalf("artifact-only tool catalog became mandatory: %v", err)
	}
	if err := applyOpenCodeNativeToolSchema(&shape, &telemetry, true, true, 0, "", errors.New("glob tool missing")); err != nil {
		t.Fatalf("available source tools were misclassified: %v", err)
	}
}

func TestOpenCodeSourceEvidenceStatusPreservesValidSourceHandleAfterTruncation(t *testing.T) {
	facts := openCodeEvidenceFacts{
		SourceToolCalls:    1,
		SourceToolAttempts: 1,
		EvidenceDiagnostics: agentanalysis.WorkspaceEvidenceHandleDiagnostics{
			Status: agentanalysis.WorkspaceEvidenceHandlesAcceptedWithWarnings, AcceptedArtifactHandleCount: 64,
			AcceptedSourceHandleCount: 1, Truncated: true, Codes: []string{agentanalysis.WorkspaceEvidenceHandleTruncated},
		},
	}
	if got := openCodeSourceEvidenceStatus(facts); got != agentanalysis.WorkspaceSourceEvidenceAccepted {
		t.Fatalf("source evidence status = %q", got)
	}
}

func TestRunOpenCodePhasesPreservesPromptErrorWhenFinalTranscriptMalformed(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceTelemetry := fmt.Sprintf(`[{"info":{"role":"assistant","error":{"name":"APIError","data":{"message":"retry","statusCode":429,"isRetryable":true}}},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
	posts := 0
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			if posts == 1 {
				fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
				return
			}
			fmt.Fprint(w, `{"info":{"role":"assistant","error":{"name":"UnknownError","data":{"message":"getaddrinfo ENOTFOUND synthetic.invalid"}}},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"running","input":{"filePath":"unpersisted"}}}]}`)
		case http.MethodGet:
			gets++
			if gets == 1 {
				fmt.Fprint(w, evidenceTelemetry)
				return
			}
			fmt.Fprint(w, `[{`)
		default:
			t.Fatalf("method=%s", r.Method)
		}
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err == nil || result.Usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || result.Telemetry.Error.Name != "UnknownError" || result.Telemetry.Error.Classification != "dns" || result.Telemetry.ProviderRequests != 1 || result.Telemetry.ProviderRequestsKnown {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Telemetry.Error.BeforeProviderRequest == nil || *result.Telemetry.Error.BeforeProviderRequest || result.Telemetry.Error.BeforeFirstTool == nil || *result.Telemetry.Error.BeforeFirstTool {
		t.Fatalf("session progress=%+v", result.Telemetry.Error)
	}
	for _, tool := range result.Telemetry.Tools {
		if tool.Name == "read" && tool.Count != 1 {
			t.Fatalf("unpersisted response parts were counted: %+v", result.Telemetry.Tools)
		}
	}
}

func TestRunOpenCodePhasesStopsBeforeFinalizationWithoutArtifactEvidence(t *testing.T) {
	workDir := t.TempDir()
	sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			fmt.Fprint(w, `{"info":{"role":"assistant"},"parts":[]}`)
			return
		}
		fmt.Fprintf(w, `[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%q}}},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}}]}]`, sourcePath)
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: workDir, Provider: testOpenCodeProvider("", "test-model"), Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err == nil || result.Telemetry.FailureCode != agentanalysis.WorkspaceEvidenceArtifactHandlesMissing || !result.Usage.Available || result.Telemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesRejected || posts != 1 {
		t.Fatalf("posts=%d result=%+v err=%v", posts, result, err)
	}
}

func TestPrimaryWorkspaceEvidenceFailureCodePrioritizesHardRejections(t *testing.T) {
	for _, test := range []struct {
		name  string
		codes []string
		want  string
	}{
		{
			name: "unsafe path after overflow",
			codes: []string{
				agentanalysis.WorkspaceEvidenceHandleTruncated,
				agentanalysis.WorkspaceEvidenceRangeOverflow,
				agentanalysis.WorkspaceEvidenceRangePathInvalid,
			},
			want: agentanalysis.WorkspaceEvidenceRangePathInvalid,
		},
		{
			name: "timeout after truncation",
			codes: []string{
				agentanalysis.WorkspaceEvidenceHandleDuplicate,
				agentanalysis.WorkspaceEvidenceHandleTimeout,
				agentanalysis.WorkspaceEvidenceHandleTruncated,
			},
			want: agentanalysis.WorkspaceEvidenceHandleTimeout,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := primaryWorkspaceEvidenceFailureCode(agentanalysis.WorkspaceEvidenceHandleDiagnostics{Status: agentanalysis.WorkspaceEvidenceHandlesRejected, Codes: test.codes})
			if got != test.want {
				t.Fatalf("code=%q want=%q", got, test.want)
			}
		})
	}
}

func TestExecuteRequiresSourceEvidenceForSourceClaims(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.Telemetry.SourceEvidenceToolCalls = 0
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalFailed || result.OpenCodeTelemetry.FailureCode != "source_evidence_unavailable" || !strings.Contains(result.FailureReason, "without successful source evidence") {
		t.Fatalf("result=%+v", result)
	}
}

func TestOpenCodePhaseToolGatesFailClosed(t *testing.T) {
	if err := validateOpenCodeEvidencePhase(openCodeEvidenceFacts{ArtifactToolCalls: 1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, facts := range []openCodeEvidenceFacts{{}, {SourceToolCalls: 1}, {ArtifactToolCalls: 1, StructuredOutputCalls: 1}} {
		if err := validateOpenCodeEvidencePhase(facts, nil); err == nil {
			t.Fatalf("evidence facts were accepted: %+v", facts)
		}
	}
	if err := validateOpenCodeFinalizationPhase(1, 0); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ structured, native int }{{0, 0}, {2, 0}, {1, 1}} {
		if err := validateOpenCodeFinalizationPhase(test.structured, test.native); err == nil {
			t.Fatalf("finalization sequence was accepted: %+v", test)
		}
	}
}

func TestExecuteAllowsNoSourceClaimsWithoutSourceEvidence(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) {
			value := testOpenCodeResult()
			value.Telemetry.SourceEvidenceToolCalls = 0
			value.Structured = []byte(strings.ReplaceAll(strings.ReplaceAll(string(executorAnalysisJSON()), `"relevant_file_ids": ["source-003"]`, `"relevant_file_ids": []`), `"source_evidence_ids": ["source-003"]`, `"source_evidence_ids": []`))
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || len(result.Analysis.SourceCitations) != 0 || len(result.Analysis.RelevantFiles) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestWriteOpenCodeConfigReferencesDirectCredentialEnvironment(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	home := t.TempDir()
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortXHigh
	if err := writeOpenCodeConfig(home, provider, 20, 200000, 8192, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "{env:"+modelprovider.TokenEnv+"}") {
		t.Fatal("analyzer config does not reference the fixed provider credential environment")
	}
	if strings.Contains(text, credential) {
		t.Fatal("analyzer config serialized the provider credential")
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	modelConfig := config["provider"].(map[string]any)["engine"].(map[string]any)["models"].(map[string]any)["fixture-model"].(map[string]any)
	if modelConfig["options"].(map[string]any)["reasoningEffort"] != "xhigh" {
		t.Fatalf("model config = %v", modelConfig)
	}
	env, err := openCodeEnvironment(home, t.TempDir(), provider)
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for _, value := range env {
		if strings.HasPrefix(value, modelprovider.TokenEnv+"=") {
			entries++
		}
	}
	if entries != 1 {
		t.Fatalf("credential environment entries = %d", entries)
	}
}

func TestExecuteRejectsCredentialBearingStructuredResult(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	root, base := executorTestFixture(t)
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	request, err := agentanalysis.NewWorkspaceExecutionRequest(base.Manifest, provider, time.Second, base.MaxSteps, base.ModelContextTokens, base.ModelOutputTokens, base.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	runResult := testOpenCodeResult()
	runResult.Structured = []byte(strings.Replace(string(runResult.Structured), "The specific failure occurred before cleanup.", credential, 1))
	result := Execute(t.Context(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(), MountVerifier: func(string, string) error { return nil },
		RunOpenCode: func(context.Context, OpenCodeSpec) (OpenCodeRunResult, error) { return runResult, nil },
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.TerminalState != engineruntime.TerminalFailed || result.FailureReason != modelprovider.ErrCredentialExposure.Error() || result.OpenCodeTelemetry.FailureCode != "credential_exposure" {
		t.Fatalf("credential-bearing analysis was not rejected: state=%s reason=%q code=%q", result.TerminalState, result.FailureReason, result.OpenCodeTelemetry.FailureCode)
	}
	if strings.Contains(string(encoded), credential) {
		t.Fatal("rejected analysis retained the provider credential")
	}
}

func TestDefaultRunOpenCodeRejectsCredentialBearingProcessStream(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	t.Setenv(modelprovider.TokenEnv, credential)
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$"+modelprovider.TokenEnv+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := defaultRunOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: t.TempDir(), HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model"),
		Prompt:   "analyze", MaxSteps: 3, ModelContextTokens: 200000, ModelOutputTokens: 8192,
	})
	if !errors.Is(err, modelprovider.ErrCredentialExposure) {
		t.Fatalf("credential-bearing process stream error = %v", err)
	}
}

func TestNonCredentialSubprocessEnvironmentExcludesProviderCredential(t *testing.T) {
	t.Setenv(modelprovider.TokenEnv, strings.Repeat("fixture-provider-credential-", 2))
	for _, value := range nonCredentialSubprocessEnvironment() {
		if strings.HasPrefix(value, modelprovider.TokenEnv+"=") {
			t.Fatal("dashboard-owned subprocess inherited the provider credential")
		}
	}
}

func TestStopOpenCodeProcessWaitsBeforeCredentialCheck(t *testing.T) {
	credential := strings.Repeat("fixture-provider-credential-", 2)
	provider := testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model")
	guard, err := modelprovider.NewCredentialGuard(provider, func(string) (string, bool) { return credential, true })
	if err != nil {
		t.Fatal(err)
	}
	detector := guard.NewDetector()
	terminated := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-terminated
		mid := len(credential) / 2
		_, _ = detector.Write([]byte(credential[:mid]))
		_, _ = detector.Write([]byte(credential[mid:]))
		done <- nil
	}()
	stopOpenCodeProcess(func() { close(terminated) }, done)
	if !detector.Detected() {
		t.Fatal("credential emitted during shutdown was checked before process completion")
	}
}

func TestWriteOpenCodeConfigUsesNativeResponsesProvider(t *testing.T) {
	home := t.TempDir()
	provider := testResponsesProvider("https://provider.example/v1/responses", "fixture-model")
	provider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	if err := writeOpenCodeConfig(home, provider, 20, 200000, 8192, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	engine := config["provider"].(map[string]any)["engine"].(map[string]any)
	options := engine["options"].(map[string]any)
	if engine["npm"] != "@ai-sdk/openai" || options["baseURL"] != "https://provider.example/v1" || options["apiKey"] != "{env:"+modelprovider.TokenEnv+"}" {
		t.Fatalf("provider config = %v", engine)
	}
	modelConfig := engine["models"].(map[string]any)["fixture-model"].(map[string]any)
	if modelConfig["options"].(map[string]any)["reasoningEffort"] != "high" {
		t.Fatalf("model config = %v", modelConfig)
	}
	if strings.Contains(string(data), "/responses") {
		t.Fatalf("config retained the operation path: %s", data)
	}
}

// OpenCode reaches the provider directly, so it must send the same GitHub
// Copilot integration header the in-process transport sends. Without it Copilot
// rejects the request with an unexplained 403.
func TestWriteOpenCodeConfigSetsCopilotIntegrationHeader(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider modelprovider.Config
		want     bool
	}{
		{"copilot chat completions", testDirectBearerProvider("https://api.githubcopilot.com/chat/completions", "fixture-model"), true},
		{"copilot responses", testResponsesProvider("https://api.githubcopilot.com/responses", "fixture-model"), true},
		{"other provider", testDirectBearerProvider("https://provider.example/v1/chat/completions", "fixture-model"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if err := writeOpenCodeConfig(home, tt.provider, 20, 200000, 8192, false); err != nil {
				t.Fatal(err)
			}
			options := readOpenCodeProviderOptions(t, home)
			headers, ok := options["headers"].(map[string]any)
			if !tt.want {
				if ok {
					t.Fatalf("headers = %v, want none for %q", headers, tt.provider.Endpoint)
				}
				return
			}
			if !ok || headers[modelprovider.CopilotIntegrationHeader] != modelprovider.CopilotIntegrationID {
				t.Fatalf("headers = %v, want %s", headers, modelprovider.CopilotIntegrationHeader)
			}
		})
	}
}

func readOpenCodeProviderOptions(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	options, ok := config["provider"].(map[string]any)["engine"].(map[string]any)["options"].(map[string]any)
	if !ok {
		t.Fatalf("provider options missing: %s", data)
	}
	return options
}
