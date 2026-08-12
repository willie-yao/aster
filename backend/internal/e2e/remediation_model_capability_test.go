package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
	"gopkg.in/yaml.v3"
)

const (
	copilotResponsesEndpoint                 = "https://api.githubcopilot.com/responses"
	remediationModelCapabilityManifest       = "testdata/benchmarks/remediation-investigation-temporal-v1.json"
	remediationModelCapabilityManifestSHA256 = "682fbec371afeeb953085016b9b0755f294cb2ed3f36735f379afda18973d529"
	remediationModelCapabilityRepetitions    = 3
)

type remediationModelCapabilityManifestDocument struct {
	Version              int                                      `json:"version"`
	Catalog              remediationModelCapabilityCatalog        `json:"catalog"`
	Versions             remediationinvestigation.Versions        `json:"versions"`
	Transport            remediationModelCapabilityTransport      `json:"transport"`
	Budgets              remediationModelCapabilityBudgets        `json:"budgets"`
	ConsumerPrompt       string                                   `json:"consumer_prompt"`
	ConsumerPromptSHA256 string                                   `json:"consumer_prompt_sha256"`
	Scorer               remediationModelCapabilityScorer         `json:"scorer"`
	Diagnostics          []string                                 `json:"diagnostics"`
	PublicEvidence       remediationModelCapabilityPublicEvidence `json:"public_evidence"`
	Cases                []remediationModelCapabilityCase         `json:"cases"`
}

type remediationModelCapabilityScorer struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type remediationModelCapabilityCatalog struct {
	FullCatalogSHA256 string                                   `json:"full_catalog_sha256"`
	QueriedAt         string                                   `json:"queried_at"`
	SelectionFixture  string                                   `json:"selection_fixture"`
	SelectionSHA256   string                                   `json:"selection_sha256"`
	Models            []remediationModelCapabilityCatalogModel `json:"models"`
}

type remediationModelCapabilityCatalogModel struct {
	ID                string `json:"id"`
	SupportedEndpoint string `json:"supported_endpoint"`
}

type remediationModelCapabilityTransport struct {
	API                        string `json:"api"`
	EndpointPath               string `json:"endpoint_path"`
	Provider                   string `json:"provider"`
	Store                      bool   `json:"store"`
	TransportFingerprintSHA256 string `json:"transport_fingerprint_sha256"`
}

type remediationModelCapabilityPublicEvidence struct {
	TriageURI                    string `json:"triage_uri"`
	TriageGzipSHA256             string `json:"triage_gzip_sha256"`
	TriageExtractedRowsSHA256    string `json:"triage_extracted_rows_sha256"`
	TriageExtractedFixture       string `json:"triage_extracted_fixture"`
	TriageExtractedFixtureSHA256 string `json:"triage_extracted_fixture_sha256"`
	RepairPRAPIURI               string `json:"repair_pr_api_uri"`
	RepairPRExtractSHA256        string `json:"repair_pr_extract_sha256"`
	RepairPRExtractFixture       string `json:"repair_pr_extract_fixture"`
	RepairPRExtractFixtureSHA256 string `json:"repair_pr_extract_fixture_sha256"`
	RepairCommitAPIURI           string `json:"repair_commit_api_uri"`
	RepairCommitFixture          string `json:"repair_commit_fixture"`
	RepairCommitFixtureSHA256    string `json:"repair_commit_fixture_sha256"`
	JobConfigCommitAPIURI        string `json:"job_config_commit_api_uri"`
	JobConfigCommitFixture       string `json:"job_config_commit_fixture"`
	JobConfigCommitFixtureSHA256 string `json:"job_config_commit_fixture_sha256"`
}

type remediationModelCapabilityTriageRow struct {
	BuildID     string `json:"build_id"`
	Elapsed     int    `json:"elapsed"`
	PR          string `json:"pr"`
	Result      string `json:"result"`
	Started     int64  `json:"started"`
	TestsFailed int    `json:"tests_failed"`
	TestsRun    int    `json:"tests_run"`
}

type remediationModelCapabilityBudgets struct {
	Timeout           string `json:"timeout"`
	MaxIters          int    `json:"max_iters"`
	ContextByteBudget int    `json:"context_byte_budget"`
	Repetitions       int    `json:"repetitions"`
}

type remediationModelCapabilityCase struct {
	ID                     string                                     `json:"id"`
	TemporalState          string                                     `json:"temporal_state"`
	JobID                  string                                     `json:"job_id"`
	JobName                string                                     `json:"job_name"`
	RootCause              string                                     `json:"root_cause"`
	Confidence             string                                     `json:"confidence"`
	RelevantFiles          []string                                   `json:"relevant_files"`
	GroundingSourcePath    string                                     `json:"grounding_source_path"`
	Builds                 []remediationModelCapabilityBuild          `json:"builds"`
	InvestigationSource    sourceinvestigation.Repository             `json:"investigation_source"`
	DestinationPolicy      remediationinvestigation.DestinationPolicy `json:"destination_policy"`
	SourceSnapshots        []remediationHistorySource                 `json:"source_snapshots"`
	ScorerPrivate          remediationModelCapabilityOracle           `json:"scorer_private"`
	ExpectedClassification remediationinvestigation.Classification    `json:"expected_classification"`
	EffectiveInputSHA256   string                                     `json:"effective_input_sha256"`
}

type remediationModelCapabilityBuild struct {
	BuildID             string `json:"build_id"`
	StartedUnix         int64  `json:"started_unix"`
	ElapsedSeconds      int    `json:"elapsed_seconds"`
	Result              string `json:"result"`
	TestsFailed         int    `json:"tests_failed"`
	TestsRun            int    `json:"tests_run"`
	ProwURL             string `json:"prow_url"`
	ArtifactFixture     string `json:"artifact_fixture"`
	ArtifactSHA256      string `json:"artifact_sha256"`
	AnalysisGeneratedAt string `json:"analysis_generated_at"`
	TestName            string `json:"test_name"`
	SourceRevision      string `json:"source_revision"`
}

type remediationModelCapabilityOracle struct {
	KnownTarget           models.RemediationTarget `json:"known_target"`
	PreFixRevision        string                   `json:"pre_fix_revision"`
	FixedRevision         string                   `json:"fixed_revision"`
	RepairCommit          string                   `json:"repair_commit"`
	RepairCommittedAt     string                   `json:"repair_committed_at"`
	FailureSourceRevision string                   `json:"failure_source_revision"`
	FailureSourceCommitAt string                   `json:"failure_source_commit_at"`
}

