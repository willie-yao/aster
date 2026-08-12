package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	remediationInvestigationManifest       = "testdata/benchmarks/remediation-investigation-v3.json"
	remediationInvestigationManifestSHA256 = "84620efb7e127207d6891bdfaa8614cb9454213d0f1155d32b60a2cc97dace72"
)

type remediationBenchmarkManifest struct {
	Version int                        `json:"version"`
	Cases   []remediationBenchmarkCase `json:"cases"`
}

type remediationBenchmarkCase struct {
	ID                   string                                     `json:"id"`
	Category             string                                     `json:"category"`
	JobID                string                                     `json:"job_id"`
	JobName              string                                     `json:"job_name"`
	RootCause            string                                     `json:"root_cause"`
	Confidence           string                                     `json:"confidence"`
	Repository           sourceinvestigation.Repository             `json:"repository"`
	FailureRevision      string                                     `json:"failure_revision"`
	DestinationPolicy    remediationinvestigation.DestinationPolicy `json:"destination_policy"`
	RelevantFiles        []string                                   `json:"relevant_files"`
	SourceFiles          map[string]string                          `json:"source_files"`
	FailureSourceFiles   map[string]string                          `json:"failure_source_files"`
	ArtifactFiles        map[string]string                          `json:"artifact_files"`
	Expected             remediationBenchmarkExpected               `json:"expected"`
	EffectiveInputSHA256 string                                     `json:"effective_input_sha256"`
}

type remediationBenchmarkExpected struct {
	Classification remediationinvestigation.Classification `json:"classification"`
	Repository     *sourceinvestigation.Repository         `json:"repository"`
	Target         *models.RemediationTarget               `json:"target"`
}

type remediationBenchmarkTrial struct {
	CaseID                   string                                  `json:"case_id"`
	Category                 string                                  `json:"category"`
	Repetition               int                                     `json:"repetition"`
	TrialStatus              string                                  `json:"trial_status"`
	EngineCommit             string                                  `json:"engine_commit"`
	ManifestSHA256           string                                  `json:"manifest_sha256"`
	EffectiveInputSHA256     string                                  `json:"effective_input_sha256"`
	PatternHash              string                                  `json:"pattern_hash"`
	CausalGroupHash          string                                  `json:"causal_group_hash"`
	ResultDigest             string                                  `json:"result_digest,omitempty"`
	EvidenceCatalogDigest    string                                  `json:"evidence_catalog_digest,omitempty"`
	ProviderFingerprint      string                                  `json:"provider_fingerprint"`
	Model                    string                                  `json:"model"`
	APIMode                  string                                  `json:"api_mode"`
	ReasoningEffort          string                                  `json:"reasoning_effort,omitempty"`
	PromptVersion            int                                     `json:"prompt_version"`
	SchemaVersion            int                                     `json:"schema_version"`
	VerificationVersion      int                                     `json:"verification_version"`
	ResultVersion            int                                     `json:"result_version"`
	ExpectedClassification   remediationinvestigation.Classification `json:"expected_classification"`
	ActualClassification     remediationinvestigation.Classification `json:"actual_classification,omitempty"`
	StructurallyValid        bool                                    `json:"structurally_valid"`
	ClassificationCorrect    bool                                    `json:"classification_correct"`
	ExpectedActionable       bool                                    `json:"expected_actionable"`
	ExpectedCandidate        bool                                    `json:"expected_candidate"`
	ActualActionable         bool                                    `json:"actual_actionable"`
	ModelCandidateKind       string                                  `json:"model_candidate_kind,omitempty"`
	ModelNonActionableReason string                                  `json:"model_non_actionable_reason,omitempty"`
	VerifiedActionable       bool                                    `json:"verified_actionable"`
	ExactTarget              bool                                    `json:"exact_target"`
	UnverifiedUnsafeProposal bool                                    `json:"unverified_unsafe_proposal"`
	VerificationStatus       string                                  `json:"verification_status"`
	UnsafeFalseAcceptance    bool                                    `json:"unsafe_false_acceptance"`
	AlreadyFixedBlocked      bool                                    `json:"already_fixed_blocked"`
	Evidence                 remediationinvestigation.EvidenceStats  `json:"evidence"`
	Metrics                  remediationinvestigation.Metrics        `json:"metrics"`
	CacheHit                 bool                                    `json:"cache_hit"`
	ErrorCode                string                                  `json:"error_code,omitempty"`
}

