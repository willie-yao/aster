package benchmarks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/analysisstager"
	"github.com/willie-yao/aster/backend/internal/fixruntime"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const agentSandboxAnalyzerBenchmarkRecordVersion = 8

var benchmarkImmutableImageRE = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

type agentSandboxAnalyzerBenchmarkConfig struct {
	KubeContext        string
	SourceRoot         string
	ProjectDir         string
	ResultsPath        string
	PreparedPath       string
	InputRoot          string
	ArmLabel           string
	ModelLabel         string
	ProviderPath       string
	TransportID        string
	EngineCommit       string
	Provider           modelprovider.Config
	Timeout            time.Duration
	OutputLimit        int64
	MaxSteps           int
	ModelContextTokens int
	ModelOutputTokens  int
	Repetitions        int
	RepetitionBase     int
	PrepareOnly        bool
	ImageContract      agentSandboxAnalyzerImageContract
}

type agentSandboxAnalyzerImageContract struct {
	Version          int    `json:"version"`
	EngineCommit     string `json:"engine_commit"`
	ImageTag         string `json:"image_tag"`
	ExecutorImage    string `json:"executor_image"`
	StagerImage      string `json:"stager_image"`
	ExecutorRevision string `json:"executor_revision"`
	StagerRevision   string `json:"stager_revision"`
	ExecutorUser     string `json:"executor_user"`
	StagerUser       string `json:"stager_user"`
	ExecutorVersion  string `json:"executor_version"`
	StagerVersion    string `json:"stager_version"`
	OpenCodeVersion  string `json:"opencode_version"`
	ContractSHA256   string `json:"contract_sha256"`
}

type agentSandboxAnalyzerPrepared struct {
	Version                 int                      `json:"version"`
	CaseID                  string                   `json:"case_id"`
	StableID                string                   `json:"stable_id"`
	EvidenceMode            string                   `json:"evidence_mode"`
	SourceExpectationSHA256 string                   `json:"source_expectation_sha256"`
	EngineCommit            string                   `json:"engine_commit"`
	BenchmarkManifestSHA256 string                   `json:"benchmark_manifest_sha256"`
	FixtureSHA256           string                   `json:"fixture_sha256"`
	BaselineConsumerCommit  string                   `json:"baseline_consumer_commit"`
	BaselinePromptSHA256    string                   `json:"baseline_prompt_sha256"`
	ProjectSHA256           string                   `json:"project_sha256"`
	EffectivePromptSHA256   string                   `json:"effective_prompt_sha256"`
	SkillSetHash            string                   `json:"skill_set_hash"`
	EffectiveInputSHA256    string                   `json:"effective_input_sha256"`
	ComparisonInputSHA256   string                   `json:"comparison_input_sha256"`
	ProviderConfigSHA256    string                   `json:"provider_config_sha256"`
	ImageContractSHA256     string                   `json:"image_contract_sha256,omitempty"`
	Pricing                 benchmarkPricingIdentity `json:"pricing"`
	SourceRevision          string                   `json:"source_revision"`
	LocalSourceModePolicy   string                   `json:"local_source_mode_policy"`
	SourceModePolicy        string                   `json:"source_mode_policy"`
	SourceRoot              string                   `json:"source_root"`
	ArtifactRoot            string                   `json:"artifact_root"`
	ManifestHash            string                   `json:"manifest_hash"`
	RequestHash             string                   `json:"request_hash"`
	StageHash               string                   `json:"stage_hash"`
	ArtifactFiles           int                      `json:"artifact_files"`
	ArtifactBytes           int64                    `json:"artifact_bytes"`
	ArtifactPaths           []string                 `json:"artifact_paths"`
	WorkspacePromptHash     string                   `json:"workspace_prompt_hash"`
	ModelContextTokens      int                      `json:"model_context_tokens"`
	ModelOutputTokens       int                      `json:"model_output_tokens"`
	MaxSteps                int                      `json:"max_steps"`
	ModelLabel              string                   `json:"model_label"`
	ArmLabel                string                   `json:"arm_label"`
}

type agentSandboxAnalyzerCitation struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Verified  bool   `json:"verified,omitempty"`
}

