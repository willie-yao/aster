package e2e

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalcritic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const causalCriticBenchmarkRecordVersion = 1

type causalCriticBenchmarkRecord struct {
	Version                     int                      `json:"version"`
	CaseID                      string                   `json:"case_id"`
	StableID                    string                   `json:"stable_id"`
	Repetition                  int                      `json:"repetition"`
	EvidenceCondition           string                   `json:"evidence_condition"`
	AuthoritativeArm            string                   `json:"authoritative_arm"`
	AuthoritativeEngineCommit   string                   `json:"authoritative_engine_commit"`
	AuthoritativeModelLabel     string                   `json:"authoritative_model_label"`
	AuthoritativeSignalHits     int                      `json:"authoritative_signal_hits"`
	AuthoritativeSignalTotal    int                      `json:"authoritative_signal_total"`
	AuthoritativeDiagnosisHits  int                      `json:"authoritative_diagnosis_signal_hits"`
	AuthoritativeDiagnosisTotal int                      `json:"authoritative_diagnosis_signal_total"`
	CriticSignalHits            int                      `json:"critic_signal_hits"`
	CriticSignalTotal           int                      `json:"critic_signal_total"`
	CriticDiagnosisHits         int                      `json:"critic_diagnosis_signal_hits"`
	CriticDiagnosisTotal        int                      `json:"critic_diagnosis_signal_total"`
	FindingClasses              []string                 `json:"finding_classes"`
	Trial                       causalcritic.TrialRecord `json:"trial"`
}

