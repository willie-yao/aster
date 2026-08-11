package analysisexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
	if result.Analysis.EvidenceCitations[0].Path != "logs/build.log" || result.Analysis.EvidenceCitations[0].Quote != "artifact-only-marker specific failure" || !result.Usage.Available || result.Usage.ModelRequests != 1 {
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

func TestExecuteRejectsWorkspaceMutation(t *testing.T) {
	root, request := executorTestFixture(t)
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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

func TestPromptOpenCodeUsesStructuredOutputSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/session-1/message" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["agent"] != "analysis" {
			t.Fatalf("agent = %v", payload["agent"])
		}
		tools := payload["tools"].(map[string]any)
		if tools["read"] != false || tools["bash"] != false || tools["edit"] != false || tools["task"] != false || tools["webfetch"] != false {
			t.Fatalf("tools = %v", tools)
		}
		if _, ok := tools["glob"]; ok {
			t.Fatal("session tool overrides must not replace agent glob permission")
		}
		if _, ok := tools["grep"]; ok {
			t.Fatal("session tool overrides must not replace agent grep permission")
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

func TestWriteOpenCodeConfigKeepsNativeHarnessButDeniesNetworkTools(t *testing.T) {
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
	permissions := config["permission"].(map[string]any)
	if len(permissions) != 1 || permissions["*"] != "deny" {
		t.Fatalf("global permissions=%v", permissions)
	}
	agents := config["agent"].(map[string]any)
	if _, ok := agents["build"]; ok || len(agents) != 1 {
		t.Fatalf("agents=%v", agents)
	}
	analysis := agents["analysis"].(map[string]any)
	if analysis["mode"] != "primary" || analysis["steps"].(float64) != 20 || !strings.Contains(analysis["prompt"].(string), "read-only Prow failure analyst") {
		t.Fatalf("analysis agent=%v", analysis)
	}
	agentPermissions := analysis["permission"].(map[string]any)
	for _, denied := range []string{"bash", "edit", "write", "apply_patch", "webfetch", "websearch", "task", "skill", "external_directory"} {
		if agentPermissions[denied] != "deny" {
			t.Fatalf("permission %s=%v", denied, agentPermissions[denied])
		}
	}
	if agentPermissions["read"] != nil || agentPermissions["glob"] != "allow" || agentPermissions["grep"] != "allow" || agentPermissions["StructuredOutput"] != "allow" {
		t.Fatalf("analysis permissions=%v", agentPermissions)
	}
	provider := config["provider"].(map[string]any)["engine"].(map[string]any)
	model := provider["models"].(map[string]any)["test-model"].(map[string]any)
	limits := model["limit"].(map[string]any)
	if limits["context"].(float64) != 200000 || limits["output"].(float64) != 8192 {
		t.Fatalf("model limits=%v", limits)
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
		Usage:      agentanalysis.WorkspaceUsage{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, ModelRequests: 1, InputTokens: 10, OutputTokens: 5, CostAvailable: true, CostUSD: "0.01000000"},
		Telemetry:  agentanalysis.WorkspaceOpenCodeTelemetry{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, EventCount: 2, StepsUsed: 1, StructuredOutputRetriesKnown: true},
	}
}

func executorAnalysisJSON() []byte {
	return []byte(`{
  "version": 1,
  "contract_version": "agent-analysis-workspace-v4",
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
		WorkspaceRoot: root, TempRoot: t.TempDir(),
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
