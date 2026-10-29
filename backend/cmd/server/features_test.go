package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	fixruntime "github.com/willie-yao/aster/backend/internal/fix/runtime"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/server"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type sourceReadTransport func(*http.Request) (*http.Response, error)

func (f sourceReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnableActionsAuthenticatesSourceChecks(t *testing.T) {
	originalRuntime := newFixRuntime
	t.Cleanup(func() { newFixRuntime = originalRuntime })
	newFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) {
		return &fixruntime.AgentSandboxRuntime{}, nil
	}
	for _, tc := range []struct {
		name, source, read, bot, github, want string
	}{
		{name: "source override", source: "source-token", read: "read-token", bot: "bot-token", github: "github-token", want: "source-token"},
		{name: "read preference", read: "read-token", bot: "bot-token", github: "github-token", want: "read-token"},
		{name: "bot fallback", bot: "bot-token", github: "github-token", want: "bot-token"},
		{name: "GitHub fallback", github: "github-token", want: "github-token"},
		{name: "anonymous only without credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOURCE_INVESTIGATION_GITHUB_TOKEN", tc.source)
			t.Setenv("GITHUB_READ_TOKEN", tc.read)
			t.Setenv("BOT_TOKEN", tc.bot)
			t.Setenv("GITHUB_TOKEN", tc.github)
			t.Setenv("ACTION_TIMEOUT", "40m")
			t.Setenv("AI_API", "")
			t.Setenv("AI_ENDPOINT", "")
			t.Setenv("AI_MODEL", "")
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			var calls int
			http.DefaultTransport = sourceReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				want := ""
				if tc.want != "" {
					want = "Bearer " + tc.want
				}
				if r.Header.Get("Authorization") != want {
					t.Errorf("source request used the wrong credential")
				}
				var body string
				switch r.URL.Path {
				case "/repos/up/stream":
					body = `{"default_branch":"main"}`
				case "/repos/up/stream/git/ref/heads/main":
					body = `{"object":{"sha":"` + strings.Repeat("a", 40) + `"}}`
				case "/repos/up/stream/git/commits/" + strings.Repeat("a", 40):
					body = `{"tree":{"sha":"tree"}}`
				default:
					return nil, fmt.Errorf("unexpected source request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
				Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"},
			}}}
			ctx, cancel := context.WithCancel(t.Context())
			service, err := enableActions(ctx, &server.Options{}, cfg, t.TempDir(), nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() {
				cancel()
				if err := service.Wait(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			repo := sourceinvestigation.Repository{Owner: "up", Name: "stream", Revision: strings.Repeat("a", 40)}
			branches, err := service.ResolveAnalysisFixBranches(t.Context(), repo, "release-1.24")
			if err != nil || branches.Default != "main" || calls != 3 {
				t.Fatalf("branches=%+v requests=%d err=%v", branches, calls, err)
			}
		})
	}
}

func TestEnableActionsConfiguresGenerationTimeouts(t *testing.T) {
	original := newFixRuntime
	t.Cleanup(func() { newFixRuntime = original })
	newFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) {
		return &fixruntime.AgentSandboxRuntime{}, nil
	}
	for _, testCase := range []struct {
		name, agentTimeout string
		fixEnabled         bool
		explicit           string
		wantHandler        time.Duration
		wantFixPreview     time.Duration
		wantError          bool
	}{
		{name: "default agent", fixEnabled: true, wantFixPreview: 15 * time.Minute},
		{name: "long agent", fixEnabled: true, agentTimeout: "30m", wantFixPreview: 35 * time.Minute},
		{name: "Fix disabled", wantFixPreview: 0},
		{name: "explicit override", fixEnabled: true, explicit: "40m", agentTimeout: "30m", wantHandler: 40 * time.Minute, wantFixPreview: 40 * time.Minute},
		{name: "invalid override", fixEnabled: true, explicit: "14m", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("ACTION_TIMEOUT", "")
			if testCase.explicit == "" {
				if err := os.Unsetenv("ACTION_TIMEOUT"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("ACTION_TIMEOUT", testCase.explicit)
			}
			cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{
				Enabled: testCase.fixEnabled, AgentRuntime: &project.FixAgentRuntime{
					Type: "agent-sandbox", Timeout: testCase.agentTimeout,
				},
			}}}
			opts := &server.Options{}
			ctx, cancel := context.WithCancel(context.Background())
			service, err := enableActions(ctx, opts, cfg, t.TempDir(), nil)
			defer func() {
				cancel()
				if service != nil {
					if err := service.Wait(context.Background()); err != nil {
						t.Error(err)
					}
				}
			}()
			if testCase.wantError {
				if err == nil || service != nil || opts.Actions != nil {
					t.Fatalf("invalid startup: service=%v options=%+v error=%v", service, opts, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.ActionTimeout != testCase.wantHandler || opts.FixPreviewTimeout != testCase.wantFixPreview ||
				opts.DisableFixActions == testCase.fixEnabled || opts.Actions != service {
				t.Fatalf("options=%+v, want handler=%s Fix preview=%s enabled=%t", opts, testCase.wantHandler, testCase.wantFixPreview, testCase.fixEnabled)
			}
		})
	}
}

