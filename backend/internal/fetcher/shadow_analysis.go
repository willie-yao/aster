package fetcher

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysispublisher"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/fixruntime"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type shadowAnalysisRunner interface {
	Analyze(context.Context, agentanalysis.WorkspaceSandboxSpec) (agentanalysis.WorkspaceSandboxResult, error)
	RuntimeIdentity() string
}

type shadowWorkspacePreparer func(context.Context, ai.FailureAnalysisRequest, sourceinvestigation.Repository, agentanalysis.WorkspacePreparationOptions) (agentanalysis.WorkspacePreparedInput, error)
type shadowWorkspaceCleaner func(string, string, string) error
type shadowWorkspacePublisher interface {
	Publish(context.Context, agentanalysis.WorkspacePublishRequest, string) (analysispublisher.Result, error)
	Cleanup(context.Context, agentanalysis.WorkspaceCleanupRequest, string) (analysispublisher.Result, error)
}
type shadowLedgerAppender func(string, string, agentanalysis.ShadowRecord) error
type shadowLedgerClaimer func(string, string, agentanalysis.ShadowRecord) (bool, error)

const shadowInputCleanupTimeout = 2 * time.Minute

type shadowCandidate struct {
	sortKey           string
	request           ai.FailureAnalysisRequest
	source            sourceinvestigation.Repository
	subject           agentanalysis.Subject
	authoritative     agentanalysis.AuthoritativeSnapshot
	requestHash       string
	authoritativeHash string
}

func normalizeShadowAnalysisOptions(cfg *ShadowAnalysisOptions) {
	if cfg == nil {
		return
	}
	cfg.ModelProvider = modelprovider.Normalize(cfg.ModelProvider)
	cfg.LedgerPath = strings.TrimSpace(cfg.LedgerPath)
	cfg.InputRoot = strings.TrimSpace(cfg.InputRoot)
	if cfg.LedgerPath != "" {
		cfg.LedgerPath = filepath.Clean(cfg.LedgerPath)
	}
	if cfg.InputRoot != "" {
		cfg.InputRoot = filepath.Clean(cfg.InputRoot)
	}
}

func validateShadowAnalysisOptions(opts Options) error {
	cfg := opts.ShadowAnalysis
	if opts.AIMaxOutputTokens < 0 || opts.AIMaxOutputTokens > 131072 {
		return fmt.Errorf("authoritative AI max output tokens must be between 0 and 131072")
	}
	if !cfg.Enabled {
		return nil
	}
	switch {
	case !opts.EnableAI:
		return fmt.Errorf("agent analysis shadow requires -ai")
	case strings.TrimSpace(cfg.LedgerPath) == "":
		return fmt.Errorf("agent analysis shadow private ledger path is required")
	case !filepath.IsAbs(cfg.LedgerPath):
		return fmt.Errorf("agent analysis shadow private ledger path must be absolute")
	case strings.TrimSpace(cfg.InputRoot) == "":
		return fmt.Errorf("agent analysis shadow private input root is required")
	case !filepath.IsAbs(cfg.InputRoot):
		return fmt.Errorf("agent analysis shadow private input root must be absolute")
	case cfg.MaxPerRun < 1 || cfg.MaxPerRun > 10:
		return fmt.Errorf("agent analysis shadow max per run must be between 1 and 10")
	case cfg.MaxSteps < 5 || cfg.MaxSteps > 100:
		return fmt.Errorf("agent analysis shadow max steps must be between 5 and 100")
	case cfg.Timeout <= 0 || cfg.Timeout > 30*time.Minute:
		return fmt.Errorf("agent analysis shadow timeout must be greater than zero and at most 30m")
	case cfg.OutputLimitBytes < 4<<10 || cfg.OutputLimitBytes > 1<<20:
		return fmt.Errorf("agent analysis shadow output limit must be between 4096 and 1048576")
	case cfg.ModelContextTokens < 8192 || cfg.ModelContextTokens > 2_000_000:
		return fmt.Errorf("agent analysis shadow model context tokens must be between 8192 and 2000000")
	case cfg.ModelOutputTokens < 1024 || cfg.ModelOutputTokens > cfg.ModelContextTokens || cfg.ModelOutputTokens > 131072:
		return fmt.Errorf("agent analysis shadow model output tokens are invalid")
	case opts.AIMaxOutputTokens != cfg.ModelOutputTokens:
		return fmt.Errorf("agent analysis shadow model output tokens must match the explicit authoritative AI output cap")
	}
	if err := agentanalysis.ValidatePrivateLedgerPath(opts.OutDir, cfg.LedgerPath); err != nil {
		return fmt.Errorf("agent analysis shadow private ledger: %w", err)
	}
	if err := agentanalysis.ValidatePrivateInputRoot(opts.OutDir, cfg.InputRoot); err != nil {
		return fmt.Errorf("agent analysis shadow private input: %w", err)
	}
	if err := modelprovider.ValidateDeploymentEndpoint(cfg.ModelProvider); err != nil {
		return fmt.Errorf("agent analysis shadow model provider: %w", err)
	}
	if _, err := modelprovider.OpenCodeBaseURL(cfg.ModelProvider); err != nil {
		return fmt.Errorf("agent analysis shadow model provider: %w", err)
	}
	return nil
}

