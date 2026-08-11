package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const agentSandboxAnalyzerBenchmarkRecordVersion = 1

type agentSandboxAnalyzerBenchmarkConfig struct {
	KubeContext    string
	SourceRoot     string
	ProjectDir     string
	ResultsPath    string
	PreparedPath   string
	ArmLabel       string
	ModelLabel     string
	ProviderPath   string
	TransportID    string
	EngineCommit   string
	Gateway        engineruntime.ModelGatewayConfig
	Timeout        time.Duration
	OutputLimit    int64
	MaxSteps       int
	Repetitions    int
	RepetitionBase int
	PrepareOnly    bool
}

type agentSandboxAnalyzerPrepared struct {
	Version                int      `json:"version"`
	CaseID                 string   `json:"case_id"`
	StableID               string   `json:"stable_id"`
	EngineCommit           string   `json:"engine_commit"`
	FixtureSHA256          string   `json:"fixture_sha256"`
	BaselineConsumerCommit string   `json:"baseline_consumer_commit"`
	BaselinePromptSHA256   string   `json:"baseline_prompt_sha256"`
	ProjectSHA256          string   `json:"project_sha256"`
	SourceRevision         string   `json:"source_revision"`
	SourceRoot             string   `json:"source_root"`
	ArtifactRoot           string   `json:"artifact_root"`
	ManifestHash           string   `json:"manifest_hash"`
	RequestHash            string   `json:"request_hash"`
	StageHash              string   `json:"stage_hash"`
	ArtifactFiles          int      `json:"artifact_files"`
	ArtifactBytes          int64    `json:"artifact_bytes"`
	ArtifactPaths          []string `json:"artifact_paths"`
	WorkspacePromptHash    string   `json:"workspace_prompt_hash"`
	ModelLabel             string   `json:"model_label"`
	ArmLabel               string   `json:"arm_label"`
}

type agentSandboxAnalyzerCitation struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Verified  bool   `json:"verified,omitempty"`
}