func TestAgentSandboxCausalCriticBenchmark(t *testing.T) {
	if os.Getenv("RUN_AGENT_SANDBOX_CAUSAL_CRITIC_BENCHMARK") == "" {
		t.Skip("set RUN_AGENT_SANDBOX_CAUSAL_CRITIC_BENCHMARK=1 to run the private Agent Sandbox critic benchmark")
	}
	contextName := requireBenchmarkEnv(t, "CRITIC_BENCH_KUBE_CONTEXT")
	verifyCausalCriticBenchmarkCluster(t, contextName)
	condition, err := benchmarkEvidenceCondition()
	if err != nil {
		t.Fatal(err)
	}
	cases := shadowBenchmarkCases(t)
	if len(cases) != 1 {
		t.Fatal("causal critic benchmark requires exactly one selected case")
	}
	bc := cases[0]
	projectSkills := shadowBenchmarkSkills(t, cases)
	inputRecords := loadCausalCriticAuthoritativeRecords(t, requireBenchmarkEnv(t, "CRITIC_BENCH_INPROCESS_JSONL"), bc.name, condition)
	gateway := engineruntime.ModelGatewayConfig{
		Endpoint:        requireBenchmarkEnv(t, "AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_ENDPOINT"),
		Model:           requireBenchmarkEnv(t, "AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_MODEL"),
		ProtocolVersion: requireBenchmarkEnv(t, "AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_PROTOCOL"),
	}
	timeout := shadowBenchmarkDuration(t, "CRITIC_BENCH_TIMEOUT", 5*time.Minute)
	outputLimit := int64(shadowBenchmarkInt(t, "CRITIC_BENCH_OUTPUT_LIMIT_BYTES", int(causalcritic.DefaultOutputLimit), 4<<10, 1<<20))
	runner, err := fixruntime.NewAgentSandboxRunnerForBenchmarkFromEnv("AGENT_SANDBOX_CRITIC_", contextName, gateway, timeout, outputLimit)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &causalcritic.Runtime{Sandbox: runner, Gateway: gateway, Timeout: timeout, OutputLimitBytes: outputLimit}
	ledgerPath := requireBenchmarkEnv(t, "CRITIC_BENCH_LEDGER_PATH")
	resultsPath := requireBenchmarkEnv(t, "CRITIC_BENCH_RESULTS_JSONL")
	publicDir := filepath.Join(t.TempDir(), "public")

	for _, authoritative := range inputRecords {
		authoritative := authoritative
		t.Run(fmt.Sprintf("rep-%02d", authoritative.Repetition), func(t *testing.T) {
			bundle := causalCriticEvidenceBundle(t, bc, condition, projectSkills)
			snapshot := agentanalysis.AuthoritativeSnapshot{
				Summary: authoritative.Summary, IsTransient: authoritative.IsTransient != nil && *authoritative.IsTransient,
				RootCause: authoritative.RootCause, Severity: authoritative.Severity, SuggestedFix: authoritative.SuggestedFix,
				EvidenceCitations: slices.Clone(authoritative.Evidence), ElapsedMs: int(authoritative.ElapsedMS),
				InputTokens: authoritative.Trace.InputTokens, OutputTokens: authoritative.Trace.OutputTokens,
				ModelRequests: authoritative.Trace.ModelRequests,
				JudgeObjected: slices.ContainsFunc(authoritative.SemanticJudgeOutcomes, func(value string) bool { return strings.Contains(value, "objected") }),
				JudgeRevised:  authoritative.SemanticRevisionSelected,
			}
			bundle, err := causalcritic.EnsureCitedEvidence(t.Context(), causalCriticBrowser(t, bc), bundle, snapshot.EvidenceCitations)
			if err != nil {
				t.Fatal(err)
			}
			input, err := causalcritic.NewInput(bundle, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			metadata := causalcritic.TrialMetadata{
				CaseID: authoritative.CaseID, StableID: authoritative.StableID, Repetition: authoritative.Repetition,
				Arm: "agent-sandbox-independent-critic", AuthoritativeArm: authoritative.Arm, AuthoritativeElapsedMs: int(authoritative.ElapsedMS),
				AuthoritativeInputTokens: authoritative.Trace.InputTokens, AuthoritativeOutputTokens: authoritative.Trace.OutputTokens,
				AuthoritativeModelRequests: authoritative.Trace.ModelRequests,
				SameModelJudgeObjected:     snapshot.JudgeObjected, SameModelJudgeRevised: snapshot.JudgeRevised,
			}
			executionID := fmt.Sprintf("critic-%s-%s-rep-%02d", input.PairHash[:10], sha256Hex([]byte(authoritative.Arm))[:6], authoritative.Repetition)
			record, runErr := causalcritic.RunTrial(t.Context(), runtime, causalcritic.TrialSpec{
				PublicDir: publicDir, LedgerPath: ledgerPath, Metadata: metadata, Input: input, ExecutionID: executionID,
				RuntimeIdentity: causalcritic.RuntimeIdentity(gateway, os.Getenv("AGENT_SANDBOX_CRITIC_IMAGE"), timeout, outputLimit),
			})
			if runErr != nil {
				t.Logf("critic runtime: %v", runErr)
			}
			benchmarkRecord := scoreCausalCriticRecord(bc, condition, authoritative, record)
			writeCausalCriticBenchmarkJSONL(t, resultsPath, benchmarkRecord)
			if record.Status != causalcritic.TrialSucceeded {
				t.Errorf("critic trial status = %s", record.Status)
			}
		})
	}
}

func causalCriticBrowser(t *testing.T, bc benchCase) artifacts.Browser {
	t.Helper()
	backend, bucketLabel := benchStorage(t, bc)
	loc := prowbuildLocation(bc)
	return artifactsBrowser(backend, bucketLabel, loc.BuildPath(), bc.jobName+"/"+bc.buildID)
}

func causalCriticEvidenceBundle(t *testing.T, bc benchCase, condition string, projectSkills *skills.Set) agentanalysis.EvidenceBundle {
	t.Helper()
	backend, bucketLabel := benchStorage(t, bc)
	loc := prowbuildLocation(bc)
	build := models.BuildInfo{
		BuildID: bc.buildID, JobName: bc.jobName, PullNumber: bc.pullNumber, WebURL: bc.webURL,
		Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs),
	}
	source, ok := ai.ResolveBuildSource(build, bc.sourceRepo[0], bc.sourceRepo[1])
	if !ok {
		t.Fatal("benchmark source is unavailable")
	}
	request := ai.FailureAnalysisRequest{
		JobID: models.JobIDFor(bc.jobType, bc.repo, bc.jobName), BuildPrefix: loc.BuildPath(), Build: build,
		TestCase: *benchTestCase(bc), ConsecutiveFailures: bc.consecutiveFailures,
	}
	repository := sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}
	browser := artifactsBrowser(backend, bucketLabel, request.BuildPrefix, bc.jobName+"/"+bc.buildID)
	if condition == benchmarkEvidenceConditionOracle {
		preparation, err := prepareBenchmarkEvidence(context.Background(), browser, bc, condition, newBenchmarkEvidenceRecorder(bc.evidenceGroups))
		if err != nil {
			t.Fatal(err)
		}
		excerpts := make([]agentanalysis.EvidenceExcerpt, 0, len(preparation.oracleExcerpts))
		for _, excerpt := range preparation.oracleExcerpts {
			excerpts = append(excerpts, agentanalysis.EvidenceExcerpt{Path: excerpt.Path, Kind: "grep", Content: excerpt.Content})
		}
		bundle, err := agentanalysis.NewEvidenceBundle(request, repository, agentanalysis.ArtifactScan{PathCount: len(excerpts), Digest: preparation.frozenSHA256}, nil, excerpts, projectSkills.Hash())
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	bundle, err := agentanalysis.FreezeEvidence(t.Context(), browser, request, repository, projectSkills)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func verifyCausalCriticBenchmarkCluster(t *testing.T, contextName string) {
	t.Helper()
	if os.Getenv("CRITIC_BENCH_AKS_VALIDATION") == "" {
		verifyShadowBenchmarkCluster(t, contextName)
		return
	}
	expectedContext := requireBenchmarkEnv(t, "CRITIC_BENCH_EXPECTED_CONTEXT")
	expectedServer := requireBenchmarkEnv(t, "CRITIC_BENCH_EXPECTED_SERVER")
	expectedTLSName := requireBenchmarkEnv(t, "CRITIC_BENCH_EXPECTED_TLS_SERVER_NAME")
	expectedCA := requireBenchmarkEnv(t, "CRITIC_BENCH_EXPECTED_CA_SHA256")
	if contextName != expectedContext {
		t.Fatalf("critic AKS validation context = %q, want %q", contextName, expectedContext)
	}
	output, err := exec.Command("kubectl", "config", "view", "--raw", "--minify", "--context", contextName, "-o", "json").CombinedOutput()
	if err != nil {
		t.Fatal("critic AKS kubeconfig lookup failed")
	}
	var view struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `json:"server"`
				TLSName                  string `json:"tls-server-name"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
				Insecure                 bool   `json:"insecure-skip-tls-verify"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(output, &view); err != nil || len(view.Clusters) != 1 {
		t.Fatal("critic AKS kubeconfig is malformed")
	}
	cluster := view.Clusters[0].Cluster
	parsed, err := url.Parse(cluster.Server)
	if err != nil || parsed.Scheme != "https" || net.ParseIP(parsed.Hostname()) == nil || cluster.Server != expectedServer || cluster.TLSName != expectedTLSName || cluster.Insecure {
		t.Fatal("critic AKS kubeconfig does not match the authorized direct-IP TLS contract")
	}
	ca, err := base64.StdEncoding.DecodeString(cluster.CertificateAuthorityData)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(ca)) != expectedCA {
		t.Fatal("critic AKS kubeconfig CA identity changed")
	}
	out, err := exec.Command("kubectl", "--context", contextName, "get", "--raw=/version").CombinedOutput()
	var version struct {
		Platform string `json:"platform"`
	}
	if err != nil || json.Unmarshal(out, &version) != nil || version.Platform != "linux/amd64" {
		t.Fatal("critic AKS API or architecture validation failed")
	}
}