func validateShadowProviderParity(authoritative project.AIProvider, shadow modelprovider.Config) error {
	if len(authoritative.Headers) > 0 {
		return fmt.Errorf("agent analysis shadow does not support authoritative provider headers")
	}
	if authoritative.API != shadow.API || strings.TrimSpace(authoritative.Endpoint) != shadow.Endpoint || strings.TrimSpace(authoritative.Model) != shadow.Model || authoritative.ReasoningEffort != shadow.ReasoningEffort {
		return fmt.Errorf("agent analysis shadow provider API, endpoint, model, and reasoning effort must match authoritative analysis")
	}
	return nil
}

func validateShadowContextParity(contextTokens int) error {
	raw := strings.TrimSpace(os.Getenv("AI_CONTEXT_WINDOW_TOKENS"))
	value, err := strconv.Atoi(raw)
	if err != nil || value < 8192 || value != contextTokens {
		return fmt.Errorf("agent analysis shadow model context tokens must match AI_CONTEXT_WINDOW_TOKENS")
	}
	return nil
}

func (p *pipeline) runShadowAnalysis(ctx context.Context, result *refreshResult) {
	if p == nil || !p.opts.ShadowAnalysis.Enabled || result == nil {
		return
	}
	candidates := p.selectShadowCandidates(result.details)
	if len(candidates) == 0 {
		log.Printf("🧪 agent analysis shadow: no eligible pinned failures")
		return
	}
	claim := agentanalysis.ClaimLedgerAttempt
	if p.shadowClaim != nil {
		claim = p.shadowClaim
	}
	attempted := 0
	for _, candidate := range candidates {
		if attempted >= p.opts.ShadowAnalysis.MaxPerRun {
			break
		}
		if p.runShadowCandidate(ctx, candidate, claim) {
			attempted++
		}
	}
}

