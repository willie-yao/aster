// Package analysisruntime wires the dashboard-owned failure analysis runtime.
package analysisruntime

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/universal"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	gcsByteBudget              = 1_000_000_000
	analysisChatGCSByteBudget  = 128 << 20
	analysisChatMaxIters       = 8
	analysisChatDefaultTimeout = 2 * time.Minute
)

// ProviderFallbacks are used when project.yaml omits provider fields.
type ProviderFallbacks struct {
	API      string
	Endpoint string
	Model    string
}

// Project holds the AI configuration shared by every analysis in one project.
type Project struct {
	Config           *project.Config
	Provider         project.AIProvider
	SystemPrompt     string
	ConsumerPrompt   string
	SkillSet         *skills.Set
	ProfileSelection skills.ProfileSelection
}

// LoadProject loads and validates the dashboard-owned AI configuration.
func LoadProject(projectDir string, cfg *project.Config, fallbacks ProviderFallbacks) (*Project, error) {
	if cfg == nil {
		var err error
		cfg, err = project.Load(filepath.Join(projectDir, "project.yaml"))
		if err != nil {
			return nil, fmt.Errorf("loading project config: %w", err)
		}
	}
	provider := cfg.ResolveAIProvider(fallbacks.API, fallbacks.Endpoint, fallbacks.Model)
	if err := project.ValidateAIAPI(provider.API); err != nil {
		return nil, err
	}
	if provider.Endpoint == "" || provider.Model == "" {
		return nil, fmt.Errorf("AI is enabled but no provider is configured: set ai.endpoint and ai.model in project.yaml, or the AI_ENDPOINT and AI_MODEL env vars")
	}
	prompt, err := project.LoadPrompt(projectDir)
	if err != nil {
		return nil, fmt.Errorf("loading AI prompt: %w", err)
	}
	set, selection, err := skills.LoadForTools(projectDir, cfg.AI.EffectiveAgentic().Tools)
	if err != nil {
		return nil, fmt.Errorf("loading AI skills: %w", err)
	}
	return &Project{
		Config:           cfg,
		Provider:         provider,
		SystemPrompt:     ai.ComposeSystemPrompt(prompt),
		ConsumerPrompt:   prompt,
		SkillSet:         set,
		ProfileSelection: selection,
	}, nil
}

// Options configure reusable model and Tool runtime state.
type Options struct {
	Token   string
	DataDir string
	Project *Project
}

// Runtime holds reusable model, cache, budget, and Tool registry state.
type Runtime struct {
	Client              *ai.Client
	Registry            *tools.Registry
	EnabledTools        []string
	ModelByteBudget     int
	ContextByteBudget   int
	ContextWindowTokens int
	RequestTokenBudget  int
	Project             *Project
}

// New creates the reusable dashboard analysis runtime.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Project == nil || opts.Project.Config == nil {
		return nil, fmt.Errorf("analysis project configuration is required")
	}
	client := ai.NewClientWithOptions(ai.Options{
		Token:        opts.Token,
		CacheDir:     opts.DataDir,
		API:          opts.Project.Provider.API,
		Endpoint:     opts.Project.Provider.Endpoint,
		Model:        opts.Project.Provider.Model,
		ExtraHeaders: opts.Project.Provider.Headers,
	})
	budgets, contextSource, err := resolveContextBudgets(ctx, client)
	if err != nil {
		return nil, err
	}
	switch contextSource {
	case "operator":
		log.Printf("🪟 operator context window override: %d tokens; request_token_budget=%d, reserved_tokens=%d, model_byte_budget=%d KB",
			budgets.ContextWindowTokens, budgets.RequestTokenBudget, budgets.ContextWindowTokens-budgets.RequestTokenBudget, budgets.ModelByteBudget/1024)
	case "detected":
		log.Printf("🪟 detected context window: %d tokens; request_token_budget=%d, reserved_tokens=%d, model_byte_budget=%d KB",
			budgets.ContextWindowTokens, budgets.RequestTokenBudget, budgets.ContextWindowTokens-budgets.RequestTokenBudget, budgets.ModelByteBudget/1024)
	default:
		log.Printf("🪟 context window unavailable; using bounded fallback: %d tokens, request_token_budget=%d",
			budgets.ContextWindowTokens, budgets.RequestTokenBudget)
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	toolNames := opts.Project.Config.AI.EffectiveAgentic().Tools
	if len(toolNames) == 0 {
		toolNames = []string{"filesystem", "k8s"}
	}
	enabled, err := registry.Enable(toolNames)
	if err != nil {
		return nil, fmt.Errorf("AI tool configuration: %w", err)
	}
	return &Runtime{
		Client:              client,
		Registry:            registry,
		EnabledTools:        enabled,
		ModelByteBudget:     budgets.ModelByteBudget,
		ContextByteBudget:   budgets.ContextByteBudget,
		ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget:  budgets.RequestTokenBudget,
		Project:             opts.Project,
	}, nil
}

// resolveContextBudgets prefers an operator-provided total window over
// endpoint metadata, then uses the bounded fallback when neither is available.
func resolveContextBudgets(ctx context.Context, client *ai.Client) (ai.ContextBudgets, string, error) {
	overrideTokens, overridden, err := ai.ParseContextWindowTokens(os.Getenv("AI_CONTEXT_WINDOW_TOKENS"))
	if err != nil {
		return ai.ContextBudgets{}, "", err
	}
	if overridden {
		return ai.DeriveContextBudgets(overrideTokens), "operator", nil
	}
	if tokens, detected := client.DetectContextWindowTokens(ctx); detected {
		return ai.DeriveContextBudgets(tokens), "detected", nil
	}
	return ai.DeriveContextBudgets(0), "fallback", nil
}

