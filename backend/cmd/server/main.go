// Command server serves the dashboard's pre-computed JSON over HTTP for the
// Kubernetes-native deploy mode. It serves the same /data/*.json contract the
// static Pages site reads, plus /api/capabilities so the frontend can light up
// server-only features. The static Pages mode keeps working unchanged.
//
// Admin-gated interactive features are enabled when -project-dir is set and
// AUTH_MODE selects an auth mechanism. ANALYSIS_CHAT_ENABLED enables read-only
// chat. ACTIONS_ENABLED controls GitHub writes and defaults off when any
// read-only interactive feature is enabled, otherwise on. BOT_TOKEN is required
// only when write actions are enabled.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/pullrequest"
	"github.com/willie-yao/aster/backend/internal/ai/modules/sharedfailure"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/chatfix"
	"github.com/willie-yao/aster/backend/internal/corrections"
	"github.com/willie-yao/aster/backend/internal/credentialenv"
	"github.com/willie-yao/aster/backend/internal/ghpr"

	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prescalation"
	"github.com/willie-yao/aster/backend/internal/project"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/server"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/storage"
)

var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	credentialenv.SanitizeAndReport()
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
	opts.Capabilities.Engine = server.EngineInfo{Version: version, Commit: commit, ImageTag: imageTag}

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
	if waiter, ok := opts.PullRequestEscalation.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for pull request escalations: %v", err)
		}
	}
	if waiter, ok := opts.SharedFailureEscalation.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for shared failure escalations: %v", err)
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

