package e2e

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
			snapshot := agentanalysis.AuthoritativeSnapshot{
				Summary: authoritative.Summary, IsTransient: authoritative.IsTransient != nil && *authoritative.IsTransient,
				RootCause: authoritative.RootCause, Severity: authoritative.Severity, SuggestedFix: authoritative.SuggestedFix,
				EvidenceCitations: slices.Clone(authoritative.Evidence), ElapsedMs: int(authoritative.ElapsedMS),
				InputTokens: authoritative.Trace.InputTokens, OutputTokens: authoritative.Trace.OutputTokens,
				ModelRequests: authoritative.Trace.ModelRequests,
				JudgeObjected: slices.ContainsFunc(authoritative.SemanticJudgeOutcomes, func(value string) bool { return strings.Contains(value, "objected") }),
				JudgeRevised:  authoritative.SemanticRevisionSelected,
			}
			writeRecord := func(record causalcritic.TrialRecord) {
				benchmarkRecord := scoreCausalCriticRecord(bc, condition, authoritative, record)
				writeCausalCriticBenchmarkJSONL(t, resultsPath, benchmarkRecord)
				if record.Status != causalcritic.TrialSucceeded {
					t.Errorf("critic trial status = %s", record.Status)
				}
			}
			request, repository := causalCriticBenchmarkCandidate(t, bc)
			preflightIdentity, err := causalcritic.PreflightIdentity(causalcritic.PreflightIdentityInput{
				RequestHash: agentanalysis.FailureRequestHash(request), AuthoritativeHash: causalCriticAuthoritativeHash(t, snapshot),
				SourceRevision: repository.Revision, SkillHash: projectSkills.Hash(), RuntimeIdentity: runtime.RuntimeIdentity(),
				TrialDiscriminator: causalCriticBenchmarkPreflightDiscriminator(condition, authoritative.Arm, authoritative.Repetition),
			})
			if err != nil {
				t.Fatal(err)
			}
			preflight, claimed, err := causalcritic.ClaimPreflightAttempt(publicDir, ledgerPath, preflightIdentity, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !claimed {
				recovered, ok, err := causalCriticBenchmarkExistingPreflight(publicDir, ledgerPath, preflight)
				if err != nil {
					t.Fatal(err)
				}
				if ok {
					writeRecord(recovered)
					return
				}
				t.Skipf("critic benchmark preflight remains active: %s", preflight.Status)
			}
			completePreflight := func(status causalcritic.PreflightStatus, failureCode, trialAttemptHash string) {
				if !claimed {
					return
				}
				if err := causalcritic.CompletePreflightAttempt(publicDir, ledgerPath, preflightIdentity, status, failureCode, trialAttemptHash, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := causalCriticEvidenceBundle(t, bc, condition, projectSkills, request, repository)
			if err != nil {
				completePreflight(causalcritic.PreflightEvidenceFailed, "evidence_freeze", "")
				t.Fatal(err)
			}
			bundle, err = causalcritic.EnsureCitedEvidence(t.Context(), causalCriticBrowser(t, bc), bundle, snapshot.EvidenceCitations)
			if err != nil {
				completePreflight(causalcritic.PreflightEvidenceFailed, "evidence_citation", "")
				t.Fatal(err)
			}
			input, err := causalcritic.NewInput(bundle, snapshot)
			if err != nil {
				failureCode := "input_invalid"
				if code := causalcritic.ValidationCodeOf(err); code != "" {
					failureCode = "validation_" + string(code)
				}
				completePreflight(causalcritic.PreflightInputInvalid, failureCode, "")
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
				RuntimeIdentity: runtime.RuntimeIdentity(),
			})
			if record.AttemptHash != "" && record.Status != causalcritic.TrialPending && !errors.Is(runErr, causalcritic.ErrTrialPersistence) {
				failureCode := record.FailureCode
				if failureCode == "" {
					failureCode = record.ErrorCode
				}
				completePreflight(causalcritic.PreflightSubmitted, failureCode, record.AttemptHash)
			}
			if errors.Is(runErr, causalcritic.ErrTrialPersistence) || errors.Is(runErr, causalcritic.ErrTrialDetailsPruned) {
				t.Fatal(runErr)
			}
			if errors.Is(runErr, causalcritic.ErrTrialAlreadyAttempted) {
				if record.Status == causalcritic.TrialPending {
					t.Skip("critic trial claim remains pending")
				}
				t.Log("critic trial already exists in the private ledger; recovering its result row")
			} else if runErr != nil {
				t.Logf("critic runtime: %v", runErr)
			}
			writeRecord(record)
		})
	}
}