type remediationModelCapabilityTrial struct {
	CaseID                       string                                  `json:"case_id"`
	TemporalState                string                                  `json:"temporal_state"`
	Repetition                   int                                     `json:"repetition"`
	Model                        string                                  `json:"model"`
	APIMode                      string                                  `json:"api_mode"`
	ProviderIdentity             string                                  `json:"provider_identity"`
	ProviderFingerprint          string                                  `json:"provider_fingerprint"`
	TransportFingerprint         string                                  `json:"transport_fingerprint"`
	TrialStatus                  string                                  `json:"trial_status"`
	ErrorCode                    string                                  `json:"error_code,omitempty"`
	EngineCommit                 string                                  `json:"engine_commit"`
	ManifestSHA256               string                                  `json:"manifest_sha256"`
	EffectiveInputSHA256         string                                  `json:"effective_input_sha256"`
	StructurallyValid            bool                                    `json:"structurally_valid"`
	CandidateKind                string                                  `json:"candidate_kind,omitempty"`
	CandidateIdentity            *models.RemediationTarget               `json:"candidate_identity,omitempty"`
	ExactIdentity                bool                                    `json:"exact_identity"`
	SelectedEvidenceIDs          []string                                `json:"selected_evidence_ids,omitempty"`
	Evidence                     remediationinvestigation.EvidenceStats  `json:"evidence"`
	Metrics                      remediationinvestigation.Metrics        `json:"metrics"`
	ActualClassification         remediationinvestigation.Classification `json:"actual_classification,omitempty"`
	VerificationStatus           string                                  `json:"verification_status"`
	VerifiedActionable           bool                                    `json:"verified_actionable"`
	CorrectTemporalResult        bool                                    `json:"correct_temporal_result"`
	UnsafeAcceptance             bool                                    `json:"unsafe_acceptance"`
	CostAvailable                bool                                    `json:"cost_available"`
	MemoMentionsTargetJob        bool                                    `json:"memo_mentions_target_job"`
	MemoMentionsTargetContainer  bool                                    `json:"memo_mentions_target_container"`
	MemoMentionsTargetName       bool                                    `json:"memo_mentions_target_name"`
	MemoMentionsTargetValue      bool                                    `json:"memo_mentions_target_value"`
	FinalResultContainsCandidate bool                                    `json:"final_result_contains_candidate"`
}

type remediationModelCapabilityMemoDiagnostics struct {
	Job       bool
	Container bool
	Name      bool
	Value     bool
}

type remediationModelCapabilityDiagnosticModel struct {
	remediationinvestigation.Model
	target      models.RemediationTarget
	diagnostics *remediationModelCapabilityMemoDiagnostics
}

func (m *remediationModelCapabilityDiagnosticModel) ToolLoop(ctx context.Context, system, user string, registry *tools.Registry, enabled []string, env *tools.Env, options ai.ToolLoopOptions) (string, error) {
	memo, err := m.Model.ToolLoop(ctx, system, user, registry, enabled, env, options)
	if err != nil {
		return memo, err
	}
	lower := strings.ToLower(memo)
	m.diagnostics.Job = containsDiagnosticTerm(lower, m.target.Job)
	m.diagnostics.Container = containsDiagnosticTerm(lower, m.target.Container)
	m.diagnostics.Name = containsDiagnosticTerm(lower, m.target.Name)
	m.diagnostics.Value = containsDiagnosticTerm(lower, m.target.Value)
	return memo, nil
}

func containsDiagnosticTerm(lowerMemo, value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	pattern := `(^|[^a-z0-9_.-])` + regexp.QuoteMeta(value) + `([^a-z0-9_.-]|$)`
	return regexp.MustCompile(pattern).FindStringIndex(lowerMemo) != nil
}

