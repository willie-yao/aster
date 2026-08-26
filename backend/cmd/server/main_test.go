package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/server"
)

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

func TestTrustedOrigins_DerivesRedirectHost(t *testing.T) {
	got := trustedOrigins("https://dash.example.net/api/auth/callback", "https://alt.example, other.example")
	want := map[string]bool{"dash.example.net": true, "https://alt.example": true, "other.example": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected origin %q in %v", g, got)
		}
	}
}

func TestTrustedOrigins_EmptyRedirect(t *testing.T) {
	if got := trustedOrigins("", ""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
	got := trustedOrigins("", "only.example")
	if len(got) != 1 || got[0] != "only.example" {
		t.Errorf("got %v, want [only.example]", got)
	}
}

func TestInteractiveFeaturesFromEnv(t *testing.T) {
	setDefaults := func(t *testing.T) {
		t.Helper()
		t.Setenv("ANALYSIS_CHAT_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "")
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "")
	}
	t.Run("legacy actions default", func(t *testing.T) {
		setDefaults(t)
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat defaults writes off", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat and actions", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "1")
		t.Setenv("ACTIONS_ENABLED", "1")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("escalation defaults writes off", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if features.Actions || !features.PullRequestEscalation {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("escalation and actions", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("PULL_REQUEST_ESCALATION_ENABLED", "true")
		t.Setenv("ACTIONS_ENABLED", "true")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.PullRequestEscalation {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		setDefaults(t)
		t.Setenv("ANALYSIS_CHAT_ENABLED", "sometimes")
		if _, err := interactiveFeaturesFromEnv(); err == nil {
			t.Fatal("invalid feature flag was accepted")
		}
	})
}

func TestConfigureAuthenticatorChatOnlyDoesNotRequireBotToken(t *testing.T) {
	for _, mode := range []string{"dev", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AUTH_MODE", mode)
			t.Setenv("BOT_TOKEN", "")
			t.Setenv("DEV_LOGIN", "alice")
			t.Setenv("AUTH_PROXY_HEADER", "X-User")
			t.Setenv("ADMIN_LOGINS", "alice")
			var opts server.Options
			if err := configureAuthenticator(&opts, false); err != nil {
				t.Fatalf("chat-only auth: %v", err)
			}
			request := httptest.NewRequest("GET", "/", nil)
			request.Header.Set("X-User", "alice")
			identity, err := opts.Auth.Authenticate(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if identity.Login != "alice" || identity.Token != "" {
				t.Fatalf("identity = %+v", identity)
			}
			var writeOpts server.Options
			if err := configureAuthenticator(&writeOpts, true); err == nil {
				t.Fatal("write auth accepted an empty BOT_TOKEN")
			}
		})
	}
}

func TestConfigureAuthenticatorOAuthUsesIdentityOnlyScope(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		actionsEnabled bool
		botToken       string
	}{
		{name: "chat only"},
		{name: "actions", actionsEnabled: true, botToken: "bot-token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AUTH_MODE", "oauth")
			t.Setenv("OAUTH_CLIENT_ID", "client")
			t.Setenv("OAUTH_CLIENT_SECRET", "secret")
			t.Setenv("OAUTH_REDIRECT_URL", "https://dashboard.test/api/auth/callback")
			t.Setenv("OAUTH_SCOPE", "")
			t.Setenv("OAUTH_PRIVATE_REPOSITORIES", "")
			t.Setenv("BOT_TOKEN", testCase.botToken)
			t.Setenv("SESSION_KEY", strings.Repeat("k", 32))
			t.Setenv("ADMIN_LOGINS", "alice")
			var opts server.Options
			if err := configureAuthenticator(&opts, testCase.actionsEnabled); err != nil {
				t.Fatal(err)
			}
			registrar, ok := opts.Auth.(interface{ Register(*http.ServeMux) })
			if !ok {
				t.Fatalf("authenticator %T does not register OAuth routes", opts.Auth)
			}
			mux := http.NewServeMux()
			registrar.Register(mux)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/auth/login", nil))
			location, err := url.Parse(recorder.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if got := location.Query().Get("scope"); got != "read:user" {
				t.Fatalf("scope = %q, want read:user", got)
			}
		})
	}

	t.Setenv("AUTH_MODE", "oauth")
	t.Setenv("OAUTH_SCOPE", "")
	t.Setenv("OAUTH_PRIVATE_REPOSITORIES", "")
	t.Setenv("BOT_TOKEN", "")
	if err := configureAuthenticator(&server.Options{}, true); err == nil || !strings.Contains(err.Error(), "BOT_TOKEN") {
		t.Fatalf("missing bot token error = %v", err)
	}
}

func TestAnalysisChatServiceOptionsFromEnv(t *testing.T) {
	for _, name := range []string{
		"ANALYSIS_CHAT_STATE_DIR",
		"ANALYSIS_CHAT_SESSION_TTL",
		"ANALYSIS_CHAT_MAX_SESSIONS",
		"ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER",
		"ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER",
		"ANALYSIS_CHAT_REQUESTS_PER_MINUTE",
	} {
		t.Setenv(name, "")
	}
	opts, err := analysisChatServiceOptionsFromEnv("/data", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if opts.StateDir != filepath.Join("/data", ".analysis-chat") || opts.SessionTTL != 2*time.Hour ||
		opts.MaxSessions != 128 || opts.MaxSessionsPerOwner != 8 || opts.TurnLeaseTTL != 90*time.Second ||
		opts.TurnTimeout != time.Minute || opts.MaxActiveTurnsPerOwner != 2 || opts.MaxRequestsPerOwnerPerMinute != 10 {
		t.Fatalf("default options = %+v", opts)
	}

	t.Setenv("ANALYSIS_CHAT_STATE_DIR", "/state/chat")
	t.Setenv("ANALYSIS_CHAT_SESSION_TTL", "45m")
	t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS", "24")
	t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", "3")
	t.Setenv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", "4")
	t.Setenv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", "20")
	opts, err = analysisChatServiceOptionsFromEnv("/data", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if opts.StateDir != "/state/chat" || opts.SessionTTL != 45*time.Minute ||
		opts.MaxSessions != 24 || opts.MaxSessionsPerOwner != 3 || opts.TurnLeaseTTL != time.Minute ||
		opts.TurnTimeout != 30*time.Second || opts.MaxActiveTurnsPerOwner != 4 || opts.MaxRequestsPerOwnerPerMinute != 20 {
		t.Fatalf("configured options = %+v", opts)
	}
}

func TestAnalysisChatServiceOptionsRejectInvalidEnv(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "ANALYSIS_CHAT_SESSION_TTL", value: "zero"},
		{name: "ANALYSIS_CHAT_MAX_SESSIONS", value: "0"},
		{name: "ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", value: "many"},
		{name: "ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", value: "0"},
		{name: "ANALYSIS_CHAT_REQUESTS_PER_MINUTE", value: "none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, name := range []string{
				"ANALYSIS_CHAT_SESSION_TTL",
				"ANALYSIS_CHAT_MAX_SESSIONS",
				"ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER",
				"ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER",
				"ANALYSIS_CHAT_REQUESTS_PER_MINUTE",
			} {
				t.Setenv(name, "")
			}
			t.Setenv(testCase.name, testCase.value)
			if _, err := analysisChatServiceOptionsFromEnv("/data", time.Minute); err == nil {
				t.Fatal("invalid analysis chat setting was accepted")
			}
		})
	}
	t.Run("owner exceeds total", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_SESSION_TTL", "")
		t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS", "2")
		t.Setenv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", "3")
		t.Setenv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", "4")
		t.Setenv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", "20")
		if _, err := analysisChatServiceOptionsFromEnv("/data", time.Minute); err == nil {
			t.Fatal("owner limit above total was accepted")
		}
	})
}

func TestAnalysisChatTimeoutFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_TIMEOUT", "")
		got, err := analysisChatTimeoutFromEnv()
		if err != nil || got != 10*time.Minute {
			t.Fatalf("timeout=%v err=%v", got, err)
		}
	})
	t.Run("slow provider", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_TIMEOUT", "10m")
		got, err := analysisChatTimeoutFromEnv()
		if err != nil || got != 10*time.Minute {
			t.Fatalf("timeout=%v err=%v", got, err)
		}
	})
	for _, value := range []string{"0s", "31m", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ANALYSIS_CHAT_TIMEOUT", value)
			if _, err := analysisChatTimeoutFromEnv(); err == nil {
				t.Fatalf("invalid timeout %q was accepted", value)
			}
		})
	}
}

func TestConfigureAuthenticatorRejectsLegacyOAuthRepositoryControls(t *testing.T) {
	for _, name := range []string{"OAUTH_SCOPE", "OAUTH_PRIVATE_REPOSITORIES"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AUTH_MODE", "oauth")
			t.Setenv("OAUTH_SCOPE", "")
			t.Setenv("OAUTH_PRIVATE_REPOSITORIES", "")
			t.Setenv(name, "repo")
			if err := configureAuthenticator(&server.Options{}, true); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("legacy repository control error = %v", err)
			}
		})
	}
}

func TestExactJUnitChatFixEnabledRequiresOptInAgentSandbox(t *testing.T) {
	for _, testCase := range []struct {
		name string
		cfg  project.FixPRs
		want bool
	}{
		{name: "enabled agent sandbox", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}, want: true},
		{name: "disabled", cfg: project.FixPRs{AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}},
		{name: "unsupported runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "opencode"}}},
		{name: "missing runtime", cfg: project.FixPRs{Enabled: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := exactJUnitChatFixEnabled(testCase.cfg); got != testCase.want {
				t.Fatalf("enabled = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestFixActionsEnabledDoesNotAdvertiseLocalRuntimeByDefault(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		cfg      project.FixPRs
		authMode string
		trusted  string
		want     bool
		wantErr  string
	}{
		{name: "agent sandbox", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}, authMode: "oauth", want: true},
		{name: "default runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{}}, authMode: "proxy", want: true},
		{name: "unsupported runtime", cfg: project.FixPRs{Enabled: true, AgentRuntime: &project.FixAgentRuntime{Type: "opencode"}}, authMode: "oauth"},
		{name: "disabled", cfg: project.FixPRs{AgentRuntime: &project.FixAgentRuntime{Type: "agent-sandbox"}}, authMode: "oauth"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("TRUSTED_LOCAL_FIX_RUNTIME", testCase.trusted)
			got, err := fixActionsEnabled(testCase.cfg, testCase.authMode)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("error=%v want=%q", err, testCase.wantErr)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("enabled=%t err=%v want=%t", got, err, testCase.want)
			}
		})
	}
}