type agentSandboxAnalyzerBenchmarkRecord struct {
	Version                 int                            `json:"version"`
	CaseID                  string                         `json:"case_id"`
	StableID                string                         `json:"stable_id"`
	Repetition              int                            `json:"repetition"`
	Runtime                 string                         `json:"runtime"`
	Arm                     string                         `json:"arm"`
	ModelLabel              string                         `json:"model_label"`
	EngineCommit            string                         `json:"engine_commit"`
	FixtureSHA256           string                         `json:"fixture_sha256"`
	BaselineConsumerCommit  string                         `json:"baseline_consumer_commit"`
	BaselinePromptSHA256    string                         `json:"baseline_prompt_sha256"`
	ProjectSHA256           string                         `json:"project_sha256"`
	ProviderPath            string                         `json:"provider_path"`
	TransportID             string                         `json:"transport_id"`
	APIMode                 string                         `json:"api_mode"`
	EvidenceCondition       string                         `json:"evidence_condition"`
	JobName                 string                         `json:"job_name"`
	BuildID                 string                         `json:"build_id"`
	TestName                string                         `json:"test_name"`
	TestSource              string                         `json:"test_source"`
	ContractVersion         string                         `json:"contract_version"`
	WorkspacePromptHash     string                         `json:"workspace_prompt_hash"`
	ManifestHash            string                         `json:"manifest_hash"`
	RequestHash             string                         `json:"request_hash"`
	RuntimeIdentityHash     string                         `json:"runtime_identity_hash"`
	ExecutionID             string                         `json:"execution_id"`
	SourceRevision          string                         `json:"source_revision"`
	ArtifactFiles           int                            `json:"artifact_files"`
	ArtifactBytes           int64                          `json:"artifact_bytes"`
	Status                  string                         `json:"status"`
	ErrorCode               string                         `json:"error_code,omitempty"`
	FailureReason           string                         `json:"failure_reason,omitempty"`
	ElapsedMS               int64                          `json:"elapsed_ms"`
	RuntimeDurationMS       int64                          `json:"runtime_duration_ms"`
	TaskFinalized           bool                           `json:"task_finalized"`
	TaskFinalizedMS         int64                          `json:"task_finalized_ms,omitempty"`
	ResultAvailable         bool                           `json:"result_available"`
	ResultAvailableMS       int64                          `json:"result_available_ms,omitempty"`
	FinalizationChecked     bool                           `json:"finalization_checked"`
	FinalizationValid       bool                           `json:"finalization_valid"`
	CleanupCompleted        bool                           `json:"cleanup_completed"`
	CleanupDurationMS       int64                          `json:"cleanup_duration_ms,omitempty"`
	AnalysisValid           bool                           `json:"analysis_valid"`
	ArtifactCitationCount   int                            `json:"artifact_citation_count"`
	SourceCitationCount     int                            `json:"source_citation_count"`
	SourceVerified          bool                           `json:"source_verified"`
	EvidenceCitations       []agentSandboxAnalyzerCitation `json:"evidence_citations,omitempty"`
	SourceCitations         []agentSandboxAnalyzerCitation `json:"source_citations,omitempty"`
	SignalHits              int                            `json:"signal_hits"`
	SignalTotal             int                            `json:"signal_total"`
	DiagnosisSignalHits     int                            `json:"diagnosis_signal_hits"`
	DiagnosisSignalTotal    int                            `json:"diagnosis_signal_total"`
	TransientCorrect        *bool                          `json:"transient_classification_correct,omitempty"`
	ForbiddenChecksPassed   int                            `json:"forbidden_checks_passed"`
	ForbiddenChecksTotal    int                            `json:"forbidden_checks_total"`
	MissingMust             []string                       `json:"missing_must,omitempty"`
	IsTransient             *bool                          `json:"is_transient,omitempty"`
	Summary                 string                         `json:"summary,omitempty"`
	RootCause               string                         `json:"root_cause,omitempty"`
	SuggestedFix            string                         `json:"suggested_fix,omitempty"`
	Severity                string                         `json:"severity,omitempty"`
	RelevantFiles           []string                       `json:"relevant_files,omitempty"`
	UnresolvedDetails       []string                       `json:"unresolved_details,omitempty"`
	ModelRequests           int                            `json:"model_requests"`
	InputTokens             int                            `json:"input_tokens"`
	CachedInputTokens       int                            `json:"cached_input_tokens"`
	OutputTokens            int                            `json:"output_tokens"`
	CostUSD                 string                         `json:"cost_usd,omitempty"`
	TokenUsageAvailable     bool                           `json:"token_usage_available"`
	CostAvailable           bool                           `json:"cost_available"`
	UsageStatus             string                         `json:"usage_status"`
	Resources               engineruntime.ResourceMetadata `json:"resources"`
	HumanScoreRubricVersion int                            `json:"human_score_rubric_version"`
	HumanScoreMax           int                            `json:"human_score_max"`
	HumanScoreDimensions    []string                       `json:"human_score_dimensions"`
}

type agentSandboxAnalyzerPreparedCase struct {
	prepared agentSandboxAnalyzerPrepared
	request  agentanalysis.WorkspaceExecutionRequest
	stage    agentanalysis.WorkspaceStageRequest
	bc       benchCase
}