func loadCausalCriticAuthoritativeRecords(t *testing.T, path, caseID, condition string) []benchmarkJSONLResult {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []benchmarkJSONLResult
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var record benchmarkJSONLResult
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.CaseID != caseID || record.EvidenceCondition != condition {
			continue
		}
		if !record.Usable || record.Summary == "" || record.RootCause == "" || record.SuggestedFix == "" || record.Severity == "" || record.IsTransient == nil {
			t.Fatalf("authoritative record is not a usable paired draft: %+v", record)
		}
		record.Evidence = slices.Clone(record.Evidence)
		record.SemanticJudgeOutcomes = slices.Clone(record.SemanticJudgeOutcomes)
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatalf("no authoritative records found for %s/%s", caseID, condition)
	}
	slices.SortFunc(records, func(left, right benchmarkJSONLResult) int { return left.Repetition - right.Repetition })
	return records
}

func scoreCausalCriticRecord(bc benchCase, condition string, authoritative benchmarkJSONLResult, trial causalcritic.TrialRecord) causalCriticBenchmarkRecord {
	findingClasses := []string{}
	criticText := ""
	if trial.Review != nil {
		for _, finding := range trial.Review.Findings {
			findingClasses = append(findingClasses, finding.Class)
			criticText += "\n" + finding.Detail
		}
		criticText += "\n" + trial.Review.AlternativeExplanation + "\n" + trial.Review.RevisionGuidance
	}
	slices.Sort(findingClasses)
	criticCase := &models.TestCase{AISummary: &models.AISummary{Summary: criticText}, AIAnalysis: &models.AIAnalysis{RootCause: criticText, SuggestedFix: criticText}}
	assessment := assessBenchmarkCase(bc, criticCase)
	return causalCriticBenchmarkRecord{
		Version: causalCriticBenchmarkRecordVersion, CaseID: authoritative.CaseID, StableID: authoritative.StableID,
		Repetition: authoritative.Repetition, EvidenceCondition: condition, AuthoritativeArm: authoritative.Arm,
		AuthoritativeEngineCommit: authoritative.EngineCommit, AuthoritativeModelLabel: authoritative.ModelLabel,
		AuthoritativeSignalHits: authoritative.SignalHits, AuthoritativeSignalTotal: authoritative.SignalTotal,
		AuthoritativeDiagnosisHits: authoritative.DiagnosisSignalHits, AuthoritativeDiagnosisTotal: authoritative.DiagnosisSignalTotal,
		CriticSignalHits: assessment.hits, CriticSignalTotal: assessment.total,
		CriticDiagnosisHits: assessment.diagnosisHits, CriticDiagnosisTotal: assessment.diagnosisTotal,
		FindingClasses: findingClasses, Trial: trial,
	}
}

