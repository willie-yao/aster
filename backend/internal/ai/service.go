package ai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
)

// Service orchestrates AI analysis for a single project. It composes a generic
// API Client with the universal prompt builder, the composed system prompt, and
// a snapshot of consecutive failure counts. Every failure is analyzed by the
// agentic tool-calling loop; there is no other path.
type Service struct {
	client         *Client
	module         Module
	systemPrompt   string
	consecutiveMap map[string]int

	// agenticOpts is the resolved agentic config.
	agenticOpts AgenticOptions

	// browserFactory provides per-build Browser instances.
	browserFactory artifacts.Factory

	// registry + enabledTools define which tools the agentic loop can call.
	registry     *tools.Registry
	enabledTools []string

	// skillSet is the merged engine and consumer recipe set.
	skillSet *skills.Set

	// toolCaches memoizes a *tools.Cache per buildPrefix so all failures
	// of one build share expensive tier-2 discovery results.
	toolCaches sync.Map // map[string]*tools.Cache

	// toolsUnsupported is set after the first agentic call that returns
	// ErrToolsUnsupported, so subsequent failures in the run skip straight
	// to "unavailable" instead of re-hitting an endpoint that can't do
	// function-calling.
	toolsUnsupported atomic.Bool

	// sourceRepoOwner/Name identify the project's own GitHub repo for resolving
	// repo-relative file citations. Empty until SetSourceRepo.
	sourceRepoOwner string
	sourceRepoName  string

	// patternRepo is the source-tree reader that grounds the recurring-pattern
	// agent's repotree tools. nil disables tool grounding, leaving the tool-free
	// correlation call plus the path-verification guard.
	patternRepo tools.RepoReader

	// patternTreeMu guards the per-run pattern memos below. patternTree caches
	// the source repo's blob paths (one recursive listing per run) for the path
	// guard; patternCache is the tools.Cache shared across pattern tool loops so
	// repotree fetches the tree and files once across jobs.
	patternTreeMu   sync.Mutex
	patternTree     []string
	patternTreeErr  error
	patternTreeDone bool
	patternCache    *tools.Cache

	// linkVerifyCache memoizes GitHub file-existence checks across all
	// analyses in a run, keyed by "owner/repo/path" to existence.
	linkVerifyCache sync.Map

	// traceStore collects private, sanitized per-analysis control flow.
	traceStore *TraceStore
}

// NewService constructs a Service. systemPrompt is the full composed prompt and
// must be non-empty. consecutiveMap is keyed by consecutiveKey and may be nil.
func NewService(client *Client, module Module, systemPrompt string, consecutiveMap map[string]int) *Service {
	if consecutiveMap == nil {
		consecutiveMap = map[string]int{}
	}
	return &Service{
		client:         client,
		module:         module,
		systemPrompt:   systemPrompt,
		consecutiveMap: consecutiveMap,
	}
}

// EnableAgentic installs the agentic loop's runtime dependencies: resolved
// options, per-build browser factory, tool registry, and enabled tool set.
// Must be called once at fetcher startup before Analyze.
//
// Safe to call once at Service construction; not safe for concurrent use.
func (s *Service) EnableAgentic(opts AgenticOptions, factory artifacts.Factory, registry *tools.Registry, enabledTools []string) {
	s.agenticOpts = opts
	s.browserFactory = factory
	s.registry = registry
	s.enabledTools = enabledTools
}

// SetSkills installs the merged diagnostic recipe set. Safe to call once
// during fetcher startup, after EnableAgentic. The agentic loop honors the
// set only when critique is enabled, because recipes feed the critique gate.
func (s *Service) SetSkills(set *skills.Set) {
	s.skillSet = set
}

// SetSourceRepo records branding.source_repo for resolving repo-relative file
// citations. Safe to call once at fetcher startup.
func (s *Service) SetSourceRepo(owner, name string) {
	s.sourceRepoOwner = owner
	s.sourceRepoName = name
}

// SetPatternRepoReader installs the source-tree reader that grounds the
// recurring-pattern agent. When set, AnalyzePattern runs a repotree tool loop
// so the model verifies file and config paths against the real repo before
// naming them. Safe to call once at fetcher startup.
func (s *Service) SetPatternRepoReader(reader tools.RepoReader) {
	s.patternRepo = reader
}