func TestAgentSandboxAnalyzerBenchmark(t *testing.T) {
	if os.Getenv("RUN_AGENT_SANDBOX_ANALYZER_BENCHMARK") == "" {
		t.Skip("set RUN_AGENT_SANDBOX_ANALYZER_BENCHMARK=1 to run the Agent Sandbox OpenCode analyzer benchmark")
	}
	cfg := loadAgentSandboxAnalyzerBenchmarkConfig(t)
	bc := agentSandboxAnalyzerBenchmarkCase(t)
	prepared := prepareAgentSandboxAnalyzerBenchmarkCase(t, cfg, bc)
	writeAgentSandboxAnalyzerPrepared(t, cfg.PreparedPath, prepared.prepared)
	if cfg.PrepareOnly {
		t.Logf("prepared analyzer input manifest %s", prepared.prepared.ManifestHash)
		return
	}
	runner, err := fixruntime.NewAgentSandboxRunnerForBenchmarkFromEnv(
		"AGENT_SANDBOX_ANALYSIS_", cfg.KubeContext, cfg.Gateway, cfg.Timeout, cfg.OutputLimit,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &agentanalysis.WorkspaceSandboxRuntime{
		Sandbox: runner, Gateway: cfg.Gateway, Timeout: cfg.Timeout, OutputLimitBytes: cfg.OutputLimit,
	}
	for index := 0; index < cfg.Repetitions; index++ {
		repetition := cfg.RepetitionBase + index
		t.Run(fmt.Sprintf("%s/rep-%02d", bc.name, repetition), func(t *testing.T) {
			runAgentSandboxAnalyzerBenchmarkTrial(t, cfg, prepared, runtime, runner, repetition)
		})
	}
}

func loadAgentSandboxAnalyzerBenchmarkConfig(t *testing.T) agentSandboxAnalyzerBenchmarkConfig {
	t.Helper()
	require := func(name string) string {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	prepareOnly := strings.TrimSpace(os.Getenv("ANALYZER_BENCH_PREPARE_ONLY")) == "1"
	gateway := engineruntime.ModelGatewayConfig{
		Endpoint:        require("AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_ENDPOINT"),
		Model:           require("AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_MODEL"),
		ProtocolVersion: require("AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_PROTOCOL"),
	}
	if err := validateBenchmarkProviderPath(require("BENCH_PROVIDER_PATH"), gateway.Model); err != nil {
		t.Fatal(err)
	}
	timeout := agentSandboxAnalyzerBenchmarkDuration(t, "AGENT_SANDBOX_ANALYSIS_TIMEOUT", 15*time.Minute)
	outputLimit := int64(agentSandboxAnalyzerBenchmarkInt(t, "AGENT_SANDBOX_ANALYSIS_OUTPUT_LIMIT_BYTES", 256<<10, 4<<10, 1<<20))
	arm := require("ANALYZER_BENCH_ARM_LABEL")
	if !benchmarkCaseIDRE.MatchString(arm) {
		t.Fatalf("ANALYZER_BENCH_ARM_LABEL must match %s", benchmarkCaseIDRE.String())
	}
	modelLabel := require("BENCH_MODEL_LABEL")
	if strings.ContainsAny(modelLabel, " /\\:@") || len(modelLabel) > 80 {
		t.Fatal("BENCH_MODEL_LABEL must be stable and anonymous")
	}
	transportID := require("BENCH_TRANSPORT_ID")
	if len(transportID) > 80 || strings.ContainsAny(transportID, " \t\r\n") {
		t.Fatal("BENCH_TRANSPORT_ID must be stable and contain no whitespace")
	}
	cfg := agentSandboxAnalyzerBenchmarkConfig{
		SourceRoot: require("ANALYZER_BENCH_SOURCE_ROOT"), ProjectDir: require("BENCH_PROJECT_DIR"),
		PreparedPath: strings.TrimSpace(os.Getenv("ANALYZER_BENCH_PREPARED_JSON")),
		ArmLabel:     arm, ModelLabel: modelLabel, ProviderPath: require("BENCH_PROVIDER_PATH"), TransportID: transportID,
		EngineCommit: benchmarkEngineCommit(t, !prepareOnly), Gateway: gateway, Timeout: timeout, OutputLimit: outputLimit,
		MaxSteps:       agentSandboxAnalyzerBenchmarkInt(t, "ANALYZER_BENCH_MAX_STEPS", 20, 1, 100),
		Repetitions:    agentSandboxAnalyzerBenchmarkInt(t, "BENCH_REPETITIONS", 1, 1, 10),
		RepetitionBase: benchmarkRepetitionStart(t), PrepareOnly: prepareOnly,
	}
	if !prepareOnly {
		cfg.KubeContext = require("ANALYZER_BENCH_KUBE_CONTEXT")
		cfg.ResultsPath = require("ANALYZER_BENCH_RESULTS_JSONL")
	}
	return cfg
}

func agentSandboxAnalyzerBenchmarkCase(t *testing.T) benchCase {
	t.Helper()
	manifest := strings.TrimSpace(os.Getenv("BENCH_MANIFEST"))
	if manifest == "" {
		t.Fatal("BENCH_MANIFEST is required")
	}
	cases, err := loadBenchmarkManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	selected := strings.TrimSpace(os.Getenv("BENCH_CASE"))
	if selected == "" {
		t.Fatal("BENCH_CASE must select exactly one pinned failure")
	}
	cases, err = selectBenchmarkCases(cases, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].stableID == "" || cases[0].consumerCommit == "" {
		t.Fatal("Agent Sandbox analyzer benchmark requires one pinned external case")
	}
	return cases[0]
}

func prepareAgentSandboxAnalyzerBenchmarkCase(t *testing.T, cfg agentSandboxAnalyzerBenchmarkConfig, bc benchCase) agentSandboxAnalyzerPreparedCase {
	t.Helper()
	if err := validateBenchmarkProjectDir(cfg.ProjectDir, bc); err != nil {
		t.Fatalf("BENCH_PROJECT_DIR=%s: %v", cfg.ProjectDir, err)
	}
	_, consumerPrompt, err := project.LoadDir(cfg.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	build := models.BuildInfo{
		BuildID: bc.buildID, JobName: bc.jobName, PullNumber: bc.pullNumber, WebURL: bc.webURL,
		Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs),
	}
	source, ok := ai.ResolveBuildSource(build, bc.sourceRepo[0], bc.sourceRepo[1])
	if !ok || len(source.Revision) != 40 {
		t.Fatal("benchmark case does not resolve one lowercase 40-character source SHA")
	}
	if err := agentanalysis.VerifySourceWorkspace(t.Context(), cfg.SourceRoot, source.Revision); err != nil {
		t.Fatalf("ANALYZER_BENCH_SOURCE_ROOT=%s: %v", cfg.SourceRoot, err)
	}
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo},
		JobName:     bc.jobName, BuildID: bc.buildID, PullNumber: bc.pullNumber,
	}
	fixtureRoot := ensureFixture(t, bc.fixtureAsset, bc.fixtureSHA256)
	artifactRoot := filepath.Join(fixtureRoot, filepath.FromSlash(loc.BuildPath()))
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatalf("snapshot benchmark artifacts %s: %v", artifactRoot, err)
	}
	request := ai.FailureAnalysisRequest{
		JobID: models.JobIDFor(bc.jobType, bc.repo, bc.jobName), BuildPrefix: loc.BuildPath(), Build: build,
		TestCase: *benchTestCase(bc), ConsecutiveFailures: bc.consecutiveFailures,
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(request, sourceinvestigation.Repository{
		Owner: source.Owner, Name: source.Name, Revision: source.Revision,
	}, consumerPrompt, files)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := agentanalysis.NewWorkspaceExecutionRequest(manifest, cfg.Gateway, cfg.Timeout, cfg.MaxSteps, cfg.OutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	projectData, err := os.ReadFile(filepath.Join(cfg.ProjectDir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var artifactBytes int64
	artifactPaths := make([]string, 0, len(files))
	for _, file := range files {
		artifactBytes += file.Size
		artifactPaths = append(artifactPaths, file.Path)
	}
	prepared := agentSandboxAnalyzerPrepared{
		Version: 1, CaseID: bc.name, StableID: bc.stableID, EngineCommit: cfg.EngineCommit,
		FixtureSHA256: bc.fixtureSHA256, BaselineConsumerCommit: bc.consumerCommit,
		BaselinePromptSHA256: bc.promptSHA256, ProjectSHA256: sha256Hex(projectData),
		SourceRevision: source.Revision, SourceRoot: filepath.Clean(cfg.SourceRoot), ArtifactRoot: artifactRoot,
		ManifestHash: manifest.Hash, RequestHash: execution.Hash, StageHash: stage.Hash,
		ArtifactFiles: len(files), ArtifactBytes: artifactBytes, ArtifactPaths: artifactPaths,
		WorkspacePromptHash: agentanalysis.WorkspaceSkillHash(), ModelLabel: cfg.ModelLabel, ArmLabel: cfg.ArmLabel,
	}
	return agentSandboxAnalyzerPreparedCase{prepared: prepared, request: execution, stage: stage, bc: bc}
}

func runAgentSandboxAnalyzerBenchmarkTrial(
	t *testing.T,
	cfg agentSandboxAnalyzerBenchmarkConfig,
	prepared agentSandboxAnalyzerPreparedCase,
	runtime *agentanalysis.WorkspaceSandboxRuntime,
	runner *fixruntime.AgentSandboxRuntime,
	repetition int,
) {
	t.Helper()
	executionID := agentSandboxAnalyzerBenchmarkExecutionID(prepared.request.Hash, runtime.RuntimeIdentity(), cfg.ArmLabel, repetition)
	started := time.Now()
	result, runErr := runtime.Analyze(t.Context(), agentanalysis.WorkspaceSandboxSpec{
		Request: prepared.request, StageRequest: prepared.stage,
		SourceRoot: prepared.prepared.SourceRoot, ArtifactRoot: prepared.prepared.ArtifactRoot,
		ExecutionID: executionID,
	})
	if result.CleanupWork != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cleanupErr := runner.Cleanup(cleanupCtx, *result.CleanupWork)
		cancel()
		if cleanupErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("cleanup retry: %w", cleanupErr))
		} else {
			result.Telemetry.CleanupCompleted = true
			result.CleanupWork = nil
			runErr = withoutCleanupPending(runErr)
		}
	}
	elapsed := time.Since(started)
	record := agentSandboxAnalyzerRecordForResult(cfg, prepared, repetition, executionID, runtime.RuntimeIdentity(), result, elapsed, runErr)
	writeAgentSandboxAnalyzerBenchmarkJSONL(t, cfg.ResultsPath, record)
	if runErr != nil {
		t.Logf("Agent Sandbox analyzer trial status=%s error=%v", record.Status, runErr)
	}
}