func causalCriticBenchmarkPreflightDiscriminator(condition, authoritativeArm string, repetition int) string {
	return fmt.Sprintf("agent-sandbox-independent-critic/%s/%s/%d", condition, authoritativeArm, repetition)
}

func causalCriticBenchmarkExistingPreflight(publicDir, ledgerPath string, preflight causalcritic.PreflightAttempt) (causalcritic.TrialRecord, bool, error) {
	if preflight.Status != causalcritic.PreflightSubmitted {
		return causalcritic.TrialRecord{}, false, nil
	}
	record, found, err := causalcritic.LoadTrialByAttempt(publicDir, ledgerPath, preflight.TrialAttemptHash)
	if err != nil {
		return causalcritic.TrialRecord{}, false, err
	}
	if !found {
		return causalcritic.TrialRecord{}, false, fmt.Errorf("%w: %s", causalcritic.ErrTrialDetailsPruned, preflight.TrialAttemptHash)
	}
	if record.Status == causalcritic.TrialPending {
		return causalcritic.TrialRecord{}, false, nil
	}
	return record, true, nil
}

func causalCriticBrowser(t *testing.T, bc benchCase) artifacts.Browser {
	t.Helper()
	backend, bucketLabel := benchStorage(t, bc)
	loc := prowbuildLocation(bc)
	return artifactsBrowser(backend, bucketLabel, loc.BuildPath(), bc.jobName+"/"+bc.buildID)
}

func causalCriticBenchmarkCandidate(t *testing.T, bc benchCase) (ai.FailureAnalysisRequest, sourceinvestigation.Repository) {
	t.Helper()
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
	return request, sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}
}