func (p *pipeline) selectShadowCandidates(details []models.JobDetail) []shadowCandidate {
	if p == nil || p.aiProject == nil || p.aiProject.Config == nil || p.aiProject.Config.AI == nil {
		return nil
	}
	// Streaks come from the runs themselves so the shadow analyzer sees the same
	// true count as the primary path, independent of the project's threshold.
	consecutive := aggregator.ConsecutiveFailureCounts(details)
	owner, name := p.aiProject.AnalysisSource.Owner, p.aiProject.AnalysisSource.Name
	var candidates []shadowCandidate
	for di := range details {
		detail := &details[di]
		jobLocation := prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo}
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			source, ok := ai.ResolveBuildSource(run.BuildInfo, owner, name)
			if !ok || len(source.Revision) != 40 {
				continue
			}
			location := prowbuild.BuildLocation{
				JobLocation: jobLocation, JobName: detail.Name,
				BuildID: run.BuildID, PullNumber: run.PullNumber,
			}
			for ti := range run.TestCases {
				testCase := &run.TestCases[ti]
				if testCase.Status != "failed" || testCase.AISummary == nil || testCase.AIAnalysis == nil || testCase.AIAnalysis.Mode != ai.AgenticMode || !ai.IsGroundedAnalysis(testCase.AIAnalysis) {
					continue
				}
				authoritative, authoritativeHash, err := agentanalysis.NewAuthoritativeSnapshot(testCase.AISummary, testCase.AIAnalysis)
				if err != nil {
					continue
				}
				subject := agentanalysis.Subject{
					JobID: detail.JobID, BuildID: run.BuildID,
					TestName: testCase.Name, TestSource: testCase.Source,
					JUnitFile: testCase.JUnitFile, SuiteName: testCase.SuiteName, ClassName: testCase.ClassName,
				}
				build := run.BuildInfo
				build.RepoRefs = maps.Clone(run.RepoRefs)
				build.JUnitURLs = slices.Clone(run.JUnitURLs)
				sortKey := strings.Join([]string{detail.JobID, run.BuildID, testCase.Source, testCase.JUnitFile, testCase.SuiteName, testCase.ClassName, testCase.Name}, "\x00")
				request := ai.FailureAnalysisRequest{
					JobID: detail.JobID, BuildPrefix: location.BuildPath(), Build: build, TestCase: *testCase,
					ProwJob: &ai.ProwJobContext{
						Name: detail.Name, JobType: detail.JobType,
						ConfigFile: detail.ConfigFile, ConfigRevision: detail.ConfigRevision,
					},
					ConsecutiveFailures: consecutive[detail.JobID+"::"+testCase.Name],
					CacheGeneration:     p.cacheGenerationFingerprint(),
				}
				candidates = append(candidates, shadowCandidate{
					sortKey: sortKey, request: request,
					source:  sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision},
					subject: subject, authoritative: authoritative, requestHash: agentanalysis.FailureRequestHash(request), authoritativeHash: authoritativeHash,
				})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sortKey < candidates[j].sortKey })
	deduped := candidates[:0]
	previous := ""
	for _, candidate := range candidates {
		if candidate.sortKey == previous {
			continue
		}
		previous = candidate.sortKey
		deduped = append(deduped, candidate)
	}
	return deduped
}