func TestRemediationInvestigationBenchmarkManifest(t *testing.T) {
	manifest, raw := loadRemediationBenchmarkManifest(t)
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != remediationInvestigationManifestSHA256 {
		t.Fatalf("manifest hash=%s want=%s", got, remediationInvestigationManifestSHA256)
	}
	if manifest.Version != 3 || len(manifest.Cases) != 12 {
		t.Fatalf("version=%d cases=%d", manifest.Version, len(manifest.Cases))
	}
	wantCategories := map[string]bool{
		"actionable_missing_call": true, "actionable_missing_configuration": true,
		"already_present_at_pinned_revision": true, "fixed_in_current_source": true,
		"external_dependency": true, "environment_or_infrastructure": true,
		"mitigation_only": true, "insufficient_or_ambiguous_evidence": true,
		"unsafe_conversion_webhook": true, "wrong_dependency_repository": true,
		"duplicated_or_unknown_target": true, "fabricated_symbol_or_configuration": true,
	}
	seenIDs, seenCategories := map[string]bool{}, map[string]bool{}
	actionableKinds := map[string]bool{}
	for _, benchmarkCase := range manifest.Cases {
		if seenIDs[benchmarkCase.ID] || benchmarkCase.ID == "" {
			t.Fatalf("duplicate or empty case ID %q", benchmarkCase.ID)
		}
		seenIDs[benchmarkCase.ID] = true
		if !wantCategories[benchmarkCase.Category] || seenCategories[benchmarkCase.Category] {
			t.Fatalf("invalid or duplicate category %q", benchmarkCase.Category)
		}
		seenCategories[benchmarkCase.Category] = true
		if got := remediationCaseHash(t, raw, benchmarkCase.ID); got != benchmarkCase.EffectiveInputSHA256 {
			t.Fatalf("case %s hash=%s want=%s", benchmarkCase.ID, got, benchmarkCase.EffectiveInputSHA256)
		}
		if len(benchmarkCase.SourceFiles) == 0 || len(benchmarkCase.FailureSourceFiles) == 0 {
			t.Fatalf("case %s must freeze current and failure source trees", benchmarkCase.ID)
		}
		if strings.TrimSpace(benchmarkCase.JobID) == "" || strings.TrimSpace(benchmarkCase.JobName) == "" {
			t.Fatalf("case %s must freeze exact job identity", benchmarkCase.ID)
		}
		input := remediationCaseInput(benchmarkCase, strings.Repeat("d", 16))
		if err := remediationinvestigation.ValidateFrozenInput(input); err != nil {
			t.Fatalf("case %s frozen input: %v", benchmarkCase.ID, err)
		}
		matching := remediationBenchmarkActual{Classification: benchmarkCase.Expected.Classification, Target: benchmarkCase.Expected.Target}
		if !scoreRemediationBenchmark(benchmarkCase.Expected, matching).ClassificationCorrect {
			t.Fatalf("case %s matching result failed scorer", benchmarkCase.ID)
		}
		opposite := remediationBenchmarkActual{Classification: remediationinvestigation.ClassificationInsufficientEvidence}
		if benchmarkCase.Expected.Classification == remediationinvestigation.ClassificationInsufficientEvidence {
			opposite.Classification = remediationinvestigation.ClassificationActionable
		}
		if scoreRemediationBenchmark(benchmarkCase.Expected, opposite).ClassificationCorrect {
			t.Fatalf("case %s adversarial opposite passed scorer", benchmarkCase.ID)
		}
		if benchmarkCase.Expected.Classification == remediationinvestigation.ClassificationActionable {
			if benchmarkCase.Expected.Target == nil || benchmarkCase.Expected.Repository == nil {
				t.Fatalf("case %s actionable expectation has incomplete proposal identity", benchmarkCase.ID)
			}
			actionableKinds[benchmarkCase.Expected.Target.Intent] = true
			if benchmarkCase.Expected.Target.Intent == models.RemediationIntentSetJobEnvironment {
				if benchmarkCase.JobName != benchmarkCase.Expected.Target.Job || benchmarkCase.JobID != benchmarkCase.Expected.Target.Job {
					t.Fatalf("case %s Prow job identity does not match expected target", benchmarkCase.ID)
				}
				content := benchmarkCase.SourceFiles[benchmarkCase.Expected.Target.Path]
				definitions, err := jobconfig.ParseCatalog([]byte(content), benchmarkCase.Expected.Target.Path)
				if err != nil {
					t.Fatalf("case %s Prow catalog: %v", benchmarkCase.ID, err)
				}
				matches := 0
				for _, definition := range definitions {
					if definition.Name == benchmarkCase.JobName && definition.ID() == benchmarkCase.JobID {
						matches++
					}
				}
				if matches != 1 {
					t.Fatalf("case %s frozen Prow job identity matched %d definitions", benchmarkCase.ID, matches)
				}
			}
			wrongRepository := *benchmarkCase.Expected.Repository
			wrongRepository.Owner = "wrong-owner"
			if scoreRemediationBenchmark(benchmarkCase.Expected, remediationBenchmarkActual{
				Classification: benchmarkCase.Expected.Classification,
				Repository:     &wrongRepository, Target: benchmarkCase.Expected.Target,
			}).ExactTarget {
				t.Fatalf("case %s scorer accepted the right target in the wrong repository", benchmarkCase.ID)
			}
		} else if benchmarkCase.Expected.Target != nil || benchmarkCase.Expected.Repository != nil {
			t.Fatalf("case %s non-actionable expectation carries proposal identity", benchmarkCase.ID)
		}
	}
	if len(seenCategories) != len(wantCategories) || len(actionableKinds) < 2 {
		t.Fatalf("categories=%d actionableKinds=%v", len(seenCategories), actionableKinds)
	}
}