func TestInteractiveFeaturesRequireLocalFixRuntimeReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outputLimit string
		local       bool
		disabled    bool
	}{
		{name: "missing deployment settings"},
		{name: "invalid deployment settings", outputLimit: "private-runtime-value"},
		{name: "valid local construction", local: true},
		{name: "Fix disabled", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range map[string]string{
				"ACTIONS_ENABLED": "true", "ANALYSIS_CHAT_ENABLED": "true", "PULL_REQUEST_ESCALATION_ENABLED": "false",
				"AUTH_MODE": "dev", "BOT_TOKEN": "private-bot-token", "AI_TOKEN": "private-ai-token",
				"AI_API": "chat_completions", "AI_ENDPOINT": "https://provider.example/v1/chat/completions",
				"AI_MODEL": "fixture-model", "AI_REASONING_EFFORT": "", "AI_MAX_OUTPUT_TOKENS": "",
				"AI_CONTEXT_WINDOW_TOKENS": "32768", "AI_CACHE_GENERATION": "",
				"AGENT_SANDBOX_OUTPUT_LIMIT_BYTES": tc.outputLimit,
				"ANALYSIS_CHAT_STATE_DIR":          "", "ANALYSIS_CHAT_TIMEOUT": "",
			} {
				t.Setenv(name, value)
			}
			t.Setenv("ACTION_TIMEOUT", "1m")
			if tc.local {
				t.Setenv("ACTION_TIMEOUT", "15m")
				original := newFixRuntime
				t.Cleanup(func() { newFixRuntime = original })
				newFixRuntime = func(cfg *project.FixAgentRuntime) (runtime.AgentRuntime, error) {
					return fixruntime.NewAgentSandboxRuntime(nil, fixruntime.AgentSandboxOptions{
						Namespace: "fix-test", Image: "example.test/fixer@sha256:" + strings.Repeat("a", 64),
						ServiceAccountName: "fix-workload", RuntimeClassName: "kata",
						ModelProvider: cfg.ModelProvider.RuntimeConfig(),
						Timeout:       cfg.ParsedTimeout(), OutputLimitBytes: cfg.OutputLimitBytes,
					})
				}
			}
			cfg, err := project.Load("../../../configs/example/project.yaml")
			if err != nil {
				t.Fatal(err)
			}
			cfg.AI = &project.AI{FixPRs: &project.FixPRs{
				Enabled: !tc.disabled, AuthorName: "Fixture", AuthorEmail: "fixture@example.test",
				AgentRuntime: &project.FixAgentRuntime{
					Type: "agent-sandbox",
					ModelProvider: project.FixModelProvider{
						CredentialMode: "direct", API: "chat_completions",
						Endpoint: "https://provider.example/v1/chat/completions", Model: "fixture-model",
						Auth: project.FixModelProviderAuth{Type: "none"},
					},
				},
			}}
			projectDir, dataDir := t.TempDir(), t.TempDir()
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
			if err := os.WriteFile(filepath.Join(projectDir, "prompts", "system.md"), []byte("Use artifact evidence."), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			originalLog := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(originalLog) })
			ctx, cancel := context.WithCancel(t.Context())
			opts := server.Options{DataDir: dataDir, Capabilities: server.DefaultCapabilities()}
			t.Cleanup(func() {
				cancel()
				for _, service := range []any{opts.Actions, opts.AnalysisChat} {
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
			handler, err := server.Handler(opts)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
			var caps server.Capabilities
			if response.Code != http.StatusOK {
				t.Fatalf("capabilities status = %d", response.Code)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &caps); err != nil {
				t.Fatal(err)
			}
			if !caps.Features.Actions || !caps.Features.AnalysisChat || !caps.Features.ActionRequests ||
				caps.Features.FixPRs != tc.local || caps.Features.ChatFix != tc.local ||
				caps.Features.JUnitChatFix != tc.local || opts.DisableFixActions == tc.local {
				t.Fatalf("capabilities = %+v, disable Fix = %t", caps, opts.DisableFixActions)
			}
			wantDiagnostic := !tc.local && !tc.disabled
			if strings.Contains(logs.String(), "Fix actions disabled: cannot construct") != wantDiagnostic {
				t.Fatalf("unexpected startup diagnostic: %s", logs.String())
			}
			for _, secret := range []string{"private-runtime-value", "private-bot-token", "private-ai-token"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("startup diagnostic exposed a private value")
				}
			}
		})
	}
}

func TestMockFeaturesDoNotRequireProductionFixRuntime(t *testing.T) {
	original := newFixRuntime
	t.Cleanup(func() { newFixRuntime = original })
	newFixRuntime = func(*project.FixAgentRuntime) (runtime.AgentRuntime, error) {
		t.Fatal("mock mode tried to construct a production Fix runtime")
		return nil, nil
	}
	t.Setenv("MOCK_LATENCY", "0s")
	opts := server.Options{}
	if err := enableMockFeatures(&opts, "../../../configs/example", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if opts.Actions == nil || opts.ChatFix == nil || !opts.Capabilities.Features.JUnitChatFix || opts.DisableFixActions {
		t.Fatal("mock mode lost its fabricated Fix capabilities")
	}
}

func TestFormatPricingRate(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "3.000000", want: "3.00"},
		{value: "0.305", want: "0.31"},
		{value: "0.304", want: "0.30"},
	} {
		if got := formatPricingRate(test.value); got != test.want {
			t.Errorf("formatPricingRate(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}