func (p *pipeline) runShadowCandidate(ctx context.Context, candidate shadowCandidate, claim shadowLedgerClaimer) bool {
	started := time.Now()
	cfg := p.opts.ShadowAnalysis
	now := time.Now
	if p.shadowNow != nil {
		now = p.shadowNow
	}
	createdAt := now().UTC()
	artifactBaseURL, artifactURLErr := p.shadowArtifactBaseURL(candidate.request.BuildPrefix)
	runner, setupErr := p.ensureShadowRunner()
	runtimeIdentity := ""
	if runner != nil {
		runtimeIdentity = runner.RuntimeIdentity()
	}
	skillSetHash := p.aiProject.SkillSet.Hash()
	effectivePromptHash := agentanalysis.EffectivePromptSHA256(p.aiProject.ConsumerPrompt)
	record := agentanalysis.ShadowRecord{
		CreatedAt: createdAt.Format(time.RFC3339Nano), Subject: candidate.subject, Source: candidate.source,
		RequestHash: candidate.requestHash, AuthoritativeHash: candidate.authoritativeHash, Authoritative: candidate.authoritative,
		Provenance: agentanalysis.Provenance{
			Runtime: "agent-sandbox-opencode", AgentNamespace: p.shadowAgentNamespace, AgentRef: p.shadowAgentRef,
			ContractVersion: agentanalysis.WorkspaceContractVersion, ToolPolicyVersion: agentanalysis.WorkspacePromptVersion,
			SkillHash: skillSetHash, SourceSHA: candidate.source.Revision, IdentityHash: runtimeIdentity,
			Timeout: cfg.Timeout.String(), MaxSteps: cfg.MaxSteps, ModelContextTokens: cfg.ModelContextTokens, ModelOutputTokens: cfg.ModelOutputTokens,
			EffectivePromptSHA256: effectivePromptHash, SkillSetHash: skillSetHash, WorkspacePromptHash: agentanalysis.WorkspaceSkillHash(), InputMode: agentanalysis.WorkspaceInputStaged,
		},
		Quality: agentanalysis.ShadowQuality{DeterministicStatus: "not_run", SemanticStatus: "unavailable", SemanticReason: "evidence_aware_semantic_judge_not_exposed"},
	}
	record.AttemptHash = agentanalysis.WorkspaceAttemptIdentity(
		candidate.subject, candidate.requestHash, candidate.authoritativeHash, skillSetHash, effectivePromptHash, candidate.source,
		runtimeIdentity, artifactBaseURL, cfg.ModelProvider, cfg.Timeout, cfg.MaxSteps, cfg.ModelContextTokens, cfg.ModelOutputTokens, cfg.OutputLimitBytes, cfg.RequireSourceEvidence,
	)
	record.ID = agentanalysis.NewRecordID(candidate.subject, createdAt, record.AttemptHash)
	claimed, ledgerErr := claim(p.opts.OutDir, cfg.LedgerPath, record)
	if ledgerErr != nil {
		log.Printf("⚠ agent analysis shadow ledger claim failed: %v", ledgerErr)
		return false
	}
	if !claimed {
		log.Printf("🧪 agent analysis shadow: exact comparison already attempted job=%s build=%s test=%s", record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return false
	}
	if setupErr != nil || artifactURLErr != nil || runner == nil || p.shadowPublisher == nil {
		record.Status, record.ErrorCode = agentanalysis.ShadowStatusSetupFailed, "runtime_setup"
		record.TotalDurationMs = time.Since(started).Milliseconds()
		p.appendShadowRecord(record)
		log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return true
	}
	browser := artifacts.NewUncachedBackendBrowser(p.backend, p.cfg.Storage.Bucket, candidate.request.BuildPrefix, candidate.request.Build.JobName+"/"+candidate.request.Build.BuildID)
	prepare := agentanalysis.PrepareWorkspaceInput
	if p.shadowPrepare != nil {
		prepare = p.shadowPrepare
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, cfg.Timeout)
	prepared, err := prepare(prepareCtx, candidate.request, candidate.source, agentanalysis.WorkspacePreparationOptions{
		PublicOutputDir: p.opts.OutDir, InputRoot: cfg.InputRoot, ConsumerPrompt: p.aiProject.ConsumerPrompt,
		SkillSet: p.aiProject.SkillSet, Browser: browser,
	})
	cancelPrepare()
	if err != nil {
		record.Status, record.ErrorCode = agentanalysis.ShadowStatusEvidenceFailed, "workspace_input"
		record.TotalDurationMs = time.Since(started).Milliseconds()
		p.appendShadowRecord(record)
		log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return true
	}
	remoteStage, err := agentanalysis.NewWorkspaceRemoteStageRequest(prepared.Manifest, artifactBaseURL, prepared.SourceModePolicy)
	if err != nil {
		p.finishShadowInputFailure(ctx, &record, prepared, started, "publisher_request", false)
		return true
	}
	publishRequest, err := agentanalysis.NewWorkspacePublishRequest(remoteStage, prepared.Manifest.Artifacts, record.ID)
	if err != nil {
		p.finishShadowInputFailure(ctx, &record, prepared, started, "publisher_request", false)
		return true
	}
	record.Provenance.PublisherRequestHash = publishRequest.Hash
	publishCtx, cancelPublish := context.WithTimeout(ctx, cfg.Timeout)
	publication, publishErr := p.shadowPublisher.Publish(publishCtx, publishRequest, record.ID)
	cancelPublish()
	record.Provenance.PublisherJob = publication.JobName
	record.Provenance.PublisherPod = publication.PodName
	record.Provenance.PublicationDurationMs = publication.Duration.Milliseconds()
	if publishErr != nil {
		p.finishShadowInputFailure(ctx, &record, prepared, started, "publisher_failed", true)
		return true
	}
	stage, err := agentanalysis.NewWorkspaceStageRequestWithSourceModePolicies(prepared.Manifest, publication.Publication, agentanalysis.WorkspaceSourceModePreserve)
	if err != nil {
		p.finishShadowInputFailure(ctx, &record, prepared, started, "stage_request", true)
		return true
	}
	request, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceEvidence(
		prepared.Manifest, agentanalysis.WorkspaceSourceModePreserve, cfg.RequireSourceEvidence, cfg.ModelProvider,
		cfg.Timeout, cfg.MaxSteps, cfg.ModelContextTokens, cfg.ModelOutputTokens, cfg.OutputLimitBytes,
	)
	if err != nil {
		p.finishShadowInputFailure(ctx, &record, prepared, started, "execution_request", true)
		return true
	}
	scan, evidence, planIDs := agentanalysis.WorkspaceEvidenceManifest(prepared.Manifest)
	record.Scan, record.Evidence, record.PlanIDs = &scan, evidence, planIDs
	record.ComparisonHash = agentanalysis.WorkspaceComparisonIdentity(record.AttemptHash, prepared.Manifest, request, stage, publishRequest.Hash)
	analysisTimeout := cfg.Timeout + agentanalysis.WorkspacePostModelGraceForSources(len(prepared.Manifest.Sources)) + 5*time.Second
	analysisCtx, cancelAnalysis := context.WithTimeout(ctx, analysisTimeout)
	generated, runErr := runner.Analyze(analysisCtx, agentanalysis.WorkspaceSandboxSpec{
		Request: request, StageRequest: stage, SourceRoot: prepared.SourceRoot, ArtifactRoot: prepared.ArtifactRoot, ExecutionID: record.ID,
	})
	cancelAnalysis()
	record.Provenance = mergeWorkspaceProvenance(record.Provenance, agentanalysis.ProvenanceFromWorkspaceResult(generated, request, stage, runtimeIdentity))
	record.Provenance.ExecutionID = record.ID
	record.CleanupWork = generated.CleanupWork
	if generated.Execution.Analysis != nil {
		analysis := *generated.Execution.Analysis
		record.Shadow = &analysis
		record.Quality = agentanalysis.EvaluateWorkspaceQuality(analysis, p.aiProject.SkillSet, candidate.request.ConsecutiveFailures)
	}
	if generated.CleanupWork == nil {
		p.cleanupShadowInputs(ctx, &record, prepared, true)
	} else {
		record.InputCleanupPending = true
		p.cleanupLocalShadowInput(&record, prepared)
	}
	record.CleanupPending = generated.CleanupWork != nil || record.InputCleanupPending
	record.Status = agentanalysis.ResolveWorkspaceShadowStatus(generated, runErr)
	if record.Status == agentanalysis.ShadowStatusSucceeded && record.InputCleanupPending {
		record.Status = agentanalysis.ShadowStatusCleanupPending
	}
	if record.Status != agentanalysis.ShadowStatusSucceeded {
		record.ErrorCode = string(record.Status)
		if record.Status == agentanalysis.ShadowStatusRuntimeFailed && generated.Telemetry.FailureCode != "" {
			record.ErrorCode = generated.Telemetry.FailureCode
		}
	}
	record.TotalDurationMs = time.Since(started).Milliseconds()
	p.appendShadowRecord(record)
	log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
	return true
}

func (p *pipeline) finishShadowInputFailure(ctx context.Context, record *agentanalysis.ShadowRecord, prepared agentanalysis.WorkspacePreparedInput, started time.Time, code string, remotePublished bool) {
	p.cleanupShadowInputs(ctx, record, prepared, remotePublished)
	record.Status, record.ErrorCode = agentanalysis.ShadowStatusEvidenceFailed, code
	if record.InputCleanupPending {
		record.Status = agentanalysis.ShadowStatusCleanupPending
	}
	record.TotalDurationMs = time.Since(started).Milliseconds()
	p.appendShadowRecord(*record)
}

func (p *pipeline) cleanupShadowInputs(_ context.Context, record *agentanalysis.ShadowRecord, prepared agentanalysis.WorkspacePreparedInput, remotePublished bool) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), shadowInputCleanupTimeout)
	defer cancel()
	remoteClean := !remotePublished
	if remotePublished {
		request, err := agentanalysis.NewWorkspaceCleanupRequest(prepared.Manifest.Hash, record.ID)
		if err == nil && p.shadowPublisher != nil {
			result, cleanupErr := p.shadowPublisher.Cleanup(cleanupCtx, request, record.ID)
			record.Provenance.CleanupJob = result.JobName
			record.Provenance.CleanupPod = result.PodName
			record.Provenance.InputCleanupDurationMs = result.Duration.Milliseconds()
			remoteClean = cleanupErr == nil
		}
	}
	localClean := p.cleanupLocalShadowInput(record, prepared)
	record.Provenance.InputCleanupCompleted = remoteClean && localClean
	record.InputCleanupPending = !record.Provenance.InputCleanupCompleted
}

