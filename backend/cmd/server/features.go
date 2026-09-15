package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/pullrequest"
	"github.com/willie-yao/aster/backend/internal/ai/modules/sharedfailure"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/chatfix"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/project"
	prescalation "github.com/willie-yao/aster/backend/internal/pullrequest/escalation"
	"github.com/willie-yao/aster/backend/internal/server"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/storage"
)

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
		opts.AIUsageModel = strings.TrimSpace(os.Getenv("AI_MODEL"))
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

			bridge := chatfix.NewService(chatService, actionService)
			opts.ChatFix = bridge
			opts.Capabilities.Features.JUnitChatFix = exactEnabled
			log.Printf("🛠️ analysis chat fix previews enabled (exact_junit=%t)", exactEnabled)
		} else {
			log.Printf("🛠️ analysis chat fix previews disabled: no compatible runtime or source and fix repositories differ")
		}
	}
	return nil
}

func enableActions(ctx context.Context, opts *server.Options, cfg *project.Config, dataDir string, usageRecorder *aiusage.Recorder) (*actions.Service, error) {
	normalized := modelprovider.Normalize(modelprovider.Config{
		API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"),
		ReasoningEffort: modelprovider.ReasoningEffort(os.Getenv(project.AIReasoningEffortEnv)),
	})
	provider := project.AIProvider{
		API: normalized.API, Endpoint: normalized.Endpoint, Model: normalized.Model, ReasoningEffort: normalized.ReasoningEffort,
	}
	if cfg.AI != nil {
		provider.ServiceTier = strings.ToLower(strings.TrimSpace(cfg.AI.ServiceTier))
		provider.Headers = cfg.AI.Headers
	}
	if err := project.ValidateAIProvider(provider); err != nil {
		return nil, err
	}
	actionService := actions.NewService(cfg, dataDir, actions.AIConfig{
		Token: os.Getenv("AI_TOKEN"), API: provider.API, Endpoint: provider.Endpoint,
		Model: provider.Model, ReasoningEffort: provider.ReasoningEffort, Headers: provider.Headers, SourceToken: os.Getenv("SOURCE_INVESTIGATION_GITHUB_TOKEN"),
		UsageRecorder: usageRecorder,
	})
	fixActions := fixActionsEnabled(cfg.EffectiveFixPRs())
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
	projectRuntime, err := analysisruntime.LoadProject(projectDir, cfg, analysisruntime.DeploymentConfig{
		API: os.Getenv("AI_API"), Endpoint: os.Getenv("AI_ENDPOINT"), Model: os.Getenv("AI_MODEL"), ReasoningEffort: os.Getenv(project.AIReasoningEffortEnv),
		CacheGeneration: os.Getenv(project.AICacheGenerationEnv),
	})
	if err != nil {
		return nil, fmt.Errorf("loading analysis chat project: %w", err)
	}
	maxOutputTokens := 0
	if raw := strings.TrimSpace(os.Getenv("AI_MAX_OUTPUT_TOKENS")); raw != "" {
		maxOutputTokens, err = strconv.Atoi(raw)
		if err != nil || maxOutputTokens < 0 {
			return nil, fmt.Errorf("AI_MAX_OUTPUT_TOKENS must be a non-negative integer")
		}
	}
	runtime, err := analysisruntime.New(context.Background(), analysisruntime.Options{
		Token: token, GitHubReadToken: githubReadTokenFromEnv(), DataDir: dataDir, Project: projectRuntime, MaxOutputTokens: maxOutputTokens,
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
	preparedGeneration := analysischat.PreparedCauseGeneration(runtime.AnalysisChatContractFingerprint())
	if err := service.ConfigurePreparedCauseFindings(preparedGeneration); err != nil {
		return nil, fmt.Errorf("configuring prepared cause findings: %w", err)
	}
	opts.AnalysisChat = service
	opts.AnalysisChatTimeout = timeout
	log.Printf("💬 analysis chat enabled (state=%s ttl=%s)", serviceOpts.StateDir, serviceOpts.SessionTTL)
	return service, nil
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
	loaded, err := analysisruntime.LoadProject(projectDir, cfg, analysisruntime.DeploymentConfig{
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
// keeping pullrequest/escalation free of a GitHub client dependency.
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
