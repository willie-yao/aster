package analysisexecutor

import (
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
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
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
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
	if result.Analysis.EvidenceCitations[0].Path != "logs/build.log" || result.Analysis.EvidenceCitations[0].Quote != "artifact-only-marker specific failure" || !result.Usage.Available || result.Usage.ModelRequests != 2 {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, agentanalysis.WorkspaceResultDir, agentanalysis.WorkspaceResultFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"quote":"artifact-only-marker specific failure"`) || !strings.Contains(string(data), `"verified":true`) {
		t.Fatalf("canonical result = %s", data)
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
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, "workspace changed") {
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
	spec := OpenCodeSpec{WorkDir: "/workspace", Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}, Prompt: "investigate"}
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
		citations := schema["properties"].(map[string]any)["evidence_citations"].(map[string]any)
		item := citations["items"].(map[string]any)
		if _, ok := item["properties"].(map[string]any)["quote"]; ok {
			t.Fatal("schema still asks the model for quote text")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"info":{"id":"message-1","role":"assistant","structured":%s},"parts":[]}`, executorAnalysisJSON())
	}))
	defer server.Close()
	spec := OpenCodeSpec{WorkDir: "/workspace", Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}, Prompt: "analyze"}
	got, err := promptOpenCode(t.Context(), server.Client(), server.URL, "session-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"evidence_citations"`) {
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
	gateway := engineruntime.ModelGatewayConfig{Endpoint: "https://model-gateway.prow-ai.svc.cluster.local:8443/v1/chat/completions", Model: "test-model", ProtocolVersion: "openai-chat-completions-v1"}
	if err := writeOpenCodeConfig(home, gateway, 20, 200000, 8192); err != nil {
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
	if config["default_agent"] != openCodeEvidenceAgent || config["permission"].(map[string]any)["*"] != "deny" {
		t.Fatalf("config=%v", config)
	}
	agents := config["agent"].(map[string]any)
	if len(agents) != 2 {
		t.Fatalf("agents=%v", agents)
	}
	evidence := agents[openCodeEvidenceAgent].(map[string]any)
	finalize := agents[openCodeFinalizationAgent].(map[string]any)
	if evidence["steps"].(float64) != 19 || finalize["steps"].(float64) != 1 {
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
	gateway := engineruntime.ModelGatewayConfig{Endpoint: "https://model-gateway.prow-ai.svc.cluster.local:8443/v1", Model: "test-model", ProtocolVersion: "openai-chat-completions-v1"}
	execution, err := agentanalysis.NewWorkspaceExecutionRequest(manifest, gateway, 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	return root, execution
}

func testOpenCodeResult() OpenCodeRunResult {
	return OpenCodeRunResult{
		Structured: executorAnalysisJSON(),
		Usage:      agentanalysis.WorkspaceUsage{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, ModelRequests: 2, InputTokens: 10, OutputTokens: 5, CostAvailable: true, CostUSD: "0.01000000"},
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
  "contract_version": "agent-analysis-workspace-v5",
  "summary": "The controller rejected the request.",
  "is_transient": false,
  "root_cause": "The specific failure occurred before cleanup.",
  "severity": "High",
  "suggested_fix": "Correct the request before retrying.",
  "relevant_files": ["pkg/controller.go"],
  "evidence_citations": [{"path":"logs/build.log","line_start":2,"line_end":2}],
  "source_citations": [{"path":"pkg/controller.go","line_start":3,"line_end":3}],
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

func TestExecuteReportsTimeoutTelemetry(t *testing.T) {
	root, request := executorTestFixture(t)
	request.TimeoutSeconds = 1
	request.Hash = ""
	data, _ := json.Marshal(request)
	_ = data
	// Rebuild through the constructor so the request hash remains canonical.
	request, err := agentanalysis.NewWorkspaceExecutionRequest(request.Manifest, request.ModelGateway, time.Second, request.MaxSteps, request.ModelContextTokens, request.ModelOutputTokens, request.OutputLimitBytes)
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
	request, err := agentanalysis.NewWorkspaceExecutionRequest(base.Manifest, base.ModelGateway, time.Second, base.MaxSteps, base.ModelContextTokens, base.ModelOutputTokens, base.OutputLimitBytes)
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
		Gateway: request.ModelGateway, Prompt: "synthetic", ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens,
	}, "1.18.2")
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
		Gateway: request.ModelGateway, Prompt: "synthetic", ModelContextTokens: request.ModelContextTokens, ModelOutputTokens: request.ModelOutputTokens,
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
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%q}}},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%q}}},
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
	spec := OpenCodeSpec{WorkDir: workDir, Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}, Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err != nil {
		t.Fatal(err)
	}
	if posts != 2 || gets != 2 || len(result.Structured) == 0 || !result.Telemetry.EvidencePhaseCompleted || !result.Telemetry.FinalizationPhaseCompleted || result.Telemetry.ArtifactEvidenceToolCalls != 1 || result.Telemetry.SourceEvidenceToolCalls != 1 || result.Telemetry.StructuredOutputToolCalls != 1 || result.Telemetry.StepsUsed != 2 || result.Usage.ModelRequests != 2 {
		t.Fatalf("posts=%d gets=%d result=%+v", posts, gets, result)
	}
	for _, tool := range result.Telemetry.Tools {
		if tool.Name == "read" && tool.Count != 2 {
			t.Fatalf("unpersisted response parts were counted: %+v", result.Telemetry.Tools)
		}
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
	evidenceTelemetry := fmt.Sprintf(`[{"info":{"role":"assistant","error":{"name":"APIError","data":{"message":"retry","statusCode":429,"isRetryable":true}}},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%q}}},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":1}}}]}]`, artifactPath)
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
	spec := OpenCodeSpec{WorkDir: workDir, Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}, Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
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
	spec := OpenCodeSpec{WorkDir: workDir, Gateway: engineruntime.ModelGatewayConfig{Model: "test-model"}, Prompt: "investigate", MaxSteps: 20, ModelContextTokens: 200000, ModelOutputTokens: 8192}
	result, err := runOpenCodePhases(t.Context(), server.Client(), server.URL, "session-1", spec, "1.18.2", newOpenCodeEvidenceRequestShape(spec, "1.18.2"))
	if err == nil || result.Telemetry.FailureCode != "evidence_unavailable" || posts != 1 {
		t.Fatalf("posts=%d result=%+v err=%v", posts, result, err)
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
			value.Structured = []byte(strings.ReplaceAll(strings.ReplaceAll(string(executorAnalysisJSON()), `"relevant_files": ["pkg/controller.go"]`, `"relevant_files": []`), `"source_citations": [{"path":"pkg/controller.go","line_start":3,"line_end":3}]`, `"source_citations": []`))
			return value, nil
		},
	})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Analysis == nil || len(result.Analysis.SourceCitations) != 0 || len(result.Analysis.RelevantFiles) != 0 {
		t.Fatalf("result=%+v", result)
	}
}