func (p *pipeline) cleanupLocalShadowInput(record *agentanalysis.ShadowRecord, prepared agentanalysis.WorkspacePreparedInput) bool {
	cleanup := agentanalysis.CleanupWorkspaceInput
	if p.shadowCleanup != nil {
		cleanup = p.shadowCleanup
	}
	return cleanup(p.opts.OutDir, p.opts.ShadowAnalysis.InputRoot, prepared.Manifest.Hash) == nil
}

func mergeWorkspaceProvenance(existing, runtime agentanalysis.Provenance) agentanalysis.Provenance {
	runtime.PublisherRequestHash = existing.PublisherRequestHash
	runtime.PublisherJob = existing.PublisherJob
	runtime.PublisherPod = existing.PublisherPod
	runtime.PublicationDurationMs = existing.PublicationDurationMs
	return runtime
}

// shadowSandboxEnvPrefix reserves the Agent Sandbox environment for the private
// shadow analysis workload, keeping it distinct from the fix and critic runners.
const shadowSandboxEnvPrefix = "AGENT_SANDBOX_ANALYSIS_SHADOW_"

func (p *pipeline) ensureShadowRunner() (shadowAnalysisRunner, error) {
	cfg := p.opts.ShadowAnalysis
	if p.shadowRunner == nil {
		agent, err := fixruntime.NewAgentSandboxProviderRunnerFromEnv(shadowSandboxEnvPrefix, cfg.ModelProvider, cfg.Timeout, cfg.OutputLimitBytes)
		if err != nil {
			return nil, err
		}
		p.shadowAgentNamespace, p.shadowAgentRef = agent.Namespace(), agent.RuntimeIdentity()
		p.shadowRunner = &agentanalysis.WorkspaceSandboxRuntime{
			Sandbox: agent, Provider: cfg.ModelProvider, SourceModePolicy: agentanalysis.WorkspaceSourceModePreserve,
			Timeout: cfg.Timeout, OutputLimitBytes: cfg.OutputLimitBytes,
		}
	}
	if p.shadowPublisher == nil {
		publisher, err := analysispublisher.NewFromEnv(shadowSandboxEnvPrefix, cfg.Timeout)
		if err != nil {
			return nil, err
		}
		p.shadowPublisher = publisher
	}
	return p.shadowRunner, nil
}