func agentSandboxAnalyzerRecordForResult(
	cfg agentSandboxAnalyzerBenchmarkConfig,
	prepared agentSandboxAnalyzerPreparedCase,
	repetition int,
	executionID, runtimeIdentity string,
	result agentanalysis.WorkspaceSandboxResult,
	elapsed time.Duration,
	runErr error,
) agentSandboxAnalyzerBenchmarkRecord {
	status, code := agentSandboxAnalyzerBenchmarkStatus(result, runErr)
	record := agentSandboxAnalyzerBenchmarkRecord{
		Version: agentSandboxAnalyzerBenchmarkRecordVersion,
		CaseID:  prepared.bc.name, StableID: prepared.bc.stableID, Repetition: repetition,
		Runtime: "agent-sandbox-opencode", Arm: cfg.ArmLabel, ModelLabel: cfg.ModelLabel,
		EngineCommit: cfg.EngineCommit, FixtureSHA256: prepared.bc.fixtureSHA256,
		BaselineConsumerCommit: prepared.bc.consumerCommit, BaselinePromptSHA256: prepared.bc.promptSHA256,
		ProjectSHA256: prepared.prepared.ProjectSHA256, ProviderPath: cfg.ProviderPath, TransportID: cfg.TransportID,
		APIMode: ai.APIChatCompletions, EvidenceCondition: benchmarkEvidenceConditionFixture,
		JobName: prepared.bc.jobName, BuildID: prepared.bc.buildID, TestName: prepared.bc.testName, TestSource: prepared.bc.testSource,
		ContractVersion: agentanalysis.WorkspaceContractVersion, WorkspacePromptHash: agentanalysis.WorkspaceSkillHash(),
		ManifestHash: prepared.prepared.ManifestHash, RequestHash: prepared.prepared.RequestHash,
		RuntimeIdentityHash: runtimeIdentity, ExecutionID: executionID, SourceRevision: prepared.prepared.SourceRevision,
		ArtifactFiles: prepared.prepared.ArtifactFiles, ArtifactBytes: prepared.prepared.ArtifactBytes,
		Status: status, ErrorCode: code, FailureReason: boundedBenchmarkFailure(runErr),
		ElapsedMS: max(elapsed.Milliseconds(), 0), RuntimeDurationMS: max(result.Execution.DurationMs, 0),
		TaskFinalized: result.Telemetry.TaskFinalized, TaskFinalizedMS: result.Telemetry.TaskFinalizedMs,
		ResultAvailable: result.Telemetry.ResultAvailable, ResultAvailableMS: result.Telemetry.ResultAvailableMs,
		FinalizationChecked: result.Telemetry.FinalizationChecked, FinalizationValid: result.Telemetry.FinalizationValid,
		CleanupCompleted: result.Telemetry.CleanupCompleted, CleanupDurationMS: result.Telemetry.CleanupDurationMs,
		TokenUsageAvailable: result.Telemetry.TokenUsageAvailable, CostAvailable: result.Telemetry.CostAvailable,
		UsageStatus: result.Telemetry.UsageStatus, Resources: result.Resources,
		HumanScoreRubricVersion: benchmarkHumanScoreRubricVersion, HumanScoreMax: benchmarkHumanScoreMax,
		HumanScoreDimensions: append([]string(nil), benchmarkHumanScoreDimensions...),
	}
	usage := result.Execution.Usage
	record.ModelRequests, record.InputTokens = usage.ModelRequests, usage.InputTokens
	record.CachedInputTokens, record.OutputTokens, record.CostUSD = usage.CachedInputTokens, usage.OutputTokens, usage.CostUSD
	record.TokenUsageAvailable = usage.Available
	record.CostAvailable = usage.Available && strings.TrimSpace(usage.CostUSD) != ""
	switch {
	case usage.Available:
		record.UsageStatus = "reported_by_executor"
	case result.Telemetry.TokenUsageAvailable || result.Telemetry.CostAvailable:
		record.UsageStatus = "runtime_reported_usage_without_values"
	case record.UsageStatus == "":
		record.UsageStatus = "unavailable_from_model_gateway"
	}
	analysis := result.Execution.Analysis
	record.AnalysisValid = analysis != nil && result.Telemetry.FinalizationValid
	assessment := assessBenchmarkCase(prepared.bc, nil)
	record.SignalTotal = assessment.total
	record.DiagnosisSignalTotal = assessment.diagnosisTotal
	record.ForbiddenChecksTotal = assessment.forbiddenTotal
	if analysis == nil {
		return record
	}
	isTransient := analysis.IsTransient
	record.IsTransient = &isTransient
	record.Summary, record.RootCause, record.SuggestedFix, record.Severity = analysis.Summary, analysis.RootCause, analysis.SuggestedFix, analysis.Severity
	record.RelevantFiles = append([]string(nil), analysis.RelevantFiles...)
	record.UnresolvedDetails = append([]string(nil), analysis.UnresolvedDetails...)
	for _, citation := range analysis.EvidenceCitations {
		record.EvidenceCitations = append(record.EvidenceCitations, agentSandboxAnalyzerCitation{
			Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd,
		})
	}
	for _, citation := range analysis.SourceCitations {
		record.SourceCitations = append(record.SourceCitations, agentSandboxAnalyzerCitation{
			Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Verified: citation.Verified,
		})
	}
	record.ArtifactCitationCount = len(record.EvidenceCitations)
	record.SourceCitationCount = len(record.SourceCitations)
	record.SourceVerified = record.SourceCitationCount > 0
	for _, citation := range record.SourceCitations {
		record.SourceVerified = record.SourceVerified && citation.Verified
	}
	testCase := workspaceAnalysisTestCase(*analysis)
	assessment = assessBenchmarkCase(prepared.bc, testCase)
	record.SignalHits, record.SignalTotal = assessment.hits, assessment.total
	record.DiagnosisSignalHits, record.DiagnosisSignalTotal = assessment.diagnosisHits, assessment.diagnosisTotal
	record.TransientCorrect = assessment.transientCorrect
	record.ForbiddenChecksPassed, record.ForbiddenChecksTotal = assessment.forbiddenPassed, assessment.forbiddenTotal
	record.MissingMust = append([]string(nil), assessment.missingMust...)
	return record
}

