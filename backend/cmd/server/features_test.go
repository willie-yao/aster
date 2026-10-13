package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/server"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type sourceReadTransport func(*http.Request) (*http.Response, error)

func (f sourceReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnableActionsAuthenticatesSourceChecks(t *testing.T) {
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