// SetTraceStore enables private per-analysis trace collection.
func (s *Service) SetTraceStore(store *TraceStore) {
	s.traceStore = store
}

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case
// using the shared single-failure contract.
func (s *Service) Analyze(ctx context.Context, httpClient *http.Client, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase) {
	result, _ := s.AnalyzeFailure(ctx, httpClient, FailureAnalysisRequest{
		JobID:               jobID,
		BuildPrefix:         buildPrefix,
		Build:               run.BuildInfo,
		TestCase:            *tc,
		ConsecutiveFailures: s.consecutiveMap[consecutiveKey(jobID, tc.Name)],
	})
	tc.AISummary = result.Summary
	tc.AIAnalysis = result.Analysis
}

func (s *Service) analyze(ctx context.Context, httpClient *http.Client, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, consecutiveFailures int) error {
	var trace *TraceSession
	if s.traceStore != nil {
		trace = s.traceStore.Start(TraceMetadata{
			JobID: jobID, BuildID: run.BuildID, TestName: tc.Name, APIMode: s.client.APIMode(),
		})
		ctx = withAnalysisTrace(ctx, trace)
	}
	if tc.AISummary != nil && tc.AIAnalysis != nil && !s.shouldReanalyze(tc) && !staleTransientVerdict(tc, consecutiveFailures) {
		recordTrace(ctx, TraceEvent{Kind: "cache", Outcome: "build_hit"})
		trace.Finish("build_cache_hit", nil)
		return nil
	}

	log.Printf("  🔍 Analyzing: %s [%s]", tc.Name, AgenticMode)

	failureSignal := evidenceplan.FailureSignal(*tc)
	userPrompt := s.module.AnalysisPrompt(ctx, httpClient, run, tc, consecutiveFailures)

	// Surface endpoints without function-calling as unavailable. There is no
	// tools-free analysis path to degrade to.
	if s.toolsUnsupported.Load() {
		err := fmt.Errorf("AI endpoint requires function-calling support")
		s.setUnavailable(tc, err)
		trace.Finish("unavailable", err)
		return err
	}
	summary, analysis, err := s.runAgentic(ctx, jobID, buildPrefix, run, tc, userPrompt, failureSignal, consecutiveFailures)
	if err != nil {
		if errors.Is(err, ErrToolsUnsupported) {
			s.toolsUnsupported.Store(true)
			log.Printf("  ⚠ AI endpoint rejected tools; analysis unavailable: %v", err)
			unavailableErr := fmt.Errorf("AI endpoint requires function-calling support: %w", err)
			s.setUnavailable(tc, unavailableErr)
			trace.Finish("unavailable", err)
			return unavailableErr
		}
		log.Printf("  ⚠ Agentic AI analysis failed for %s: %v", tc.Name, err)
		s.setUnavailable(tc, err)
		trace.Finish("error", err)
		return err
	}
	tc.AISummary = summary
	tc.AIAnalysis = analysis
	if analysis != nil {
		analysis.FileLinks = s.resolveFileLinks(ctx, httpClient, tc)
	}
	if analysis != nil && analysis.CacheHit {
		recordTrace(ctx, TraceEvent{Kind: "cache", Outcome: "ai_hit"})
		trace.Finish("ai_cache_hit", nil)
	} else {
		trace.Finish("success", nil)
	}
	return nil
}

// runAgentic does the per-failure agentic call setup. Kept separate so
// Analyze stays readable.
func (s *Service) runAgentic(ctx context.Context, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, userPrompt, failureSignal string, consecutiveFailures int) (*models.AISummary, *models.AIAnalysis, error) {
	if s.browserFactory == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no browser factory configured")
	}
	if s.registry == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no tool registry configured")
	}
	browser := s.browserFactory.ForBuild(buildPrefix, run.JobName+"/"+run.BuildID)
	cache := s.toolCacheFor(buildPrefix)
	cacheKey := s.agenticCacheKey(jobID, run.BuildID, tc.Name, tc.FailureMessage)
	in := AgenticInputs{
		Browser:             browser,
		Opts:                s.agenticOpts,
		Registry:            s.registry,
		EnabledTools:        s.enabledTools,
		Cache:               cache,
		WebURLBase:          run.WebURL,
		Mode:                AgenticMode,
		Skills:              s.skillSet,
		ConsecutiveFailures: consecutiveFailures,
		FailureSignal:       failureSignal,
	}
	return s.client.doAnalyzeAgentic(ctx, in, cacheKey, s.systemPrompt, userPrompt)
}

// toolCacheFor returns the *tools.Cache scoped to one build, creating it
// lazily on first use. Caches live for one fetcher run.
func (s *Service) toolCacheFor(buildPrefix string) *tools.Cache {
	if existing, ok := s.toolCaches.Load(buildPrefix); ok {
		return existing.(*tools.Cache)
	}
	fresh := tools.NewCache()
	actual, _ := s.toolCaches.LoadOrStore(buildPrefix, fresh)
	return actual.(*tools.Cache)
}