func withoutCleanupPending(err error) error {
	if err == nil || err == engineruntime.ErrCleanupPending {
		return nil
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		remaining := make([]error, 0, len(many.Unwrap()))
		for _, child := range many.Unwrap() {
			if stripped := withoutCleanupPending(child); stripped != nil {
				remaining = append(remaining, stripped)
			}
		}
		return errors.Join(remaining...)
	}
	if one, ok := err.(interface{ Unwrap() error }); ok && errors.Is(one.Unwrap(), engineruntime.ErrCleanupPending) {
		return withoutCleanupPending(one.Unwrap())
	}
	return err
}

func agentSandboxAnalyzerBenchmarkStatus(result agentanalysis.WorkspaceSandboxResult, err error) (string, string) {
	if result.Execution.Analysis != nil && result.Telemetry.FinalizationValid {
		if !result.Telemetry.CleanupCompleted || result.CleanupWork != nil {
			return "cleanup_pending", "cleanup_pending"
		}
		if err == nil {
			return "succeeded", ""
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || result.Execution.TerminalState == engineruntime.TerminalTimedOut:
		return "timeout", "timeout"
	case errors.Is(err, context.Canceled) || errors.Is(err, engineruntime.ErrCancelled) || result.Execution.TerminalState == engineruntime.TerminalCancelled:
		return "cancellation", "cancellation"
	case result.Telemetry.TaskFinalized && !result.Telemetry.ResultAvailable:
		return "no_result", "no_result"
	case result.Telemetry.ResultAvailable && !result.Telemetry.FinalizationValid:
		return "invalid_result", "invalid_result"
	case result.Execution.TerminalState == engineruntime.TerminalFailed && strings.Contains(result.Execution.FailureReason, "invalid agent analysis result:"):
		return "invalid_result", "invalid_result"
	case errors.Is(err, engineruntime.ErrMalformedResult) || errors.Is(err, engineruntime.ErrResultContract):
		return "invalid_result", "invalid_result"
	default:
		return "runtime_failure", "runtime_failure"
	}
}

func workspaceAnalysisTestCase(analysis agentanalysis.WorkspaceAnalysis) *models.TestCase {
	return &models.TestCase{
		Status:    "failed",
		AISummary: &models.AISummary{Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		AIAnalysis: &models.AIAnalysis{
			RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
			RelevantFiles:     append([]string(nil), analysis.RelevantFiles...),
			EvidenceCitations: append([]models.EvidenceCitation(nil), analysis.EvidenceCitations...),
			Mode:              "agent-sandbox-opencode", CritiquePassed: false,
		},
	}
}

func agentSandboxAnalyzerBenchmarkExecutionID(requestHash, runtimeIdentity, arm string, repetition int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{requestHash, runtimeIdentity, arm, strconv.Itoa(repetition)}, "\x00")))
	return fmt.Sprintf("analysis-bench-%x", sum[:10])
}

