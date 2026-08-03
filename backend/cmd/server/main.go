// Command server serves the dashboard's pre-computed JSON over HTTP for the
// Kubernetes-native deploy mode. It serves the same /data/*.json contract the
// static Pages site reads, plus /api/capabilities so the frontend can light up
// server-only features. The static Pages mode keeps working unchanged.
//
// Admin-gated interactive features are enabled when -project-dir is set and
// AUTH_MODE selects an auth mechanism. ANALYSIS_CHAT_ENABLED enables read-only
// chat. ACTIONS_ENABLED controls GitHub writes and defaults off when chat is
// enabled, otherwise on for backward compatibility. BOT_TOKEN is required only
// when write actions are enabled.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/chatfix"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/corrections"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/server"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
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
	flag.StringVar(&projectDir, "project-dir", "", "project.yaml directory; enables admin features when set with AUTH_MODE")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hstsEnabled, err := optionalBoolEnv("HSTS_ENABLED", false)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	opts := server.Options{
		DataDir:      dataDir,
		StaticDir:    staticDir,
		Capabilities: server.DefaultCapabilities(),
		HSTSEnabled:  hstsEnabled,
	}

	// Enable admin-gated features only when a project config and an auth mode are
	// both provided. Otherwise the server stays read-only.
	if projectDir != "" && os.Getenv("AUTH_MODE") != "" {
		if err := enableInteractiveFeatures(ctx, &opts, projectDir, dataDir); err != nil {
			log.Fatalf("server: enabling interactive features: %v", err)
		}
		log.Printf("🔐 admin features enabled (auth mode: %s)", opts.AuthMode)
	} else {
		log.Println("interactive features disabled (set -project-dir and AUTH_MODE to enable)")
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
	if waiter, ok := opts.AnalysisChat.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for analysis chat turns: %v", err)
		}
	}
	if waiter, ok := opts.Actions.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for action requests: %v", err)
		}
	}
}

// enableInteractiveFeatures loads the project config and authenticated services.
func enableInteractiveFeatures(ctx context.Context, opts *server.Options, projectDir, dataDir string) error {
	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	features, err := interactiveFeaturesFromEnv()
	if err != nil {
		return err
	}
	if err := configureAuthenticator(opts, features.Actions); err != nil {
		return err
	}
	opts.TrustedOrigins = trustedOrigins(os.Getenv("OAUTH_REDIRECT_URL"), os.Getenv("TRUSTED_ORIGINS"))
	var actionService *actions.Service
	if features.Actions {
		actionService, err = enableActions(ctx, opts, cfg, dataDir)
		if err != nil {
			return err
		}
	}
	var chatService *analysischat.Service
	if features.AnalysisChat {
		chatService, err = enableAnalysisChat(ctx, opts, cfg, projectDir, dataDir)
		if err != nil {
			return err
		}
	}
	if features.SourceInvestigation {
		if err := enableSourceInvestigation(opts, cfg, chatService); err != nil {
			return err
		}
	}
	if actionService != nil && chatService != nil {
		opts.ChatFix = chatfix.NewService(chatService, actionService)
		opts.Capabilities.Features.ChatFixMinConfidence = cfg.EffectiveFixPRs().MinConfidence
		log.Printf("🛠️ analysis chat fix previews enabled")
	}
	if features.AnalysisCorrections {
		correctionService, err := corrections.NewService(dataDir, chatService, corrections.Options{})
		if err != nil {
			return fmt.Errorf("configuring analysis corrections: %w", err)
		}
		opts.AnalysisCorrections = correctionService
		log.Printf("📝 analysis correction promotion enabled")
	}
	return nil
}

type interactiveFeatures struct {
	Actions             bool
	AnalysisChat        bool
	AnalysisCorrections bool
	SourceInvestigation bool
}