func (s *Service) setUnavailable(tc *models.TestCase, err error) {
	// Overwrite only an engine-written "unavailable" placeholder with no model
	// analysis attached. Errored failures are re-analyzed on every run, so stale
	// endpoint outage or misconfiguration errors must not persist after the
	// cause changes. Real summaries and transient classifications are preserved.
	if tc.AISummary != nil && (tc.AIAnalysis != nil || !isUnavailableSummary(tc.AISummary)) {
		return
	}
	tc.AISummary = &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		// This summary is published in jobs/*.json. A transport error embeds the
		// full request URL (the hidden AI endpoint), so strip URLs before it is
		// serialized.
		Summary:     unavailablePrefix + redact.URLs(err.Error()),
		IsTransient: false,
	}
}

// unavailablePrefix marks a summary the engine wrote because analysis could
// not complete and no model result exists.
const unavailablePrefix = "AI analysis unavailable: "

// isUnavailableSummary reports whether a later run should replace an
// engine-written "unavailable" placeholder.
func isUnavailableSummary(s *models.AISummary) bool {
	return s != nil && !s.IsTransient && strings.HasPrefix(s.Summary, unavailablePrefix)
}

// staleTransientVerdict reports whether a cached transient verdict must be
// re-analyzed because the failure has since become persistent. A transient
// flake that recurs across transientPersistThreshold consecutive builds is
// contradicted by the critique gate, so a cached transient answer produced when
// the failure was not yet persistent must not keep being served.
func staleTransientVerdict(tc *models.TestCase, consecutiveFailures int) bool {
	return tc.AISummary != nil && tc.AISummary.IsTransient && consecutiveFailures >= transientPersistThreshold
}

// shouldReanalyze returns true when a cached analysis must be discarded
// because it predates the single agentic path or fails any current quality gate.
func (s *Service) shouldReanalyze(tc *models.TestCase) bool {
	if tc.AIAnalysis.Mode != AgenticMode {
		return true
	}
	return s.belowCurrentAgenticFloor(tc)
}

// belowCurrentAgenticFloor returns true when the cached analysis fails any
// current quality gate: tool-call or GCS-byte floor, critique-not-passed when
// critique is on, critique version older than the engine's current version, or
// skill-set hash mismatch.
func (s *Service) belowCurrentAgenticFloor(tc *models.TestCase) bool {
	if tc.AIAnalysis.ToolCalls < s.agenticOpts.MinToolCalls {
		return true
	}
	if tc.AIAnalysis.GCSBytes < s.agenticOpts.MinGCSBytes {
		return true
	}
	if !tc.AIAnalysis.CritiquePassed {
		return true
	}
	if tc.AIAnalysis.CritiqueVersion < currentCritiqueVersion {
		return true
	}
	// Invalidate entries whose SkillSetHash doesn't match the loaded set so
	// engine or consumer recipe edits trigger re-analysis. Skills feed the
	// critique gate, so the hash is part of the contract. Empty wantHash matches an
	// entry stamped with no recipes.
	wantHash := ""
	if s.skillSet != nil {
		wantHash = s.skillSet.Hash()
	}
	if tc.AIAnalysis.SkillSetHash != wantHash {
		return true
	}
	// A model or endpoint swap invalidates the entry so a new model does not
	// serve the prior model's cached verdict.
	if s.client != nil && tc.AIAnalysis.ModelHash != s.client.modelFingerprint() {
		return true
	}
	// The prompt is always sent to the model, so prompt edits invalidate the
	// entry without a critique dependency.
	if tc.AIAnalysis.PromptHash != PromptFingerprint(s.systemPrompt) {
		return true
	}
	return false
}

// agenticCacheKey scopes agentic results by job+build because the model's
// answer cites build-specific artifact paths and line numbers.
func (s *Service) agenticCacheKey(jobID, buildID, testName, failureMessage string) string {
	return AgenticCacheKey(s.module.Name(), jobID, buildID, testName, failureMessage)
}

// AgenticCacheKey returns the stable per-failure cache key.
func AgenticCacheKey(moduleName, jobID, buildID, testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	return fmt.Sprintf("agentic:%s:%s:%s:%x", moduleName, jobID, buildID, hash)
}

// consecutiveKey scopes consecutive-failure counts by JobID + test name so
// same-named tests in different jobs do not share streaks.
func consecutiveKey(jobID, testName string) string {
	return jobID + "::" + testName
}

// failureHash builds the deterministic hash used by both cache key flavors.
func failureHash(testName, failureMessage string) []byte {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return h[:8]
}