func formatPricingRate(value string) string {
	rate, ok := new(big.Rat).SetString(value)
	if !ok {
		return value
	}
	return rate.FloatString(2)
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
	usageRecorder, err := analysisruntime.NewUsageRecorder(dataDir, output.AIUsageServerFilename, cfg)
	if err != nil {
		return fmt.Errorf("configuring AI usage accounting: %w", err)
	}
	opts.AIUsageEnabled = usageRecorder != nil
	if cfg.AI != nil {
		opts.AIUsageModel = cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv)).Model
		pricing := cfg.AI.EffectiveUsage().Pricing
		if pricing.Currency != "" {
			table, priceErr := aiusage.NewPriceTable(aiusage.Rates{
				Currency: pricing.Currency, InputPerMillion: pricing.InputPerMillion,
				CachedInputPerMillion: pricing.CachedInputPerMillion, CacheWriteInputPerMillion: pricing.CacheWriteInputPerMillion,
				OutputPerMillion: pricing.OutputPerMillion,
			})
			if priceErr != nil {
				return fmt.Errorf("configuring AI usage pricing: %w", priceErr)
			}
			opts.AIUsagePricing = table
			parts := []string{fmt.Sprintf("%s input=%s", pricing.Currency, formatPricingRate(pricing.InputPerMillion)), "cached_input=" + formatPricingRate(pricing.CachedInputPerMillion)}
			if pricing.CacheWriteInputPerMillion != "" {
				parts = append(parts, "cache_write_input="+formatPricingRate(pricing.CacheWriteInputPerMillion))
			}
			parts = append(parts, "output="+formatPricingRate(pricing.OutputPerMillion), "per million tokens")
			opts.AIUsagePricingRule = strings.Join(parts, " ")
		}
	}
	opts.TrustedOrigins = trustedOrigins(os.Getenv("OAUTH_REDIRECT_URL"), os.Getenv("TRUSTED_ORIGINS"))
	var actionService *actions.Service
	if features.Actions {
		actionService, err = enableActions(ctx, opts, cfg, dataDir, usageRecorder)
		if err != nil {
			return err
		}
	}
	var chatService *analysischat.Service
	if features.AnalysisChat {
		chatService, err = enableAnalysisChat(ctx, opts, cfg, projectDir, dataDir, usageRecorder)
		if err != nil {
			return err
		}
	}
	if features.PullRequestEscalation {
		if err := enablePullRequestEscalation(ctx, opts, cfg, projectDir, dataDir); err != nil {
			return err
		}
	}
	if actionService != nil && chatService != nil {
		analysisRepo := cfg.EffectiveAnalysisSourceRepo()
		fixConfig := cfg.EffectiveFixPRs()
		exactEnabled := exactJUnitChatFixEnabled(fixConfig) && !opts.DisableFixActions
		if exactEnabled && fixConfig.Repo != nil && strings.EqualFold(analysisRepo.Owner, fixConfig.Repo.Owner) && strings.EqualFold(analysisRepo.Name, fixConfig.Repo.Name) {
			if exactEnabled {
				if err := chatService.ConfigureTestFixPreflight(actionService.PreflightAnalysisFixSource); err != nil {
					return fmt.Errorf("configuring exact JUnit Fix source preflight: %w", err)
				}
			}
			bridge := chatfix.NewService(chatService, actionService)
			opts.ChatFix = bridge
			actionService.ConfigureAnalysisPreviewValidator(bridge)
			opts.Capabilities.Features.JUnitChatFix = exactEnabled
			opts.Capabilities.Features.ChatFixMinConfidence = fixConfig.MinConfidence
			log.Printf("🛠️ analysis chat fix previews enabled (exact_junit=%t)", exactEnabled)
		} else {
			log.Printf("🛠️ analysis chat fix previews disabled: no compatible runtime or source and fix repositories differ")
		}
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

func exactJUnitChatFixEnabled(fixConfig project.FixPRs) bool {
	return fixConfig.Enabled && fixConfig.AgentRuntime != nil && fixConfig.AgentRuntime.Type == "agent-sandbox"
}

func fixActionsEnabled(fixConfig project.FixPRs, authMode string) (bool, error) {
	if !fixConfig.Enabled || fixConfig.AgentRuntime == nil {
		return false, nil
	}
	if fixConfig.Verify != nil && fixConfig.Verify.Enabled {
		trusted, err := engineruntime.TrustedLocalRuntimeEnabled()
		if err != nil {
			return false, err
		}
		if !trusted {
			return false, fmt.Errorf("ai.fix_prs.verify requires %s=true on a trusted development or CI host", engineruntime.TrustedLocalRuntimeEnv)
		}
	}
	return fixConfig.AgentRuntime.Type == "" || fixConfig.AgentRuntime.Type == "agent-sandbox", nil
}

type interactiveFeatures struct {
	Actions               bool
	AnalysisChat          bool
	AnalysisCorrections   bool
	PullRequestEscalation bool
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
	pullRequestEscalation, err := optionalBoolEnv("PULL_REQUEST_ESCALATION_ENABLED", false)
	if err != nil {
		return interactiveFeatures{}, err
	}
	actions, err := optionalBoolEnv("ACTIONS_ENABLED", !chat && !pullRequestEscalation)
	if err != nil {
		return interactiveFeatures{}, err
	}
	return interactiveFeatures{
		Actions: actions, AnalysisChat: chat, AnalysisCorrections: correctionsEnabled,
		PullRequestEscalation: pullRequestEscalation,
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
			return fmt.Errorf("OAUTH_SCOPE is no longer supported; OAuth login uses read:user and BOT_TOKEN performs writes")
		}
		if strings.TrimSpace(os.Getenv("OAUTH_PRIVATE_REPOSITORIES")) != "" {
			return fmt.Errorf("OAUTH_PRIVATE_REPOSITORIES is no longer supported; grant repository access to BOT_TOKEN instead")
		}
		botToken := os.Getenv("BOT_TOKEN")
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("oauth auth mode requires BOT_TOKEN when actions are enabled")
		}
		o, err := auth.NewOAuth(auth.OAuthConfig{
			ClientID:      os.Getenv("OAUTH_CLIENT_ID"),
			ClientSecret:  os.Getenv("OAUTH_CLIENT_SECRET"),
			RedirectURL:   os.Getenv("OAUTH_REDIRECT_URL"),
			Scope:         "read:user",
			WriteToken:    botToken,
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

func enableActions(ctx context.Context, opts *server.Options, cfg *project.Config, dataDir string, usageRecorder *aiusage.Recorder) (*actions.Service, error) {
	provider := cfg.ResolveAIProvider(os.Getenv("AI_API"), os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv(project.AIReasoningEffortEnv))
	if err := project.ValidateAIProvider(provider); err != nil {
		return nil, err
	}
	actionService := actions.NewService(cfg, dataDir, actions.AIConfig{
		Token: os.Getenv("AI_TOKEN"), API: provider.API, Endpoint: provider.Endpoint,
		Model: provider.Model, ReasoningEffort: provider.ReasoningEffort, Headers: provider.Headers, SourceToken: os.Getenv("SOURCE_INVESTIGATION_GITHUB_TOKEN"),
		UsageRecorder: usageRecorder,
	})
	fixActions, err := fixActionsEnabled(cfg.EffectiveFixPRs(), opts.AuthMode)
	if err != nil {
		return nil, err
	}
	actionService.ConfigureFixActions(fixActions)
	opts.DisableFixActions = !fixActions
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

func enableAnalysisChat(ctx context.Context, opts *server.Options, cfg *project.Config, projectDir, dataDir string, usageRecorder *aiusage.Recorder) (*analysischat.Service, error) {
	timeout, err := analysisChatTimeoutFromEnv()
	if err != nil {
		return nil, err
	}
	serviceOpts, err := analysisChatServiceOptionsFromEnv(dataDir, timeout)
	if err != nil {
		return nil, err
	}
	serviceOpts.UsageRecorder = usageRecorder
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("analysis chat requires AI_TOKEN")
	}
	projectRuntime, err := analysisruntime.LoadProject(projectDir, cfg, analysisruntime.ProviderFallbacks{
		API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"), ReasoningEffort: os.Getenv(project.AIReasoningEffortEnv),
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
	sourceRepo := cfg.EffectiveAnalysisSourceRepo()
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: sourceRepo.Owner, Name: sourceRepo.Name}); err != nil {
		return nil, fmt.Errorf("configuring analysis chat source repository: %w", err)
	}
	opts.AnalysisChat = service
	opts.AnalysisChatTimeout = timeout
	log.Printf("💬 analysis chat enabled (state=%s ttl=%s)", serviceOpts.StateDir, serviceOpts.SessionTTL)
	return service, nil
}

func analysisChatTimeoutFromEnv() (time.Duration, error) {
	const maxTimeout = 30 * time.Minute
	timeout := analysischat.DefaultTurnTimeout
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

// enablePullRequestEscalation wires on-demand analysis for pull request
// failures the deterministic pass could not explain. It reuses the analysis
// runtime, so it needs the same AI configuration as any other analysis.
func enablePullRequestEscalation(
	ctx context.Context,
	opts *server.Options,
	cfg *project.Config,
	projectDir, dataDir string,
) error {
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		return fmt.Errorf("pull request escalation requires AI_TOKEN")
	}
	if cfg.PullRequests == nil || !cfg.PullRequests.Enabled {
		return fmt.Errorf("pull request escalation requires pull_requests.enabled in project.yaml")
	}
	loaded, err := analysisruntime.LoadProject(projectDir, cfg, analysisruntime.ProviderFallbacks{
		API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"),
		ReasoningEffort: os.Getenv(project.AIReasoningEffortEnv),
		CacheGeneration: os.Getenv(project.AICacheGenerationEnv),
	})
	if err != nil {
		return fmt.Errorf("loading pull request escalation project: %w", err)
	}
	runtime, err := analysisruntime.New(ctx, analysisruntime.Options{
		Token: token, DataDir: dataDir, Project: loaded,
	})
	if err != nil {
		return fmt.Errorf("configuring pull request escalation runtime: %w", err)
	}
	backend, err := storage.New(cfg.StorageConfig(), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("configuring pull request escalation storage: %w", err)
	}
	repo := cfg.Branding.SourceRepo
	githubToken := githubReadTokenFromEnv()
	resolver := &prescalation.DataResolver{
		DataDir: dataDir, Backend: backend,
		Repo:  repo.Owner + "/" + repo.Name,
		Owner: repo.Owner, Name: repo.Name,
		Lister:          escalationChangedFiles{client: ghpr.NewClient(nil, githubToken)},
		CacheGeneration: loaded.CacheGenerationFingerprint,
	}
	runner := &prescalation.AnalysisRunner{
		NewAnalyzer: func(subject pullrequest.Subject) (ai.FailureAnalyzer, error) {
			return runtime.NewService(analysisruntime.ServiceOptions{
				Backend:         backend,
				GitHubReadToken: githubToken,
				Module:          pullrequest.New(subject),
			})
		},
	}
	// Both escalation kinds share one analysis slot, so the server runs a
	// single analysis at a time rather than one per kind.
	gate := prescalation.NewGate(1)
	service, err := prescalation.New(ctx, resolver, runner, prescalation.Options[prescalation.Ref]{
		Store: prescalation.FileStore[prescalation.Ref]{Dir: dataDir, Name: prescalation.StateFileName},
		Gate:  gate,
	})
	if err != nil {
		return fmt.Errorf("configuring pull request escalation: %w", err)
	}
	opts.PullRequestEscalation = service
	log.Printf("🔬 pull request escalation enabled (repo=%s/%s)", repo.Owner, repo.Name)

	clusterResolver := &prescalation.ClusterResolver{
		DataDir: dataDir, Backend: backend,
		Repo:            repo.Owner + "/" + repo.Name,
		CacheGeneration: loaded.CacheGenerationFingerprint,
	}
	clusterRunner := &prescalation.ClusterAnalysisRunner{
		NewAnalyzer: func(subject sharedfailure.Subject) (ai.FailureAnalyzer, error) {
			return runtime.NewService(analysisruntime.ServiceOptions{
				Backend:         backend,
				GitHubReadToken: githubToken,
				Module:          sharedfailure.New(subject),
			})
		},
	}
	clusterService, err := prescalation.New(ctx, clusterResolver, clusterRunner,
		prescalation.Options[prescalation.ClusterRef]{
			Store: prescalation.FileStore[prescalation.ClusterRef]{
				Dir: dataDir, Name: prescalation.ClusterStateFileName,
			},
			Gate: gate,
			// A finished shared failure analysis is only revalidated when a
			// request reaches Start, so status reads need their own way to
			// notice the evidence build has moved.
			CurrentEvidence: clusterResolver.CurrentEvidence,
		})
	if err != nil {
		// Pull request escalation is already wired and useful on its own, so a
		// shared failure service that cannot start withholds only its own
		// controls rather than taking both down.
		log.Printf("⚠ Shared failure escalation is unavailable: %v", err)
		return nil
	}
	opts.SharedFailureEscalation = clusterService
	log.Printf("🔬 shared failure escalation enabled (repo=%s/%s)", repo.Owner, repo.Name)
	return nil
}

// escalationChangedFiles adapts the GitHub client to the resolver's contract,
// keeping prescalation free of a GitHub client dependency.
type escalationChangedFiles struct{ client *ghpr.Client }

func (e escalationChangedFiles) ChangedFiles(ctx context.Context, owner, repo string, number int) (prescalation.ChangedFileSet, error) {
	set, err := e.client.ChangedFiles(ctx, owner, repo, number)
	if err != nil {
		return prescalation.ChangedFileSet{}, err
	}
	out := prescalation.ChangedFileSet{Truncated: set.FilesTruncated}
	// The head the diff describes lets the resolver notice a force-push that
	// landed after the dashboard published this pull request.
	if pull, err := e.client.GetPullRequest(ctx, owner, repo, number); err == nil {
		out.HeadSHA = pull.Head.SHA
	}
	for _, file := range set.Files {
		out.Files = append(out.Files, prescalation.ChangedFile{
			Path: file.Path, Status: file.Status, Generated: file.Generated, Patch: file.Patch,
		})
	}
	return out, nil
}

// githubReadTokenFromEnv mirrors the fetcher's token preference order.
func githubReadTokenFromEnv() string {
	for _, name := range []string{"GITHUB_READ_TOKEN", "BOT_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			return token
		}
	}
	return ""
}