type agentSandboxAnalyzerBenchmarkRecord struct {
	Version                      int                                    `json:"version"`
	CaseID                       string                                 `json:"case_id"`
	StableID                     string                                 `json:"stable_id"`
	Repetition                   int                                    `json:"repetition"`
	Runtime                      string                                 `json:"runtime"`
	Arm                          string                                 `json:"arm"`
	ModelLabel                   string                                 `json:"model_label"`
	EngineCommit                 string                                 `json:"engine_commit"`
	BenchmarkManifestSHA256      string                                 `json:"benchmark_manifest_sha256"`
	FixtureSHA256                string                                 `json:"fixture_sha256"`
	BaselineConsumerCommit       string                                 `json:"baseline_consumer_commit"`
	BaselinePromptSHA256         string                                 `json:"baseline_prompt_sha256"`
	ProjectSHA256                string                                 `json:"project_sha256"`
	EffectivePromptSHA256        string                                 `json:"effective_prompt_sha256"`
	SkillSetHash                 string                                 `json:"skill_set_hash"`
	EffectiveInputSHA256         string                                 `json:"effective_input_sha256"`
	ComparisonInputSHA256        string                                 `json:"comparison_input_sha256"`
	ProviderPath                 string                                 `json:"provider_path"`
	ProviderConfigSHA256         string                                 `json:"provider_config_sha256"`
	TransportID                  string                                 `json:"transport_id"`
	APIMode                      string                                 `json:"api_mode"`
	ReasoningEffort              string                                 `json:"reasoning_effort,omitempty"`
	EvidenceCondition            string                                 `json:"evidence_condition"`
	EvidenceMode                 string                                 `json:"evidence_mode"`
	EvidenceContractPassed       bool                                   `json:"evidence_contract_passed"`
	EvidenceContractStatus       string                                 `json:"evidence_contract_status"`
	SourceExpectationSHA256      string                                 `json:"source_expectation_sha256"`
	SourceExpectationPaths       []string                               `json:"source_expectation_paths"`
	SourceExpectationHits        int                                    `json:"source_expectation_hits"`
	SourceExpectationTotal       int                                    `json:"source_expectation_total"`
	SourceSignalHits             int                                    `json:"source_signal_hits"`
	SourceSignalTotal            int                                    `json:"source_signal_total"`
	JobName                      string                                 `json:"job_name"`
	BuildID                      string                                 `json:"build_id"`
	TestName                     string                                 `json:"test_name"`
	TestSource                   string                                 `json:"test_source"`
	ContractVersion              string                                 `json:"contract_version"`
	WorkspacePromptHash          string                                 `json:"workspace_prompt_hash"`
	ModelContextTokens           int                                    `json:"model_context_tokens"`
	ModelOutputTokens            int                                    `json:"model_output_tokens"`
	MaxSteps                     int                                    `json:"max_steps"`
	ManifestHash                 string                                 `json:"manifest_hash"`
	RequestHash                  string                                 `json:"request_hash"`
	RuntimeIdentityHash          string                                 `json:"runtime_identity_hash"`
	ImageContractSHA256          string                                 `json:"image_contract_sha256"`
	ExecutorImage                string                                 `json:"executor_image"`
	StagerImage                  string                                 `json:"stager_image"`
	ExecutorAsterRevision        string                                 `json:"executor_aster_revision"`
	StagerAsterRevision          string                                 `json:"stager_aster_revision"`
	ExpectedOpenCodeVersion      string                                 `json:"expected_opencode_version"`
	ExecutionID                  string                                 `json:"execution_id"`
	SourceRevision               string                                 `json:"source_revision"`
	SourceModePolicy             string                                 `json:"source_mode_policy"`
	ArtifactFiles                int                                    `json:"artifact_files"`
	ArtifactBytes                int64                                  `json:"artifact_bytes"`
	Status                       string                                 `json:"status"`
	ErrorCode                    string                                 `json:"error_code,omitempty"`
	FailureReason                string                                 `json:"failure_reason,omitempty"`
	ElapsedMS                    int64                                  `json:"elapsed_ms"`
	RuntimeDurationMS            int64                                  `json:"runtime_duration_ms"`
	TaskFinalized                bool                                   `json:"task_finalized"`
	TaskFinalizedMS              int64                                  `json:"task_finalized_ms,omitempty"`
	ResultAvailable              bool                                   `json:"result_available"`
	ResultAvailableMS            int64                                  `json:"result_available_ms,omitempty"`
	FinalizationChecked          bool                                   `json:"finalization_checked"`
	FinalizationValid            bool                                   `json:"finalization_valid"`
	CleanupCompleted             bool                                   `json:"cleanup_completed"`
	SchedulingAvailable          bool                                   `json:"scheduling_available"`
	SchedulingMS                 int64                                  `json:"scheduling_ms"`
	StagingAvailable             bool                                   `json:"staging_available"`
	StagingMS                    int64                                  `json:"staging_ms"`
	ExecutionAvailable           bool                                   `json:"execution_available"`
	ExecutionMS                  int64                                  `json:"execution_ms"`
	PublicationAvailable         bool                                   `json:"publication_available"`
	PublicationMS                int64                                  `json:"publication_ms"`
	PhaseTimingStatus            string                                 `json:"phase_timing_status"`
	CleanupDurationMS            int64                                  `json:"cleanup_duration_ms,omitempty"`
	AnalysisValid                bool                                   `json:"analysis_valid"`
	AnalysisDisposition          string                                 `json:"analysis_disposition,omitempty"`
	DispositionWarnings          []string                               `json:"disposition_warnings,omitempty"`
	StructuredValid              bool                                   `json:"structured_valid"`
	Displayable                  bool                                   `json:"displayable"`
	Grounded                     bool                                   `json:"grounded"`
	ResultValidationStatus       string                                 `json:"result_validation_status,omitempty"`
	ResultValidationCodes        []string                               `json:"result_validation_codes,omitempty"`
	ArtifactCitationCount        int                                    `json:"artifact_citation_count"`
	SourceCitationCount          int                                    `json:"source_citation_count"`
	SourceVerified               bool                                   `json:"source_verified"`
	EvidenceCitations            []agentSandboxAnalyzerCitation         `json:"evidence_citations,omitempty"`
	SourceCitations              []agentSandboxAnalyzerCitation         `json:"source_citations,omitempty"`
	SignalHits                   int                                    `json:"signal_hits"`
	SignalTotal                  int                                    `json:"signal_total"`
	DiagnosisSignalHits          int                                    `json:"diagnosis_signal_hits"`
	DiagnosisSignalTotal         int                                    `json:"diagnosis_signal_total"`
	TransientCorrect             *bool                                  `json:"transient_classification_correct,omitempty"`
	ForbiddenChecksPassed        int                                    `json:"forbidden_checks_passed"`
	ForbiddenChecksTotal         int                                    `json:"forbidden_checks_total"`
	MissingMust                  []string                               `json:"missing_must,omitempty"`
	IsTransient                  *bool                                  `json:"is_transient,omitempty"`
	Summary                      string                                 `json:"summary,omitempty"`
	RootCause                    string                                 `json:"root_cause,omitempty"`
	SuggestedFix                 string                                 `json:"suggested_fix,omitempty"`
	Severity                     string                                 `json:"severity,omitempty"`
	RelevantFiles                []string                               `json:"relevant_files,omitempty"`
	UnresolvedDetails            []string                               `json:"unresolved_details,omitempty"`
	ModelRequests                int                                    `json:"model_requests"`
	InputTokens                  int                                    `json:"input_tokens"`
	CachedInputTokens            int                                    `json:"cached_input_tokens"`
	OutputTokens                 int                                    `json:"output_tokens"`
	ReasoningTokens              int                                    `json:"reasoning_tokens"`
	CostUSD                      string                                 `json:"cost_usd,omitempty"`
	TokenUsageAvailable          bool                                   `json:"token_usage_available"`
	CostAvailable                bool                                   `json:"cost_available"`
	UsageStatus                  string                                 `json:"usage_status"`
	OpenCodeTelemetryAvailable   bool                                   `json:"opencode_telemetry_available"`
	OpenCodeTelemetryStatus      string                                 `json:"opencode_telemetry_status"`
	OpenCodeEventCount           int                                    `json:"opencode_event_count"`
	ProviderRequests             int                                    `json:"provider_requests"`
	ProviderRequestsKnown        bool                                   `json:"provider_requests_known"`
	RequestShapeAvailable        bool                                   `json:"request_shape_available"`
	StreamingMode                string                                 `json:"streaming_mode,omitempty"`
	RequestModelID               string                                 `json:"request_model_id,omitempty"`
	SystemPromptBytesAvailable   bool                                   `json:"system_prompt_bytes_available"`
	SystemPromptBytes            int                                    `json:"system_prompt_bytes,omitempty"`
	UserPromptBytes              int                                    `json:"user_prompt_bytes,omitempty"`
	ToolSchemaAvailable          bool                                   `json:"tool_schema_available"`
	ToolSchemaCount              int                                    `json:"tool_schema_count,omitempty"`
	ToolSchemaSHA256             string                                 `json:"tool_schema_sha256,omitempty"`
	ResponseSchemaSHA256         string                                 `json:"response_schema_sha256,omitempty"`
	ToolChoiceMode               string                                 `json:"tool_choice_mode,omitempty"`
	RequestContextLimit          int                                    `json:"request_context_limit,omitempty"`
	RequestOutputTokenLimit      int                                    `json:"request_output_token_limit,omitempty"`
	OpenCodeVersion              string                                 `json:"opencode_version,omitempty"`
	OpenCodeErrorAvailable       bool                                   `json:"opencode_error_available"`
	OpenCodeErrorName            string                                 `json:"opencode_error_name,omitempty"`
	OpenCodeHTTPStatusCode       int                                    `json:"opencode_http_status_code,omitempty"`
	OpenCodeRetryableKnown       bool                                   `json:"opencode_retryable_known"`
	OpenCodeRetryable            bool                                   `json:"opencode_retryable"`
	OpenCodeErrorClassification  string                                 `json:"opencode_error_classification,omitempty"`
	OpenCodeMetadataCode         string                                 `json:"opencode_metadata_code,omitempty"`
	OpenCodeCauseName            string                                 `json:"opencode_cause_name,omitempty"`
	OpenCodeCauseCode            string                                 `json:"opencode_cause_code,omitempty"`
	OpenCodeMessagePresent       bool                                   `json:"opencode_message_present"`
	OpenCodeMessageBytes         int                                    `json:"opencode_message_bytes,omitempty"`
	OpenCodeMessageSHA256        string                                 `json:"opencode_redacted_message_sha256,omitempty"`
	BeforeProviderRequest        *bool                                  `json:"before_provider_request,omitempty"`
	BeforeFirstTool              *bool                                  `json:"before_first_tool,omitempty"`
	DuringStreamProcessing       *bool                                  `json:"during_stream_processing,omitempty"`
	DuringToolExecution          *bool                                  `json:"during_tool_execution,omitempty"`
	DuringSessionPersistence     *bool                                  `json:"during_session_persistence,omitempty"`
	OpenCodeHeaderTimeout        bool                                   `json:"opencode_header_timeout"`
	OpenCodeResponseStreamError  bool                                   `json:"opencode_response_stream_error"`
	OpenCodeContextOverflow      bool                                   `json:"opencode_context_overflow"`
	ResponseContentTypePresent   bool                                   `json:"response_content_type_present"`
	ResponseBodyPresent          bool                                   `json:"response_body_present"`
	ResponseBodyBytesBounded     int                                    `json:"response_body_bytes_bounded,omitempty"`
	ResponseBodySHA256           string                                 `json:"response_body_sha256,omitempty"`
	OpenCodeTools                []agentanalysis.WorkspaceToolTelemetry `json:"opencode_tools,omitempty"`
	DeniedToolCount              int                                    `json:"denied_tool_count"`
	ToolFailureCount             int                                    `json:"tool_failure_count"`
	StepsUsed                    int                                    `json:"steps_used"`
	StructuredOutputRetriesKnown bool                                   `json:"structured_output_retries_known"`
	StructuredOutputRetries      int                                    `json:"structured_output_retries"`
	StructuredOutputErrors       int                                    `json:"structured_output_errors"`
	EvidencePhaseCompleted       bool                                   `json:"evidence_phase_completed"`
	EvidencePhaseSteps           int                                    `json:"evidence_phase_steps"`
	EvidencePhaseRequests        int                                    `json:"evidence_phase_requests"`
	ArtifactEvidenceToolCalls    int                                    `json:"artifact_evidence_tool_calls"`
	SourceEvidenceToolCalls      int                                    `json:"source_evidence_tool_calls"`
	FinalizationPhaseCompleted   bool                                   `json:"finalization_phase_completed"`
	FinalizationPhaseSteps       int                                    `json:"finalization_phase_steps"`
	FinalizationPhaseRequests    int                                    `json:"finalization_phase_requests"`
	StructuredOutputToolCalls    int                                    `json:"structured_output_tool_calls"`
	ContextLimit                 bool                                   `json:"context_limit"`
	OpenCodeTimedOut             bool                                   `json:"opencode_timed_out"`
	OpenCodeFailureCode          string                                 `json:"opencode_failure_code,omitempty"`
	OpenCodeStdoutTruncated      bool                                   `json:"opencode_stdout_truncated"`
	OpenCodeStderrTruncated      bool                                   `json:"opencode_stderr_truncated"`
	Resources                    engineruntime.ResourceMetadata         `json:"resources"`
	HumanScoreRubricVersion      int                                    `json:"human_score_rubric_version"`
	HumanScoreMax                int                                    `json:"human_score_max"`
	HumanScoreDimensions         []string                               `json:"human_score_dimensions"`
	Pricing                      benchmarkPricingIdentity               `json:"pricing"`
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
	var sealed *agentSandboxAnalyzerPrepared
	if !cfg.PrepareOnly {
		value := readAgentSandboxAnalyzerPrepared(t, cfg.PreparedPath)
		sealed = &value
	}
	prepared := prepareAgentSandboxAnalyzerBenchmarkCase(t, cfg, bc, sealed)
	if cfg.PrepareOnly {
		writeAgentSandboxAnalyzerPrepared(t, cfg.PreparedPath, prepared.prepared)
		t.Logf("prepared analyzer input manifest %s", prepared.prepared.ManifestHash)
		return
	}
	runner, err := fixruntime.NewAgentSandboxProviderRunnerForBenchmarkFromEnv(
		"AGENT_SANDBOX_ANALYSIS_", cfg.KubeContext, cfg.Provider, cfg.Timeout, cfg.OutputLimit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ExecutorImage() != cfg.ImageContract.ExecutorImage || runner.StagerImage() != cfg.ImageContract.StagerImage {
		t.Fatal("Agent Sandbox runtime images differ from the frozen image contract")
	}
	runtime := &agentanalysis.WorkspaceSandboxRuntime{
		Sandbox: runner, Provider: cfg.Provider, SourceModePolicy: prepared.request.SourceModePolicy, Timeout: cfg.Timeout, OutputLimitBytes: cfg.OutputLimit,
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
	provider := modelprovider.Normalize(modelprovider.Config{
		CredentialMode:  require("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_CREDENTIAL_MODE"),
		API:             require("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_API"),
		Endpoint:        require("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_ENDPOINT"),
		Model:           require("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_MODEL"),
		ReasoningEffort: modelprovider.ReasoningEffort(strings.TrimSpace(os.Getenv("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_REASONING_EFFORT"))),
		Auth:            modelprovider.Auth{Type: require("AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_TYPE")},
	})
	if err := validateBenchmarkProviderPath(require("BENCH_PROVIDER_PATH"), provider.Model); err != nil {
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
		PreparedPath: require("ANALYZER_BENCH_PREPARED_JSON"),
		ArmLabel:     arm, ModelLabel: modelLabel, ProviderPath: require("BENCH_PROVIDER_PATH"), TransportID: transportID,
		EngineCommit: benchmarkEngineCommit(t, !prepareOnly), Provider: provider, Timeout: timeout, OutputLimit: outputLimit,
		MaxSteps:           agentSandboxAnalyzerBenchmarkInt(t, "ANALYZER_BENCH_MAX_STEPS", 20, 1, 100),
		ModelContextTokens: agentSandboxAnalyzerBenchmarkInt(t, "BENCH_MODEL_CONTEXT_TOKENS", 0, 8192, 2_000_000),
		ModelOutputTokens:  agentSandboxAnalyzerBenchmarkInt(t, "BENCH_MODEL_OUTPUT_TOKENS", 0, 1024, 131072),
		Repetitions:        agentSandboxAnalyzerBenchmarkInt(t, "BENCH_REPETITIONS", 1, 1, 10),
		RepetitionBase:     benchmarkRepetitionStart(t), PrepareOnly: prepareOnly,
	}
	if cfg.ModelContextTokens == 0 || cfg.ModelOutputTokens == 0 {
		t.Fatal("BENCH_MODEL_CONTEXT_TOKENS and BENCH_MODEL_OUTPUT_TOKENS are required from the configured model")
	}
	if cfg.ModelOutputTokens > cfg.ModelContextTokens {
		t.Fatal("BENCH_MODEL_OUTPUT_TOKENS must not exceed the configured model context")
	}
	if !prepareOnly {
		cfg.KubeContext = require("ANALYZER_BENCH_KUBE_CONTEXT")
		cfg.ResultsPath = require("ANALYZER_BENCH_RESULTS_JSONL")
	} else {
		cfg.InputRoot = require("ANALYZER_BENCH_INPUT_ROOT")
	}
	contractPath := strings.TrimSpace(os.Getenv("ANALYZER_BENCH_IMAGE_CONTRACT_JSON"))
	if !prepareOnly && contractPath == "" {
		t.Fatal("ANALYZER_BENCH_IMAGE_CONTRACT_JSON is required for scored execution")
	}
	if contractPath != "" {
		cfg.ImageContract = readAgentSandboxAnalyzerImageContract(t, contractPath, cfg.EngineCommit)
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

func prepareAgentSandboxAnalyzerBenchmarkCase(t *testing.T, cfg agentSandboxAnalyzerBenchmarkConfig, bc benchCase, sealed *agentSandboxAnalyzerPrepared) agentSandboxAnalyzerPreparedCase {
	t.Helper()
	if err := validateBenchmarkProjectDir(cfg.ProjectDir, bc); err != nil {
		t.Fatalf("BENCH_PROJECT_DIR=%s: %v", cfg.ProjectDir, err)
	}
	projectConfig, consumerPrompt, err := project.LoadDir(cfg.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	agentic := projectConfig.AI.EffectiveAgentic()
	projectSkills, _, err := skills.LoadForTools(cfg.ProjectDir, agentic.Tools)
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
	localSourceModePolicy, err := sealOrVerifyAgentSandboxAnalyzerSource(t.Context(), cfg.SourceRoot, source.Revision, sealed)
	if err != nil {
		t.Fatalf("verify ANALYZER_BENCH_SOURCE_ROOT=%s: %v", cfg.SourceRoot, err)
	}
	if err := verifyAgentSandboxAnalyzerSourceExpectations(cfg.SourceRoot, bc.sourcePaths); err != nil {
		t.Fatalf("verify ANALYZER_BENCH_SOURCE_ROOT source expectations: %v", err)
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
	manifest, err := agentanalysis.NewWorkspaceManifestWithSkills(request, sourceinvestigation.Repository{
		Owner: source.Owner, Name: source.Name, Revision: source.Revision,
	}, consumerPrompt, projectSkills, files)
	if err != nil {
		t.Fatal(err)
	}
	inputSourceModePolicy := localSourceModePolicy
	if cfg.PrepareOnly {
		inputSourceModePolicy, err = analysisstager.PublishPreparedSnapshot(t.Context(), cfg.InputRoot, manifest, cfg.SourceRoot, artifactRoot, localSourceModePolicy)
		if err != nil {
			t.Fatal(err)
		}
	} else if sealed != nil {
		inputSourceModePolicy = agentanalysis.WorkspaceSourceModePolicy(sealed.SourceModePolicy)
	}
	execution, err := agentanalysis.NewWorkspaceExecutionRequestWithSourceEvidence(manifest, agentanalysis.WorkspaceSourceModePreserve, bc.evidenceMode == benchmarkEvidenceModeArtifactAndSource, cfg.Provider, cfg.Timeout, cfg.MaxSteps, cfg.ModelContextTokens, cfg.ModelOutputTokens, cfg.OutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := agentanalysis.NewWorkspaceStageRequestWithSourceModePolicies(manifest, inputSourceModePolicy, agentanalysis.WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	projectData, err := os.ReadFile(filepath.Join(cfg.ProjectDir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cacheGeneration, err := benchmarkCacheGenerationFingerprint(projectConfig.AI.CacheGeneration)
	if err != nil {
		t.Fatal(err)
	}
	identity := benchmarkRunIdentity{
		Arm: cfg.ArmLabel, EngineCommit: cfg.EngineCommit, BaselineConsumerCommit: bc.consumerCommit, BaselinePromptSHA256: bc.promptSHA256,
		BenchmarkManifestSHA256: benchmarkManifestIdentity(t),
		FixtureSHA256:           bc.fixtureSHA256, ProjectSHA256: sha256Hex(projectData), EffectivePromptSHA256: manifest.EffectivePromptSHA256,
		SkillSetHash: manifest.SkillSetHash, EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceStageSHA256: benchmarkEvidenceStageSHA256(bc.evidenceGroups),
		APIMode: cfg.Provider.API, ReasoningEffort: ai.ReasoningEffort(cfg.Provider.ReasoningEffort), ProviderPath: cfg.ProviderPath,
		ProviderConfigSHA256: benchmarkProviderConfigSHA256(cfg.Provider.API, cfg.Provider.Endpoint, cfg.Provider.Model, ai.ReasoningEffort(cfg.Provider.ReasoningEffort)), TransportID: cfg.TransportID,
		ModelContextTokens: cfg.ModelContextTokens, ModelOutputTokens: cfg.ModelOutputTokens,
	}
	identity.Pricing, err = newBenchmarkPricingIdentity(projectConfig.AI.EffectiveUsage().Pricing)
	if err != nil || identity.Pricing.SHA256 == "" {
		t.Fatalf("benchmark pricing identity: %v", err)
	}
	identity.EffectiveInputSHA256 = benchmarkEffectiveInputSHA256(identity, agentic, cacheGeneration)
	identity.ComparisonInputSHA256 = benchmarkComparisonInputSHA256(bc, identity)
	if err := validateBenchmarkRunIdentity(identity); err != nil {
		t.Fatal(err)
	}
	var artifactBytes int64
	artifactPaths := make([]string, 0, len(files))
	for _, file := range files {
		artifactBytes += file.Size
		artifactPaths = append(artifactPaths, file.Path)
	}
	prepared := agentSandboxAnalyzerPrepared{
		Version: 8, CaseID: bc.name, StableID: bc.stableID, EvidenceMode: bc.evidenceMode, SourceExpectationSHA256: benchmarkSourceExpectationSHA256(bc), EngineCommit: cfg.EngineCommit, BenchmarkManifestSHA256: identity.BenchmarkManifestSHA256,
		FixtureSHA256: bc.fixtureSHA256, BaselineConsumerCommit: bc.consumerCommit,
		BaselinePromptSHA256: bc.promptSHA256, ProjectSHA256: identity.ProjectSHA256,
		EffectivePromptSHA256: identity.EffectivePromptSHA256, SkillSetHash: identity.SkillSetHash, EffectiveInputSHA256: identity.EffectiveInputSHA256, ComparisonInputSHA256: identity.ComparisonInputSHA256,
		ProviderConfigSHA256: identity.ProviderConfigSHA256, ImageContractSHA256: cfg.ImageContract.ContractSHA256, Pricing: identity.Pricing,
		SourceRevision: source.Revision, LocalSourceModePolicy: string(localSourceModePolicy), SourceModePolicy: string(inputSourceModePolicy), SourceRoot: filepath.Clean(cfg.SourceRoot), ArtifactRoot: artifactRoot,
		ManifestHash: manifest.Hash, RequestHash: execution.Hash, StageHash: stage.Hash,
		ArtifactFiles: len(files), ArtifactBytes: artifactBytes, ArtifactPaths: artifactPaths,
		WorkspacePromptHash: agentanalysis.WorkspaceSkillHash(), ModelContextTokens: cfg.ModelContextTokens, ModelOutputTokens: cfg.ModelOutputTokens, MaxSteps: cfg.MaxSteps, ModelLabel: cfg.ModelLabel, ArmLabel: cfg.ArmLabel,
	}
	if sealed != nil && !reflect.DeepEqual(prepared, *sealed) {
		t.Fatal("prepared analyzer identity changed")
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
	record := agentSandboxAnalyzerRecordForResult(
		cfg, prepared, repetition, executionID, runtime.RuntimeIdentity(), runner.ExecutorImage(), runner.StagerImage(), result, elapsed, runErr,
	)
	writeAgentSandboxAnalyzerBenchmarkJSONL(t, cfg.ResultsPath, record)
	if runErr != nil {
		t.Logf("Agent Sandbox analyzer trial status=%s error=%v", record.Status, runErr)
	}
}

func agentSandboxAnalyzerRecordForResult(
	cfg agentSandboxAnalyzerBenchmarkConfig,
	prepared agentSandboxAnalyzerPreparedCase,
	repetition int,
	executionID, runtimeIdentity, executorImage, stagerImage string,
	result agentanalysis.WorkspaceSandboxResult,
	elapsed time.Duration,
	runErr error,
) agentSandboxAnalyzerBenchmarkRecord {
	status, code := agentSandboxAnalyzerBenchmarkStatus(result, runErr)
	record := agentSandboxAnalyzerBenchmarkRecord{
		Version: agentSandboxAnalyzerBenchmarkRecordVersion,
		CaseID:  prepared.bc.name, StableID: prepared.bc.stableID, Repetition: repetition,
		Runtime: "agent-sandbox-opencode", Arm: cfg.ArmLabel, ModelLabel: cfg.ModelLabel,
		EngineCommit: cfg.EngineCommit, BenchmarkManifestSHA256: prepared.prepared.BenchmarkManifestSHA256, FixtureSHA256: prepared.bc.fixtureSHA256,
		BaselineConsumerCommit: prepared.bc.consumerCommit, BaselinePromptSHA256: prepared.bc.promptSHA256,
		ProjectSHA256: prepared.prepared.ProjectSHA256, EffectivePromptSHA256: prepared.prepared.EffectivePromptSHA256,
		SkillSetHash: prepared.prepared.SkillSetHash, EffectiveInputSHA256: prepared.prepared.EffectiveInputSHA256,
		ComparisonInputSHA256: prepared.prepared.ComparisonInputSHA256,
		ProviderPath:          cfg.ProviderPath, ProviderConfigSHA256: prepared.prepared.ProviderConfigSHA256, TransportID: cfg.TransportID,
		APIMode: cfg.Provider.API, ReasoningEffort: string(cfg.Provider.ReasoningEffort), EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceMode: prepared.bc.evidenceMode,
		EvidenceContractStatus: "analysis_unavailable", SourceExpectationSHA256: benchmarkSourceExpectationSHA256(prepared.bc), SourceExpectationPaths: append([]string{}, prepared.bc.sourcePaths...),
		SourceExpectationTotal: len(prepared.bc.sourcePaths), SourceSignalTotal: len(prepared.bc.sourceSignals),
		JobName: prepared.bc.jobName, BuildID: prepared.bc.buildID, TestName: prepared.bc.testName, TestSource: prepared.bc.testSource,
		ContractVersion: agentanalysis.WorkspaceContractVersion, WorkspacePromptHash: agentanalysis.WorkspaceSkillHash(),
		ModelContextTokens: prepared.request.ModelContextTokens, ModelOutputTokens: prepared.request.ModelOutputTokens, MaxSteps: prepared.request.MaxSteps,
		ManifestHash: prepared.prepared.ManifestHash, RequestHash: prepared.prepared.RequestHash,
		RuntimeIdentityHash: runtimeIdentity, ImageContractSHA256: prepared.prepared.ImageContractSHA256, ExecutorImage: executorImage, StagerImage: stagerImage,
		ExecutorAsterRevision: cfg.ImageContract.ExecutorRevision, StagerAsterRevision: cfg.ImageContract.StagerRevision, ExpectedOpenCodeVersion: cfg.ImageContract.OpenCodeVersion,
		ExecutionID: executionID, SourceRevision: prepared.prepared.SourceRevision, SourceModePolicy: prepared.prepared.SourceModePolicy,
		ArtifactFiles: prepared.prepared.ArtifactFiles, ArtifactBytes: prepared.prepared.ArtifactBytes,
		Status: status, ErrorCode: code, FailureReason: boundedBenchmarkFailure(runErr),
		ElapsedMS: max(elapsed.Milliseconds(), 0), RuntimeDurationMS: max(result.Execution.DurationMs, 0),
		TaskFinalized: result.Telemetry.TaskFinalized, TaskFinalizedMS: result.Telemetry.TaskFinalizedMs,
		ResultAvailable: result.Telemetry.ResultAvailable, ResultAvailableMS: result.Telemetry.ResultAvailableMs,
		FinalizationChecked: result.Telemetry.FinalizationChecked, FinalizationValid: result.Telemetry.FinalizationValid,
		CleanupCompleted: result.Telemetry.CleanupCompleted, CleanupDurationMS: result.Telemetry.CleanupDurationMs,
		SchedulingAvailable: result.Telemetry.SchedulingAvailable, SchedulingMS: result.Telemetry.SchedulingMs,
		StagingAvailable: result.Telemetry.StagingAvailable, StagingMS: result.Telemetry.StagingMs,
		ExecutionAvailable: result.Telemetry.ExecutionAvailable, ExecutionMS: result.Telemetry.ExecutionMs,
		PublicationAvailable: result.Telemetry.PublicationAvailable, PublicationMS: result.Telemetry.PublicationMs,
		PhaseTimingStatus:   result.Telemetry.PhaseTimingStatus,
		TokenUsageAvailable: result.Telemetry.TokenUsageAvailable, CostAvailable: result.Telemetry.CostAvailable,
		UsageStatus: result.Telemetry.UsageStatus, Resources: result.Resources,
		HumanScoreRubricVersion: benchmarkHumanScoreRubricVersion, HumanScoreMax: benchmarkHumanScoreMax,
		HumanScoreDimensions: append([]string(nil), benchmarkHumanScoreDimensions...), Pricing: prepared.prepared.Pricing,
	}
	usage := result.Execution.Usage
	record.ModelRequests, record.InputTokens = usage.ModelRequests, usage.InputTokens
	record.CachedInputTokens, record.OutputTokens, record.ReasoningTokens, record.CostUSD = usage.CachedInputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CostUSD
	record.TokenUsageAvailable = usage.Available
	record.CostAvailable = usage.CostAvailable
	record.UsageStatus = usage.Status
	telemetry := result.Execution.OpenCodeTelemetry
	record.OpenCodeTelemetryAvailable = telemetry.Available
	record.OpenCodeTelemetryStatus = telemetry.Status
	record.OpenCodeEventCount = telemetry.EventCount
	record.ProviderRequests = telemetry.ProviderRequests
	record.ProviderRequestsKnown = telemetry.ProviderRequestsKnown
	shape := telemetry.RequestShape
	record.RequestShapeAvailable = shape.Available
	record.StreamingMode = shape.StreamingMode
	record.RequestModelID = shape.ModelID
	record.SystemPromptBytesAvailable = shape.SystemPromptBytesAvailable
	record.SystemPromptBytes = shape.SystemPromptBytes
	record.UserPromptBytes = shape.UserPromptBytes
	record.ToolSchemaAvailable = shape.ToolSchemaAvailable
	record.ToolSchemaCount = shape.ToolCount
	record.ToolSchemaSHA256 = shape.ToolSchemaSHA256
	record.ResponseSchemaSHA256 = shape.ResponseSchemaSHA256
	record.ToolChoiceMode = shape.ToolChoiceMode
	record.RequestContextLimit = shape.ContextLimit
	record.RequestOutputTokenLimit = shape.OutputTokenLimit
	record.OpenCodeVersion = shape.OpenCodeVersion
	errorTelemetry := telemetry.Error
	record.OpenCodeErrorAvailable = errorTelemetry.Available
	record.OpenCodeErrorName = errorTelemetry.Name
	record.OpenCodeHTTPStatusCode = errorTelemetry.HTTPStatusCode
	record.OpenCodeRetryableKnown = errorTelemetry.RetryableKnown
	record.OpenCodeRetryable = errorTelemetry.Retryable
	record.OpenCodeErrorClassification = errorTelemetry.Classification
	record.OpenCodeMetadataCode = errorTelemetry.MetadataCode
	record.OpenCodeCauseName = errorTelemetry.CauseName
	record.OpenCodeCauseCode = errorTelemetry.CauseCode
	record.OpenCodeMessagePresent = errorTelemetry.MessagePresent
	record.OpenCodeMessageBytes = errorTelemetry.MessageBytes
	record.OpenCodeMessageSHA256 = errorTelemetry.RedactedMessageSHA256
	record.BeforeProviderRequest = errorTelemetry.BeforeProviderRequest
	record.BeforeFirstTool = errorTelemetry.BeforeFirstTool
	record.DuringStreamProcessing = errorTelemetry.DuringStreamProcessing
	record.DuringToolExecution = errorTelemetry.DuringToolExecution
	record.DuringSessionPersistence = errorTelemetry.DuringSessionPersistence
	record.OpenCodeHeaderTimeout = errorTelemetry.HeaderTimeout
	record.OpenCodeResponseStreamError = errorTelemetry.ResponseStreamError
	record.OpenCodeContextOverflow = errorTelemetry.ContextOverflow
	record.ResponseContentTypePresent = errorTelemetry.ResponseContentTypePresent
	record.ResponseBodyPresent = errorTelemetry.ResponseBodyPresent
	record.ResponseBodyBytesBounded = errorTelemetry.ResponseBodyBytesBounded
	record.ResponseBodySHA256 = errorTelemetry.ResponseBodySHA256
	record.OpenCodeTools = append([]agentanalysis.WorkspaceToolTelemetry(nil), telemetry.Tools...)
	record.DeniedToolCount = telemetry.DeniedToolCount
	record.ToolFailureCount = telemetry.ToolFailureCount
	record.StepsUsed = telemetry.StepsUsed
	record.StructuredOutputRetriesKnown = telemetry.StructuredOutputRetriesKnown
	record.StructuredOutputRetries = telemetry.StructuredOutputRetries
	record.StructuredOutputErrors = telemetry.StructuredOutputErrors
	record.EvidencePhaseCompleted = telemetry.EvidencePhaseCompleted
	record.EvidencePhaseSteps = telemetry.EvidencePhaseSteps
	record.EvidencePhaseRequests = telemetry.EvidencePhaseRequests
	record.ArtifactEvidenceToolCalls = telemetry.ArtifactEvidenceToolCalls
	record.SourceEvidenceToolCalls = telemetry.SourceEvidenceToolCalls
	record.FinalizationPhaseCompleted = telemetry.FinalizationPhaseCompleted
	record.FinalizationPhaseSteps = telemetry.FinalizationPhaseSteps
	record.FinalizationPhaseRequests = telemetry.FinalizationPhaseRequests
	record.StructuredOutputToolCalls = telemetry.StructuredOutputToolCalls
	record.ContextLimit = telemetry.ContextLimit
	record.OpenCodeTimedOut = telemetry.TimedOut
	record.OpenCodeFailureCode = telemetry.FailureCode
	record.OpenCodeStdoutTruncated = telemetry.StdoutTruncated
	record.OpenCodeStderrTruncated = telemetry.StderrTruncated
	if record.UsageStatus == "" {
		record.UsageStatus = agentanalysis.WorkspaceTelemetryUnavailable
	}
	if record.OpenCodeTelemetryStatus == "" {
		record.OpenCodeTelemetryStatus = agentanalysis.WorkspaceTelemetryUnavailable
	}
	validation := result.Execution.ResultValidation
	record.ResultValidationStatus = validation.Status
	record.ResultValidationCodes = append([]string(nil), validation.Codes...)
	analysis := result.Execution.Analysis
	if analysis != nil && result.Telemetry.FinalizationValid {
		record.AnalysisDisposition, record.DispositionWarnings = agentanalysis.WorkspaceAnalysisDisposition(*analysis, validation, prepared.bc.evidenceMode == benchmarkEvidenceModeArtifactAndSource)
	}
	record.StructuredValid = record.AnalysisDisposition != ""
	record.Displayable = record.StructuredValid
	record.Grounded = record.AnalysisDisposition == models.AnalysisDispositionGrounded
	record.AnalysisValid = record.StructuredValid
	assessment := assessBenchmarkCase(prepared.bc, nil)
	record.SignalTotal = assessment.total
	record.DiagnosisSignalTotal = assessment.diagnosisTotal
	record.SourceSignalTotal = assessment.sourceTotal
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
	record.SourceExpectationHits = agentSandboxAnalyzerSourceExpectationHits(prepared.bc.sourcePaths, record.SourceCitations)
	testCase := workspaceAnalysisTestCase(*analysis, record.AnalysisDisposition, record.DispositionWarnings)
	assessment = assessBenchmarkCase(prepared.bc, testCase)
	record.SignalHits, record.SignalTotal = assessment.hits, assessment.total
	record.DiagnosisSignalHits, record.DiagnosisSignalTotal = assessment.diagnosisHits, assessment.diagnosisTotal
	record.SourceSignalHits, record.SourceSignalTotal = assessment.sourceHits, assessment.sourceTotal
	record.EvidenceContractPassed, record.EvidenceContractStatus = agentSandboxAnalyzerEvidenceContract(prepared.bc, record)
	record.TransientCorrect = assessment.transientCorrect
	record.ForbiddenChecksPassed, record.ForbiddenChecksTotal = assessment.forbiddenPassed, assessment.forbiddenTotal
	record.MissingMust = append([]string(nil), assessment.missingMust...)
	return record
}

func agentSandboxAnalyzerEvidenceContract(bc benchCase, record agentSandboxAnalyzerBenchmarkRecord) (bool, string) {
	if !record.AnalysisValid {
		return false, "analysis_unavailable"
	}
	if record.ArtifactEvidenceToolCalls < 1 {
		return false, "artifact_evidence_missing"
	}
	if record.ArtifactCitationCount < 1 {
		return false, "artifact_citation_missing"
	}
	hasSourceOutput := record.SourceCitationCount > 0 || len(record.RelevantFiles) > 0 || record.SourceSignalHits > 0
	if hasSourceOutput && record.SourceEvidenceToolCalls < 1 {
		return false, "unsupported_source_claim"
	}
	if bc.evidenceMode == benchmarkEvidenceModeArtifactAndSource {
		if record.SourceEvidenceToolCalls < 1 {
			return false, "source_evidence_missing"
		}
		if record.SourceCitationCount < 1 || !record.SourceVerified {
			return false, "source_citation_missing"
		}
		if record.SourceExpectationTotal < 1 || record.SourceExpectationHits != record.SourceExpectationTotal {
			return false, "source_expectation_missing"
		}
		if record.SourceSignalTotal < 1 || record.SourceSignalHits != record.SourceSignalTotal {
			return false, "source_diagnosis_missing"
		}
	}
	return true, "passed"
}

func agentSandboxAnalyzerSourceExpectationHits(paths []string, citations []agentSandboxAnalyzerCitation) int {
	verified := make(map[string]bool, len(citations))
	for _, citation := range citations {
		if citation.Verified {
			verified[citation.Path] = true
		}
	}
	hits := 0
	for _, path := range paths {
		if verified[path] {
			hits++
		}
	}
	return hits
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
	case result.Execution.ResultValidation.Status == agentanalysis.WorkspaceResultRejected:
		code := "invalid_result"
		if len(result.Execution.ResultValidation.Codes) == 1 {
			code = result.Execution.ResultValidation.Codes[0]
		}
		return "invalid_result", code
	case errors.Is(err, engineruntime.ErrMalformedResult) || errors.Is(err, engineruntime.ErrResultContract):
		return "invalid_result", "invalid_result"
	default:
		return "runtime_failure", "runtime_failure"
	}
}

func workspaceAnalysisTestCase(analysis agentanalysis.WorkspaceAnalysis, disposition string, warnings []string) *models.TestCase {
	return &models.TestCase{
		Status:    "failed",
		AISummary: &models.AISummary{Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		AIAnalysis: &models.AIAnalysis{
			RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
			RelevantFiles:     append([]string(nil), analysis.RelevantFiles...),
			EvidenceCitations: append([]models.EvidenceCitation(nil), analysis.EvidenceCitations...),
			Mode:              "agent-sandbox-opencode", Disposition: disposition,
			DispositionWarnings: append([]string(nil), warnings...), CritiquePassed: false,
		},
	}
}

func verifyAgentSandboxAnalyzerSourceExpectations(root string, paths []string) error {
	for _, path := range paths {
		info, err := os.Lstat(filepath.Join(filepath.Clean(root), filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("expected source file %s is unavailable", path)
		}
	}
	return nil
}

func agentSandboxAnalyzerBenchmarkExecutionID(requestHash, runtimeIdentity, arm string, repetition int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{requestHash, runtimeIdentity, arm, strconv.Itoa(repetition)}, "\x00")))
	return fmt.Sprintf("analysis-bench-%x", sum[:10])
}

func sealOrVerifyAgentSandboxAnalyzerSource(ctx context.Context, root, revision string, sealed *agentSandboxAnalyzerPrepared) (agentanalysis.WorkspaceSourceModePolicy, error) {
	if sealed == nil {
		return agentanalysis.ConfigurePreparedSourceModePolicy(ctx, root, revision)
	}
	policy := agentanalysis.WorkspaceSourceModePolicy(sealed.LocalSourceModePolicy)
	if err := agentanalysis.VerifyPreparedSourceWorkspace(ctx, root, revision, policy); err != nil {
		return "", err
	}
	return policy, nil
}

func readAgentSandboxAnalyzerPrepared(t *testing.T, path string) agentSandboxAnalyzerPrepared {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || len(data) > 1<<20 {
		t.Fatal("prepared analyzer record is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var prepared agentSandboxAnalyzerPrepared
	if err := decoder.Decode(&prepared); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("prepared analyzer record has trailing data")
	}
	if prepared.Version != 8 || !validBenchmarkEvidenceMode(prepared.EvidenceMode) || !benchmarkSHA256RE.MatchString(prepared.SourceExpectationSHA256) || !benchmarkSHA256RE.MatchString(prepared.ProviderConfigSHA256) || validateBenchmarkPricingIdentity(prepared.Pricing) != nil {
		t.Fatal("prepared analyzer record version is invalid")
	}
	return prepared
}

func readAgentSandboxAnalyzerImageContract(t *testing.T, path, expectedCommit string) agentSandboxAnalyzerImageContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || len(data) > 64<<10 {
		t.Fatal("analysis image contract is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var contract agentSandboxAnalyzerImageContract
	if err := decoder.Decode(&contract); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("analysis image contract has trailing data")
	}
	if contract.Version != 1 || contract.EngineCommit != expectedCommit || contract.ExecutorRevision != expectedCommit || contract.StagerRevision != expectedCommit ||
		!benchmarkImmutableImageRE.MatchString(contract.ExecutorImage) || !benchmarkImmutableImageRE.MatchString(contract.StagerImage) ||
		contract.ExecutorUser != "65532:65532" || contract.StagerUser != "65532:65532" || contract.OpenCodeVersion != "1.18.2" ||
		contract.ImageTag == "" || strings.ContainsAny(contract.ImageTag, " \t\r\n") || !benchmarkSHA256RE.MatchString(contract.ContractSHA256) ||
		!strings.Contains(contract.ExecutorVersion, "commit="+expectedCommit) || !strings.Contains(contract.StagerVersion, "commit="+expectedCommit) ||
		!strings.Contains(contract.ExecutorVersion, "image="+contract.ImageTag) || !strings.Contains(contract.StagerVersion, "image="+contract.ImageTag) {
		t.Fatal("analysis image contract identity is invalid")
	}
	payload := map[string]any{
		"version": contract.Version, "engine_commit": contract.EngineCommit, "image_tag": contract.ImageTag,
		"executor_image": contract.ExecutorImage, "stager_image": contract.StagerImage,
		"executor_revision": contract.ExecutorRevision, "stager_revision": contract.StagerRevision,
		"executor_user": contract.ExecutorUser, "stager_user": contract.StagerUser,
		"executor_version": contract.ExecutorVersion, "stager_version": contract.StagerVersion,
		"opencode_version": contract.OpenCodeVersion,
	}
	encoded, err := json.Marshal(payload)
	if err != nil || sha256Hex(encoded) != contract.ContractSHA256 {
		t.Fatal("analysis image contract hash changed")
	}
	return contract
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
	if record.Version != agentSandboxAnalyzerBenchmarkRecordVersion || !benchmarkImmutableImageRE.MatchString(record.ExecutorImage) || !benchmarkImmutableImageRE.MatchString(record.StagerImage) {
		t.Fatal("Agent Sandbox analyzer runtime image identity is invalid")
	}
	if record.ExecutorAsterRevision != record.EngineCommit || record.StagerAsterRevision != record.EngineCommit || record.ExpectedOpenCodeVersion == "" {
		t.Fatal("Agent Sandbox analyzer embedded runtime identity is invalid")
	}
	if !benchmarkSHA256RE.MatchString(record.ImageContractSHA256) {
		t.Fatal("Agent Sandbox analyzer image contract identity is invalid")
	}
	if record.RequestShapeAvailable && record.OpenCodeVersion != record.ExpectedOpenCodeVersion {
		t.Fatal("Agent Sandbox analyzer OpenCode version differs from the frozen image identity")
	}
	if !benchmarkSHA256RE.MatchString(record.ProviderConfigSHA256) || validateBenchmarkPricingIdentity(record.Pricing) != nil {
		t.Fatal("Agent Sandbox analyzer provider or pricing identity is invalid")
	}
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

func TestAgentSandboxAnalyzerExecutionRejectsChangedSourceModePolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	run("config", "commit.gpgsign", "false")
	run("add", "source.go")
	run("commit", "-qm", "fixture")
	revision := run("rev-parse", "HEAD")
	policy, err := sealOrVerifyAgentSandboxAnalyzerSource(t.Context(), root, revision, nil)
	if err != nil || policy != agentanalysis.WorkspaceSourceModePreserve {
		t.Fatalf("policy=%q err=%v", policy, err)
	}
	sealed := agentSandboxAnalyzerPrepared{Version: 8, EvidenceMode: benchmarkEvidenceModeArtifactOnly, LocalSourceModePolicy: string(policy), SourceModePolicy: string(policy)}
	run("config", "--local", "core.filemode", "false")
	if _, err := sealOrVerifyAgentSandboxAnalyzerSource(t.Context(), root, revision, &sealed); agentanalysis.SourceIntegrityCategory(err) != agentanalysis.SourceModePolicyChanged {
		t.Fatalf("error=%v category=%q", err, agentanalysis.SourceIntegrityCategory(err))
	}
	if mode := run("config", "--local", "--bool", "--get", "core.filemode"); mode != "false" {
		t.Fatalf("execution rewrote core.filemode=%s", mode)
	}
	if sealed.SourceModePolicy != string(agentanalysis.WorkspaceSourceModePreserve) {
		t.Fatalf("sealed record changed: %+v", sealed)
	}
}

func TestAgentSandboxAnalyzerEvidenceContract(t *testing.T) {
	base := agentSandboxAnalyzerBenchmarkRecord{
		AnalysisValid: true, ArtifactEvidenceToolCalls: 1, ArtifactCitationCount: 1,
	}
	for _, test := range []struct {
		name   string
		mode   string
		mutate func(*agentSandboxAnalyzerBenchmarkRecord)
		passed bool
		status string
	}{
		{name: "artifact only", mode: benchmarkEvidenceModeArtifactOnly, passed: true, status: "passed"},
		{name: "source grounded", mode: benchmarkEvidenceModeArtifactAndSource, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.SourceEvidenceToolCalls = 1
			record.SourceCitationCount = 1
			record.SourceVerified = true
			record.SourceExpectationHits, record.SourceExpectationTotal = 1, 1
			record.SourceSignalHits, record.SourceSignalTotal = 1, 1
		}, passed: true, status: "passed"},
		{name: "source evidence missing", mode: benchmarkEvidenceModeArtifactAndSource, passed: false, status: "source_evidence_missing"},
		{name: "source citation missing", mode: benchmarkEvidenceModeArtifactAndSource, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.SourceEvidenceToolCalls = 1
			record.SourceExpectationHits, record.SourceExpectationTotal = 1, 1
			record.SourceSignalHits, record.SourceSignalTotal = 1, 1
		}, passed: false, status: "source_citation_missing"},
		{name: "source expectation missing", mode: benchmarkEvidenceModeArtifactAndSource, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.SourceEvidenceToolCalls = 1
			record.SourceCitationCount = 1
			record.SourceVerified = true
			record.SourceExpectationTotal = 1
			record.SourceSignalHits, record.SourceSignalTotal = 1, 1
		}, passed: false, status: "source_expectation_missing"},
		{name: "source diagnosis missing", mode: benchmarkEvidenceModeArtifactAndSource, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.SourceEvidenceToolCalls = 1
			record.SourceCitationCount = 1
			record.SourceVerified = true
			record.SourceExpectationHits, record.SourceExpectationTotal = 1, 1
			record.SourceSignalTotal = 1
		}, passed: false, status: "source_diagnosis_missing"},
		{name: "artifact only source citation without read", mode: benchmarkEvidenceModeArtifactOnly, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.SourceCitationCount = 1
			record.SourceVerified = true
		}, passed: false, status: "unsupported_source_claim"},
		{name: "artifact only relevant file without read", mode: benchmarkEvidenceModeArtifactOnly, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.RelevantFiles = []string{"pkg/file.go"}
		}, passed: false, status: "unsupported_source_claim"},
		{name: "artifact evidence missing", mode: benchmarkEvidenceModeArtifactOnly, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.ArtifactEvidenceToolCalls = 0
		}, passed: false, status: "artifact_evidence_missing"},
		{name: "artifact citation missing", mode: benchmarkEvidenceModeArtifactOnly, mutate: func(record *agentSandboxAnalyzerBenchmarkRecord) {
			record.ArtifactCitationCount = 0
		}, passed: false, status: "artifact_citation_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := base
			if test.mutate != nil {
				test.mutate(&record)
			}
			passed, status := agentSandboxAnalyzerEvidenceContract(benchCase{evidenceMode: test.mode}, record)
			if passed != test.passed || status != test.status {
				t.Fatalf("contract = %v %q, want %v %q", passed, status, test.passed, test.status)
			}
		})
	}
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
			name: "case", stableID: "stable", evidenceMode: benchmarkEvidenceModeArtifactOnly, fixtureSHA256: strings.Repeat("f", 64),
			consumerCommit: strings.Repeat("1", 40), promptSHA256: strings.Repeat("2", 64),
		},
	}
	usage := agentanalysis.WorkspaceUsage{
		Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable, ModelRequests: 3, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 40, CostAvailable: true, CostUSD: "0.42",
	}
	for _, test := range []struct {
		name       string
		result     agentanalysis.WorkspaceSandboxResult
		err        error
		wantStatus string
		wantCode   string
	}{
		{name: "invalid", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{Usage: usage}, Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true}}, err: engineruntime.ErrMalformedResult, wantStatus: "invalid_result"},
		{name: "validated invalid", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{TerminalState: engineruntime.TerminalFailed, FailureReason: agentanalysis.WorkspaceResultRejectedReason, ResultValidation: agentanalysis.WorkspaceResultValidation{Status: agentanalysis.WorkspaceResultRejected, Codes: []string{agentanalysis.WorkspaceInvalidArtifactPath}}, Usage: usage}, Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, FinalizationValid: true}}, err: errors.New("rejected"), wantStatus: "invalid_result", wantCode: agentanalysis.WorkspaceInvalidArtifactPath},
		{name: "no result", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{Usage: usage}, Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true}}, err: errors.New("missing"), wantStatus: "no_result"},
		{name: "timeout", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{TerminalState: engineruntime.TerminalTimedOut, Usage: usage}}, err: context.DeadlineExceeded, wantStatus: "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := agentSandboxAnalyzerRecordForResult(cfg, prepared, 1, "execution", "runtime", "registry.example.test/executor@sha256:"+strings.Repeat("a", 64), "registry.example.test/stager@sha256:"+strings.Repeat("b", 64), test.result, time.Second, test.err)
			if record.AnalysisValid || record.Status != test.wantStatus || record.ModelRequests != 3 || record.InputTokens != 100 || record.CachedInputTokens != 20 || record.OutputTokens != 40 || record.CostUSD != "0.42" || !record.TokenUsageAvailable || !record.CostAvailable || record.UsageStatus != agentanalysis.WorkspaceTelemetryAvailable {
				t.Fatalf("record = %+v", record)
			}
			if test.wantCode != "" && (record.ResultValidationStatus != agentanalysis.WorkspaceResultRejected || !slices.Equal(record.ResultValidationCodes, []string{test.wantCode})) {
				t.Fatalf("result validation = %q %v", record.ResultValidationStatus, record.ResultValidationCodes)
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
		{name: "validated failure envelope", result: agentanalysis.WorkspaceSandboxResult{Execution: agentanalysis.WorkspaceExecutionResult{TerminalState: engineruntime.TerminalFailed, FailureReason: "workspace analysis result rejected", ResultValidation: agentanalysis.WorkspaceResultValidation{Status: agentanalysis.WorkspaceResultRejected, Codes: []string{agentanalysis.WorkspaceInvalidArtifactLineRange}}}, Telemetry: engineruntime.GenerateTelemetry{ResultAvailable: true, FinalizationValid: true}}, err: errors.New("workspace analysis failed"), want: "invalid_result"},
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

func TestVerifyAgentSandboxAnalyzerSourceExpectations(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "file.go"), []byte("package pkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAgentSandboxAnalyzerSourceExpectations(root, []string{"pkg/file.go"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyAgentSandboxAnalyzerSourceExpectations(root, []string{"pkg"}); err == nil {
		t.Fatal("directory source expectation was accepted")
	}
	if err := verifyAgentSandboxAnalyzerSourceExpectations(root, []string{"missing.go"}); err == nil {
		t.Fatal("missing source expectation was accepted")
	}
}

func TestReadAgentSandboxAnalyzerImageContract(t *testing.T) {
	commit := strings.Repeat("a", 40)
	payload := map[string]any{
		"version": 1, "engine_commit": commit, "image_tag": "sha-" + commit,
		"executor_image":    "registry.example.test/executor@sha256:" + strings.Repeat("b", 64),
		"stager_image":      "registry.example.test/stager@sha256:" + strings.Repeat("c", 64),
		"executor_revision": commit, "stager_revision": commit,
		"executor_user": "65532:65532", "stager_user": "65532:65532",
		"executor_version": "analysisexecutor version=eval commit=" + commit + " image=sha-" + commit + " go=go1.25.12",
		"stager_version":   "analysisstager version=eval commit=" + commit + " image=sha-" + commit + " go=go1.25.12",
		"opencode_version": "1.18.2",
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	payload["contract_sha256"] = sha256Hex(canonical)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image-contract.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	contract := readAgentSandboxAnalyzerImageContract(t, path, commit)
	if contract.ContractSHA256 != payload["contract_sha256"] || contract.OpenCodeVersion != "1.18.2" {
		t.Fatalf("contract=%+v", contract)
	}
}
