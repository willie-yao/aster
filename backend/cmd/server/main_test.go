package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/server"
)

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
	t.Run("legacy actions default", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat defaults writes off", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "true")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat and actions", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "1")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "1")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("chat corrections", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "true")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "true")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "")
		t.Setenv("ACTIONS_ENABLED", "")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.AnalysisChat || !features.AnalysisCorrections || features.Actions {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("source investigation", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "true")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "true")
		t.Setenv("ACTIONS_ENABLED", "")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.AnalysisChat || !features.SourceInvestigation || features.Actions {
			t.Fatalf("features = %+v", features)
		}
	})
	t.Run("source investigation requires chat", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "false")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "")
		t.Setenv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", "true")
		if _, err := interactiveFeaturesFromEnv(); err == nil {
			t.Fatal("source investigation was accepted without chat")
		}
	})
	t.Run("corrections require chat", func(t *testing.T) {
		t.Setenv("ANALYSIS_CHAT_ENABLED", "false")
		t.Setenv("ANALYSIS_CORRECTIONS_ENABLED", "true")
		if _, err := interactiveFeaturesFromEnv(); err == nil {
			t.Fatal("analysis corrections were accepted without chat")
		}
	})
	t.Run("invalid", func(t *testing.T) {
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

func TestConfigureAuthenticatorOAuthScopeByFeature(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		actionsEnabled bool
		wantScope      string
	}{
		{name: "chat only", wantScope: "read:user"},
		{name: "public actions", actionsEnabled: true, wantScope: "public_repo"},
		{name: "private actions", actionsEnabled: true, wantScope: "repo"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AUTH_MODE", "oauth")
			t.Setenv("OAUTH_CLIENT_ID", "client")
			t.Setenv("OAUTH_CLIENT_SECRET", "secret")
			t.Setenv("OAUTH_REDIRECT_URL", "https://dashboard.test/api/auth/callback")
			t.Setenv("OAUTH_SCOPE", "")
			t.Setenv("OAUTH_PRIVATE_REPOSITORIES", strconv.FormatBool(testCase.name == "private actions"))
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
			if got := location.Query().Get("scope"); got != testCase.wantScope {
				t.Fatalf("scope = %q, want %q", got, testCase.wantScope)
			}
		})
	}
}

func TestSourceInvestigationKubeContextUsesOrkaSelector(t *testing.T) {
	t.Setenv("KUBECONTEXT", "wrong-cluster")
	t.Setenv("ORKA_KUBE_CONTEXT", "source-cluster")
	if got := sourceInvestigationKubeContext(); got != "source-cluster" {
		t.Fatalf("source investigation kube context = %q", got)
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
		if err != nil || got != 2*time.Minute {
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

func TestConfigureAuthenticatorRejectsLegacyOAuthScope(t *testing.T) {
	t.Setenv("AUTH_MODE", "oauth")
	t.Setenv("OAUTH_SCOPE", "repo")
	if err := configureAuthenticator(&server.Options{}, true); err == nil || !strings.Contains(err.Error(), "OAUTH_SCOPE") {
		t.Fatalf("legacy scope error = %v", err)
	}
}
