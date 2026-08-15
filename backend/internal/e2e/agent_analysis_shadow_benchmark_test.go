package e2e

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/orka"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	shadowBenchmarkRecordVersion   = 3
	shadowBenchmarkKindContext     = "kind-prow-ai-shadow-bench"
	shadowBenchmarkAgentVersionKey = "prow-ai-dashboard/benchmark-agent-version"
	shadowBenchmarkOrkaCommitKey   = "prow-ai-dashboard/benchmark-orka-commit"
)

type shadowBenchmarkConfig struct {
	KubeContext    string
	OrkaAPI        string
	Namespace      string
	AgentRef       string
	AgentVersion   string
	GitSecret      string
	ProviderPath   string
	AgentBaseURL   string
	EngineCommit   string
	OrkaCommit     string
	TransportID    string
	Model          string
	ModelLabel     string
	MaxTurns       int
	Timeout        time.Duration
	Retries        int
	ResultsPath    string
	Repetitions    int
	RepetitionBase int
}

type shadowBenchmarkClusterIdentity struct {
	Server                   string
	CertificateAuthorityData string
}

type shadowEvidenceCitation struct {
	ExcerptID string `json:"excerpt_id"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

type shadowSourceCitation struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Verified  bool   `json:"verified"`
}

type shadowBenchmarkRecord struct {
	Version                   int                      `json:"version"`
	CaseID                    string                   `json:"case_id"`
	StableID                  string                   `json:"stable_id"`
	Repetition                int                      `json:"repetition"`
	Runtime                   string                   `json:"runtime"`
	ProviderPath              string                   `json:"provider_path"`
	ModelLabel                string                   `json:"model_label"`
	Arm                       string                   `json:"arm"`
	EngineCommit              string                   `json:"engine_commit"`
	FixtureSHA256             string                   `json:"fixture_sha256"`
	BaselineConsumerCommit    string                   `json:"baseline_consumer_commit"`
	BaselinePromptSHA256      string                   `json:"baseline_prompt_sha256"`
	ProjectSHA256             string                   `json:"project_sha256"`
	SkillSetHash              string                   `json:"skill_set_hash"`
	APIMode                   string                   `json:"api_mode"`
	TransportID               string                   `json:"transport_id"`
	EvidenceCondition         string                   `json:"evidence_condition"`
	EvidenceStageSHA256       string                   `json:"evidence_stage_sha256"`
	EvidenceStageIDs          []string                 `json:"evidence_stage_ids"`
	Status                    string                   `json:"status"`
	ErrorCode                 string                   `json:"error_code,omitempty"`
	SourceRevision            string                   `json:"source_revision"`
	EvidenceHash              string                   `json:"evidence_hash"`
	AgentSkillHash            string                   `json:"agent_skill_hash"`
	ContractVersion           string                   `json:"contract_version"`
	ToolPolicyVersion         string                   `json:"tool_policy_version"`
	AgentNamespace            string                   `json:"agent_namespace"`
	AgentRef                  string                   `json:"agent_ref"`
	AgentVersion              string                   `json:"agent_version"`
	AgentConfigSHA256         string                   `json:"agent_config_sha256"`
	OrkaCommit                string                   `json:"orka_commit"`
	RuntimeIdentityHash       string                   `json:"runtime_identity_hash"`
	ExecutionID               string                   `json:"execution_id"`
	MaxTurns                  int                      `json:"max_turns"`
	Timeout                   string                   `json:"timeout"`
	Retries                   int                      `json:"retries"`
	Attempts                  int                      `json:"attempts"`
	ElapsedMS                 int64                    `json:"elapsed_ms"`
	RuntimeDurationMS         int64                    `json:"runtime_duration_ms,omitempty"`
	FinalizationDurationMS    int64                    `json:"finalization_duration_ms,omitempty"`
	TaskFinalized             bool                     `json:"task_finalized"`
	TaskFinalizedMS           int64                    `json:"task_finalized_ms,omitempty"`
	ResultAvailable           bool                     `json:"result_available"`
	ResultAvailableMS         int64                    `json:"result_available_ms,omitempty"`
	FinalizationChecked       bool                     `json:"finalization_checked"`
	FinalizationValid         bool                     `json:"finalization_valid"`
	CleanupCompleted          bool                     `json:"cleanup_completed"`
	CleanupDurationMS         int64                    `json:"cleanup_duration_ms,omitempty"`
	ModelIdentityAvailable    bool                     `json:"model_identity_available"`
	ProviderIdentityAvailable bool                     `json:"provider_identity_available"`
	IdentityStatus            string                   `json:"identity_status"`
	DeterministicStatus       string                   `json:"deterministic_status"`
	DeterministicPassed       bool                     `json:"deterministic_passed"`
	DeterministicRuleIDs      []string                 `json:"deterministic_rule_ids,omitempty"`
	DeterministicHardRules    []string                 `json:"deterministic_hard_rules,omitempty"`
	DeterministicSoftRules    []string                 `json:"deterministic_soft_rules,omitempty"`
	SemanticStatus            string                   `json:"semantic_status"`
	SemanticValid             bool                     `json:"semantic_valid"`
	SemanticObjections        []string                 `json:"semantic_objections,omitempty"`
	SemanticReason            string                   `json:"semantic_reason,omitempty"`
	CleanupPending            bool                     `json:"cleanup_pending,omitempty"`
	IsTransient               *bool                    `json:"is_transient,omitempty"`
	Summary                   string                   `json:"summary,omitempty"`
	RootCause                 string                   `json:"root_cause,omitempty"`
	SuggestedFix              string                   `json:"suggested_fix,omitempty"`
	Severity                  string                   `json:"severity,omitempty"`
	RelevantFiles             []string                 `json:"relevant_files,omitempty"`
	EvidenceCitations         []shadowEvidenceCitation `json:"evidence_citations,omitempty"`
	SourceCitations           []shadowSourceCitation   `json:"source_citations,omitempty"`
	UnresolvedDetails         []string                 `json:"unresolved_details"`
	ArtifactCitationCount     int                      `json:"artifact_citation_count"`
	SourceCitationCount       int                      `json:"source_citation_count"`
	SourceVerified            bool                     `json:"source_verified"`
	SignalHits                int                      `json:"signal_hits"`
	SignalTotal               int                      `json:"signal_total"`
	MissingMust               []string                 `json:"missing_must,omitempty"`
	TokenUsageAvailable       bool                     `json:"token_usage_available"`
	CostStatus                string                   `json:"cost_status"`
	HumanScoreRubricVersion   int                      `json:"human_score_rubric_version"`
	HumanScoreMax             int                      `json:"human_score_max"`
	HumanScoreDimensions      []string                 `json:"human_score_dimensions"`
}

func TestAgentAnalysisShadowBenchmark(t *testing.T) {
	if os.Getenv("RUN_AGENT_ANALYSIS_SHADOW_BENCHMARK") == "" {
		t.Skip("set RUN_AGENT_ANALYSIS_SHADOW_BENCHMARK=1 to run the Orka OpenCode comparison benchmark")
	}
	cfg := loadShadowBenchmarkConfig(t)
	condition, err := benchmarkEvidenceCondition()
	if err != nil {
		t.Fatal(err)
	}
	if condition != benchmarkEvidenceConditionFixture {
		t.Fatalf("shadow benchmark supports only %q evidence until the shared broker is available", benchmarkEvidenceConditionFixture)
	}
	verifyShadowBenchmarkCluster(t, cfg.KubeContext)
	cases := shadowBenchmarkCases(t)
	projectSkills := shadowBenchmarkSkills(t, cases)
	agentConfigSHA256 := verifyShadowBenchmarkAgent(t, cfg)
	for _, bc := range cases {
		bc := bc
		for index := 0; index < cfg.Repetitions; index++ {
			repetition := cfg.RepetitionBase + index
			t.Run(fmt.Sprintf("%s/rep-%02d", bc.name, repetition), func(t *testing.T) {
				runShadowBenchmarkCase(t, cfg, bc, repetition, projectSkills, agentConfigSHA256)
			})
		}
	}
}

func loadShadowBenchmarkConfig(t *testing.T) shadowBenchmarkConfig {
	t.Helper()
	require := func(name string) string {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	model := require("AI_MODEL")
	providerPath := require("BENCH_PROVIDER_PATH")
	if err := validateShadowBenchmarkProvider(providerPath, model); err != nil {
		t.Fatal(err)
	}
	maxTurns := shadowBenchmarkInt(t, "SHADOW_BENCH_MAX_TURNS", 12, 1, 1000)
	retries := shadowBenchmarkInt(t, "SHADOW_BENCH_RETRIES", 0, 0, 2)
	repetitions := shadowBenchmarkInt(t, "BENCH_REPETITIONS", 1, 1, 10)
	timeout := shadowBenchmarkDuration(t, "SHADOW_BENCH_TIMEOUT", 10*time.Minute)
	if timeout <= 0 || timeout > 30*time.Minute {
		t.Fatalf("SHADOW_BENCH_TIMEOUT must be greater than zero and at most 30m")
	}
	modelLabel := require("BENCH_MODEL_LABEL")
	if strings.ContainsAny(modelLabel, " /\\:@") || len(modelLabel) > 80 {
		t.Fatal("BENCH_MODEL_LABEL must be stable and anonymous")
	}
	transportID := require("BENCH_TRANSPORT_ID")
	if len(transportID) > 80 || strings.ContainsAny(transportID, " \t\r\n") {
		t.Fatal("BENCH_TRANSPORT_ID must be stable and contain no whitespace")
	}
	orkaCommit := require("SHADOW_BENCH_ORKA_COMMIT")
	if !benchmarkCommitRE.MatchString(orkaCommit) {
		t.Fatal("SHADOW_BENCH_ORKA_COMMIT must be a lowercase 40-character commit SHA")
	}
	return shadowBenchmarkConfig{
		KubeContext: require("SHADOW_BENCH_KUBE_CONTEXT"), OrkaAPI: require("SHADOW_BENCH_ORKA_API"),
		Namespace: require("SHADOW_BENCH_NAMESPACE"), AgentRef: require("SHADOW_BENCH_AGENT_REF"),
		AgentVersion: require("SHADOW_BENCH_AGENT_VERSION"), GitSecret: strings.TrimSpace(os.Getenv("SHADOW_BENCH_GIT_SECRET")),
		ProviderPath: providerPath, AgentBaseURL: require("SHADOW_BENCH_AGENT_BASE_URL"), Model: model, ModelLabel: modelLabel,
		EngineCommit: benchmarkEngineCommit(t, true), OrkaCommit: orkaCommit, TransportID: transportID,
		MaxTurns: maxTurns, Timeout: timeout, Retries: retries, ResultsPath: require("SHADOW_BENCH_RESULTS_JSONL"),
		Repetitions: repetitions, RepetitionBase: benchmarkRepetitionStart(t),
	}
}

func validateShadowBenchmarkProvider(providerPath, model string) error {
	want := "github-copilot/" + strings.TrimSpace(model)
	if providerPath != want {
		return fmt.Errorf("shadow benchmark provider must be %q", want)
	}
	return nil
}

func shadowBenchmarkExecutionID(bundleHash, agentConfigSHA256, providerPath string, repetition int) string {
	digest := sha256Hex([]byte(strings.Join([]string{
		strings.TrimSpace(bundleHash), strings.TrimSpace(agentConfigSHA256), strings.TrimSpace(providerPath), strconv.Itoa(repetition),
	}, "\x00")))
	return "agent-analysis-" + digest[:16]
}

func shadowBenchmarkCases(t *testing.T) []benchCase {
	t.Helper()
	cases := benchCases
	var err error
	if manifest := strings.TrimSpace(os.Getenv("BENCH_MANIFEST")); manifest != "" {
		cases, err = loadBenchmarkManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
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
		t.Fatal("shadow comparison requires one pinned external benchmark case")
	}
	return cases
}

func shadowBenchmarkSkills(t *testing.T, cases []benchCase) *skills.Set {
	t.Helper()
	dir := t.TempDir()
	agentic := defaultBenchAgentic()
	if projectDir := strings.TrimSpace(os.Getenv("BENCH_PROJECT_DIR")); projectDir != "" {
		if len(cases) == 1 && cases[0].consumerCommit != "" {
			if err := validateBenchmarkProjectDir(projectDir, cases[0]); err != nil {
				t.Fatal(err)
			}
		}
		cfg, _, err := project.LoadDir(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		agentic = cfg.AI.EffectiveAgentic()
		dir = projectDir
	} else if cases[0].consumerCommit != "" {
		t.Fatal("pinned external benchmark cases require BENCH_PROJECT_DIR")
	}
	set, _, err := skills.LoadForTools(dir, agentic.Tools)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func verifyShadowBenchmarkCluster(t *testing.T, contextName string) {
	t.Helper()
	clustersOutput, err := exec.Command("kind", "get", "clusters").CombinedOutput()
	if err != nil {
		t.Fatal("kind cluster discovery failed")
	}
	clusterName := strings.TrimPrefix(shadowBenchmarkKindContext, "kind-")
	kindKubeconfig, err := exec.Command("kind", "get", "kubeconfig", "--name", clusterName).Output()
	if err != nil {
		t.Fatal("kind kubeconfig discovery failed")
	}
	kindPath := filepath.Join(t.TempDir(), "kind-kubeconfig")
	if err := os.WriteFile(kindPath, kindKubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	selected := kubectlConfigIdentity(t, "--context", contextName)
	expected := kubectlConfigIdentity(t, "--kubeconfig", kindPath)
	if err := admitShadowBenchmarkCluster(contextName, strings.Fields(string(clustersOutput)), selected, expected); err != nil {
		t.Fatal(err)
	}
}

func kubectlConfigIdentity(t *testing.T, selectorFlag, selectorValue string) shadowBenchmarkClusterIdentity {
	t.Helper()
	output, err := exec.Command("kubectl", "config", "view", "--raw", "--minify", selectorFlag, selectorValue, "-o", "json").CombinedOutput()
	if err != nil {
		t.Fatal("kubectl context identity lookup failed")
	}
	var view struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `json:"server"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(output, &view); err != nil || len(view.Clusters) != 1 {
		t.Fatal("kubectl context identity is malformed")
	}
	return shadowBenchmarkClusterIdentity{
		Server:                   strings.TrimSpace(view.Clusters[0].Cluster.Server),
		CertificateAuthorityData: strings.TrimSpace(view.Clusters[0].Cluster.CertificateAuthorityData),
	}
}

