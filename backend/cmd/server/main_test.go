package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
		t.Setenv("ACTIONS_ENABLED", "1")
		features, err := interactiveFeaturesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !features.Actions || !features.AnalysisChat {
			t.Fatalf("features = %+v", features)
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
		{name: "actions", actionsEnabled: true, wantScope: "repo"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AUTH_MODE", "oauth")
			t.Setenv("OAUTH_CLIENT_ID", "client")
			t.Setenv("OAUTH_CLIENT_SECRET", "secret")
			t.Setenv("OAUTH_REDIRECT_URL", "https://dashboard.test/api/auth/callback")
			t.Setenv("OAUTH_SCOPE", "")
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
