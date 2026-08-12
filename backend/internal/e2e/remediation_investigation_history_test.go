package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	remediationHistoryManifest       = "testdata/benchmarks/remediation-investigation-history-v1.json"
	remediationHistoryManifestSHA256 = "426ecf50a6f7773c55463eb6f920af4eef153d4c70facbd5234b57c0b199a094"
)

type remediationHistoryManifestDocument struct {
	Version int                      `json:"version"`
	Cases   []remediationHistoryCase `json:"cases"`
}

type remediationHistoryCase struct {
	ID                   string                                     `json:"id"`
	Category             string                                     `json:"category"`
	JobID                string                                     `json:"job_id"`
	JobName              string                                     `json:"job_name"`
	RootCause            string                                     `json:"root_cause"`
	Confidence           string                                     `json:"confidence"`
	InvestigationSource  sourceinvestigation.Repository             `json:"investigation_source"`
	DestinationPolicy    remediationinvestigation.DestinationPolicy `json:"destination_policy"`
	RelevantFiles        []string                                   `json:"relevant_files"`
	Builds               []remediationHistoryBuild                  `json:"builds"`
	SourceSnapshots      []remediationHistorySource                 `json:"source_snapshots"`
	PublishedIdentity    remediationHistoryPublishedIdentity        `json:"published_identity"`
	KnownFix             *remediationHistoryKnownFix                `json:"known_fix"`
	Expected             remediationBenchmarkExpected               `json:"expected"`
	EffectiveInputSHA256 string                                     `json:"effective_input_sha256"`
}

type remediationHistoryBuild struct {
	BuildID             string `json:"build_id"`
	Revision            string `json:"revision"`
	ProwURL             string `json:"prow_url"`
	ArtifactURI         string `json:"artifact_uri"`
	ArtifactSHA256      string `json:"artifact_sha256"`
	ArtifactBytes       int    `json:"artifact_bytes"`
	ExcerptFile         string `json:"excerpt_file"`
	ExcerptSHA256       string `json:"excerpt_sha256"`
	AnalysisGeneratedAt string `json:"analysis_generated_at"`
	AnalysisRootCause   string `json:"analysis_root_cause"`
	TestName            string `json:"test_name"`
}