func writeAgentSandboxAnalyzerPrepared(t *testing.T, path string, prepared agentSandboxAnalyzerPrepared) {
	t.Helper()
	if strings.TrimSpace(path) == "" {
		return
	}
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(path), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeAgentSandboxAnalyzerBenchmarkJSONL(t *testing.T, path string, record agentSandboxAnalyzerBenchmarkRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(record); err != nil {
		t.Fatal(err)
	}
}

func agentSandboxAnalyzerBenchmarkInt(t *testing.T, name string, fallback, minValue, maxValue int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		t.Fatalf("%s must be between %d and %d", name, minValue, maxValue)
	}
	return value
}

func agentSandboxAnalyzerBenchmarkDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 || value > 30*time.Minute {
		t.Fatalf("%s must be a positive Go duration at most 30m", name)
	}
	return value
}

func boundedBenchmarkFailure(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(err.Error()))
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func TestWithoutCleanupPending(t *testing.T) {
	other := errors.New("other")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "direct", err: engineruntime.ErrCleanupPending},
		{name: "wrapped", err: fmt.Errorf("wrapped: %w", engineruntime.ErrCleanupPending)},
		{name: "joined", err: errors.Join(engineruntime.ErrCleanupPending, other), want: other},
		{name: "unrelated", err: other, want: other},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := withoutCleanupPending(test.err)
			if test.want == nil {
				if got != nil {
					t.Fatalf("error = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, test.want) || errors.Is(got, engineruntime.ErrCleanupPending) {
				t.Fatalf("error = %v, want %v without cleanup pending", got, test.want)
			}
		})
	}
	valid := agentanalysis.WorkspaceSandboxResult{
		Execution: agentanalysis.WorkspaceExecutionResult{Analysis: &agentanalysis.WorkspaceAnalysis{}},
		Telemetry: engineruntime.GenerateTelemetry{FinalizationValid: true, CleanupCompleted: true},
	}
	status, _ := agentSandboxAnalyzerBenchmarkStatus(valid, withoutCleanupPending(fmt.Errorf("wrapped: %w", engineruntime.ErrCleanupPending)))
	if status != "succeeded" {
		t.Fatalf("post-retry status = %q", status)
	}
}

