package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/project"
	prescalation "github.com/willie-yao/aster/backend/internal/pullrequest/escalation"
	"github.com/willie-yao/aster/backend/internal/server"
)

func TestEscalationsHonorOutputLimitAndRecordServerUsage(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.MaxTokens != 321 {
			t.Errorf("max_tokens=%d, want 321", request.MaxTokens)
		}
		calls.Add(1)
		message := map[string]any{"role": "assistant"}
		if request.Messages[len(request.Messages)-1].Role != "tool" {
			message["tool_calls"] = []any{map[string]any{
				"id": "read-log", "type": "function",
				"function": map[string]any{"name": "read_artifact", "arguments": `{"path":"build-log.txt"}`},
			}}
		} else {
			message["content"] = `{"summary":"configuration mismatch","root_cause":"configuration mismatch","severity":"High","suggested_fix":"Update the configured endpoint.","is_transient":false,"relevant_files":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": message}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 2},
		})
	}))
	defer provider.Close()
	for name, value := range map[string]string{
		"AI_TOKEN": "fixture-token", "AI_API": "chat_completions", "AI_ENDPOINT": provider.URL,
		"AI_MODEL": "fixture-model", "AI_REASONING_EFFORT": "", "AI_MAX_OUTPUT_TOKENS": "321",
		"AI_CONTEXT_WINDOW_TOKENS": "32768", "AI_CACHE_GENERATION": "", "AUTH_MODE": "dev",
		"ACTIONS_ENABLED": "false", "ANALYSIS_CHAT_ENABLED": "false", "PULL_REQUEST_ESCALATION_ENABLED": "true",
	} {
		t.Setenv(name, value)
	}
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	transport := originalTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == provider.Listener.Addr().String() {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		return nil, fmt.Errorf("external request blocked by escalation test")
	}
	http.DefaultTransport = transport
	t.Cleanup(transport.CloseIdleConnections)
	projectDir, dataDir, bucket := t.TempDir(), t.TempDir(), t.TempDir()
	cfg, err := project.Load("../../../configs/example/project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Branding.SourceRepo = project.SourceRepo{Owner: "org", Name: "repo"}
	cfg.Storage = project.Storage{Provider: "local", Base: bucket}
	cfg.PullRequests = &project.PullRequests{Enabled: true}
	cfg.AI = &project.AI{
		Agentic: project.Agentic{MinToolCalls: 1, MaxIters: 4, Tools: []string{"filesystem"}},
		Usage:   &project.AIUsage{Enabled: new(true)},
	}
	write := func(path string, value any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configYAML, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), configYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(projectDir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "prompts", "system.md"), []byte("Read the failure artifacts."), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, number := range []int{1, 2} {
		buildID := fmt.Sprint(number * 100)
		write(filepath.Join(dataDir, "pull-requests", models.PullRequestDataFilename(number)), models.PullRequestDetail{
			PullRequestSummary: models.PullRequestSummary{Number: number, HeadSHA: "abc123", BaseRef: "main"},
			Checks: []models.PullRequestCheck{{
				JobID: "org/repo/pull-e2e", JobName: "pull-e2e", BuildID: buildID,
				Failures: []models.PullRequestFailure{{TestCase: models.TestCase{Name: "TestA", FailureMessage: "configuration mismatch"}}},
			}},
		})
		buildDir := filepath.Join(bucket, "pr-logs", "pull", "org_repo", fmt.Sprint(number), "pull-e2e", buildID)
		write(filepath.Join(buildDir, "started.json"), map[string]any{"timestamp": now.Unix(), "repo-commit": "abc123"})
		write(filepath.Join(buildDir, "finished.json"), map[string]any{"timestamp": now.Unix() + 1, "passed": false, "result": "FAILURE"})
		if err := os.WriteFile(filepath.Join(buildDir, "build-log.txt"), []byte("configuration mismatch\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dataDir, output.SharedFailureIndexFilename), models.SharedFailureIndex{
		Repo: "org/repo", Failures: []models.SharedFailure{{
			ID: "shared", BaseRef: "main", JobName: "pull-e2e", JobID: "org/repo/pull-e2e",
			TestName: "TestA", Escalatable: true,
			PullRequests: []models.SharedFailureMember{{
				Number: 2, BuildID: "200", Started: now, Finished: now.Add(time.Second),
			}},
		}},
	})
	opts := server.Options{DataDir: dataDir, Capabilities: server.DefaultCapabilities()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() {
		cancel()
		for _, service := range []any{opts.PullRequestEscalation, opts.SharedFailureEscalation} {
			if waiter, ok := service.(interface{ Wait(context.Context) error }); ok {
				if err := waiter.Wait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}
	})
	if err := enableInteractiveFeatures(ctx, &opts, projectDir, dataDir); err != nil {
		t.Fatal(err)
	}
	prRef := prescalation.Ref{PullNumber: 1, JobID: "org/repo/pull-e2e", BuildID: "100", TestName: "TestA"}
	if _, err := opts.PullRequestEscalation.Start(t.Context(), prRef, "admin", "pr"); err != nil {
		t.Fatal(err)
	}
	clusterRef := prescalation.ClusterRef{ID: "shared"}
	if _, err := opts.SharedFailureEscalation.Start(t.Context(), clusterRef, "admin", "shared"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for {
		pr, prErr := opts.PullRequestEscalation.Get(prRef)
		shared, sharedErr := opts.SharedFailureEscalation.Get(clusterRef)
		if prErr != nil || sharedErr != nil || pr.State == prescalation.StateFailed || shared.State == prescalation.StateFailed {
			t.Fatalf("pr=%+v (%v), shared=%+v (%v)", pr, prErr, shared, sharedErr)
		}
		if pr.State == prescalation.StateComplete && shared.State == prescalation.StateComplete {
			if pr.Disposition != models.AnalysisDispositionPreliminary || shared.Disposition != models.AnalysisDispositionPreliminary ||
				len(pr.DispositionWarnings) == 0 || len(shared.DispositionWarnings) == 0 {
				t.Fatalf("lost quality: pr=%+v shared=%+v", pr, shared)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("escalations did not finish")
		case <-time.After(5 * time.Millisecond):
		}
	}
	for _, service := range []any{opts.PullRequestEscalation, opts.SharedFailureEscalation} {
		if err := service.(interface{ Wait(context.Context) error }).Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dataDir, output.AIUsageServerFilename))
	if err != nil {
		t.Fatal(err)
	}
	var ledger aiusage.UsageLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || len(ledger.RecentOperations) != 2 {
		t.Fatalf("calls=%d operations=%+v", calls.Load(), ledger.RecentOperations)
	}
	for _, op := range ledger.RecentOperations {
		if op.Origin != aiusage.OriginServer || op.Feature != aiusage.FeatureFailureAnalysis ||
			op.ModelRequests != 2 || op.ReportedRequests != 2 || op.InputTokens != 20 || op.OutputTokens != 4 {
			t.Fatalf("usage=%+v", op)
		}
	}
}

func TestMaxOutputTokensFromEnv(t *testing.T) {
	for _, value := range []string{"", "0", "321", "-1", "invalid", "131073"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AI_MAX_OUTPUT_TOKENS", value)
			limit, err := maxOutputTokensFromEnv()
			wantError := value == "-1" || value == "invalid" || value == "131073"
			if (err != nil) != wantError || (!wantError && value == "321" && limit != 321) {
				t.Fatalf("limit=%d err=%v", limit, err)
			}
		})
	}
}