func (p *pipeline) shadowArtifactBaseURL(buildPrefix string) (string, error) {
	storageConfig := p.cfg.StorageConfig()
	var base *url.URL
	switch storageConfig.Provider {
	case storage.ProviderGCS:
		base = &url.URL{Scheme: "https", Host: "storage.googleapis.com", Path: "/" + storageConfig.Bucket}
	case storage.ProviderGCSWeb:
		parsed, err := url.Parse(strings.TrimRight(storageConfig.Base, "/"))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("agent analysis shadow gcsweb base is invalid")
		}
		base = parsed
		base.Path = strings.TrimRight(base.Path, "/") + "/" + storageConfig.Bucket
	default:
		return "", fmt.Errorf("agent analysis shadow requires gcs or gcsweb artifact storage")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.Trim(buildPrefix, "/")
	if configured := strings.TrimSpace(os.Getenv(shadowSandboxEnvPrefix + "STAGING_FQDNS")); configured != "" {
		allowed := map[string]bool{}
		for _, value := range strings.Split(configured, ",") {
			allowed[strings.ToLower(strings.TrimSpace(value))] = true
		}
		if !allowed["github.com"] || !allowed[strings.ToLower(base.Hostname())] {
			return "", fmt.Errorf("agent analysis shadow staging FQDN policy does not cover source and artifact hosts")
		}
	}
	return strings.TrimRight(base.String(), "/"), nil
}

func (p *pipeline) appendShadowRecord(record agentanalysis.ShadowRecord) {
	appendLedger := agentanalysis.AppendLedger
	if p.shadowAppend != nil {
		appendLedger = p.shadowAppend
	}
	if err := appendLedger(p.opts.OutDir, p.opts.ShadowAnalysis.LedgerPath, record); err != nil {
		log.Printf("⚠ agent analysis shadow ledger write failed: %v", err)
	}
}