func TestAgentSandboxAnalyzerRecordRetainsFailureUsage(t *testing.T) {
	cfg := agentSandboxAnalyzerBenchmarkConfig{
		ArmLabel: "arm-b", ModelLabel: "model-a", EngineCommit: strings.Repeat("a", 40),
		ProviderPath: "provider/model", TransportID: "transport-v1",
	}
	prepared := agentSandboxAnalyzerPreparedCase{
		prepared: agentSandboxAnalyzerPrepared{
			ProjectSHA256: strings.Repeat("b", 64), ManifestHash: strings.Repeat("c", 64),
			RequestHash: strings.Repeat("d", 64), SourceRevision: strings.Repeat("e", 40),
		},
		bc: benchCase{
			name: "case", stableID: "stable", fixtureSHA256: strings.Repeat("f", 64),
			consumerCommit: strings.Repeat("1", 40), promptSHA256: strings.Repeat("2", 64),
		},
	}
	usage := agentanalysis.WorkspaceUsage{
		Available: true, ModelRequests: 3, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 40, CostUSD: "0.42",
	}
	for _, test := range []struct {
		name       string
		result     agentanalysis.WorkspaceSandboxResult
		err        error
		wantStatus string
	}{
		{name: "invalid", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{Usage: usage}, Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true}}, err: engineruntime.ErrMalformedResult, wantStatus: "invalid_result"},
		{name: "no result", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{Usage: usage}, Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true}}, err: errors.New("missing"), wantStatus: "no_result"},
		{name: "timeout", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{TerminalState: engineruntime.TerminalTimedOut, Usage: usage}}, err: context.DeadlineExceeded, wantStatus: "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := agentSandboxAnalyzerRecordForResult(cfg, prepared, 1, "execution", "runtime", test.result, time.Second, test.err)
			if record.AnalysisValid || record.Status != test.wantStatus || record.ModelRequests != 3 || record.InputTokens != 100 || record.CachedInputTokens != 20 || record.OutputTokens != 40 || record.CostUSD != "0.42" || !record.TokenUsageAvailable || !record.CostAvailable || record.UsageStatus != "reported_by_executor" {
				t.Fatalf("record = %+v", record)
			}
		})
	}
}