// ServiceOptions configure one dashboard analysis Service.
type ServiceOptions struct {
	Backend             storage.Backend
	ConsecutiveFailures map[string]int
	TraceStore          *ai.TraceStore
	GitHubReadToken     string
}

type analysisChatBrowserFactory struct {
	backend storage.Backend
	bucket  string
}

func (f analysisChatBrowserFactory) ForBuild(buildPrefix, displayName string) artifacts.Browser {
	return artifacts.NewUncachedBackendBrowser(f.backend, f.bucket, buildPrefix, displayName)
}

// NewAnalysisChatAgent creates the interactive read-only conversation runner.
func (r *Runtime) NewAnalysisChatAgent(backend storage.Backend) (*ai.AnalysisChatAgent, error) {
	if r == nil || r.Project == nil || r.Project.Config == nil || r.Client == nil {
		return nil, fmt.Errorf("analysis runtime is not initialized")
	}
	if backend == nil {
		return nil, fmt.Errorf("analysis chat storage backend is required")
	}
	effective := r.Project.Config.AI.EffectiveAgentic()
	maxIters := min(effective.MaxIters, analysisChatMaxIters)
	if maxIters <= 0 {
		maxIters = analysisChatMaxIters
	}
	timeout := effective.Timeout
	if timeout <= 0 || timeout > analysisChatDefaultTimeout {
		timeout = analysisChatDefaultTimeout
	}
	return ai.NewAnalysisChatAgent(
		r.Client,
		ai.ComposeAnalysisChatSystemPrompt(r.Project.ConsumerPrompt),
		r.Registry,
		r.EnabledTools,
		analysisChatBrowserFactory{backend: backend, bucket: r.Project.Config.Storage.Bucket},
		ai.AnalysisChatOptions{
			MaxIters: maxIters, ModelByteBudget: r.ModelByteBudget,
			GCSByteBudget: analysisChatGCSByteBudget, ContextByteBudget: r.ContextByteBudget,
			Timeout: timeout, SingleToolCall: effective.SingleToolCall,
		},
	)
}

// NewService creates the canonical dashboard analysis service.
func (r *Runtime) NewService(opts ServiceOptions) (*ai.Service, error) {
	if r == nil || r.Project == nil || r.Project.Config == nil || r.Client == nil {
		return nil, fmt.Errorf("analysis runtime is not initialized")
	}
	if opts.Backend == nil {
		return nil, fmt.Errorf("analysis storage backend is required")
	}
	cfg := r.Project.Config
	service := ai.NewService(r.Client, universal.New(), r.Project.SystemPrompt, opts.ConsecutiveFailures)
	if opts.TraceStore != nil {
		service.SetTraceStore(opts.TraceStore)
	}
	service.SetSourceRepo(cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name)
	if cfg.Branding.SourceRepo.Owner != "" && cfg.Branding.SourceRepo.Name != "" && opts.GitHubReadToken != "" {
		service.SetPatternRepoReader(ai.NewGitHubRepoReader(
			cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name, "", opts.GitHubReadToken))
		log.Printf("🔎 Pattern agent grounded on %s/%s source tree",
			cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name)
	}

	eff := cfg.AI.EffectiveAgentic()
	factory := artifacts.NewBackendFactory(opts.Backend, cfg.Storage.Bucket)
	service.EnableAgentic(ai.AgenticOptions{
		MaxIters:            eff.MaxIters,
		ModelByteBudget:     r.ModelByteBudget,
		GCSByteBudget:       gcsByteBudget,
		Timeout:             eff.Timeout,
		ContextByteBudget:   r.ContextByteBudget,
		ContextWindowTokens: r.ContextWindowTokens,
		RequestTokenBudget:  r.RequestTokenBudget,
		MinToolCalls:        eff.MinToolCalls,
		MinGCSBytes:         eff.MinGCSBytes,
		CritiqueMaxRetries:  *eff.Critique.MaxRetries,
		SingleToolCall:      eff.SingleToolCall,
		SemanticJudge:       true,
	}, factory, r.Registry, r.EnabledTools)
	service.SetSkills(r.Project.SkillSet)
	return service, nil
}

// SaveCache persists the dashboard AI cache when DataDir is configured.
func (r *Runtime) SaveCache() error {
	if r == nil || r.Client == nil {
		return nil
	}
	return r.Client.Cache().Save()
}

// LogConfiguration writes the non-sensitive runtime summary.
func (r *Runtime) LogConfiguration() {
	if r == nil || r.Project == nil || r.Project.Config == nil {
		return
	}
	eff := r.Project.Config.AI.EffectiveAgentic()
	skillsLog := "off"
	if r.Project.SkillSet != nil && len(r.Project.SkillSet.Skills()) > 0 {
		skillsLog = fmt.Sprintf("on/%d", len(r.Project.SkillSet.Skills()))
	}
	log.Printf("🤖 Agentic AI enabled (%d iters, %dKB model, %dMB gcs, %s timeout, min_tools=%d, min_gcs_kb=%d, critique=on/%d, skills=%s, tools=%v)",
		eff.MaxIters, r.ModelByteBudget/1024, gcsByteBudget/1024/1024, eff.Timeout, eff.MinToolCalls, eff.MinGCSBytes/1024, *eff.Critique.MaxRetries, skillsLog, r.EnabledTools)
	if os.Getenv("AI_LOG_ENDPOINT") == "1" {
		log.Printf("Using AI endpoint: %s, model: %s", r.Client.Endpoint(), r.Client.ModelName())
	} else {
		log.Printf("AI client configured (set AI_LOG_ENDPOINT=1 to log endpoint and model)")
	}
}

// ShortHash returns a short hash prefix for startup logs.
func ShortHash(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}