func writeCausalCriticBenchmarkJSONL(t *testing.T, path string, record causalCriticBenchmarkRecord) {
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

func requireBenchmarkEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func prowbuildLocation(bc benchCase) prowbuild.BuildLocation {
	return prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo}, JobName: bc.jobName,
		BuildID: bc.buildID, PullNumber: bc.pullNumber,
	}
}

func artifactsBrowser(backend storage.Backend, bucketLabel, buildPrefix, buildID string) artifacts.Browser {
	return artifacts.NewUncachedBackendBrowser(backend, bucketLabel, buildPrefix, buildID)
}

type causalCriticCaseSummary struct {
	Trials                      int            `json:"trials"`
	Statuses                    map[string]int `json:"statuses"`
	Finalized                   int            `json:"finalized"`
	ValidReviews                int            `json:"valid_reviews"`
	CleanupSucceeded            int            `json:"cleanup_succeeded"`
	MalformedOrContractFailures int            `json:"malformed_or_contract_failures"`
	Timeouts                    int            `json:"timeouts"`
	Unavailable                 int            `json:"unavailable"`
	SameModelJudgeObjections    int            `json:"same_model_judge_objections"`
	CriticObjections            int            `json:"critic_objections"`
	FindingClasses              map[string]int `json:"finding_classes"`
	AuthoritativeDiagnosisHits  []int          `json:"authoritative_diagnosis_signal_hits"`
	CriticDiagnosisHits         []int          `json:"critic_diagnosis_signal_hits"`
	AuthoritativeModelRequests  []int          `json:"authoritative_model_requests"`
	CriticInputTokens           []int64        `json:"critic_input_tokens"`
	CriticOutputTokens          []int64        `json:"critic_output_tokens"`
	CriticCostsUSD              []string       `json:"critic_costs_usd"`
	CriticNanoAIU               []int64        `json:"critic_nano_aiu"`
	CriticDurationsMs           []int64        `json:"critic_durations_ms"`
	PublicationRegressions      int            `json:"publication_regressions"`
}

type causalCriticBenchmarkSummary struct {
	Version int                                `json:"version"`
	Cases   map[string]causalCriticCaseSummary `json:"cases"`
}

func TestAgentSandboxCausalCriticBenchmarkReport(t *testing.T) {
	if os.Getenv("RUN_AGENT_SANDBOX_CAUSAL_CRITIC_REPORT") == "" {
		t.Skip("set RUN_AGENT_SANDBOX_CAUSAL_CRITIC_REPORT=1 to summarize private critic records")
	}
	records := loadCausalCriticBenchmarkRecords(t, requireBenchmarkEnv(t, "CRITIC_BENCH_RESULTS_JSONL"))
	summary := summarizeCausalCriticBenchmark(records)
	output := requireBenchmarkEnv(t, "CRITIC_BENCH_SUMMARY_JSON")
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(output)), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(output), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadCausalCriticBenchmarkRecords(t *testing.T, path string) []causalCriticBenchmarkRecord {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []causalCriticBenchmarkRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var record causalCriticBenchmarkRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.Version != causalCriticBenchmarkRecordVersion || record.CaseID == "" || record.StableID == "" || record.Repetition < 1 || record.Trial.PairHash == "" {
			t.Fatalf("invalid causal critic benchmark record: %+v", record)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("causal critic benchmark results are empty")
	}
	return records
}