func TestAgentSandboxAnalyzerBenchmarkExecutionID(t *testing.T) {
	base := agentSandboxAnalyzerBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "arm-b", 1)
	for _, changed := range []string{
		agentSandboxAnalyzerBenchmarkExecutionID(strings.Repeat("c", 64), strings.Repeat("b", 64), "arm-b", 1),
		agentSandboxAnalyzerBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("c", 64), "arm-b", 1),
		agentSandboxAnalyzerBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "arm-c", 1),
		agentSandboxAnalyzerBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "arm-b", 2),
	} {
		if changed == base {
			t.Fatalf("execution identity did not change: %s", base)
		}
	}
	if len(base) != len("analysis-bench-")+20 {
		t.Fatalf("execution id = %q", base)
	}
}

func TestAgentSandboxAnalyzerBenchmarkStatus(t *testing.T) {
	valid := agentanalysis.WorkspaceSandboxResult{
		Execution: agentanalysis.WorkspaceExecutionResult{Analysis: &agentanalysis.WorkspaceAnalysis{}},
		Telemetry: engineruntime.GenerateTelemetry{FinalizationValid: true, CleanupCompleted: true},
	}
	for _, test := range []struct {
		name   string
		result agentanalysis.WorkspaceSandboxResult
		err    error
		want   string
	}{
		{name: "success", result: valid, want: "succeeded"},
		{name: "cleanup", result: func() agentanalysis.WorkspaceSandboxResult {
			r := valid
			r.Telemetry.CleanupCompleted = false
			return r
		}(), err: engineruntime.ErrCleanupPending, want: "cleanup_pending"},
		{name: "no result", result: agentanalysis.WorkspaceSandboxResult{Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true}}, err: errors.New("missing"), want: "no_result"},
		{name: "invalid", result: agentanalysis.WorkspaceSandboxResult{Telemetry: engineruntime.GenerateTelemetry{ResultAvailable: true}}, err: engineruntime.ErrMalformedResult, want: "invalid_result"},
		{name: "validated failure envelope", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{TerminalState: engineruntime.TerminalFailed, FailureReason: "invalid agent analysis result: artifact citation 0 quote does not match"}, Telemetry: engineruntime.GenerateTelemetry{ResultAvailable: true, FinalizationValid: true}}, err: errors.New("workspace analysis failed"), want: "invalid_result"},
		{name: "timeout", result: agentanalysis.WorkspaceSandboxResult{}, err: context.DeadlineExceeded, want: "timeout"},
		{name: "runtime", result: agentanalysis.WorkspaceSandboxResult{}, err: errors.New("failed"), want: "runtime_failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := agentSandboxAnalyzerBenchmarkStatus(test.result, test.err)
			if got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAgentSandboxAnalyzerBenchmarkRecordOmitsCitationQuotes(t *testing.T) {
	record := agentSandboxAnalyzerBenchmarkRecord{
		Version:           1,
		EvidenceCitations: []agentSandboxAnalyzerCitation{{Path: "artifact.log", LineStart: 1, LineEnd: 1}},
		SourceCitations:   []agentSandboxAnalyzerCitation{{Path: "pkg/file.go", LineStart: 2, LineEnd: 2, Verified: true}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "quote") {
		t.Fatalf("record persisted citation quote field: %s", data)
	}
}