func interactiveFeaturesFromEnv() (interactiveFeatures, error) {
	chat, err := optionalBoolEnv("ANALYSIS_CHAT_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	correctionsEnabled, err := optionalBoolEnv("ANALYSIS_CORRECTIONS_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	if correctionsEnabled && !chat {
		return interactiveFeatures{}, fmt.Errorf("ANALYSIS_CORRECTIONS_ENABLED requires ANALYSIS_CHAT_ENABLED")
	}
	sourceEnabled, err := optionalBoolEnv("ANALYSIS_SOURCE_INVESTIGATION_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	if sourceEnabled && !chat {
		return interactiveFeatures{}, fmt.Errorf("ANALYSIS_SOURCE_INVESTIGATION_ENABLED requires ANALYSIS_CHAT_ENABLED")
	}
	actions, err := optionalBoolEnv("ACTIONS_ENABLED", !chat)
	if err != nil {
		return interactiveFeatures{}, err
	}
	return interactiveFeatures{
		Actions: actions, AnalysisChat: chat, AnalysisCorrections: correctionsEnabled,
		SourceInvestigation: sourceEnabled,
	}, nil
}

func optionalBoolEnv(name string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	return parsed, nil
}

func configureAuthenticator(opts *server.Options, actionsEnabled bool) error {
	admins := splitList(os.Getenv("ADMIN_LOGINS"))
	switch mode := os.Getenv("AUTH_MODE"); mode {
	case "oauth":
		if strings.TrimSpace(os.Getenv("OAUTH_SCOPE")) != "" {
			return fmt.Errorf("OAUTH_SCOPE is no longer supported; use OAUTH_PRIVATE_REPOSITORIES=true for private action targets")
		}
		privateRepositories, err := optionalBoolEnv("OAUTH_PRIVATE_REPOSITORIES", false)
		if err != nil {
			return err
		}
		if privateRepositories && !actionsEnabled {
			return fmt.Errorf("OAUTH_PRIVATE_REPOSITORIES requires actions to be enabled")
		}
		scope := "read:user"
		if actionsEnabled {
			scope = "public_repo"
			if privateRepositories {
				scope = "repo"
				log.Printf("⚠️ OAuth private-repository access enabled; requesting the broad GitHub repo scope")
			}
		}
		o, err := auth.NewOAuth(auth.OAuthConfig{
			ClientID:      os.Getenv("OAUTH_CLIENT_ID"),
			ClientSecret:  os.Getenv("OAUTH_CLIENT_SECRET"),
			RedirectURL:   os.Getenv("OAUTH_REDIRECT_URL"),
			Scope:         scope,
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
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("proxy auth mode requires BOT_TOKEN when actions are enabled")
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
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("dev auth mode requires BOT_TOKEN when actions are enabled")
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
	return nil
}

func enableActions(ctx context.Context, opts *server.Options, cfg *project.Config, dataDir string) (*actions.Service, error) {
	provider := cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"))
	if err := project.ValidateAIAPI(provider.API); err != nil {
		return nil, err
	}
	actionService := actions.NewService(cfg, dataDir, actions.AIConfig{
		Token: os.Getenv("AI_TOKEN"), API: provider.API, Endpoint: provider.Endpoint,
		Model: provider.Model, Headers: provider.Headers, SourceToken: os.Getenv("SOURCE_INVESTIGATION_GITHUB_TOKEN"),
	})
	opts.Actions = actionService
	if value := os.Getenv("ACTION_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("invalid ACTION_TIMEOUT %q: %w", value, err)
		}
		opts.ActionTimeout = timeout
	}
	requestTimeout := opts.ActionTimeout
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Minute
	}
	actionService.ConfigureAsyncRequestsWithContext(ctx, requestTimeout, actionRequestNotifier(cfg))
	return actionService, nil
}

func enableAnalysisChat(ctx context.Context, opts *server.Options, cfg *project.Config, projectDir, dataDir string) (*analysischat.Service, error) {
	timeout, err := analysisChatTimeoutFromEnv()
	if err != nil {
		return nil, err
	}
	serviceOpts, err := analysisChatServiceOptionsFromEnv(dataDir, timeout)
	if err != nil {
		return nil, err
	}
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("analysis chat requires AI_TOKEN")
	}
	projectRuntime, err := analysisruntime.LoadProject(projectDir, cfg, analysisruntime.ProviderFallbacks{
		API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"),
		CacheGeneration: os.Getenv(project.AICacheGenerationEnv),
	})
	if err != nil {
		return nil, fmt.Errorf("loading analysis chat project: %w", err)
	}
	runtime, err := analysisruntime.New(context.Background(), analysisruntime.Options{
		Token: token, DataDir: dataDir, Project: projectRuntime,
	})
	if err != nil {
		return nil, fmt.Errorf("configuring analysis chat runtime: %w", err)
	}
	backend, err := storage.New(cfg.StorageConfig(), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("configuring analysis chat storage: %w", err)
	}
	agent, err := runtime.NewAnalysisChatAgentWithTimeout(backend, timeout)
	if err != nil {
		return nil, fmt.Errorf("configuring analysis chat agent: %w", err)
	}
	service, err := analysischat.NewService(ctx, dataDir, agent, serviceOpts)
	if err != nil {
		return nil, err
	}
	opts.AnalysisChat = service
	opts.AnalysisChatTimeout = timeout
	log.Printf("💬 analysis chat enabled (state=%s ttl=%s)", serviceOpts.StateDir, serviceOpts.SessionTTL)
	return service, nil
}

func sourceInvestigationKubeContext() string {
	return os.Getenv("ORKA_KUBE_CONTEXT")
}

func enableSourceInvestigation(
	opts *server.Options,
	cfg *project.Config,
	chatService *analysischat.Service,
) error {
	if chatService == nil {
		return fmt.Errorf("source investigation requires analysis chat")
	}
	if cfg.AI == nil || cfg.AI.SourceInvestigation == nil {
		return fmt.Errorf("source investigation requires ai.source_investigation in project.yaml")
	}
	runtimeConfig := cfg.EffectiveSourceInvestigation()
	timeout, err := time.ParseDuration(runtimeConfig.Timeout)
	if err != nil {
		return fmt.Errorf("configuring source investigation timeout: %w", err)
	}
	runner, err := orka.NewSourceInvestigatorFromEnv(orka.SourceInvestigationFromEnvConfig{
		Namespace:   runtimeConfig.Namespace,
		AgentRef:    runtimeConfig.AgentRef,
		API:         runtimeConfig.API,
		GitSecret:   runtimeConfig.GitSecret,
		Version:     runtimeConfig.Version,
		MaxRetries:  *runtimeConfig.Retries,
		MaxTurns:    runtimeConfig.MaxTurns,
		KubeContext: sourceInvestigationKubeContext(),
		GitHubToken: os.Getenv("SOURCE_INVESTIGATION_GITHUB_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("configuring source investigation runtime: %w", err)
	}
	serviceOpts := analysischat.SourceInvestigationOptions{
		Timeout: timeout, LeaseTTL: timeout + 30*time.Second, MaxPerSession: 8, MaxActivePerOwner: 1,
	}
	serviceOpts.MaxPerSession, err = positiveIntEnv("ANALYSIS_SOURCE_INVESTIGATION_MAX_PER_SESSION", serviceOpts.MaxPerSession)
	if err != nil {
		return err
	}
	serviceOpts.MaxActivePerOwner, err = positiveIntEnv("ANALYSIS_SOURCE_INVESTIGATION_MAX_ACTIVE_PER_OWNER", serviceOpts.MaxActivePerOwner)
	if err != nil {
		return err
	}
	sourceRepo := cfg.EffectiveAnalysisSourceRepo()
	if err := chatService.ConfigureSourceInvestigation(runner, sourceinvestigation.Repository{
		Owner: sourceRepo.Owner, Name: sourceRepo.Name,
	}, serviceOpts); err != nil {
		return fmt.Errorf("configuring source investigation service: %w", err)
	}
	opts.SourceInvestigation = chatService
	opts.SourceInvestigationTimeout = timeout
	log.Printf("🔎 source investigation enabled (repo=%s/%s timeout=%s)", sourceRepo.Owner, sourceRepo.Name, timeout)
	return nil
}

func analysisChatTimeoutFromEnv() (time.Duration, error) {
	const maxTimeout = 30 * time.Minute
	timeout := 2 * time.Minute
	if value := os.Getenv("ANALYSIS_CHAT_TIMEOUT"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("invalid ANALYSIS_CHAT_TIMEOUT %q: %w", value, err)
		}
		timeout = parsed
	}
	if timeout <= 0 || timeout > maxTimeout {
		return 0, fmt.Errorf("ANALYSIS_CHAT_TIMEOUT must be greater than zero and at most %s", maxTimeout)
	}
	return timeout, nil
}

func analysisChatServiceOptionsFromEnv(dataDir string, timeout time.Duration) (analysischat.Options, error) {
	opts := analysischat.Options{
		StateDir:                     strings.TrimSpace(os.Getenv("ANALYSIS_CHAT_STATE_DIR")),
		SessionTTL:                   2 * time.Hour,
		MaxSessions:                  128,
		MaxSessionsPerOwner:          8,
		TurnLeaseTTL:                 timeout + 30*time.Second,
		TurnTimeout:                  timeout,
		MaxActiveTurnsPerOwner:       2,
		MaxRequestsPerOwnerPerMinute: 10,
	}
	if opts.StateDir == "" {
		opts.StateDir = filepath.Join(dataDir, ".analysis-chat")
	}
	if value := os.Getenv("ANALYSIS_CHAT_SESSION_TTL"); value != "" {
		ttl, err := time.ParseDuration(value)
		if err != nil {
			return analysischat.Options{}, fmt.Errorf("invalid ANALYSIS_CHAT_SESSION_TTL %q: %w", value, err)
		}
		if ttl <= 0 {
			return analysischat.Options{}, fmt.Errorf("ANALYSIS_CHAT_SESSION_TTL must be greater than zero")
		}
		opts.SessionTTL = ttl
	}
	var err error
	opts.MaxSessions, err = positiveIntEnv("ANALYSIS_CHAT_MAX_SESSIONS", opts.MaxSessions)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxSessionsPerOwner, err = positiveIntEnv("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER", opts.MaxSessionsPerOwner)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxActiveTurnsPerOwner, err = positiveIntEnv("ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER", opts.MaxActiveTurnsPerOwner)
	if err != nil {
		return analysischat.Options{}, err
	}
	opts.MaxRequestsPerOwnerPerMinute, err = positiveIntEnv("ANALYSIS_CHAT_REQUESTS_PER_MINUTE", opts.MaxRequestsPerOwnerPerMinute)
	if err != nil {
		return analysischat.Options{}, err
	}
	if opts.MaxSessionsPerOwner > opts.MaxSessions {
		return analysischat.Options{}, fmt.Errorf("ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER cannot exceed ANALYSIS_CHAT_MAX_SESSIONS")
	}
	return opts, nil
}

func positiveIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func actionRequestNotifier(cfg *project.Config) actions.RequestReadyNotifier {
	email, enabled := cfg.EffectiveEmailNotifications()
	if !enabled || !email.ActionLinks {
		return nil
	}
	password := os.Getenv("EMAIL_SMTP_PASSWORD")
	if email.SMTP.Username != "" && password == "" {
		log.Println("async action emails disabled (EMAIL_SMTP_PASSWORD is unset in the server)")
		return nil
	}
	from, recipients, err := notify.ParseAddresses(email.From, email.To)
	if err != nil {
		log.Printf("async action emails disabled: %v", err)
		return nil
	}
	sender, err := notify.NewSMTPSender(notify.SMTPConfig{
		Host: email.SMTP.Host, Port: email.SMTP.Port, Username: email.SMTP.Username,
		Password: password, TLSMode: email.SMTP.TLS,
	})
	if err != nil {
		log.Printf("async action emails disabled: %v", err)
		return nil
	}
	baseURL := strings.TrimRight(cfg.Branding.SiteURL, "/")
	return func(ctx context.Context, request actions.ActionRequestView) error {
		title := "draft"
		if request.Preview != nil && request.Preview.Title != "" {
			title = request.Preview.Title
		}
		message := notify.ActionDraftReadyMessage(notify.ActionDraftReady{
			From: from, To: recipients, Project: cfg.Name, Owner: request.Owner,
			RequestID: request.ID, Kind: request.Kind, Title: title,
			ReviewURL: baseURL + "/action-request/" + url.PathEscape(request.ID),
		})
		return sender.Send(ctx, message)
	}
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