func TestRemediationModelCapabilityManifestAndPreflight(t *testing.T) {
	manifest, raw := loadRemediationModelCapabilityManifest(t)
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != remediationModelCapabilityManifestSHA256 {
		t.Fatalf("manifest hash = %s, want %s", got, remediationModelCapabilityManifestSHA256)
	}
	if manifest.Version != 1 || len(manifest.Cases) != 2 || manifest.Budgets.Repetitions != remediationModelCapabilityRepetitions {
		t.Fatalf("version=%d cases=%d repetitions=%d", manifest.Version, len(manifest.Cases), manifest.Budgets.Repetitions)
	}
	wantDiagnostics := []string{
		"memo_mentions_target_job", "memo_mentions_target_container", "memo_mentions_target_name",
		"memo_mentions_target_value", "final_result_contains_candidate",
	}
	if !slices.Equal(manifest.Diagnostics, wantDiagnostics) {
		t.Fatalf("diagnostics=%v want=%v", manifest.Diagnostics, wantDiagnostics)
	}
	if manifest.Transport.API != ai.APIResponses || manifest.Transport.EndpointPath != "/responses" ||
		manifest.Transport.Provider != "github_copilot" || manifest.Transport.Store ||
		manifest.Transport.TransportFingerprintSHA256 != "f040744e3082f5cb72da45d764f9975e81569acc43db4e2d18012a3b878214ca" {
		t.Fatalf("transport=%+v", manifest.Transport)
	}
	if manifest.Catalog.FullCatalogSHA256 != "2e2e2b6488ac30ed770f0b6357ab8c04997464a527245ee28cd708c5e6a8026d" {
		t.Fatalf("full Copilot catalog hash=%s", manifest.Catalog.FullCatalogSHA256)
	}
	if _, err := time.Parse(time.RFC3339, manifest.Catalog.QueriedAt); err != nil {
		t.Fatalf("catalog queried_at=%q: %v", manifest.Catalog.QueriedAt, err)
	}
	verifyFixtureHash(t, manifest.Catalog.SelectionFixture, manifest.Catalog.SelectionSHA256)
	selectionRaw, err := os.ReadFile(manifest.Catalog.SelectionFixture)
	if err != nil {
		t.Fatal(err)
	}
	var selection []struct {
		ID                 string   `json:"id"`
		SupportedEndpoints []string `json:"supported_endpoints"`
	}
	if err := json.Unmarshal(selectionRaw, &selection); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-5.4", "gpt-5.6-sol"} {
		if !catalogSupportsModel(manifest.Catalog, model, "/responses") {
			t.Fatalf("catalog does not freeze %s with /responses", model)
		}
		found := false
		for _, item := range selection {
			found = found || item.ID == model && slices.Contains(item.SupportedEndpoints, "/responses")
		}
		if !found {
			t.Fatalf("catalog selection does not contain exact model %s with /responses", model)
		}
	}
	if remediationinvestigation.CurrentVersions() != manifest.Versions {
		t.Fatalf("versions=%+v current=%+v", manifest.Versions, remediationinvestigation.CurrentVersions())
	}
	if remediationinvestigation.HashText(manifest.ConsumerPrompt) != manifest.ConsumerPromptSHA256 {
		t.Fatal("consumer prompt hash mismatch")
	}
	verifyFixtureHash(t, filepath.Join("..", "..", "..", manifest.Scorer.Path), manifest.Scorer.SHA256)
	verifyRemediationModelCapabilityPublicEvidence(t, manifest)
	for _, capabilityCase := range manifest.Cases {
		for _, build := range capabilityCase.Builds {
			verifyFixtureHash(t, build.ArtifactFixture, build.ArtifactSHA256)
		}
		for _, snapshot := range capabilityCase.SourceSnapshots {
			for _, file := range snapshot.Files {
				verifyFixtureHash(t, file.Fixture, file.SHA256)
			}
		}
		if got := remediationModelCapabilityCaseHash(t, raw, capabilityCase.ID); got != capabilityCase.EffectiveInputSHA256 {
			t.Fatalf("case %s hash = %s, want %s", capabilityCase.ID, got, capabilityCase.EffectiveInputSHA256)
		}
		if err := preflightRemediationTemporalOracle(t, capabilityCase); err != nil {
			t.Fatalf("case %s preflight: %v", capabilityCase.ID, err)
		}
	}
	pre := temporalCaseByState(t, manifest.Cases, "pre_fix")
	input, _, _ := remediationModelCapabilityInput(t, pre, strings.Repeat("d", 16))
	encoded, _ := json.Marshal(input)
	for _, forbidden := range []string{pre.ScorerPrivate.FixedRevision, pre.ScorerPrivate.KnownTarget.Value, string(pre.ExpectedClassification), "already_fixed"} {
		if forbidden != "" && strings.Contains(string(encoded), forbidden) {
			t.Fatalf("pre-fix provider input exposes scorer-private value %q", forbidden)
		}
	}
}

func TestRemediationModelCapabilityScorerRejectsIncompleteAndDuplicateTrials(t *testing.T) {
	rows := make([]map[string]any, 0, 12)
	for _, model := range []string{"gpt-5.4", "gpt-5.6-sol"} {
		for _, state := range []string{"pre_fix", "post_fix"} {
			caseID := "capg-kubernetes-version-" + strings.ReplaceAll(state, "_", "-")
			for repetition := 1; repetition <= 3; repetition++ {
				rows = append(rows, map[string]any{
					"model": model, "temporal_state": state, "case_id": caseID, "repetition": repetition,
					"engine_commit": "commit", "manifest_sha256": "manifest", "api_mode": ai.APIResponses,
					"provider_identity": "github_copilot", "provider_fingerprint": model + "-fingerprint",
					"transport_fingerprint":  "f040744e3082f5cb72da45d764f9975e81569acc43db4e2d18012a3b878214ca",
					"effective_input_sha256": state + "-input", "trial_status": "no_result",
					"memo_mentions_target_job": false, "memo_mentions_target_container": false,
					"memo_mentions_target_name": false, "memo_mentions_target_value": false,
					"final_result_contains_candidate": false,
				})
			}
		}
	}
	run := func(name string, input []map[string]any, wantSuccess bool, wantText string, extraArgs ...string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), name+".jsonl")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for _, row := range input {
			if err := encoder.Encode(row); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		args := []string{filepath.Join("..", "..", "..", "hack", "summarize-remediation-model-capability.py"), path}
		args = append(args, extraArgs...)
		command := exec.CommandContext(t.Context(), "python3", args...)
		output, err := command.CombinedOutput()
		if wantSuccess && err != nil {
			t.Fatalf("scorer failed: %v\n%s", err, output)
		}
		if !wantSuccess && (err == nil || !strings.Contains(string(output), wantText)) {
			t.Fatalf("scorer error=%v output=%s", err, output)
		}
	}
	run("complete", rows, true, "")
	run("gpt-5.4-only", rows[:6], true, "", "--model", "gpt-5.4")
	run("missing", rows[:len(rows)-1], false, "expected exactly 12 trials")
	duplicate := append([]map[string]any(nil), rows...)
	duplicate[len(duplicate)-1] = duplicate[0]
	run("duplicate", duplicate, false, "duplicate trial")
}

