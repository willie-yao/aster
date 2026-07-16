// Command server serves the dashboard's pre-computed JSON over HTTP for the
// Kubernetes-native deploy mode. It serves the same /data/*.json contract the
// static Pages site reads, plus /api/capabilities so the frontend can light up
// server-only features. The static Pages mode keeps working unchanged.
//
// Admin-gated write actions (create-issue, propose-fix) are enabled when
// -project-dir is set and AUTH_MODE selects an auth mechanism:
//
//	oauth: GitHub OAuth App login; each admin's own token performs the write.
//	       Needs OAUTH_CLIENT_ID, OAUTH_CLIENT_SECRET, OAUTH_REDIRECT_URL,
//	       SESSION_KEY, and ADMIN_LOGINS.
//	proxy: an upstream SSO proxy authenticates and passes AUTH_PROXY_HEADER;
//	       a bot token (BOT_TOKEN) performs the write. Requires ADMIN_LOGINS.
//	dev:   local development only; authenticates every request as an admin.
//	       Needs BOT_TOKEN (the write credential); DEV_LOGIN sets the identity.
//	       Never use in a deployment reachable by untrusted clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/server"
)

func main() {
	var (
		addr       string
		dataDir    string
		staticDir  string
		projectDir string
	)
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&dataDir, "data-dir", "data", "directory of fetcher JSON output served at /data")
	flag.StringVar(&staticDir, "static-dir", "", "optional built frontend (dist) served at / with SPA fallback")
	flag.StringVar(&projectDir, "project-dir", "", "project.yaml directory; enables admin actions when set with AUTH_MODE")
	flag.Parse()

	opts := server.Options{
		DataDir:      dataDir,
		StaticDir:    staticDir,
		Capabilities: server.DefaultCapabilities(),
	}

	// Enable admin-gated actions only when a project config and an auth mode are
	// both provided. Otherwise the server stays read-only.
	if projectDir != "" && os.Getenv("AUTH_MODE") != "" {
		if err := enableActions(&opts, projectDir, dataDir); err != nil {
			log.Fatalf("server: enabling actions: %v", err)
		}
		log.Printf("🔐 admin actions enabled (auth mode: %s)", opts.AuthMode)
	} else {
		log.Println("actions disabled (set -project-dir and AUTH_MODE to enable)")
	}

	handler, err := server.Handler(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// Bound the header read so a slow-header client cannot tie up a
		// connection. WriteTimeout is intentionally unset: an action request
		// (draft a fix PR) can legitimately run for minutes. IdleTimeout caps
		// idle keep-alive connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shut down gracefully on SIGINT/SIGTERM so K8s rollouts drain cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("🌐 serving %s -> data=%s static=%q", addr, dataDir, staticDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: graceful shutdown: %v", err)
	}
}

// enableActions loads the project config, builds the action service, and wires
// the authenticator selected by AUTH_MODE onto opts.
func enableActions(opts *server.Options, projectDir, dataDir string) error {
	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	provider := cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"))
	opts.Actions = actions.NewService(cfg, dataDir, actions.AIConfig{
		Token:    os.Getenv("AI_TOKEN"),
		Endpoint: provider.Endpoint,
		Model:    provider.Model,
		Headers:  provider.Headers,
	})

	// A single fix draft runs locate + edit + critique against the model; the
	// 5-minute default is tight for slow self-hosted endpoints, so allow an
	// override (e.g. ACTION_TIMEOUT=15m).
	if v := os.Getenv("ACTION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid ACTION_TIMEOUT %q: %w", v, err)
		}
		opts.ActionTimeout = d
	}

	admins := splitList(os.Getenv("ADMIN_LOGINS"))
	switch mode := os.Getenv("AUTH_MODE"); mode {
	case "oauth":
		o, err := auth.NewOAuth(auth.OAuthConfig{
			ClientID:      os.Getenv("OAUTH_CLIENT_ID"),
			ClientSecret:  os.Getenv("OAUTH_CLIENT_SECRET"),
			RedirectURL:   os.Getenv("OAUTH_REDIRECT_URL"),
			Scope:         os.Getenv("OAUTH_SCOPE"),
			Admins:        admins,
			SessionKey:    os.Getenv("SESSION_KEY"),
			SecureCookies: os.Getenv("COOKIE_INSECURE") != "1",
		})
		if err != nil {
			return err
		}
		opts.Auth = o
		opts.AuthMode = "oauth"
		opts.LoginURL = "/api/auth/login"
	case "proxy":
		botToken := os.Getenv("BOT_TOKEN")
		if botToken == "" {
			return fmt.Errorf("proxy auth mode requires BOT_TOKEN")
		}
		header := os.Getenv("AUTH_PROXY_HEADER")
		if header == "" {
			return fmt.Errorf("proxy auth mode requires AUTH_PROXY_HEADER (the trusted identity header)")
		}
		if len(admins) == 0 {
			return fmt.Errorf("proxy auth mode requires ADMIN_LOGINS (the allowlist of identities that may act)")
		}
		opts.Auth = auth.NewBotAuthenticator(header, botToken, admins, os.Getenv("AUTH_PROXY_SECRET"))
		opts.AuthMode = "proxy"
	case "dev":
		botToken := os.Getenv("BOT_TOKEN")
		if botToken == "" {
			return fmt.Errorf("dev auth mode requires BOT_TOKEN (the write credential)")
		}
		login := os.Getenv("DEV_LOGIN")
		if login == "" {
			login = "dev-admin"
		}
		log.Printf("⚠️  AUTH_MODE=dev: authenticating every request as admin %q; local use only, never expose this server", login)
		opts.Auth = auth.NewDevAuthenticator(login, botToken)
		opts.AuthMode = "dev"
	default:
		return fmt.Errorf("unknown AUTH_MODE %q (want oauth, proxy, or dev)", mode)
	}

	// Behind a reverse proxy that terminates the public hostname (e.g. Azure
	// Front Door), the browser's Origin is the public host but r.Host is the
	// forwarded origin host, so the CSRF guard needs the public origin(s). The
	// OAuth redirect URL's host is exactly that public origin; TRUSTED_ORIGINS
	// adds any extra hosts (and covers proxy auth mode, which has no redirect).
	opts.TrustedOrigins = trustedOrigins(os.Getenv("OAUTH_REDIRECT_URL"), os.Getenv("TRUSTED_ORIGINS"))
	return nil
}

// trustedOrigins collects the public origins the CSRF guard should accept: the
// host of the OAuth redirect URL when set, plus a comma/space separated
// TRUSTED_ORIGINS list.
func trustedOrigins(redirectURL, extra string) []string {
	var out []string
	if redirectURL != "" {
		if u, err := url.Parse(redirectURL); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return append(out, splitList(extra)...)
}

// splitList parses a comma or whitespace separated list, dropping blanks.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	var out []string
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