func TestRemediationInvestigationBenchmark(t *testing.T) {
	if os.Getenv("RUN_REMEDIATION_INVESTIGATION_BENCHMARK") != "1" {
		t.Skip("set RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 for repeated private provider holdouts")
	}
	endpoint := strings.TrimSpace(os.Getenv("AI_ENDPOINT"))
	modelName := strings.TrimSpace(os.Getenv("AI_MODEL"))
	resultsPath := strings.TrimSpace(os.Getenv("REMEDIATION_BENCH_RESULTS_JSONL"))
	if endpoint == "" || modelName == "" || resultsPath == "" {
		t.Fatal("AI_ENDPOINT, AI_MODEL, and REMEDIATION_BENCH_RESULTS_JSONL are required")
	}
	apiMode := strings.TrimSpace(os.Getenv("AI_API"))
	if apiMode == "" {
		apiMode = ai.APIChatCompletions
	}
	repetitions := 3
	if value := strings.TrimSpace(os.Getenv("REMEDIATION_BENCH_REPETITIONS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 10 {
			t.Fatalf("REMEDIATION_BENCH_REPETITIONS must be 1-10")
		}
		repetitions = parsed
	}
	manifest, raw := loadRemediationBenchmarkManifest(t)
	engineCommit := remediationBenchmarkEngineCommit(t)
	caseFilter := remediationBenchmarkCaseFilter(os.Getenv("REMEDIATION_BENCH_CASES"))
	manifestSum := sha256.Sum256(raw)
	manifestHash := hex.EncodeToString(manifestSum[:])
	reasoningEffort := benchmarkReasoningEffort(t)
	if err := os.MkdirAll(filepath.Dir(resultsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_ = file.Chmod(0o600)

	for _, benchmarkCase := range manifest.Cases {
		if len(caseFilter) > 0 && !caseFilter[benchmarkCase.ID] {
			continue
		}
		for repetition := 1; repetition <= repetitions; repetition++ {
			client := ai.NewClientWithOptions(ai.Options{
				Token: os.Getenv("AI_TOKEN"), API: apiMode, Endpoint: endpoint, Model: modelName, ReasoningEffort: reasoningEffort,
			})
			input := remediationCaseInput(benchmarkCase, client.ModelFingerprint())
			row := remediationBenchmarkTrial{
				CaseID: benchmarkCase.ID, Category: benchmarkCase.Category, Repetition: repetition,
				EngineCommit: engineCommit, ManifestSHA256: manifestHash, EffectiveInputSHA256: benchmarkCase.EffectiveInputSHA256,
				PatternHash: input.PatternHash, CausalGroupHash: input.CausalGroupHash,
				ProviderFingerprint: client.ModelFingerprint(), Model: modelName, APIMode: apiMode, ReasoningEffort: string(client.ReasoningEffort()),
				PromptVersion: remediationinvestigation.PromptVersion, SchemaVersion: remediationinvestigation.SchemaVersion,
				VerificationVersion:    remediationinvestigation.VerificationVersion,
				ResultVersion:          remediationinvestigation.ResultVersion,
				ExpectedClassification: benchmarkCase.Expected.Classification,
				ExpectedActionable:     benchmarkCase.Expected.Classification == remediationinvestigation.ClassificationActionable,
				ExpectedCandidate: benchmarkCase.Expected.Classification == remediationinvestigation.ClassificationActionable ||
					benchmarkCase.Expected.Classification == remediationinvestigation.ClassificationAlreadyFixed,
				VerificationStatus: "not_run_invalid_result",
			}
			recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 1, RecentOperations: 10})
			if err != nil {
				t.Fatal(err)
			}
			cache, err := remediationinvestigation.NewCache("", remediationinvestigation.CacheOptions{})
			if err != nil {
				t.Fatal(err)
			}
			source := newBenchmarkSource(benchmarkCase)
			browser := benchmarkBrowser{files: benchmarkCase.ArtifactFiles}
			service, err := remediationinvestigation.NewService(client, source, cache, remediationinvestigation.ServiceOptions{
				Timeout: 10 * time.Minute, UsageRecorder: recorder,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := service.Investigate(t.Context(), input, browser, false)
			if runErr != nil {
				row.TrialStatus, row.ErrorCode = remediationTrialFailure(runErr)
				row.Metrics = remediationBenchmarkUsageMetrics(recorder)
			} else {
				row.TrialStatus = "valid_result"
				row.StructurallyValid = true
				row.ActualActionable = len(result.Entry.Result.Hypotheses) > 0
				target := remediationFirstHypothesisTarget(result.Entry.Result)
				row.ModelCandidateKind = remediationCandidateKind(target)
				if reason := remediationResultNonActionableReason(result.Entry.Result); reason != nil {
					row.ModelNonActionableReason = string(*reason)
				}
				row.ResultDigest = result.Entry.ResultDigest
				row.EvidenceCatalogDigest = result.Entry.EvidenceCatalogDigest
				row.Evidence = result.Entry.Provenance.Evidence
				row.Metrics = result.Entry.Provenance.Metrics
				row.CacheHit = result.CacheHit
				verifier, verifyErr := remediationinvestigation.NewVerifier(source)
				if verifyErr != nil {
					t.Fatal(verifyErr)
				}
				verified, verifyErr := verifier.Verify(t.Context(), input, result.Entry, browser)
				if verifyErr != nil {
					row.VerificationStatus = "verification_error"
				} else {
					row.ActualClassification = verified.Classification
					row.VerificationStatus = string(verified.Classification)
					row.VerifiedActionable = verified.Classification == remediationinvestigation.ClassificationActionable && verified.Proposal != nil
					actual := remediationBenchmarkActual{Classification: verified.Classification}
					if verified.Proposal != nil {
						repository := verified.Proposal.Repository
						target := verified.Proposal.Target
						actual.Repository = &repository
						actual.Target = &target
					}
					score := scoreRemediationBenchmark(benchmarkCase.Expected, actual)
					row.ClassificationCorrect = score.ClassificationCorrect
					row.ExactTarget = score.ExactTarget
				}
				row.UnverifiedUnsafeProposal = row.ActualActionable && !row.ExpectedCandidate
				row.UnsafeFalseAcceptance = row.VerifiedActionable && !row.ExpectedActionable
				row.AlreadyFixedBlocked = benchmarkCase.Expected.Classification != remediationinvestigation.ClassificationAlreadyFixed || !row.VerifiedActionable
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(append(encoded, '\n')); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func remediationFirstHypothesisTarget(result remediationinvestigation.Result) remediationinvestigation.CandidateTarget {
	if len(result.Hypotheses) == 0 {
		return nil
	}
	return result.Hypotheses[0].Target
}

func remediationResultNonActionableReason(result remediationinvestigation.Result) *remediationinvestigation.NonActionableReason {
	if result.NonActionable == nil {
		return nil
	}
	reason := result.NonActionable.NonActionableReason
	return &reason
}

func remediationResultEvidenceIDs(result remediationinvestigation.Result) []string {
	seen := map[string]bool{}
	var ids []string
	for _, hypothesis := range result.Hypotheses {
		for _, id := range hypothesis.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if result.NonActionable != nil {
		for _, id := range result.NonActionable.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func remediationCandidateKind(candidate remediationinvestigation.CandidateTarget) string {
	switch candidate.(type) {
	case *remediationinvestigation.RequiredCallCandidate:
		return string(remediationinvestigation.CandidateRequiredCall)
	case *remediationinvestigation.SymbolAdditionCandidate:
		return string(remediationinvestigation.CandidateSymbolAddition)
	case *remediationinvestigation.ProwEnvironmentEntryCandidate:
		return string(remediationinvestigation.CandidateProwEnvironmentEntry)
	case *remediationinvestigation.ConfigurationFieldCandidate:
		return string(remediationinvestigation.CandidateConfigurationField)
	default:
		return ""
	}
}

func remediationBenchmarkUsageMetrics(recorder *aiusage.Recorder) remediationinvestigation.Metrics {
	if recorder == nil {
		return remediationinvestigation.Metrics{}
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.RecentOperations) == 0 {
		return remediationinvestigation.Metrics{}
	}
	usage := snapshot.RecentOperations[0]
	elapsed := 0
	started, startErr := time.Parse(time.RFC3339Nano, usage.StartedAt)
	completed, completedErr := time.Parse(time.RFC3339Nano, usage.CompletedAt)
	if startErr == nil && completedErr == nil {
		elapsed = int(completed.Sub(started).Milliseconds())
	}
	return remediationinvestigation.Metrics{
		ElapsedMs: elapsed, ModelRequests: usage.ModelRequests, ReportedRequests: usage.ReportedRequests,
		UnreportedRequests: usage.UnreportedRequests, CoverageCountsKnown: usage.CoverageCountsKnown,
		UsageInvalid: usage.UsageInvalid, Currency: usage.Currency, PricingHash: usage.PricingHash,
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		OutputTokens: usage.OutputTokens, ReasoningTokens: usage.ReasoningTokens,
		EstimatedCostNanos: usage.EstimatedCostNanos,
	}
}

func remediationBenchmarkEngineCommit(t *testing.T) string {
	t.Helper()
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = "../.."
	output, err := status.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(output))) != 0 {
		t.Fatal("remediation provider benchmark requires a clean committed worktree")
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = "../.."
	output, err = command.Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatalf("invalid engine commit %q", commit)
	}
	return commit
}

func remediationBenchmarkCaseFilter(value string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = true
		}
	}
	return out
}

type remediationBenchmarkActual struct {
	Classification remediationinvestigation.Classification
	Repository     *sourceinvestigation.Repository
	Target         *models.RemediationTarget
}

type remediationBenchmarkScore struct {
	ClassificationCorrect bool
	ExactTarget           bool
}

func scoreRemediationBenchmark(expected remediationBenchmarkExpected, actual remediationBenchmarkActual) remediationBenchmarkScore {
	score := remediationBenchmarkScore{ClassificationCorrect: expected.Classification == actual.Classification}
	if expected.Target == nil || expected.Repository == nil {
		score.ExactTarget = actual.Target == nil && actual.Repository == nil
		return score
	}
	score.ExactTarget = actual.Target != nil && actual.Repository != nil &&
		*expected.Target == *actual.Target && *expected.Repository == *actual.Repository
	return score
}

func remediationCaseInput(benchmarkCase remediationBenchmarkCase, providerFingerprint string) remediationinvestigation.FrozenInput {
	builds := []string{"1", "2"}
	group := models.PatternCausalGroup{Builds: builds, RootCause: benchmarkCase.RootCause, Confidence: benchmarkCase.Confidence}
	patternID := benchmarkShortHash("pattern\x00" + benchmarkCase.ID)
	group.ContentHash = models.PatternCausalGroupHash(group)
	group.ID = models.PatternCausalGroupID(patternID, group)
	analyses := make([]remediationinvestigation.AnalysisReference, 0, len(builds))
	buildRefs := make([]remediationinvestigation.BuildReference, 0, len(builds))
	failureSource := sourceinvestigation.Repository{
		Owner: benchmarkCase.Repository.Owner, Name: benchmarkCase.Repository.Name, Revision: benchmarkCase.FailureRevision,
	}
	for _, buildID := range builds {
		analysisGeneratedAt := "2026-08-11T00:00:0" + buildID + "Z"
		analyses = append(analyses, remediationinvestigation.AnalysisReference{
			BuildID: buildID, TestName: "benchmark test", GeneratedAt: analysisGeneratedAt,
			RootCause: benchmarkCase.RootCause, Severity: "High", RelevantFiles: benchmarkCase.RelevantFiles,
			Evidence:         []models.EvidenceCitation{{Path: "log.txt", LineStart: 1, LineEnd: 1, Quote: strings.TrimSpace(benchmarkCase.ArtifactFiles["builds/"+buildID+"/log.txt"])}},
			SourceRepository: &failureSource,
		})
		buildRefs = append(buildRefs, remediationinvestigation.BuildReference{
			BuildID: buildID, BuildPrefix: "frozen/" + benchmarkCase.ID + "/" + buildID + "/", Source: &failureSource,
		})
	}
	consumerPrompt := "Use pinned source and recurring-build evidence. Prefer a safe non-actionable classification over an invented target."
	return remediationinvestigation.FrozenInput{
		PatternID: patternID, PatternHash: strings.Repeat(benchmarkShortHash("pattern-hash\x00"+benchmarkCase.ID), 4),
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash, JobID: benchmarkCase.JobID,
		JobName:    benchmarkCase.JobName,
		Recurrence: models.PatternRecurrenceSharedCause, Group: group,
		Builds: buildRefs, Analyses: analyses, RelevantFiles: benchmarkCase.RelevantFiles,
		InvestigationSource: benchmarkCase.Repository, DestinationPolicy: benchmarkCase.DestinationPolicy,
		ConsumerPrompt: consumerPrompt, ConsumerPromptHash: remediationinvestigation.HashText(consumerPrompt),
		SkillHash: strings.Repeat("c", 64), ProviderFingerprint: providerFingerprint,
		Versions: remediationinvestigation.CurrentVersions(),
	}
}

func loadRemediationBenchmarkManifest(t *testing.T) (remediationBenchmarkManifest, []byte) {
	t.Helper()
	raw, err := os.ReadFile(remediationInvestigationManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest remediationBenchmarkManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func remediationCaseHash(t *testing.T, manifestRaw []byte, caseID string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(manifestRaw, &document); err != nil {
		t.Fatal(err)
	}
	cases, _ := document["cases"].([]any)
	for _, value := range cases {
		benchmarkCase, _ := value.(map[string]any)
		if benchmarkCase["id"] != caseID {
			continue
		}
		delete(benchmarkCase, "effective_input_sha256")
		encoded, err := json.Marshal(benchmarkCase)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(encoded)
		return hex.EncodeToString(sum[:])
	}
	t.Fatalf("case %s not found", caseID)
	return ""
}

func benchmarkShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func remediationTrialFailure(err error) (string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "timeout"
	case errors.Is(err, context.Canceled):
		return "runtime_failure", "cancelled"
	case strings.Contains(err.Error(), "finalization"):
		if code := remediationinvestigation.ErrorCode(err); code != "" {
			return "invalid_result", code
		}
		return "invalid_result", "structured_finalization"
	case strings.Contains(err.Error(), "evidence phase"):
		return "no_result", "evidence_phase"
	default:
		return "runtime_failure", "runtime"
	}
}

type benchmarkSource struct{ files map[string]map[string]string }

func newBenchmarkSource(benchmarkCase remediationBenchmarkCase) benchmarkSource {
	current := benchmarkCase.Repository
	failure := sourceinvestigation.Repository{Owner: current.Owner, Name: current.Name, Revision: benchmarkCase.FailureRevision}
	files := map[string]map[string]string{benchmarkSourceKey(current): benchmarkCase.SourceFiles}
	files[benchmarkSourceKey(failure)] = benchmarkCase.FailureSourceFiles
	return benchmarkSource{files: files}
}

func benchmarkSourceKey(repository sourceinvestigation.Repository) string {
	return strings.ToLower(repository.Owner + "/" + repository.Name + "@" + repository.Revision)
}

func (s benchmarkSource) ListFiles(_ context.Context, repository sourceinvestigation.Repository) ([]string, error) {
	source := s.files[benchmarkSourceKey(repository)]
	if source == nil {
		return nil, fmt.Errorf("revision unavailable")
	}
	paths := make([]string, 0, len(source))
	for file := range source {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	return paths, nil
}
func (s benchmarkSource) ReadFile(_ context.Context, repository sourceinvestigation.Repository, file string) (string, error) {
	content, ok := s.files[benchmarkSourceKey(repository)][file]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return content, nil
}

type benchmarkBrowser struct{ files map[string]string }

func (benchmarkBrowser) BuildRoot() string { return "frozen remediation benchmark builds" }
func (benchmarkBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return &artifacts.Listing{}, nil
}
func (b benchmarkBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	paths := make([]string, 0, len(b.files))
	for file := range b.files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	return paths, false, nil
}
func (b benchmarkBrowser) Read(_ context.Context, file string, offset, length int) ([]byte, int64, error) {
	content, ok := b.files[file]
	if !ok {
		return nil, -1, fmt.Errorf("not found")
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	end := offset + length
	if end > len(content) {
		end = len(content)
	}
	return []byte(content[offset:end]), int64(len(content)), nil
}
func (b benchmarkBrowser) Tail(ctx context.Context, file string, _ int, maxBytes int) (*artifacts.TailResult, error) {
	content, size, err := b.Read(ctx, file, 0, maxBytes)
	return &artifacts.TailResult{FileSize: size, LinesReturned: strings.Count(string(content), "\n") + 1, Content: content}, err
}
func (b benchmarkBrowser) Grep(_ context.Context, file string, re *regexp.Regexp, _ int, maxMatches, _ int, _ int) (*artifacts.GrepResult, error) {
	content, ok := b.files[file]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	result := &artifacts.GrepResult{FileSize: int64(len(content)), BytesScanned: int64(len(content))}
	for index, line := range strings.Split(content, "\n") {
		if re.MatchString(line) && len(result.Matches) < maxMatches {
			result.TotalMatches++
			result.Matches = append(result.Matches, artifacts.GrepMatch{LineNo: index + 1, Context: []string{"> " + line}})
		}
	}
	return result, nil
}

func TestRemediationInvestigationPrivateCacheIsStrippedFromPages(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "reusable-deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, path := range []string{
		`.remediation-investigations/cache.json`,
		`.remediation-investigations/cache.json.tmp-*`,
		`.remediation-investigations/cache.lock`,
	} {
		if !strings.Contains(text, path) {
			t.Fatalf("Pages workflow does not strip %s", path)
		}
	}
	if strings.Contains(text, `rm -rf "$d"/.remediation-investigations`) {
		t.Fatal("Pages workflow uses recursive deletion for remediation cache")
	}
}

func TestRemediationBenchmarkSourceDistinguishesCurrentAndFailureRevisions(t *testing.T) {
	manifest, _ := loadRemediationBenchmarkManifest(t)
	for _, benchmarkCase := range manifest.Cases {
		if benchmarkCase.ID != "fixed-in-current-source" {
			continue
		}
		source := newBenchmarkSource(benchmarkCase)
		current, err := source.ReadFile(t.Context(), benchmarkCase.Repository, "controllers/reconcile.go")
		if err != nil {
			t.Fatal(err)
		}
		failureRepository := sourceinvestigation.Repository{Owner: benchmarkCase.Repository.Owner, Name: benchmarkCase.Repository.Name, Revision: benchmarkCase.FailureRevision}
		failure, err := source.ReadFile(t.Context(), failureRepository, "controllers/reconcile.go")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(current, "applyFix()") || strings.Contains(failure, "applyFix()\n\treturn") {
			t.Fatalf("current and failure source transition is not frozen: current=%q failure=%q", current, failure)
		}
		return
	}
	t.Fatal("fixed-in-current-source case not found")
}