type remediationHistoryPublishedIdentity struct {
	PatternID      string `json:"pattern_id"`
	PatternHash    string `json:"pattern_hash"`
	GeneratedAt    string `json:"generated_at"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Reconstruction string `json:"reconstruction,omitempty"`
}

type remediationHistorySource struct {
	Repository sourceinvestigation.Repository `json:"repository"`
	Files      []remediationHistoryFile       `json:"files"`
}

type remediationHistoryFile struct {
	Path    string `json:"path"`
	Fixture string `json:"fixture"`
	SHA256  string `json:"sha256"`
}

type remediationHistoryKnownFix struct {
	RepositoryIdentity sourceinvestigation.Repository `json:"repository_identity"`
	Target             models.RemediationTarget       `json:"target"`
}

type remediationHistoryTrial struct {
	remediationBenchmarkTrial
	KnownFixExpected bool `json:"known_fix_expected,omitempty"`
	KnownFixMatch    bool `json:"known_fix_match,omitempty"`
}

func TestRemediationInvestigationHistoricalManifest(t *testing.T) {
	manifest, raw := loadRemediationHistoryManifest(t)
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != remediationHistoryManifestSHA256 {
		t.Fatalf("historical manifest hash = %s, want %s", got, remediationHistoryManifestSHA256)
	}
	if manifest.Version != 1 || len(manifest.Cases) != 4 {
		t.Fatalf("version=%d cases=%d", manifest.Version, len(manifest.Cases))
	}
	categories := map[string]bool{}
	for _, historyCase := range manifest.Cases {
		categories[historyCase.Category] = true
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(historyCase.PublishedIdentity.PatternID) ||
			!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(historyCase.PublishedIdentity.PatternHash) ||
			!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(historyCase.PublishedIdentity.SnapshotSHA256) {
			t.Fatalf("case %s has invalid published identity %+v", historyCase.ID, historyCase.PublishedIdentity)
		}
		if _, err := time.Parse(time.RFC3339, historyCase.PublishedIdentity.GeneratedAt); err != nil {
			t.Fatalf("case %s has invalid published timestamp: %v", historyCase.ID, err)
		}
		if len(historyCase.Builds) < 2 || len(historyCase.SourceSnapshots) == 0 {
			t.Fatalf("case %s is not recurrent or lacks source", historyCase.ID)
		}
		for _, build := range historyCase.Builds {
			if !regexp.MustCompile(`^[0-9]{16,20}$`).MatchString(build.BuildID) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(build.Revision) {
				t.Fatalf("case %s invalid build identity %+v", historyCase.ID, build)
			}
			verifyFixtureHash(t, build.ExcerptFile, build.ExcerptSHA256)
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(build.ArtifactSHA256) || build.ArtifactBytes <= 0 {
				t.Fatalf("case %s invalid artifact identity %+v", historyCase.ID, build)
			}
			if historyCase.KnownFix != nil {
				for _, private := range []string{historyCase.KnownFix.Target.Path, historyCase.KnownFix.Target.Symbol, historyCase.KnownFix.Target.RequiredCall} {
					if private != "" && (strings.Contains(historyCase.RootCause, private) || strings.Contains(build.AnalysisRootCause, private)) {
						t.Fatalf("case %s leaks scorer-only target %q into provider input", historyCase.ID, private)
					}
				}
			}
		}
		for _, snapshot := range historyCase.SourceSnapshots {
			for _, file := range snapshot.Files {
				verifyFixtureHash(t, file.Fixture, file.SHA256)
			}
		}
		if got := remediationHistoryCaseHash(t, raw, historyCase.ID); got != historyCase.EffectiveInputSHA256 {
			t.Fatalf("case %s hash = %s, want %s", historyCase.ID, got, historyCase.EffectiveInputSHA256)
		}
	}
	for _, category := range []string{"known_source_repair", "fixed_in_later_source", "external_dependency", "environment_or_infrastructure"} {
		if !categories[category] {
			t.Fatalf("missing historical category %s", category)
		}
	}
}

func TestRemediationInvestigationHistoricalBenchmark(t *testing.T) {
	if os.Getenv("RUN_REMEDIATION_INVESTIGATION_HISTORY_BENCHMARK") != "1" {
		t.Skip("set RUN_REMEDIATION_INVESTIGATION_HISTORY_BENCHMARK=1 for private provider holdouts")
	}
	endpoint, modelName := strings.TrimSpace(os.Getenv("AI_ENDPOINT")), strings.TrimSpace(os.Getenv("AI_MODEL"))
	resultsPath := strings.TrimSpace(os.Getenv("REMEDIATION_HISTORY_RESULTS_JSONL"))
	if endpoint == "" || modelName == "" || resultsPath == "" {
		t.Fatal("AI_ENDPOINT, AI_MODEL, and REMEDIATION_HISTORY_RESULTS_JSONL are required")
	}
	apiMode := strings.TrimSpace(os.Getenv("AI_API"))
	if apiMode == "" {
		apiMode = ai.APIChatCompletions
	}
	repetitions := 2
	manifest, raw := loadRemediationHistoryManifest(t)
	engineCommit := remediationBenchmarkEngineCommit(t)
	manifestSum := sha256.Sum256(raw)
	reasoningEffort := benchmarkReasoningEffort(t)
	if err := os.MkdirAll(filepath.Dir(resultsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, historyCase := range manifest.Cases {
		for repetition := 1; repetition <= repetitions; repetition++ {
			client := ai.NewClientWithOptions(ai.Options{Token: os.Getenv("AI_TOKEN"), API: apiMode, Endpoint: endpoint, Model: modelName, ReasoningEffort: reasoningEffort})
			input, source, browser := remediationHistoryInput(t, historyCase, client.ModelFingerprint())
			row := remediationHistoryTrial{remediationBenchmarkTrial: remediationBenchmarkTrial{
				CaseID: historyCase.ID, Category: historyCase.Category, Repetition: repetition,
				EngineCommit: engineCommit, ManifestSHA256: hex.EncodeToString(manifestSum[:]), EffectiveInputSHA256: historyCase.EffectiveInputSHA256,
				PatternHash: input.PatternHash, CausalGroupHash: input.CausalGroupHash,
				ProviderFingerprint: client.ModelFingerprint(), Model: modelName, APIMode: apiMode, ReasoningEffort: string(client.ReasoningEffort()),
				PromptVersion: remediationinvestigation.PromptVersion, SchemaVersion: remediationinvestigation.SchemaVersion,
				VerificationVersion: remediationinvestigation.VerificationVersion, ResultVersion: remediationinvestigation.ResultVersion,
				ExpectedClassification: historyCase.Expected.Classification,
				ExpectedActionable:     historyCase.Expected.Classification == remediationinvestigation.ClassificationActionable,
				ExpectedCandidate:      historyCase.KnownFix != nil, VerificationStatus: "not_run_invalid_result",
			}, KnownFixExpected: historyCase.KnownFix != nil}
			recorder, _ := aiusage.NewRecorder("", aiusage.RecorderOptions{RetentionDays: 1, RecentOperations: 10})
			cache, _ := remediationinvestigation.NewCache("", remediationinvestigation.CacheOptions{})
			service, err := remediationinvestigation.NewService(client, source, cache, remediationinvestigation.ServiceOptions{Timeout: 10 * time.Minute, UsageRecorder: recorder})
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := service.Investigate(t.Context(), input, browser, false)
			if runErr != nil {
				row.TrialStatus, row.ErrorCode = remediationTrialFailure(runErr)
				row.Metrics = remediationBenchmarkUsageMetrics(recorder)
			} else {
				row.TrialStatus, row.StructurallyValid = "valid_result", true
				row.ActualActionable = result.Entry.Result.Candidate != nil
				row.ModelCandidateKind = remediationCandidateKind(result.Entry.Result.Candidate)
				if result.Entry.Result.NonActionableReason != nil {
					row.ModelNonActionableReason = string(*result.Entry.Result.NonActionableReason)
				}
				row.ResultDigest, row.EvidenceCatalogDigest = result.Entry.ResultDigest, result.Entry.EvidenceCatalogDigest
				row.Evidence, row.Metrics, row.CacheHit = result.Entry.Provenance.Evidence, result.Entry.Provenance.Metrics, result.CacheHit
				row.KnownFixMatch = remediationHistoryKnownFixMatches(historyCase.KnownFix, result.Entry.Result.Candidate)
				verifier, _ := remediationinvestigation.NewVerifier(source)
				verified, verifyErr := verifier.Verify(t.Context(), input, result.Entry, browser)
				if verifyErr != nil {
					row.VerificationStatus = "verification_error"
				} else {
					row.ActualClassification, row.VerificationStatus = verified.Classification, string(verified.Classification)
					row.VerifiedActionable = verified.Classification == remediationinvestigation.ClassificationActionable && verified.Proposal != nil
					score := scoreRemediationBenchmark(historyCase.Expected, remediationBenchmarkActual{Classification: verified.Classification})
					row.ClassificationCorrect, row.ExactTarget = score.ClassificationCorrect, score.ExactTarget
					if historyCase.KnownFix != nil {
						row.ExactTarget = row.KnownFixMatch
					}
				}
				row.UnverifiedUnsafeProposal = row.ActualActionable && !row.KnownFixMatch
				row.UnsafeFalseAcceptance = row.VerifiedActionable && !row.ExpectedActionable
				row.AlreadyFixedBlocked = historyCase.Expected.Classification != remediationinvestigation.ClassificationAlreadyFixed || !row.VerifiedActionable
			}
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

func remediationHistoryInput(t *testing.T, historyCase remediationHistoryCase, providerFingerprint string) (remediationinvestigation.FrozenInput, benchmarkSource, benchmarkBrowser) {
	t.Helper()
	buildIDs := make([]string, 0, len(historyCase.Builds))
	buildRefs := make([]remediationinvestigation.BuildReference, 0, len(historyCase.Builds))
	analyses := make([]remediationinvestigation.AnalysisReference, 0, len(historyCase.Builds))
	artifacts := map[string]string{}
	for _, build := range historyCase.Builds {
		content, err := os.ReadFile(build.ExcerptFile)
		if err != nil {
			t.Fatal(err)
		}
		file := "builds/" + build.BuildID + "/build-log.txt"
		artifacts[file] = string(content)
		repository := sourceinvestigation.Repository{Owner: historyCase.InvestigationSource.Owner, Name: historyCase.InvestigationSource.Name, Revision: build.Revision}
		buildIDs = append(buildIDs, build.BuildID)
		buildRefs = append(buildRefs, remediationinvestigation.BuildReference{BuildID: build.BuildID, BuildPrefix: build.ProwURL, Source: &repository})
		analyses = append(analyses, remediationinvestigation.AnalysisReference{
			BuildID: build.BuildID, TestName: build.TestName, GeneratedAt: build.AnalysisGeneratedAt,
			RootCause: build.AnalysisRootCause, Severity: "High", RelevantFiles: historyCase.RelevantFiles,
			Evidence:         []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: strings.TrimSpace(string(content))}},
			SourceRepository: &repository,
		})
	}
	group := models.PatternCausalGroup{Builds: buildIDs, RootCause: historyCase.RootCause, Confidence: historyCase.Confidence}
	patternID := historyCase.PublishedIdentity.PatternID
	group.ContentHash, group.ID = models.PatternCausalGroupHash(group), models.PatternCausalGroupID(patternID, group)
	consumerPrompt := "Use only the frozen recurring-build evidence and pinned source. Do not infer dependency ownership from module-cache paths."
	input := remediationinvestigation.FrozenInput{
		PatternID: patternID, PatternHash: historyCase.PublishedIdentity.PatternHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash, JobID: historyCase.JobID, JobName: historyCase.JobName,
		Recurrence: models.PatternRecurrenceSharedCause, Group: group, Builds: buildRefs, Analyses: analyses,
		RelevantFiles: historyCase.RelevantFiles, InvestigationSource: historyCase.InvestigationSource,
		DestinationPolicy: historyCase.DestinationPolicy, ConsumerPrompt: consumerPrompt,
		ConsumerPromptHash: remediationinvestigation.HashText(consumerPrompt), SkillHash: strings.Repeat("c", 64),
		ProviderFingerprint: providerFingerprint, Versions: remediationinvestigation.CurrentVersions(),
	}
	return input, loadRemediationHistorySource(t, historyCase), benchmarkBrowser{files: artifacts}
}

func loadRemediationHistorySource(t *testing.T, historyCase remediationHistoryCase) benchmarkSource {
	t.Helper()
	files := map[string]map[string]string{}
	for _, snapshot := range historyCase.SourceSnapshots {
		contents := map[string]string{}
		for _, file := range snapshot.Files {
			raw, err := os.ReadFile(file.Fixture)
			if err != nil {
				t.Fatal(err)
			}
			contents[file.Path] = string(raw)
		}
		files[benchmarkSourceKey(snapshot.Repository)] = contents
	}
	return benchmarkSource{files: files}
}

func remediationHistoryKnownFixMatches(fix *remediationHistoryKnownFix, candidate remediationinvestigation.CandidateTarget) bool {
	if fix == nil || candidate == nil {
		return fix == nil && candidate == nil
	}
	value, ok := candidate.(*remediationinvestigation.RequiredCallCandidate)
	return ok && fix.Target.Path == value.Path && fix.Target.Symbol == value.ContainingSymbol && fix.Target.RequiredCall == value.RequiredCall
}

func loadRemediationHistoryManifest(t *testing.T) (remediationHistoryManifestDocument, []byte) {
	t.Helper()
	raw, err := os.ReadFile(remediationHistoryManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest remediationHistoryManifestDocument
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func remediationHistoryCaseHash(t *testing.T, raw []byte, caseID string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, value := range document["cases"].([]any) {
		historyCase := value.(map[string]any)
		if historyCase["id"] != caseID {
			continue
		}
		delete(historyCase, "effective_input_sha256")
		encoded, _ := json.Marshal(historyCase)
		sum := sha256.Sum256(encoded)
		return hex.EncodeToString(sum[:])
	}
	t.Fatalf("case %s not found", caseID)
	return ""
}

func verifyFixtureHash(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("fixture %s hash = %s, want %s", path, got, want)
	}
}