func causalCriticAuthoritativeHash(t *testing.T, snapshot agentanalysis.AuthoritativeSnapshot) string {
	t.Helper()
	_, hash, err := agentanalysis.NewAuthoritativeSnapshot(
		&models.AISummary{Summary: snapshot.Summary, IsTransient: snapshot.IsTransient},
		&models.AIAnalysis{
			RootCause: snapshot.RootCause, Severity: snapshot.Severity, SuggestedFix: snapshot.SuggestedFix,
			RelevantFiles: slices.Clone(snapshot.RelevantFiles), EvidenceCitations: slices.Clone(snapshot.EvidenceCitations),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func causalCriticEvidenceBundle(t *testing.T, bc benchCase, condition string, projectSkills *skills.Set, request ai.FailureAnalysisRequest, repository sourceinvestigation.Repository) (agentanalysis.EvidenceBundle, error) {
	t.Helper()
	backend, bucketLabel := benchStorage(t, bc)
	browser := artifactsBrowser(backend, bucketLabel, request.BuildPrefix, bc.jobName+"/"+bc.buildID)
	if condition == benchmarkEvidenceConditionOracle {
		preparation, err := prepareBenchmarkEvidence(context.Background(), browser, bc, condition, newBenchmarkEvidenceRecorder(bc.evidenceGroups))
		if err != nil {
			return agentanalysis.EvidenceBundle{}, err
		}
		excerpts := make([]agentanalysis.EvidenceExcerpt, 0, len(preparation.oracleExcerpts))
		for _, excerpt := range preparation.oracleExcerpts {
			excerpts = append(excerpts, agentanalysis.EvidenceExcerpt{Path: excerpt.Path, Kind: "grep", Content: excerpt.Content})
		}
		return agentanalysis.NewEvidenceBundle(request, repository, agentanalysis.ArtifactScan{PathCount: len(excerpts), Digest: preparation.frozenSHA256}, nil, excerpts, projectSkills.Hash())
	}
	return agentanalysis.FreezeEvidence(t.Context(), browser, request, repository, projectSkills)
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
	if err := upsertCausalCriticBenchmarkJSONL(path, record); err != nil {
		t.Fatal(err)
	}
}

func upsertCausalCriticBenchmarkJSONL(path string, record causalCriticBenchmarkRecord) error {
	if strings.TrimSpace(record.Trial.AttemptHash) == "" {
		return fmt.Errorf("causal critic benchmark attempt hash is required")
	}
	records, err := readCausalCriticBenchmarkJSONL(path)
	if err != nil {
		return err
	}
	changed := false
	found := false
	for index, existing := range records {
		if existing.Trial.AttemptHash != record.Trial.AttemptHash {
			continue
		}
		found = true
		left, _ := json.Marshal(existing)
		right, _ := json.Marshal(record)
		if string(left) == string(right) {
			return nil
		}
		existingRank := causalCriticBenchmarkRecordRank(existing)
		newRank := causalCriticBenchmarkRecordRank(record)
		switch {
		case existingRank < 2 && newRank >= existingRank:
			records[index] = record
			changed = true
		case newRank < existingRank:
			return nil
		default:
			return fmt.Errorf("conflicting terminal causal critic benchmark record for %s", record.Trial.AttemptHash)
		}
		break
	}
	if !found {
		records = append(records, record)
		changed = true
	}
	if !changed {
		return nil
	}
	cleanPath := filepath.Clean(path)
	parent := filepath.Dir(cleanPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, filepath.Base(cleanPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	for _, item := range records {
		if err := encoder.Encode(item); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, cleanPath)
}

func readCausalCriticBenchmarkJSONL(path string) ([]causalCriticBenchmarkRecord, error) {
	file, err := os.Open(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var records []causalCriticBenchmarkRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var record causalCriticBenchmarkRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func causalCriticBenchmarkRecordRank(record causalCriticBenchmarkRecord) int {
	trial := record.Trial
	if trial.Status == "" || trial.Status == causalcritic.TrialPending {
		return 0
	}
	if trial.Status == causalcritic.TrialCleanupPending || trial.CleanupWork != nil {
		return 1
	}
	if trial.Status == causalcritic.TrialSucceeded && (!trial.Finalized || trial.Review == nil || !trial.Telemetry.CleanupCompleted) {
		return 1
	}
	return 2
}

func TestWriteCausalCriticBenchmarkJSONLIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "critic.jsonl")
	record := causalCriticBenchmarkRecord{Version: causalCriticBenchmarkRecordVersion, Trial: causalcritic.TrialRecord{AttemptHash: strings.Repeat("a", 64), PairHash: strings.Repeat("b", 64), Status: causalcritic.TrialPending}}
	writeCausalCriticBenchmarkJSONL(t, path, record)
	writeCausalCriticBenchmarkJSONL(t, path, record)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; got != 1 {
		t.Fatalf("rows=%d data=%q", got, data)
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
	ErrorCodes                  map[string]int `json:"error_codes"`
	FailureCodes                map[string]int `json:"failure_codes"`
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
	Version               int                                `json:"version"`
	PreflightStatuses     map[string]int                     `json:"preflight_statuses"`
	PreflightFailureCodes map[string]int                     `json:"preflight_failure_codes"`
	Cases                 map[string]causalCriticCaseSummary `json:"cases"`
}

type causalCriticPreflightLedger struct {
	SchemaVersion int                             `json:"schema_version"`
	Preflights    []causalcritic.PreflightAttempt `json:"preflight_attempts"`
}

func TestAgentSandboxCausalCriticBenchmarkReport(t *testing.T) {
	if os.Getenv("RUN_AGENT_SANDBOX_CAUSAL_CRITIC_REPORT") == "" {
		t.Skip("set RUN_AGENT_SANDBOX_CAUSAL_CRITIC_REPORT=1 to summarize private critic records")
	}
	records := loadCausalCriticBenchmarkRecords(t, requireBenchmarkEnv(t, "CRITIC_BENCH_RESULTS_JSONL"))
	var preflights []causalcritic.PreflightAttempt
	if ledgerPath := strings.TrimSpace(os.Getenv("CRITIC_BENCH_LEDGER_PATH")); ledgerPath != "" {
		preflights = loadCausalCriticBenchmarkPreflights(t, ledgerPath)
	}
	summary := summarizeCausalCriticBenchmark(records, preflights)
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

func loadCausalCriticBenchmarkPreflights(t *testing.T, path string) []causalcritic.PreflightAttempt {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var ledger causalCriticPreflightLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.SchemaVersion != 2 && ledger.SchemaVersion != causalcritic.LedgerSchemaVersion {
		t.Fatalf("unsupported causal critic ledger schema %d", ledger.SchemaVersion)
	}
	return ledger.Preflights
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

func summarizeCausalCriticBenchmark(records []causalCriticBenchmarkRecord, preflights []causalcritic.PreflightAttempt) causalCriticBenchmarkSummary {
	summary := causalCriticBenchmarkSummary{
		Version: causalCriticBenchmarkRecordVersion, PreflightStatuses: map[string]int{}, PreflightFailureCodes: map[string]int{},
		Cases: map[string]causalCriticCaseSummary{},
	}
	for _, preflight := range preflights {
		summary.PreflightStatuses[string(preflight.Status)]++
		if preflight.FailureCode != "" {
			summary.PreflightFailureCodes[preflight.FailureCode]++
		}
	}
	for _, record := range records {
		key := record.CaseID + "/" + record.EvidenceCondition + "/" + record.AuthoritativeArm
		item := summary.Cases[key]
		if item.Statuses == nil {
			item.Statuses = map[string]int{}
			item.ErrorCodes = map[string]int{}
			item.FailureCodes = map[string]int{}
			item.FindingClasses = map[string]int{}
		}
		item.Trials++
		item.Statuses[string(record.Trial.Status)]++
		if record.Trial.ErrorCode != "" {
			item.ErrorCodes[record.Trial.ErrorCode]++
		}
		if record.Trial.FailureCode != "" {
			item.FailureCodes[record.Trial.FailureCode]++
		}
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
			Trial: causalcritic.TrialRecord{Status: causalcritic.TrialMalformedResult, ErrorCode: "malformed_result", FailureCode: "review_parse"},
		},
	}
	preflights := []causalcritic.PreflightAttempt{
		{Status: causalcritic.PreflightEvidenceFailed, FailureCode: "evidence_freeze"},
		{Status: causalcritic.PreflightSubmitted, FailureCode: "review_parse"},
	}
	summary := summarizeCausalCriticBenchmark(records, preflights)
	item := summary.Cases["case/fixture-v1/"]
	if item.Trials != 2 || item.Finalized != 1 || item.ValidReviews != 1 || item.MalformedOrContractFailures != 1 || item.CriticObjections != 1 || item.FindingClasses[causalcritic.FindingSpecificErrorIgnored] != 1 || item.ErrorCodes["malformed_result"] != 1 || item.FailureCodes["review_parse"] != 1 || item.PublicationRegressions != 0 {
		t.Fatalf("summary = %+v", item)
	}
	if summary.PreflightStatuses[string(causalcritic.PreflightEvidenceFailed)] != 1 || summary.PreflightStatuses[string(causalcritic.PreflightSubmitted)] != 1 || summary.PreflightFailureCodes["review_parse"] != 1 {
		t.Fatalf("preflight summary = %+v", summary)
	}
}

func TestCausalCriticBenchmarkPreflightDiscriminatorSeparatesEvidenceConditions(t *testing.T) {
	fixture := causalCriticBenchmarkPreflightDiscriminator(benchmarkEvidenceConditionFixture, "baseline", 1)
	oracle := causalCriticBenchmarkPreflightDiscriminator(benchmarkEvidenceConditionOracle, "baseline", 1)
	otherArm := causalCriticBenchmarkPreflightDiscriminator(benchmarkEvidenceConditionFixture, "same-model-judge", 1)
	otherRepetition := causalCriticBenchmarkPreflightDiscriminator(benchmarkEvidenceConditionFixture, "baseline", 2)
	if fixture == oracle || fixture == otherArm || fixture == otherRepetition {
		t.Fatalf("discriminators collided: fixture=%q oracle=%q arm=%q repetition=%q", fixture, oracle, otherArm, otherRepetition)
	}
}

func TestCausalCriticBenchmarkExistingPreflightStopsActiveStatuses(t *testing.T) {
	for _, status := range []causalcritic.PreflightStatus{causalcritic.PreflightPending, causalcritic.PreflightEvidenceFailed, causalcritic.PreflightInputInvalid} {
		t.Run(string(status), func(t *testing.T) {
			if record, recovered, err := causalCriticBenchmarkExistingPreflight(t.TempDir(), filepath.Join(t.TempDir(), "critic.json"), causalcritic.PreflightAttempt{Status: status}); err != nil || recovered || record.AttemptHash != "" {
				t.Fatalf("record=%+v recovered=%v err=%v", record, recovered, err)
			}
		})
	}
	preflight := causalcritic.PreflightAttempt{Status: causalcritic.PreflightSubmitted, TrialAttemptHash: strings.Repeat("a", 64)}
	if _, recovered, err := causalCriticBenchmarkExistingPreflight(t.TempDir(), filepath.Join(t.TempDir(), "critic.json"), preflight); !errors.Is(err, causalcritic.ErrTrialDetailsPruned) || recovered {
		t.Fatalf("recovered=%v err=%v", recovered, err)
	}
	root := t.TempDir()
	publicDir, ledgerPath := filepath.Join(root, "public"), filepath.Join(root, "private", "critic.json")
	created := time.Unix(6000, 0).UTC().Format(time.RFC3339Nano)
	pending := causalcritic.TrialRecord{
		ID: "critic-pending", CreatedAt: created, AttemptHash: preflight.TrialAttemptHash, RuntimeIdentity: strings.Repeat("b", 64),
		Status: causalcritic.TrialPending, Metadata: causalcritic.TrialMetadata{CaseID: "case", StableID: "0123456789abcdef0123", Repetition: 1, Arm: "agent-sandbox-independent-critic", AuthoritativeArm: "baseline"},
		EvidenceHash: strings.Repeat("c", 64), DraftHash: strings.Repeat("d", 64), PairHash: strings.Repeat("e", 64),
		Usage: causalcritic.GatewayUsage{Status: "unavailable", Source: "gateway_response"},
	}
	ledger := map[string]any{
		"schema_version": causalcritic.LedgerSchemaVersion,
		"attempts":       []causalcritic.TrialAttempt{{Hash: preflight.TrialAttemptHash, CreatedAt: created, Status: causalcritic.TrialPending}},
		"records":        []causalcritic.TrialRecord{pending},
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, recovered, err := causalCriticBenchmarkExistingPreflight(publicDir, ledgerPath, preflight); err != nil || recovered {
		t.Fatalf("pending submitted recovered=%v err=%v", recovered, err)
	}
}

func TestUpsertCausalCriticBenchmarkJSONLReplacesIncompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "critic.jsonl")
	attemptHash := strings.Repeat("c", 64)
	pairHash := strings.Repeat("d", 64)
	pending := causalCriticBenchmarkRecord{
		Version: causalCriticBenchmarkRecordVersion,
		Trial:   causalcritic.TrialRecord{AttemptHash: attemptHash, PairHash: pairHash, Status: causalcritic.TrialPending},
	}
	review := &causalcritic.Review{Verdict: "pass"}
	terminal := pending
	terminal.Trial.Status = causalcritic.TrialSucceeded
	terminal.Trial.Review = review
	terminal.Trial.Telemetry.CleanupCompleted = true
	terminal.Trial.Finalized = true
	if err := upsertCausalCriticBenchmarkJSONL(path, pending); err != nil {
		t.Fatal(err)
	}
	if err := upsertCausalCriticBenchmarkJSONL(path, terminal); err != nil {
		t.Fatal(err)
	}
	records, err := readCausalCriticBenchmarkJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Trial.Status != causalcritic.TrialSucceeded || !records[0].Trial.Finalized {
		t.Fatalf("records=%+v", records)
	}
	if err := upsertCausalCriticBenchmarkJSONL(path, terminal); err != nil {
		t.Fatal(err)
	}
	conflict := terminal
	conflict.Trial.RuntimeDurationMs = 99
	if err := upsertCausalCriticBenchmarkJSONL(path, conflict); err == nil {
		t.Fatal("conflicting terminal row was accepted")
	}
}