func TestRemediationModelCapabilityBenchmark(t *testing.T) {
	if os.Getenv("RUN_REMEDIATION_MODEL_CAPABILITY_BENCHMARK") != "1" {
		t.Skip("set RUN_REMEDIATION_MODEL_CAPABILITY_BENCHMARK=1 for the private provider comparison")
	}
	endpoint, modelName := strings.TrimSpace(os.Getenv("AI_ENDPOINT")), strings.TrimSpace(os.Getenv("AI_MODEL"))
	resultsPath := strings.TrimSpace(os.Getenv("REMEDIATION_MODEL_CAPABILITY_RESULTS_JSONL"))
	if endpoint == "" || modelName == "" || resultsPath == "" {
		t.Fatal("AI_ENDPOINT, AI_MODEL, and REMEDIATION_MODEL_CAPABILITY_RESULTS_JSONL are required")
	}
	manifest, raw := loadRemediationModelCapabilityManifest(t)
	if !catalogSupportsModel(manifest.Catalog, modelName, "/responses") {
		t.Fatalf("model %q is not frozen with /responses support", modelName)
	}
	if endpoint != copilotResponsesEndpoint {
		t.Fatalf("AI_ENDPOINT must be the exact Copilot Responses endpoint")
	}
	if value := strings.TrimSpace(os.Getenv("AI_API")); value != "" && value != ai.APIResponses {
		t.Fatalf("AI_API must be %q", ai.APIResponses)
	}
	repetitions := manifest.Budgets.Repetitions
	if value := strings.TrimSpace(os.Getenv("REMEDIATION_MODEL_CAPABILITY_REPETITIONS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > manifest.Budgets.Repetitions {
			t.Fatalf("REMEDIATION_MODEL_CAPABILITY_REPETITIONS must be 1-%d", manifest.Budgets.Repetitions)
		}
		repetitions = parsed
	}
	caseFilter := remediationModelCapabilityCaseFilter(os.Getenv("REMEDIATION_MODEL_CAPABILITY_CASES"))
	for _, capabilityCase := range manifest.Cases {
		if err := preflightRemediationTemporalOracle(t, capabilityCase); err != nil {
			t.Fatalf("case %s preflight failed before provider request: %v", capabilityCase.ID, err)
		}
	}
	timeout, err := time.ParseDuration(manifest.Budgets.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	engineCommit := remediationBenchmarkEngineCommit(t)
	manifestSum := sha256.Sum256(raw)
	if err := os.MkdirAll(filepath.Dir(resultsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, capabilityCase := range manifest.Cases {
		if len(caseFilter) > 0 && !caseFilter[capabilityCase.ID] {
			continue
		}
		for repetition := 1; repetition <= repetitions; repetition++ {
			client := ai.NewClientWithOptions(ai.Options{Token: os.Getenv("AI_TOKEN"), API: ai.APIResponses, Endpoint: endpoint, Model: modelName})
			input, source, browser := remediationModelCapabilityInput(t, capabilityCase, client.ModelFingerprint())
			diagnostics := &remediationModelCapabilityMemoDiagnostics{}
			model := &remediationModelCapabilityDiagnosticModel{Model: client, target: capabilityCase.ScorerPrivate.KnownTarget, diagnostics: diagnostics}
			row := remediationModelCapabilityTrial{
				CaseID: capabilityCase.ID, TemporalState: capabilityCase.TemporalState, Repetition: repetition,
				Model: modelName, APIMode: ai.APIResponses, ProviderIdentity: manifest.Transport.Provider,
				ProviderFingerprint: client.ModelFingerprint(), TransportFingerprint: manifest.Transport.TransportFingerprintSHA256,
				TrialStatus: "no_result", EngineCommit: engineCommit, ManifestSHA256: hex.EncodeToString(manifestSum[:]),
				EffectiveInputSHA256: capabilityCase.EffectiveInputSHA256,
				VerificationStatus:   "not_run_invalid_result",
			}
			recorder, _ := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 1, RecentOperations: 10})
			cache, _ := remediationinvestigation.NewCache("", remediationinvestigation.CacheOptions{})
			service, err := remediationinvestigation.NewService(model, source, cache, remediationinvestigation.ServiceOptions{
				Timeout: timeout, MaxIters: manifest.Budgets.MaxIters,
				ContextByteBudget: manifest.Budgets.ContextByteBudget, UsageRecorder: recorder,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := service.Investigate(t.Context(), input, browser, false)
			row.MemoMentionsTargetJob = diagnostics.Job
			row.MemoMentionsTargetContainer = diagnostics.Container
			row.MemoMentionsTargetName = diagnostics.Name
			row.MemoMentionsTargetValue = diagnostics.Value
			if runErr != nil {
				row.TrialStatus, row.ErrorCode = remediationTrialFailure(runErr)
				row.Metrics = remediationBenchmarkUsageMetrics(recorder)
			} else {
				row.TrialStatus, row.StructurallyValid = "valid_result", true
				row.FinalResultContainsCandidate = result.Entry.Result.Candidate != nil
				row.CandidateKind = remediationCandidateKind(result.Entry.Result.Candidate)
				row.CandidateIdentity = temporalCandidateIdentity(result.Entry.Result.Candidate, capabilityCase.InvestigationSource)
				row.ExactIdentity = temporalTargetIdentityEqual(row.CandidateIdentity, capabilityCase.ScorerPrivate.KnownTarget, capabilityCase.InvestigationSource)
				row.SelectedEvidenceIDs = slices.Clone(result.Entry.Result.EvidenceIDs)
				row.Evidence, row.Metrics = result.Entry.Provenance.Evidence, result.Entry.Provenance.Metrics
				verifier, _ := remediationinvestigation.NewVerifier(source)
				verified, verifyErr := verifier.Verify(t.Context(), input, result.Entry, browser)
				if verifyErr != nil {
					row.VerificationStatus = "verification_error"
				} else {
					row.ActualClassification, row.VerificationStatus = verified.Classification, string(verified.Classification)
					row.VerifiedActionable = verified.Classification == remediationinvestigation.ClassificationActionable && verified.Proposal != nil
					if capabilityCase.TemporalState == "pre_fix" {
						row.CorrectTemporalResult = row.VerifiedActionable && row.ExactIdentity
					} else {
						row.CorrectTemporalResult = verified.Classification == remediationinvestigation.ClassificationAlreadyFixed && row.ExactIdentity
					}
				}
				row.UnsafeAcceptance = row.VerifiedActionable && !(capabilityCase.TemporalState == "pre_fix" && row.ExactIdentity)
			}
			row.CostAvailable = row.Metrics.Currency != "" && row.Metrics.PricingHash != "" && row.Metrics.CoverageCountsKnown && !row.Metrics.UsageInvalid && row.Metrics.UnreportedRequests == 0
			encoded, _ := json.Marshal(row)
			if _, err := file.Write(append(encoded, '\n')); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func verifyRemediationModelCapabilityPublicEvidence(t *testing.T, manifest remediationModelCapabilityManifestDocument) {
	t.Helper()
	public := manifest.PublicEvidence
	for _, digest := range []string{
		public.TriageGzipSHA256,
		public.TriageExtractedRowsSHA256,
		public.RepairPRExtractSHA256,
		public.TriageExtractedFixtureSHA256,
		public.RepairPRExtractFixtureSHA256,
		public.RepairCommitFixtureSHA256,
		public.JobConfigCommitFixtureSHA256,
	} {
		if len(digest) != 64 {
			t.Fatalf("public provenance digest %q is not sha256", digest)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			t.Fatalf("public provenance digest %q is not hex: %v", digest, err)
		}
	}
	verifyFixtureHash(t, public.TriageExtractedFixture, public.TriageExtractedFixtureSHA256)
	verifyFixtureHash(t, public.RepairPRExtractFixture, public.RepairPRExtractFixtureSHA256)
	verifyFixtureHash(t, public.RepairCommitFixture, public.RepairCommitFixtureSHA256)
	verifyFixtureHash(t, public.JobConfigCommitFixture, public.JobConfigCommitFixtureSHA256)

	triageRaw, err := os.ReadFile(public.TriageExtractedFixture)
	if err != nil {
		t.Fatal(err)
	}
	var rows []remediationModelCapabilityTriageRow
	if err := json.Unmarshal(triageRaw, &rows); err != nil {
		t.Fatal(err)
	}
	compactRows, _ := json.Marshal(rows)
	if remediationinvestigation.HashText(string(compactRows)) != public.TriageExtractedRowsSHA256 {
		t.Fatal("compact triage-row derivation hash mismatch")
	}
	if len(rows) != 3 {
		t.Fatalf("triage rows=%d, want 3", len(rows))
	}

	var repair struct {
		Body    string `json:"body"`
		HeadSHA string `json:"head_sha"`
	}
	repairRaw, err := os.ReadFile(public.RepairPRExtractFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(repairRaw, &repair); err != nil {
		t.Fatal(err)
	}
	if remediationinvestigation.HashText(string(repairRaw)) != public.RepairPRExtractSHA256 {
		t.Fatal("repair PR extract hash mismatch")
	}
	repairLog, ok := remediationModelCapabilityFencedLog(repair.Body)
	if !ok {
		t.Fatal("repair PR extract lacks one frozen log block")
	}

	var repairCommit struct {
		SHA     string   `json:"sha"`
		Date    string   `json:"date"`
		Parents []string `json:"parents"`
		Files   []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
	}
	repairCommitRaw, err := os.ReadFile(public.RepairCommitFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(repairCommitRaw, &repairCommit); err != nil {
		t.Fatal(err)
	}

	var jobCommit struct {
		SHA   string `json:"sha"`
		Date  string `json:"date"`
		Files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
		} `json:"files"`
	}
	jobCommitRaw, err := os.ReadFile(public.JobConfigCommitFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(jobCommitRaw, &jobCommit); err != nil {
		t.Fatal(err)
	}

	for _, capabilityCase := range manifest.Cases {
		oracle := capabilityCase.ScorerPrivate
		if repair.HeadSHA != oracle.FixedRevision || repairCommit.SHA != oracle.FixedRevision || repairCommit.Date != oracle.RepairCommittedAt ||
			len(repairCommit.Parents) != 1 || repairCommit.Parents[0] != oracle.PreFixRevision ||
			jobCommit.SHA != oracle.FailureSourceRevision || jobCommit.Date != oracle.FailureSourceCommitAt {
			t.Fatalf("case %s public source provenance does not match oracle revisions", capabilityCase.ID)
		}
		if len(repairCommit.Files) != 1 || repairCommit.Files[0].Filename != oracle.KnownTarget.Path || repairCommit.Files[0].Status != "modified" ||
			repairCommit.Files[0].Additions != 2 || repairCommit.Files[0].Deletions != 0 {
			t.Fatalf("case %s repair commit provenance does not bind one file-only addition", capabilityCase.ID)
		}
		if len(jobCommit.Files) != 1 || jobCommit.Files[0].Filename != oracle.KnownTarget.Path || jobCommit.Files[0].Status != "added" {
			t.Fatalf("case %s job-config provenance does not bind the target file", capabilityCase.ID)
		}
		failureSourceTime, err := time.Parse(time.RFC3339, oracle.FailureSourceCommitAt)
		if err != nil {
			t.Fatal(err)
		}
		repairTime, err := time.Parse(time.RFC3339, oracle.RepairCommittedAt)
		if err != nil {
			t.Fatal(err)
		}
		if len(capabilityCase.Builds) != len(rows) {
			t.Fatalf("case %s builds=%d triage rows=%d", capabilityCase.ID, len(capabilityCase.Builds), len(rows))
		}
		rowByBuild := make(map[string]remediationModelCapabilityTriageRow, len(rows))
		for _, row := range rows {
			rowByBuild[row.BuildID] = row
		}
		for _, build := range capabilityCase.Builds {
			row, found := rowByBuild[build.BuildID]
			if !found || row.Elapsed != build.ElapsedSeconds || row.Result != build.Result || row.Started != build.StartedUnix ||
				row.TestsFailed != build.TestsFailed || row.TestsRun != build.TestsRun {
				t.Fatalf("case %s build %s does not match compact triage provenance", capabilityCase.ID, build.BuildID)
			}
			if build.SourceRevision != oracle.FailureSourceRevision {
				t.Fatalf("case %s build %s source revision=%s", capabilityCase.ID, build.BuildID, build.SourceRevision)
			}
			if build.StartedUnix <= failureSourceTime.Unix() || build.StartedUnix >= repairTime.Unix() {
				t.Fatalf("case %s build %s is outside the frozen job-config and repair interval", capabilityCase.ID, build.BuildID)
			}
			artifact, err := os.ReadFile(build.ArtifactFixture)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(artifact), "\n"), "\n")
			compactRow, _ := json.Marshal(row)
			if len(lines) == 0 || lines[0] != string(compactRow) {
				t.Fatalf("case %s build %s artifact does not begin with its canonical triage row", capabilityCase.ID, build.BuildID)
			}
			if build.BuildID == "1521965516254613504" {
				if len(lines) != 2 || lines[1] != repairLog {
					t.Fatalf("case %s repair log provenance mismatch", capabilityCase.ID)
				}
			} else if len(lines) != 1 {
				t.Fatalf("case %s build %s artifact has unexpected provenance lines", capabilityCase.ID, build.BuildID)
			}
		}
	}
}

func remediationModelCapabilityFencedLog(body string) (string, bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	const prefix = "```log\n"
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", false
	}
	remaining := body[start+len(prefix):]
	end := strings.Index(remaining, "\n```")
	if end < 0 {
		return "", false
	}
	value := remaining[:end]
	return value, value != "" && !strings.Contains(value, "\n")
}

func preflightRemediationTemporalOracle(t *testing.T, capabilityCase remediationModelCapabilityCase) error {
	t.Helper()
	ctx := t.Context()
	oracle := capabilityCase.ScorerPrivate
	if oracle.KnownTarget.Intent != models.RemediationIntentSetJobEnvironment {
		return fmt.Errorf("historical repair is not one supported Prow environment target")
	}
	policy, ok := temporalTargetPolicy(capabilityCase, oracle.KnownTarget)
	if !ok || !slices.Contains(policy.AllowedPaths, oracle.KnownTarget.Path) {
		return fmt.Errorf("repository or path policy does not match target")
	}
	source := loadRemediationModelCapabilitySource(capabilityCase)
	preRepo := sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: oracle.PreFixRevision}
	fixedRepo := sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: oracle.FixedRevision}
	preTarget := oracle.KnownTarget
	preTarget.Revision = oracle.PreFixRevision
	fixedTarget := oracle.KnownTarget
	fixedTarget.Revision = oracle.FixedRevision
	if reason := actionverify.InvalidTargetReason(preTarget); reason != "" {
		return fmt.Errorf("typed target is invalid: %s", reason)
	}
	pre, err := sourceinvestigation.VerifyTargetState(ctx, source, preRepo, preTarget)
	if err != nil || pre.State != actionverify.StateUnresolved {
		return fmt.Errorf("pre-fix target is not deterministically actionable: state=%s err=%v", pre.State, err)
	}
	fixed, err := sourceinvestigation.VerifyTargetState(ctx, source, fixedRepo, fixedTarget)
	if err != nil || fixed.State != actionverify.StateAlreadyPresent {
		return fmt.Errorf("fixed target is not deterministically present: state=%s err=%v", fixed.State, err)
	}
	preContent, err := source.ReadFile(ctx, preRepo, oracle.KnownTarget.Path)
	if err != nil {
		return err
	}
	definitions, err := jobconfig.ParseCatalog([]byte(preContent), oracle.KnownTarget.Path)
	if err != nil {
		return err
	}
	matches := 0
	for _, definition := range definitions {
		if definition.Name == capabilityCase.JobName && definition.ID() == capabilityCase.JobID && definition.ConfigFile == oracle.KnownTarget.Path {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("pre-fix Prow job identity matched %d definitions", matches)
	}
	if !prowEnvironmentParentExists(preContent, oracle.KnownTarget) {
		return fmt.Errorf("pre-fix Prow container or env parent is missing")
	}
	if capabilityCase.GroundingSourcePath == oracle.KnownTarget.Path || !slices.Contains(capabilityCase.RelevantFiles, capabilityCase.GroundingSourcePath) {
		return fmt.Errorf("pre-fix grounding source is not independently frozen")
	}
	groundingContent, err := source.ReadFile(ctx, preRepo, capabilityCase.GroundingSourcePath)
	if err != nil {
		return fmt.Errorf("read pre-fix grounding source: %w", err)
	}
	if !prowEnvironmentPairExists(groundingContent, oracle.KnownTarget.Name, oracle.KnownTarget.Value) {
		return fmt.Errorf("pre-fix source evidence does not establish the exact environment value")
	}
	failureSourceTime, err := time.Parse(time.RFC3339, oracle.FailureSourceCommitAt)
	if err != nil {
		return fmt.Errorf("invalid failure-source timestamp: %w", err)
	}
	repairTime, err := time.Parse(time.RFC3339, oracle.RepairCommittedAt)
	if err != nil {
		return fmt.Errorf("invalid repair timestamp: %w", err)
	}
	for _, build := range capabilityCase.Builds {
		if build.SourceRevision != oracle.FailureSourceRevision {
			return fmt.Errorf("build %s source revision does not match the frozen job definition", build.BuildID)
		}
		if build.StartedUnix <= failureSourceTime.Unix() || build.StartedUnix >= repairTime.Unix() {
			return fmt.Errorf("build %s does not fall between the frozen job definition and repair", build.BuildID)
		}
	}
	fixedContent, err := source.ReadFile(ctx, fixedRepo, oracle.KnownTarget.Path)
	if err != nil {
		return err
	}
	if !prowEnvironmentOnlyRepair(preContent, fixedContent, oracle.KnownTarget) {
		return fmt.Errorf("historical repair is not representable by exactly one Prow environment target")
	}
	input, _, browser := remediationModelCapabilityInput(t, capabilityCase, strings.Repeat("f", 64))
	entry, err := remediationModelCapabilityOracleEntry(ctx, input, source, browser, oracle.KnownTarget)
	if err != nil {
		return err
	}
	verifier, err := remediationinvestigation.NewVerifier(source)
	if err != nil {
		return err
	}
	verified, err := verifier.Verify(ctx, input, entry, browser)
	if err != nil {
		return fmt.Errorf("production verifier failed: %w", err)
	}
	if verified.Classification != capabilityCase.ExpectedClassification {
		return fmt.Errorf("production verifier classification=%s want=%s", verified.Classification, capabilityCase.ExpectedClassification)
	}
	if capabilityCase.ExpectedClassification == remediationinvestigation.ClassificationActionable {
		if verified.Proposal == nil || !temporalTargetIdentityEqual(&verified.Proposal.Target, oracle.KnownTarget, capabilityCase.InvestigationSource) {
			return fmt.Errorf("production verifier did not preserve the exact actionable target")
		}
	} else if verified.Proposal != nil {
		return fmt.Errorf("production verifier exposed a proposal for %s", verified.Classification)
	}
	return nil
}

func remediationModelCapabilityOracleEntry(ctx context.Context, input remediationinvestigation.FrozenInput, source benchmarkSource, browser benchmarkBrowser, target models.RemediationTarget) (remediationinvestigation.CacheEntry, error) {
	sourceContent, err := source.ReadFile(ctx, input.InvestigationSource, target.Path)
	if err != nil {
		return remediationinvestigation.CacheEntry{}, err
	}
	catalog := remediationinvestigation.EvidenceCatalog{Version: remediationinvestigation.EvidenceCatalogVersion}
	sourceRecord := remediationinvestigation.EvidenceRecord{
		Kind: remediationinvestigation.EvidenceSource,
		Source: &remediationinvestigation.SourceEvidenceIdentity{
			Repository: input.InvestigationSource, Path: target.Path, ContentDigest: remediationinvestigation.HashText(sourceContent),
		},
	}
	sourceRecord.ID = remediationModelCapabilityEvidenceRecordID(sourceRecord)
	catalog.Records = append(catalog.Records, sourceRecord)
	for _, analysis := range input.Analyses {
		record := remediationinvestigation.EvidenceRecord{
			Kind: remediationinvestigation.EvidenceAnalysis,
			Analysis: &remediationinvestigation.AnalysisEvidenceIdentity{
				BuildID: analysis.BuildID, GeneratedAt: analysis.GeneratedAt, RootCauseDigest: remediationinvestigation.HashText(analysis.RootCause),
			},
		}
		record.ID = remediationModelCapabilityEvidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	artifactPaths := make([]string, 0, len(browser.files))
	for path := range browser.files {
		artifactPaths = append(artifactPaths, path)
	}
	sort.Strings(artifactPaths)
	for _, path := range artifactPaths {
		parts := strings.Split(path, "/")
		if len(parts) < 3 {
			return remediationinvestigation.CacheEntry{}, fmt.Errorf("invalid benchmark artifact path %q", path)
		}
		record := remediationinvestigation.EvidenceRecord{
			Kind: remediationinvestigation.EvidenceArtifact,
			Artifact: &remediationinvestigation.ArtifactEvidenceIdentity{
				BuildID: parts[1], Path: path, ContentDigest: remediationinvestigation.HashText(browser.files[path]),
			},
		}
		record.ID = remediationModelCapabilityEvidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	sort.Slice(catalog.Records, func(i, j int) bool { return catalog.Records[i].ID < catalog.Records[j].ID })
	evidenceIDs := make([]string, 0, len(catalog.Records))
	for _, record := range catalog.Records {
		evidenceIDs = append(evidenceIDs, record.ID)
	}
	result := remediationinvestigation.Result{
		Version: remediationinvestigation.ResultVersion, CauseAssessment: remediationinvestigation.CauseSupports,
		Reason: "The recurring failure evidence and pinned job source identify one missing environment entry.",
		Candidate: &remediationinvestigation.ProwEnvironmentEntryCandidate{
			Kind: remediationinvestigation.CandidateProwEnvironmentEntry, ConfigPath: target.Path, Job: target.Job,
			Container: target.Container, Name: target.Name, Value: target.Value,
		},
		EvidenceIDs: evidenceIDs,
	}
	key, err := remediationinvestigation.CacheKey(input)
	if err != nil {
		return remediationinvestigation.CacheEntry{}, err
	}
	provenance := remediationinvestigation.NewProvenance(input, "historical-oracle", ai.APIResponses, remediationinvestigation.EvidenceStats{
		ToolCalls: 1 + len(input.Analyses) + len(browser.files), SourceReads: 1, SourceReadBytes: len(sourceContent),
		ArtifactReads: len(browser.files),
	}, remediationinvestigation.Metrics{}, time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC))
	return remediationinvestigation.CacheEntry{
		Key: key, Result: result, ResultDigest: remediationinvestigation.ResultDigest(result),
		EvidenceCatalog: catalog, EvidenceCatalogDigest: remediationinvestigation.EvidenceCatalogDigest(catalog), Provenance: provenance,
	}, nil
}

func remediationModelCapabilityEvidenceRecordID(record remediationinvestigation.EvidenceRecord) string {
	record.ID = ""
	encoded, _ := json.Marshal(record)
	sum := sha256.Sum256(encoded)
	return string(record.Kind) + ":" + hex.EncodeToString(sum[:])
}

func prowEnvironmentPairExists(content, name, value string) bool {
	var document any
	if yaml.Unmarshal([]byte(content), &document) != nil {
		return false
	}
	found := false
	var visit func(any)
	visit = func(current any) {
		switch item := current.(type) {
		case map[string]any:
			if itemName, _ := item["name"].(string); itemName == name {
				itemValue, valueExists := item["value"]
				found = found || valueExists && fmt.Sprint(itemValue) == value
			}
			for _, child := range item {
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(document)
	return found
}

func prowEnvironmentParentExists(content string, target models.RemediationTarget) bool {
	var document any
	if yaml.Unmarshal([]byte(content), &document) != nil {
		return false
	}
	found := false
	var visit func(any)
	visit = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			if name, _ := item["name"].(string); name == target.Job {
				spec, _ := item["spec"].(map[string]any)
				containers, _ := spec["containers"].([]any)
				if len(containers) == 1 && target.Container == "test" {
					container, _ := containers[0].(map[string]any)
					_, found = container["env"].([]any)
				}
			}
			for _, child := range item {
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(document)
	return found
}

func prowEnvironmentOnlyRepair(preContent, fixedContent string, target models.RemediationTarget) bool {
	var pre, fixed any
	if yaml.Unmarshal([]byte(preContent), &pre) != nil || yaml.Unmarshal([]byte(fixedContent), &fixed) != nil {
		return false
	}
	removed := 0
	var remove func(any)
	remove = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			if name, _ := item["name"].(string); name == target.Job {
				if spec, ok := item["spec"].(map[string]any); ok {
					if containers, ok := spec["containers"].([]any); ok && len(containers) == 1 && target.Container == "test" {
						if container, ok := containers[0].(map[string]any); ok {
							if env, ok := container["env"].([]any); ok {
								kept := env[:0]
								for _, raw := range env {
									entry, _ := raw.(map[string]any)
									if entry["name"] == target.Name && entry["value"] == target.Value {
										removed++
										continue
									}
									kept = append(kept, raw)
								}
								container["env"] = kept
							}
						}
					}
				}
			}
			for _, child := range item {
				remove(child)
			}
		case []any:
			for _, child := range item {
				remove(child)
			}
		}
	}
	remove(fixed)
	return removed == 1 && reflect.DeepEqual(pre, fixed)
}

func temporalTargetPolicy(capabilityCase remediationModelCapabilityCase, target models.RemediationTarget) (remediationinvestigation.RepositoryPolicy, bool) {
	for _, policy := range capabilityCase.DestinationPolicy.Repositories {
		if strings.EqualFold(policy.Repository, target.Repository) {
			return policy, true
		}
	}
	return remediationinvestigation.RepositoryPolicy{}, false
}

func remediationModelCapabilityInput(t *testing.T, capabilityCase remediationModelCapabilityCase, providerFingerprint string) (remediationinvestigation.FrozenInput, benchmarkSource, benchmarkBrowser) {
	t.Helper()
	artifacts := map[string]string{}
	buildIDs := make([]string, 0, len(capabilityCase.Builds))
	buildRefs := make([]remediationinvestigation.BuildReference, 0, len(capabilityCase.Builds))
	analyses := make([]remediationinvestigation.AnalysisReference, 0, len(capabilityCase.Builds))
	for _, build := range capabilityCase.Builds {
		raw, err := os.ReadFile(build.ArtifactFixture)
		if err != nil {
			t.Fatal(err)
		}
		file := "builds/" + build.BuildID + "/failure.txt"
		artifacts[file] = string(raw)
		buildIDs = append(buildIDs, build.BuildID)
		failureSource := sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: build.SourceRevision}
		buildRefs = append(buildRefs, remediationinvestigation.BuildReference{BuildID: build.BuildID, BuildPrefix: build.ProwURL, Source: &failureSource})
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		analyses = append(analyses, remediationinvestigation.AnalysisReference{
			BuildID: build.BuildID, TestName: build.TestName, GeneratedAt: build.AnalysisGeneratedAt,
			RootCause: capabilityCase.RootCause, Severity: "High", RelevantFiles: slices.Clone(capabilityCase.RelevantFiles),
			Evidence:         []models.EvidenceCitation{{Path: "failure.txt", LineStart: 1, LineEnd: len(lines), Quote: strings.Join(lines, "\n")}},
			SourceRepository: &failureSource,
		})
	}
	group := models.PatternCausalGroup{Builds: buildIDs, RootCause: capabilityCase.RootCause, Confidence: capabilityCase.Confidence}
	patternID := benchmarkShortHash("temporal-pattern\x00" + capabilityCase.ID)
	group.ContentHash, group.ID = models.PatternCausalGroupHash(group), models.PatternCausalGroupID(patternID, group)
	manifest, _ := loadRemediationModelCapabilityManifest(t)
	input := remediationinvestigation.FrozenInput{
		PatternID: patternID, PatternHash: strings.Repeat(benchmarkShortHash("temporal-hash\x00"+capabilityCase.ID), 4),
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash, JobID: capabilityCase.JobID, JobName: capabilityCase.JobName,
		Recurrence: models.PatternRecurrenceSharedCause, Group: group, Builds: buildRefs, Analyses: analyses,
		RelevantFiles: capabilityCase.RelevantFiles, InvestigationSource: capabilityCase.InvestigationSource,
		DestinationPolicy: capabilityCase.DestinationPolicy, ConsumerPrompt: manifest.ConsumerPrompt,
		ConsumerPromptHash: manifest.ConsumerPromptSHA256, SkillHash: strings.Repeat("c", 64),
		ProviderFingerprint: providerFingerprint, Versions: manifest.Versions,
	}
	return input, loadRemediationModelCapabilitySource(capabilityCase), benchmarkBrowser{files: artifacts}
}

func loadRemediationModelCapabilitySource(capabilityCase remediationModelCapabilityCase) benchmarkSource {
	files := map[string]map[string]string{}
	for _, snapshot := range capabilityCase.SourceSnapshots {
		contents := map[string]string{}
		for _, file := range snapshot.Files {
			raw, err := os.ReadFile(file.Fixture)
			if err != nil {
				panic(err)
			}
			contents[file.Path] = string(raw)
		}
		files[benchmarkSourceKey(snapshot.Repository)] = contents
	}
	return benchmarkSource{files: files}
}

func temporalCandidateIdentity(candidate remediationinvestigation.CandidateTarget, repository sourceinvestigation.Repository) *models.RemediationTarget {
	value, ok := candidate.(*remediationinvestigation.ProwEnvironmentEntryCandidate)
	if !ok {
		return nil
	}
	return &models.RemediationTarget{
		Intent: models.RemediationIntentSetJobEnvironment, Path: value.ConfigPath,
		Repository: repository.Owner + "/" + repository.Name, Revision: repository.Revision,
		Job: value.Job, Container: value.Container, Name: value.Name, Value: value.Value,
	}
}

func temporalTargetIdentityEqual(actual *models.RemediationTarget, expected models.RemediationTarget, repository sourceinvestigation.Repository) bool {
	if actual == nil {
		return false
	}
	expected.Repository, expected.Revision = repository.Owner+"/"+repository.Name, repository.Revision
	return *actual == expected
}

func catalogSupportsModel(catalog remediationModelCapabilityCatalog, model, endpoint string) bool {
	for _, item := range catalog.Models {
		if item.ID == model && item.SupportedEndpoint == endpoint {
			return true
		}
	}
	return false
}

func TestRemediationModelCapabilityDiagnosticTermsUseExactBoundaries(t *testing.T) {
	if containsDiagnosticTerm("the tests passed", "test") {
		t.Fatal("container diagnostic matched a longer word")
	}
	for _, value := range []string{"test", "KUBERNETES_VERSION", "v1.23.5", "soak-tests-capz-windows-2019"} {
		if !containsDiagnosticTerm("candidate "+strings.ToLower(value)+" identified", value) {
			t.Fatalf("diagnostic did not match %q", value)
		}
	}
}

func remediationModelCapabilityCaseFilter(value string) map[string]bool {
	filter := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			filter[item] = true
		}
	}
	return filter
}

func temporalCaseByState(t *testing.T, cases []remediationModelCapabilityCase, state string) remediationModelCapabilityCase {
	t.Helper()
	for _, item := range cases {
		if item.TemporalState == state {
			return item
		}
	}
	t.Fatalf("temporal state %s not found", state)
	return remediationModelCapabilityCase{}
}

func loadRemediationModelCapabilityManifest(t *testing.T) (remediationModelCapabilityManifestDocument, []byte) {
	t.Helper()
	raw, err := os.ReadFile(remediationModelCapabilityManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest remediationModelCapabilityManifestDocument
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func remediationModelCapabilityCaseHash(t *testing.T, raw []byte, caseID string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, value := range document["cases"].([]any) {
		capabilityCase := value.(map[string]any)
		if capabilityCase["id"] != caseID {
			continue
		}
		delete(capabilityCase, "effective_input_sha256")
		encoded, _ := json.Marshal(capabilityCase)
		sum := sha256.Sum256(encoded)
		return hex.EncodeToString(sum[:])
	}
	t.Fatalf("case %s not found", caseID)
	return ""
}