func summarizeCausalCriticBenchmark(records []causalCriticBenchmarkRecord) causalCriticBenchmarkSummary {
	summary := causalCriticBenchmarkSummary{Version: causalCriticBenchmarkRecordVersion, Cases: map[string]causalCriticCaseSummary{}}
	for _, record := range records {
		key := record.CaseID + "/" + record.EvidenceCondition + "/" + record.AuthoritativeArm
		item := summary.Cases[key]
		if item.Statuses == nil {
			item.Statuses = map[string]int{}
			item.FindingClasses = map[string]int{}
		}
		item.Trials++
		item.Statuses[string(record.Trial.Status)]++
		if record.Trial.Finalized {
			item.Finalized++
		}
		if record.Trial.Review != nil {
			item.ValidReviews++
			if record.Trial.Review.Verdict == "object" {
				item.CriticObjections++
			}
		}
		if record.Trial.Telemetry.CleanupCompleted {
			item.CleanupSucceeded++
		}
		if record.Trial.Status == causalcritic.TrialMalformedResult || record.Trial.Status == causalcritic.TrialContractViolation {
			item.MalformedOrContractFailures++
		}
		if record.Trial.Status == causalcritic.TrialTimeout {
			item.Timeouts++
		}
		if record.Trial.Status == causalcritic.TrialUnavailable {
			item.Unavailable++
		}
		if record.Trial.Metadata.SameModelJudgeObjected {
			item.SameModelJudgeObjections++
		}
		for _, finding := range record.FindingClasses {
			item.FindingClasses[finding]++
		}
		item.AuthoritativeDiagnosisHits = append(item.AuthoritativeDiagnosisHits, record.AuthoritativeDiagnosisHits)
		item.CriticDiagnosisHits = append(item.CriticDiagnosisHits, record.CriticDiagnosisHits)
		item.AuthoritativeModelRequests = append(item.AuthoritativeModelRequests, record.Trial.Metadata.AuthoritativeModelRequests)
		item.CriticInputTokens = append(item.CriticInputTokens, record.Trial.Usage.InputTokens)
		item.CriticOutputTokens = append(item.CriticOutputTokens, record.Trial.Usage.OutputTokens)
		if record.Trial.Usage.CostUSD != "" {
			item.CriticCostsUSD = append(item.CriticCostsUSD, record.Trial.Usage.CostUSD)
		}
		item.CriticNanoAIU = append(item.CriticNanoAIU, record.Trial.Usage.NanoAIU)
		item.CriticDurationsMs = append(item.CriticDurationsMs, record.Trial.RuntimeDurationMs)
		// The critic has no publication path, so it cannot introduce a published regression.
		item.PublicationRegressions = 0
		summary.Cases[key] = item
	}
	return summary
}

func TestSummarizeCausalCriticBenchmarkSeparatesQualityAndLifecycle(t *testing.T) {
	records := []causalCriticBenchmarkRecord{
		{
			Version: causalCriticBenchmarkRecordVersion, CaseID: "case", EvidenceCondition: "fixture-v1",
			AuthoritativeDiagnosisHits: 1, CriticDiagnosisHits: 2, FindingClasses: []string{causalcritic.FindingSpecificErrorIgnored},
			Trial: causalcritic.TrialRecord{
				Status: causalcritic.TrialSucceeded, Finalized: true,
				Metadata: causalcritic.TrialMetadata{AuthoritativeModelRequests: 7, SameModelJudgeObjected: true},
				Review:   &causalcritic.Review{Verdict: "object"}, Usage: causalcritic.GatewayUsage{InputTokens: 100, OutputTokens: 20, CostUSD: "0.01"},
				Telemetry: causalcritic.TrialTelemetry{CleanupCompleted: true}, RuntimeDurationMs: 500,
			},
		},
		{
			Version: causalCriticBenchmarkRecordVersion, CaseID: "case", EvidenceCondition: "fixture-v1",
			Trial: causalcritic.TrialRecord{Status: causalcritic.TrialMalformedResult},
		},
	}
	item := summarizeCausalCriticBenchmark(records).Cases["case/fixture-v1/"]
	if item.Trials != 2 || item.Finalized != 1 || item.ValidReviews != 1 || item.MalformedOrContractFailures != 1 || item.CriticObjections != 1 || item.FindingClasses[causalcritic.FindingSpecificErrorIgnored] != 1 || item.PublicationRegressions != 0 {
		t.Fatalf("summary = %+v", item)
	}
}