func admitShadowBenchmarkCluster(contextName string, clusters []string, selected, expected shadowBenchmarkClusterIdentity) error {
	if strings.TrimSpace(contextName) != shadowBenchmarkKindContext {
		return fmt.Errorf("shadow benchmark requires disposable context %q", shadowBenchmarkKindContext)
	}
	clusterName := strings.TrimPrefix(shadowBenchmarkKindContext, "kind-")
	if !slices.Contains(clusters, clusterName) {
		return fmt.Errorf("shadow benchmark kind cluster %q is not present", clusterName)
	}
	if selected.Server == "" || selected.CertificateAuthorityData == "" || selected != expected {
		return fmt.Errorf("shadow benchmark context does not target the disposable kind cluster")
	}
	return nil
}

func verifyShadowBenchmarkAgent(t *testing.T, cfg shadowBenchmarkConfig) string {
	t.Helper()
	raw := kubectlJSON(t, cfg.KubeContext, "-n", cfg.Namespace, "get", "agent", cfg.AgentRef, "-o", "json")
	var agent struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			SystemPrompt struct {
				Inline string `json:"inline"`
			} `json:"systemPrompt"`
			Model struct {
				Name string `json:"name"`
			} `json:"model"`
			SecretRef *struct {
				Name string `json:"name"`
			} `json:"secretRef"`
			Runtime *struct {
				Type                string   `json:"type"`
				DefaultAllowBash    bool     `json:"defaultAllowBash"`
				DefaultAllowedTools []string `json:"defaultAllowedTools"`
				DefaultMaxTurns     int      `json:"defaultMaxTurns"`
			} `json:"runtime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	wantTools := []string{"Read", "Glob", "Grep", "Write"}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != "opencode" || agent.Spec.Runtime.DefaultAllowBash ||
		agent.Spec.Runtime.DefaultMaxTurns != cfg.MaxTurns || !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) ||
		agent.Spec.Model.Name != cfg.Model || agent.Spec.SecretRef == nil ||
		agent.Metadata.Annotations[shadowBenchmarkAgentVersionKey] != cfg.AgentVersion ||
		agent.Metadata.Annotations[shadowBenchmarkOrkaCommitKey] != cfg.OrkaCommit {
		t.Fatal("Agent does not match required OpenCode benchmark contract")
	}
	secretRaw := kubectlJSON(t, cfg.KubeContext, "-n", cfg.Namespace, "get", "secret", agent.Spec.SecretRef.Name, "-o", "json")
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(secretRaw, &secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 2 || secret.Data["OPENAI_API_KEY"] == "" || secret.Data["OPENAI_BASE_URL"] == "" {
		t.Fatal("Agent Secret must contain exactly OPENAI_API_KEY and OPENAI_BASE_URL")
	}
	apiKey, err := base64.StdEncoding.DecodeString(secret.Data["OPENAI_API_KEY"])
	if err != nil || strings.TrimSpace(string(apiKey)) == "" {
		t.Fatal("Agent API key is missing or malformed")
	}
	decoded, err := base64.StdEncoding.DecodeString(secret.Data["OPENAI_BASE_URL"])
	baseURL := strings.TrimRight(strings.TrimSpace(string(decoded)), "/")
	if err != nil || baseURL != strings.TrimRight(cfg.AgentBaseURL, "/") {
		t.Fatal("Agent endpoint does not match SHADOW_BENCH_AGENT_BASE_URL")
	}
	secretKeys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		secretKeys = append(secretKeys, key)
	}
	slices.Sort(secretKeys)
	identity, err := json.Marshal(struct {
		AgentNamespace string   `json:"agent_namespace"`
		AgentRef       string   `json:"agent_ref"`
		AgentVersion   string   `json:"agent_version"`
		OrkaCommit     string   `json:"orka_commit"`
		Model          string   `json:"model"`
		RuntimeType    string   `json:"runtime_type"`
		AllowBash      bool     `json:"allow_bash"`
		AllowedTools   []string `json:"allowed_tools"`
		MaxTurns       int      `json:"max_turns"`
		SystemPrompt   string   `json:"system_prompt"`
		BaseURL        string   `json:"base_url"`
		SecretKeys     []string `json:"secret_keys"`
	}{
		AgentNamespace: cfg.Namespace, AgentRef: cfg.AgentRef, AgentVersion: cfg.AgentVersion, OrkaCommit: cfg.OrkaCommit,
		Model: agent.Spec.Model.Name, RuntimeType: agent.Spec.Runtime.Type, AllowBash: agent.Spec.Runtime.DefaultAllowBash,
		AllowedTools: append([]string(nil), agent.Spec.Runtime.DefaultAllowedTools...), MaxTurns: agent.Spec.Runtime.DefaultMaxTurns,
		SystemPrompt: agent.Spec.SystemPrompt.Inline, BaseURL: baseURL, SecretKeys: secretKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex(identity)
}

func runShadowBenchmarkCase(t *testing.T, cfg shadowBenchmarkConfig, bc benchCase, repetition int, projectSkills *skills.Set, agentConfigSHA256 string) {
	t.Helper()
	backend, bucketLabel := benchStorage(t, bc)
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo},
		JobName:     bc.jobName, BuildID: bc.buildID, PullNumber: bc.pullNumber,
	}
	build := models.BuildInfo{
		BuildID: bc.buildID, JobName: bc.jobName, PullNumber: bc.pullNumber, WebURL: bc.webURL,
		Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs),
	}
	source, ok := ai.ResolveBuildSource(build, bc.sourceRepo[0], bc.sourceRepo[1])
	if !ok || len(source.Revision) != 40 {
		t.Fatal("benchmark case does not resolve one lowercase 40-character source SHA")
	}
	jobID := models.JobIDFor(bc.jobType, bc.repo, bc.jobName)
	request := ai.FailureAnalysisRequest{
		JobID: jobID, BuildPrefix: loc.BuildPath(), Build: build, TestCase: *benchTestCase(bc),
		ConsecutiveFailures: bc.consecutiveFailures,
	}
	browser := artifacts.NewUncachedBackendBrowser(backend, bucketLabel, request.BuildPrefix, bc.jobName+"/"+bc.buildID)
	bundle, err := agentanalysis.FreezeEvidence(t.Context(), browser, request, sourceinvestigation.Repository{
		Owner: source.Owner, Name: source.Name, Revision: source.Revision,
	}, projectSkills)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := orka.NewAgentRuntimeFromEnv(orka.FromEnvConfig{
		Namespace: cfg.Namespace, AgentRef: cfg.AgentRef, GitSecret: cfg.GitSecret, API: cfg.OrkaAPI,
		Version: cfg.AgentVersion, MaxRetries: cfg.Retries, Purpose: orka.AgentPurposeFailureAnalysis,
		KubeContext: cfg.KubeContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &agentanalysis.Runtime{
		Agent: agent, Name: "orka-opencode", AgentNamespace: cfg.Namespace,
		AgentRef: cfg.AgentRef, AgentVersion: cfg.AgentVersion, Retries: cfg.Retries,
	}
	started := time.Now()
	executionID := shadowBenchmarkExecutionID(bundle.Hash, agentConfigSHA256, cfg.ProviderPath, repetition)
	result, runErr := runner.Generate(t.Context(), agentanalysis.Spec{
		Repo:   agentruntime.RepoRef{Owner: source.Owner, Name: source.Name, Ref: source.Revision},
		Bundle: bundle, SourceReader: orka.NewGitHubSourceReader("", os.Getenv("GITHUB_READ_TOKEN")),
		MaxTurns: cfg.MaxTurns, Timeout: cfg.Timeout, ExecutionID: executionID,
	})
	if result.Analysis.Summary != "" {
		result.Quality = agentanalysis.EvaluateQuality(bundle, result.Analysis, projectSkills, bc.consecutiveFailures)
	}
	elapsed := time.Since(started)
	if runErr != nil {
		t.Logf("shadow runtime error: %v", runErr)
	}
	record := shadowRecordForResult(cfg, bc, repetition, bundle, result, elapsed, runErr, agentConfigSHA256)
	record.SignalTotal = assessBenchmarkCase(bc, nil).total
	if result.Analysis.Summary != "" {
		testCase := shadowAnalysisTestCase(result.Analysis)
		assessment := assessBenchmarkCase(bc, testCase)
		record.SignalHits, record.SignalTotal, record.MissingMust = assessment.hits, assessment.total, assessment.missingMust
		scoreBenchCase(t, bc, testCase, benchmarkOutcomeUsable, elapsed.Round(time.Second), "Orka OpenCode shadow", 0, benchmarkToolUsage{}, benchmarkTraceSummary{}, nil, 0)
	}
	writeShadowBenchmarkJSONL(t, cfg.ResultsPath, record)
	if runErr != nil && !errors.Is(runErr, agentruntime.ErrCleanupPending) {
		t.Fatalf("shadow benchmark failed with %s", record.ErrorCode)
	}
	if errors.Is(runErr, agentruntime.ErrCleanupPending) {
		t.Errorf("shadow analysis was valid but Task cleanup remains pending")
	}
}

func shadowRecordForResult(cfg shadowBenchmarkConfig, bc benchCase, repetition int, bundle agentanalysis.EvidenceBundle, result agentanalysis.Result, elapsed time.Duration, err error, agentConfigSHA256 string) shadowBenchmarkRecord {
	statusValue := result.Status
	if statusValue == "" {
		statusValue = agentanalysis.ResolveShadowStatus(result, err)
	}
	status, code := string(statusValue), ""
	if statusValue != agentanalysis.ShadowStatusSucceeded {
		code = status
	}
	record := shadowBenchmarkRecord{
		Version: shadowBenchmarkRecordVersion, CaseID: bc.name, StableID: bc.stableID, Repetition: repetition,
		Runtime: "orka-opencode-shadow", ProviderPath: cfg.ProviderPath, ModelLabel: cfg.ModelLabel,
		Arm: "baseline", EngineCommit: cfg.EngineCommit, FixtureSHA256: bc.fixtureSHA256,
		BaselineConsumerCommit: bc.consumerCommit, BaselinePromptSHA256: bc.promptSHA256,
		ProjectSHA256: bc.projectSHA256, SkillSetHash: bundle.SkillSetHash, APIMode: ai.APIChatCompletions, TransportID: cfg.TransportID,
		EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceStageSHA256: benchmarkEvidenceStageSHA256(bc.evidenceGroups), EvidenceStageIDs: benchmarkEvidenceStageIDs(bc.evidenceGroups),
		Status: status, ErrorCode: code, SourceRevision: result.SourceSHA, EvidenceHash: result.EvidenceHash,
		AgentSkillHash: result.SkillHash, ContractVersion: agentanalysis.ContractVersion, ToolPolicyVersion: agentanalysis.ToolPolicyVersion,
		AgentNamespace: cfg.Namespace, AgentRef: cfg.AgentRef, AgentVersion: cfg.AgentVersion,
		AgentConfigSHA256: agentConfigSHA256, OrkaCommit: cfg.OrkaCommit,
		RuntimeIdentityHash: result.IdentityHash, ExecutionID: result.ExecutionID,
		MaxTurns: cfg.MaxTurns, Timeout: cfg.Timeout.String(), Retries: cfg.Retries,
		Attempts: result.Attempts, ElapsedMS: elapsed.Milliseconds(), RuntimeDurationMS: result.Duration.Milliseconds(), FinalizationDurationMS: result.FinalizationDuration.Milliseconds(),
		TaskFinalized: result.Telemetry.TaskFinalized, TaskFinalizedMS: result.Telemetry.TaskFinalizedMs,
		ResultAvailable: result.Telemetry.ResultAvailable, ResultAvailableMS: result.Telemetry.ResultAvailableMs,
		FinalizationChecked: result.Telemetry.FinalizationChecked, FinalizationValid: result.Telemetry.FinalizationValid,
		CleanupCompleted: result.Telemetry.CleanupCompleted, CleanupDurationMS: result.Telemetry.CleanupDurationMs,
		ModelIdentityAvailable: false, ProviderIdentityAvailable: false, IdentityStatus: "agent_owned_identity_unavailable",
		DeterministicStatus: result.Quality.DeterministicStatus, DeterministicPassed: result.Quality.DeterministicPassed,
		DeterministicRuleIDs: append([]string(nil), result.Quality.RuleIDs...), DeterministicHardRules: append([]string(nil), result.Quality.HardRules...), DeterministicSoftRules: append([]string(nil), result.Quality.SoftRules...),
		SemanticStatus: result.Quality.SemanticStatus, SemanticValid: result.Quality.SemanticValid,
		SemanticObjections: append([]string(nil), result.Quality.SemanticObjections...), SemanticReason: result.Quality.SemanticReason,
		CleanupPending:      result.CleanupPending,
		TokenUsageAvailable: result.Telemetry.TokenUsageAvailable, CostStatus: result.Telemetry.UsageStatus,
		HumanScoreRubricVersion: benchmarkHumanScoreRubricVersion, HumanScoreMax: benchmarkHumanScoreMax,
		HumanScoreDimensions: append([]string(nil), benchmarkHumanScoreDimensions...), UnresolvedDetails: []string{},
	}
	if record.CostStatus == "" {
		record.CostStatus = "unavailable_from_agent_runtime"
	}
	if record.DeterministicStatus == "" {
		record.DeterministicStatus = "not_run"
	}
	if record.SemanticStatus == "" {
		record.SemanticStatus = "unavailable"
		record.SemanticReason = "evidence_aware_semantic_judge_not_exposed"
	}

	if result.Analysis.Summary != "" {
		analysis := result.Analysis
		isTransient := analysis.IsTransient
		record.IsTransient = &isTransient
		record.Summary, record.RootCause, record.SuggestedFix, record.Severity = analysis.Summary, analysis.RootCause, analysis.SuggestedFix, analysis.Severity
		record.RelevantFiles = append([]string(nil), analysis.RelevantFiles...)
		excerptPaths := make(map[string]string, len(bundle.Excerpts))
		for _, excerpt := range bundle.Excerpts {
			excerptPaths[excerpt.ID] = excerpt.Path
		}
		for _, citation := range analysis.EvidenceCitations {
			record.EvidenceCitations = append(record.EvidenceCitations, shadowEvidenceCitation{
				ExcerptID: citation.ExcerptID, Path: excerptPaths[citation.ExcerptID], LineStart: citation.LineStart, LineEnd: citation.LineEnd,
			})
		}
		for _, citation := range analysis.SourceCitations {
			record.SourceCitations = append(record.SourceCitations, shadowSourceCitation{
				Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Verified: citation.Verified,
			})
		}
		record.UnresolvedDetails = append([]string{}, analysis.UnresolvedDetails...)
		record.ArtifactCitationCount = len(analysis.EvidenceCitations)
		record.SourceCitationCount = len(analysis.SourceCitations)
		record.SourceVerified = len(analysis.SourceCitations) > 0
		for _, citation := range analysis.SourceCitations {
			record.SourceVerified = record.SourceVerified && citation.Verified
		}
	}
	return record
}

func shadowAnalysisTestCase(analysis agentanalysis.Analysis) *models.TestCase {
	return &models.TestCase{
		Status:    "failed",
		AISummary: &models.AISummary{Summary: analysis.Summary, IsTransient: analysis.IsTransient},
		AIAnalysis: &models.AIAnalysis{
			RootCause: analysis.RootCause, Severity: analysis.Severity, SuggestedFix: analysis.SuggestedFix,
			RelevantFiles: append([]string(nil), analysis.RelevantFiles...), Mode: "agent-shadow", CritiquePassed: false,
		},
	}
}

func writeShadowBenchmarkJSONL(t *testing.T, path string, record shadowBenchmarkRecord) {
	t.Helper()
	if path == "" {
		return
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

func kubectlJSON(t *testing.T, contextName string, args ...string) []byte {
	t.Helper()
	commandArgs := append([]string{"--context", contextName}, args...)
	output, err := exec.Command("kubectl", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl query failed: %v", err)
	}
	return output
}

func shadowBenchmarkInt(t *testing.T, name string, fallback, minValue, maxValue int) int {
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

func shadowBenchmarkDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s must be a Go duration", name)
	}
	return value
}

func TestValidateShadowBenchmarkProvider(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.6", "claude-opus-5"} {
		if err := validateShadowBenchmarkProvider("github-copilot/"+model, model); err != nil {
			t.Fatalf("model %q: %v", model, err)
		}
	}
	for _, test := range []struct {
		provider string
		model    string
	}{
		{provider: "other/claude-opus-5", model: "claude-opus-5"},
		{provider: "github-copilot/claude-opus-5", model: "claude-sonnet-5"},
		{provider: "github-copilot/extra/claude-opus-5", model: "claude-opus-5"},
		{provider: "github-copilot//claude-opus-5", model: "claude-opus-5"},
	} {
		if err := validateShadowBenchmarkProvider(test.provider, test.model); err == nil {
			t.Fatalf("provider=%q model=%q was accepted", test.provider, test.model)
		}
	}
}

func TestShadowBenchmarkExecutionIDIncludesAgentConfiguration(t *testing.T) {
	base := shadowBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "github-copilot/claude-sonnet-4.6", 1)
	for _, changed := range []string{
		shadowBenchmarkExecutionID(strings.Repeat("c", 64), strings.Repeat("b", 64), "github-copilot/claude-sonnet-4.6", 1),
		shadowBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("d", 64), "github-copilot/claude-sonnet-4.6", 1),
		shadowBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "github-copilot/claude-opus-5", 1),
		shadowBenchmarkExecutionID(strings.Repeat("a", 64), strings.Repeat("b", 64), "github-copilot/claude-sonnet-4.6", 2),
	} {
		if changed == base {
			t.Fatalf("execution identity did not change: %s", base)
		}
	}
	if !regexp.MustCompile(`^agent-analysis-[0-9a-f]{16}$`).MatchString(base) {
		t.Fatalf("execution id = %q", base)
	}
}

func TestShadowAnalysisTestCase(t *testing.T) {
	analysis := agentanalysis.Analysis{
		Summary: "summary", IsTransient: true, RootCause: "cause", Severity: "Transient-Ignore",
		SuggestedFix: "fix", RelevantFiles: []string{"build-log.txt"},
	}
	got := shadowAnalysisTestCase(analysis)
	if got.AISummary == nil || got.AIAnalysis == nil || got.AISummary.Summary != "summary" || !got.AISummary.IsTransient || got.AIAnalysis.Mode != "agent-shadow" || got.AIAnalysis.CritiquePassed {
		t.Fatalf("test case = %+v", got)
	}
}

func TestShadowRecordForResult(t *testing.T) {
	cfg := shadowBenchmarkConfig{EngineCommit: strings.Repeat("d", 40), OrkaCommit: strings.Repeat("e", 40), Namespace: "orka-system", AgentRef: "agent", AgentVersion: "v1", ProviderPath: "github-copilot/claude-sonnet-4.6", TransportID: "copilot-structural-proxy-v1", ModelLabel: "copilot-sonnet-4-6", MaxTurns: 12, Timeout: time.Minute}
	result := agentanalysis.Result{
		SourceSHA: strings.Repeat("a", 40), EvidenceHash: strings.Repeat("b", 64), SkillHash: strings.Repeat("c", 64),
		ContractVersion: agentanalysis.ContractVersion, ToolPolicyVersion: agentanalysis.ToolPolicyVersion,
		IdentityHash: strings.Repeat("f", 64), ExecutionID: "agent-analysis-0123456789abcdef", Attempts: 2,
		Analysis: agentanalysis.Analysis{
			Summary:           "summary",
			EvidenceCitations: []agentanalysis.EvidenceCitation{{ExcerptID: "e", LineStart: 1, LineEnd: 1, Quote: "PRIVATE_ARTIFACT_SENTINEL"}},
			SourceCitations:   []sourceinvestigation.Citation{{Path: "pkg/file.go", LineStart: 2, LineEnd: 3, Quote: "PRIVATE_SOURCE_SENTINEL", Verified: true}},
		},
	}
	bundle := agentanalysis.EvidenceBundle{SkillSetHash: strings.Repeat("1", 64), Excerpts: []agentanalysis.EvidenceExcerpt{{ID: "e", Path: "build-log.txt"}}}
	bc := benchCase{name: "case", stableID: "0123456789abcdef0123", fixtureSHA256: strings.Repeat("2", 64), consumerCommit: strings.Repeat("3", 40), promptSHA256: strings.Repeat("4", 64), projectSHA256: strings.Repeat("5", 64)}
	record := shadowRecordForResult(cfg, bc, 1, bundle, result, time.Second, agentruntime.ErrCleanupPending, strings.Repeat("6", 64))
	if record.Status != "cleanup_pending" || record.Attempts != 2 || record.ArtifactCitationCount != 1 || record.TokenUsageAvailable || record.CostStatus == "" || record.ToolPolicyVersion != agentanalysis.ToolPolicyVersion || record.EvidenceStageSHA256 != benchmarkEvidenceStageSHA256(bc.evidenceGroups) || record.AgentConfigSHA256 != strings.Repeat("6", 64) || record.RuntimeIdentityHash != strings.Repeat("f", 64) ||
		len(record.EvidenceCitations) != 1 || record.EvidenceCitations[0].Path != "build-log.txt" || len(record.SourceCitations) != 1 || !record.SourceCitations[0].Verified {
		t.Fatalf("record = %+v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "PRIVATE_ARTIFACT_SENTINEL") || strings.Contains(string(encoded), "PRIVATE_SOURCE_SENTINEL") {
		t.Fatal("private citation quote was persisted")
	}
}

func TestAdmitShadowBenchmarkCluster(t *testing.T) {
	identity := shadowBenchmarkClusterIdentity{Server: "https://127.0.0.1:6443", CertificateAuthorityData: "ca"}
	if err := admitShadowBenchmarkCluster(shadowBenchmarkKindContext, []string{"prow-ai-shadow-bench"}, identity, identity); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		context  string
		clusters []string
		selected shadowBenchmarkClusterIdentity
		expected shadowBenchmarkClusterIdentity
	}{
		{context: "production", clusters: []string{"prow-ai-shadow-bench"}, selected: identity, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"other"}, selected: identity, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"prow-ai-shadow-bench"}, selected: shadowBenchmarkClusterIdentity{Server: "https://other", CertificateAuthorityData: "ca"}, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"prow-ai-shadow-bench"}, selected: shadowBenchmarkClusterIdentity{Server: identity.Server, CertificateAuthorityData: "other-ca"}, expected: identity},
	} {
		if err := admitShadowBenchmarkCluster(test.context, test.clusters, test.selected, test.expected); err == nil {
			t.Fatalf("context=%q clusters=%v was admitted", test.context, test.clusters)
		}
	}
}
