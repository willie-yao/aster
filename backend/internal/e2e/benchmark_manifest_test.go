package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const benchmarkManifestVersion = 1

var benchmarkCaseIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var benchmarkStableIDRE = regexp.MustCompile(`^[0-9a-f]{20}$`)
var benchmarkCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var benchmarkSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type benchmarkManifest struct {
	Version int                     `json:"version"`
	Cases   []benchmarkManifestCase `json:"cases"`
}

func TestCrossProjectEvaluationManifest(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "testdata", "benchmarks", "cross-project-eval.json")
	cases, err := loadBenchmarkManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("cases = %d, want 3", len(cases))
	}
	allowedUnavailable := 0
	adversarialFailures := map[string][]string{
		"secrets-store-csi-image-scan":        {"forbidden: temporary security-scanner attribution"},
		"kueue-was-podgroup-api-mismatch":     {"forbidden: API mismatch treated as incidental", "forbidden: transient readiness as primary cause"},
		"gcp-pd-csi-windows-mount-visibility": {"recognizes NodePublishVolume succeeded", "forbidden: unsupported component ownership"},
	}
	for _, bc := range cases {
		if bc.fixtureAsset == "" || len(bc.fixtureSHA256) != 64 {
			t.Fatalf("case %q has incomplete fixture identity", bc.name)
		}
		if bc.consumerCommit == "" || bc.projectSHA256 == "" || bc.promptSHA256 == "" || bc.expectedTransient == nil || bc.referenceDiagnosis == "" {
			t.Fatalf("case %q has incomplete consumer or transient identity", bc.name)
		}
		if bc.allowUnavailable {
			allowedUnavailable++
			if bc.name != "gcp-pd-csi-windows-mount-visibility" {
				t.Fatalf("case %q unexpectedly allows unavailable", bc.name)
			}
		}
		reference := &models.TestCase{
			AISummary:  &models.AISummary{Summary: bc.referenceDiagnosis, IsTransient: bc.referenceTransient},
			AIAnalysis: &models.AIAnalysis{RootCause: bc.referenceDiagnosis},
		}
		if assessment := assessBenchmarkCase(bc, reference); len(assessment.missingMust) > 0 {
			t.Errorf("case %q rejects locked reference: %v", bc.name, assessment.missingMust)
		}
		opposite := &models.TestCase{
			AISummary:  &models.AISummary{Summary: bc.oppositeDiagnosis, IsTransient: bc.oppositeTransient},
			AIAnalysis: &models.AIAnalysis{RootCause: bc.oppositeDiagnosis},
		}
		assessment := assessBenchmarkCase(bc, opposite)
		for _, want := range adversarialFailures[bc.name] {
			if !slices.Contains(assessment.missingMust, want) {
				t.Errorf("case %q adversarial diagnosis did not fail %q: %v", bc.name, want, assessment.missingMust)
			}
		}
		if bc.name == "secrets-store-csi-image-scan" {
			alternate := &models.TestCase{
				AISummary:  &models.AISummary{Summary: "Trivy found four fixable vulnerabilities and the --exit-code 1 gate fired."},
				AIAnalysis: &models.AIAnalysis{RootCause: "A temporary scanner database state was responsible for the failure."},
			}
			if assessment := assessBenchmarkCase(bc, alternate); !slices.Contains(assessment.missingMust, "forbidden: temporary security-scanner attribution") {
				t.Errorf("case %q accepts responsible-for scanner attribution: %v", bc.name, assessment.missingMust)
			}
		}
		if bc.name == "gcp-pd-csi-windows-mount-visibility" {
			negated := &models.TestCase{
				AISummary:  &models.AISummary{Summary: bc.referenceDiagnosis},
				AIAnalysis: &models.AIAnalysis{RootCause: bc.referenceDiagnosis + " The GCE PD node driver is not definitively responsible."},
			}
			if assessment := assessBenchmarkCase(bc, negated); slices.Contains(assessment.missingMust, "forbidden: unsupported component ownership") {
				t.Errorf("case %q rejects negated ownership statement: %v", bc.name, assessment.missingMust)
			}
		}
		if bc.name == "kueue-was-podgroup-api-mismatch" {
			var responseSignal *benchSignal
			for _, signal := range bc.signals {
				if signal.name == "identifies unavailable PodGroup API response" {
					copy := signal
					responseSignal = &copy
				}
			}
			if responseSignal == nil {
				t.Fatalf("case %q is missing unavailable PodGroup API response signal", bc.name)
			}
			for _, text := range []string{
				"The API server response for v1beta1 PodGroups returned 404.",
				"The v1beta1 PodGroup API endpoint was not served.",
				"The scheduler request for v1beta1 PodGroup was unavailable.",
				"The API server returned NotFound when the scheduler listed v1beta1 PodGroups.",
				"The API server returned 404 for that version.",
				"Skipping API scheduling.k8s.io/v1beta1 because it has no resources.",
			} {
				if !responseSignal.matches(text) {
					t.Errorf("case %q rejects equivalent unavailable-API wording %q", bc.name, text)
				}
			}
			for _, text := range []string{
				"No resources were available to place the workload.",
				"The service account was forbidden to list podgroups, while the v1beta1 endpoint was available.",
				"The v1beta1 PodGroup request succeeded.",
				"The API server response for v1beta1 PodGroup was not 404; it returned 200 OK.",
				"The scheduler request for v1beta1 PodGroup was not unavailable; it completed successfully.",
				"The scheduler list contained a v1beta1 PodGroup whose workload was unavailable.",
				"The scheduler request for v1beta1 PodGroup succeeded. The image registry returned 404.",
				"The scheduler listed a v1beta1 PodGroup whose workload endpoint returned 404.",
				"The API server returned 404 for an unrelated admission webhook. The scheduler requested v1beta1 PodGroup, whose API request succeeded.",
				"The API server returned 404 for v1alpha3, not for v1beta1 PodGroup.",
			} {
				if responseSignal.matches(text) {
					t.Errorf("case %q accepts unrelated or successful API wording %q", bc.name, text)
				}
			}
			base := "The scheduler requested v1beta1 PodGroup while the API server served v1alpha3. Scheduler handlers never synchronized. "
			for _, text := range []string{
				"No resources were available to place the workload.",
				"The service account was forbidden to list podgroups, while the v1beta1 endpoint was available.",
				"The scheduler request for v1beta1 PodGroup succeeded. The image registry returned 404.",
				"The scheduler listed a v1beta1 PodGroup whose workload endpoint returned 404.",
				"The API server returned 404 for an unrelated admission webhook. The scheduler requested v1beta1 PodGroup, whose API request succeeded.",
				"The API server returned 404 for v1alpha3, not for v1beta1 PodGroup.",
			} {
				wrong := &models.TestCase{AISummary: &models.AISummary{Summary: base + text}, AIAnalysis: &models.AIAnalysis{RootCause: base + text}}
				if assessment := assessBenchmarkCase(bc, wrong); !slices.Contains(assessment.missingMust, "identifies unavailable PodGroup API response") {
					t.Errorf("case %q accepts wrong complete diagnosis %q: %v", bc.name, text, assessment.missingMust)
				}
			}
			correct := &models.TestCase{
				AISummary:  &models.AISummary{Summary: base + "The API server returned NotFound when the scheduler listed v1beta1 PodGroups."},
				AIAnalysis: &models.AIAnalysis{RootCause: base + "The API server returned NotFound when the scheduler listed v1beta1 PodGroups."},
			}
			if assessment := assessBenchmarkCase(bc, correct); slices.Contains(assessment.missingMust, "identifies unavailable PodGroup API response") {
				t.Errorf("case %q rejects correct response-first NotFound diagnosis: %v", bc.name, assessment.missingMust)
			}
			for _, text := range []string{
				"The API server served v1alpha3. The v1alpha3 API was available, but the scheduler request for v1beta1 PodGroup returned NotFound. Scheduler handlers never synchronized.",
				"The API server served v1alpha3 and that endpoint was available only for v1alpha3; the scheduler request for v1beta1 PodGroup returned NotFound. Scheduler handlers never synchronized.",
			} {
				correct := &models.TestCase{AISummary: &models.AISummary{Summary: text}, AIAnalysis: &models.AIAnalysis{RootCause: text}}
				if assessment := assessBenchmarkCase(bc, correct); slices.Contains(assessment.missingMust, "identifies unavailable PodGroup API response") {
					t.Errorf("case %q rejects v1alpha3-only availability %q: %v", bc.name, text, assessment.missingMust)
				}
			}
		}
	}
	if allowedUnavailable != 1 {
		t.Fatalf("allow_unavailable cases = %d, want 1", allowedUnavailable)
	}
}

type benchmarkManifestCase struct {
	ID                  string                    `json:"id"`
	StableID            string                    `json:"stable_id"`
	Bucket              string                    `json:"bucket"`
	FixtureAsset        string                    `json:"fixture_asset,omitempty"`
	FixtureSHA256       string                    `json:"fixture_sha256,omitempty"`
	JobType             string                    `json:"job_type"`
	Repo                string                    `json:"repo,omitempty"`
	JobName             string                    `json:"job_name"`
	BuildID             string                    `json:"build_id"`
	PullNumber          string                    `json:"pull_number,omitempty"`
	WebURL              string                    `json:"web_url"`
	Commit              string                    `json:"commit"`
	RepoVersion         string                    `json:"repo_version"`
	RepoRefs            map[string]string         `json:"repo_refs"`
	SourceOwner         string                    `json:"source_owner"`
	SourceName          string                    `json:"source_name"`
	TestName            string                    `json:"test_name"`
	TestSource          string                    `json:"test_source,omitempty"`
	JUnitFile           string                    `json:"junit_file,omitempty"`
	FailureMessage      string                    `json:"failure_message"`
	ConsecutiveFailures int                       `json:"consecutive_failures,omitempty"`
	OppositeDiagnosis   string                    `json:"opposite_diagnosis,omitempty"`
	OppositeTransient   bool                      `json:"opposite_is_transient,omitempty"`
	ReferenceDiagnosis  string                    `json:"reference_diagnosis,omitempty"`
	ReferenceTransient  bool                      `json:"reference_is_transient,omitempty"`
	AllowUnavailable    bool                      `json:"allow_unavailable,omitempty"`
	ExpectedTransient   *bool                     `json:"expected_transient,omitempty"`
	Forbidden           []benchmarkManifestSignal `json:"forbidden,omitempty"`
	ConsumerCommit      string                    `json:"consumer_commit,omitempty"`
	ProjectSHA256       string                    `json:"project_sha256,omitempty"`
	PromptSHA256        string                    `json:"prompt_sha256,omitempty"`
	Signals             []benchmarkManifestSignal `json:"signals"`
}

type benchmarkManifestSignal struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Negated string `json:"negated,omitempty"`
	Must    bool   `json:"must,omitempty"`
}

func loadBenchmarkManifest(path string) ([]benchCase, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 2<<20 {
		return nil, fmt.Errorf("benchmark manifest exceeds 2 MiB")
	}
	decoder := json.NewDecoder(bufio.NewReader(file))
	decoder.DisallowUnknownFields()
	var manifest benchmarkManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode benchmark manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("benchmark manifest must contain one JSON object")
	}
	if manifest.Version != benchmarkManifestVersion {
		return nil, fmt.Errorf("benchmark manifest version %d is unsupported", manifest.Version)
	}
	if len(manifest.Cases) == 0 || len(manifest.Cases) > 50 {
		return nil, fmt.Errorf("benchmark manifest case count must be 1..50")
	}
	seen := map[string]bool{}
	out := make([]benchCase, 0, len(manifest.Cases))
	for index, item := range manifest.Cases {
		if !benchmarkCaseIDRE.MatchString(item.ID) || seen[item.ID] {
			return nil, fmt.Errorf("benchmark manifest case %d has invalid or duplicate id", index)
		}
		seen[item.ID] = true
		if !benchmarkStableIDRE.MatchString(item.StableID) {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid stable_id", item.ID)
		}
		if item.JobType != models.JobTypePeriodic && item.JobType != models.JobTypePresubmit {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid job_type", item.ID)
		}
		if item.JobType == models.JobTypePresubmit && (item.Repo == "" || item.PullNumber == "") {
			return nil, fmt.Errorf("benchmark manifest presubmit case %q requires repo and pull_number", item.ID)
		}
		if item.ConsecutiveFailures < 0 {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid consecutive_failures", item.ID)
		}
		if _, err := strconv.ParseUint(item.BuildID, 10, 64); err != nil {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid build_id", item.ID)
		}
		if !benchmarkCommitRE.MatchString(item.Commit) || item.RepoVersion != item.Commit {
			return nil, fmt.Errorf("benchmark manifest case %q requires matching exact commit and repo_version", item.ID)
		}
		if len(item.RepoRefs) == 0 || len(item.RepoRefs) > 8 {
			return nil, fmt.Errorf("benchmark manifest case %q repo_refs count must be 1..8", item.ID)
		}
		sourceKey := item.SourceOwner + "/" + item.SourceName
		if _, ok := item.RepoRefs[sourceKey]; !ok {
			return nil, fmt.Errorf("benchmark manifest case %q repo_refs omit configured source", item.ID)
		}
		for repo, ref := range item.RepoRefs {
			if repo == "" || ref == "" || len(repo) > 256 || len(ref) > 256 || strings.ContainsAny(repo+ref, "\r\n\x00") {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid repo_refs", item.ID)
			}
		}
		if item.Bucket == "" || item.JobName == "" || item.WebURL == "" || item.TestName == "" || item.FailureMessage == "" || item.SourceOwner == "" || item.SourceName == "" {
			return nil, fmt.Errorf("benchmark manifest case %q is missing required identity", item.ID)
		}
		if item.TestSource != "" && item.TestSource != models.TestCaseSourceBuild {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid test_source", item.ID)
		}
		if item.TestSource == models.TestCaseSourceBuild && item.JUnitFile != "" {
			return nil, fmt.Errorf("benchmark manifest case %q build source must not set junit_file", item.ID)
		}
		for label, value := range map[string]string{
			"bucket": item.Bucket, "job_name": item.JobName, "repo": item.Repo, "web_url": item.WebURL,
			"source_owner": item.SourceOwner, "source_name": item.SourceName, "junit_file": item.JUnitFile,
		} {
			if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid %s", item.ID, label)
			}
		}
		if len(item.TestName) > 4096 || len(item.FailureMessage) > 16384 || len(item.OppositeDiagnosis) > 16384 {
			return nil, fmt.Errorf("benchmark manifest case %q text exceeds limits", item.ID)
		}
		consumerPinned := item.ConsumerCommit != "" || item.ProjectSHA256 != "" || item.PromptSHA256 != ""
		if consumerPinned && (!benchmarkCommitRE.MatchString(item.ConsumerCommit) || !benchmarkSHA256RE.MatchString(item.ProjectSHA256) || !benchmarkSHA256RE.MatchString(item.PromptSHA256)) {
			return nil, fmt.Errorf("benchmark manifest case %q has incomplete consumer identity", item.ID)
		}
		if item.FixtureAsset != "" {
			if filepath.Base(item.FixtureAsset) != item.FixtureAsset || !strings.HasSuffix(item.FixtureAsset, ".tar.gz") || len(item.FixtureSHA256) != 64 {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid fixture identity", item.ID)
			}
		}
		if len(item.Signals) == 0 || len(item.Signals) > 32 {
			return nil, fmt.Errorf("benchmark manifest case %q signal count must be 1..32", item.ID)
		}
		signals := make([]benchSignal, 0, len(item.Signals))
		for signalIndex, signal := range item.Signals {
			if signal.Name == "" || signal.Pattern == "" {
				return nil, fmt.Errorf("benchmark manifest case %q signal %d is incomplete", item.ID, signalIndex)
			}
			positive, err := regexp.Compile(signal.Pattern)
			if err != nil {
				return nil, fmt.Errorf("benchmark manifest case %q signal %d pattern: %w", item.ID, signalIndex, err)
			}
			var negative *regexp.Regexp
			if signal.Negated != "" {
				negative, err = regexp.Compile(signal.Negated)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q signal %d negated: %w", item.ID, signalIndex, err)
				}
			}
			signals = append(signals, benchSignal{name: signal.Name, re: positive, negated: negative, must: signal.Must})
		}
		if len(item.Forbidden) > 16 {
			return nil, fmt.Errorf("benchmark manifest case %q forbidden count exceeds 16", item.ID)
		}
		forbidden := make([]benchSignal, 0, len(item.Forbidden))
		for forbiddenIndex, signal := range item.Forbidden {
			if signal.Name == "" || signal.Pattern == "" || signal.Must {
				return nil, fmt.Errorf("benchmark manifest case %q forbidden %d is invalid", item.ID, forbiddenIndex)
			}
			pattern, err := regexp.Compile(signal.Pattern)
			if err != nil {
				return nil, fmt.Errorf("benchmark manifest case %q forbidden %d pattern: %w", item.ID, forbiddenIndex, err)
			}
			var negative *regexp.Regexp
			if signal.Negated != "" {
				negative, err = regexp.Compile(signal.Negated)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q forbidden %d negated: %w", item.ID, forbiddenIndex, err)
				}
			}
			forbidden = append(forbidden, benchSignal{name: signal.Name, re: pattern, negated: negative})
		}
		out = append(out, benchCase{
			name: item.ID, stableID: item.StableID, bucket: item.Bucket, fixtureAsset: item.FixtureAsset,
			fixtureSHA256: item.FixtureSHA256, jobType: item.JobType, repo: item.Repo, jobName: item.JobName,
			buildID: item.BuildID, pullNumber: item.PullNumber, webURL: item.WebURL,
			commit: item.Commit, repoVersion: item.RepoVersion, repoRefs: maps.Clone(item.RepoRefs),
			sourceRepo: [2]string{item.SourceOwner, item.SourceName}, testName: item.TestName, testSource: item.TestSource,
			junitFile: item.JUnitFile, failureMsg: item.FailureMessage, consecutiveFailures: item.ConsecutiveFailures,
			oppositeDiagnosis: item.OppositeDiagnosis, oppositeTransient: item.OppositeTransient,
			referenceDiagnosis: item.ReferenceDiagnosis, referenceTransient: item.ReferenceTransient,
			allowUnavailable: item.AllowUnavailable, expectedTransient: item.ExpectedTransient, forbidden: forbidden,
			consumerCommit: item.ConsumerCommit, projectSHA256: item.ProjectSHA256, promptSHA256: item.PromptSHA256,
			signals: signals,
		})
	}
	return out, nil
}

type benchmarkJSONLResult struct {
	CaseID                  string                     `json:"case_id"`
	StableID                string                     `json:"stable_id"`
	Repetition              int                        `json:"repetition"`
	ModelLabel              string                     `json:"model_label"`
	JobName                 string                     `json:"job_name"`
	BuildID                 string                     `json:"build_id"`
	CheckoutCommit          string                     `json:"checkout_commit"`
	SourceRevision          string                     `json:"source_revision,omitempty"`
	SourceUnavailable       bool                       `json:"source_unavailable,omitempty"`
	TestName                string                     `json:"test_name"`
	TestSource              string                     `json:"test_source,omitempty"`
	ElapsedMS               int64                      `json:"elapsed_ms"`
	Outcome                 string                     `json:"outcome"`
	Usable                  bool                       `json:"usable"`
	IsTransient             *bool                      `json:"is_transient,omitempty"`
	Summary                 string                     `json:"summary,omitempty"`
	RootCause               string                     `json:"root_cause,omitempty"`
	SuggestedFix            string                     `json:"suggested_fix,omitempty"`
	Severity                string                     `json:"severity,omitempty"`
	Evidence                []models.EvidenceCitation  `json:"evidence_citations,omitempty"`
	FileLinks               map[string]string          `json:"file_links,omitempty"`
	SignalHits              int                        `json:"signal_hits"`
	SignalTotal             int                        `json:"signal_total"`
	MissingMust             []string                   `json:"missing_must,omitempty"`
	SelectedAttempt         int                        `json:"selected_attempt,omitempty"`
	Drafts                  []benchmarkJSONLDraft      `json:"drafts,omitempty"`
	DraftDecisions          []ai.DraftDecisionTrace    `json:"draft_decisions,omitempty"`
	ToolNames               []string                   `json:"tool_names,omitempty"`
	ToolCounts              []string                   `json:"tool_counts,omitempty"`
	GCSBytes                int                        `json:"gcs_bytes,omitempty"`
	EvidencePlanCovered     bool                       `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted  bool                       `json:"gcs_floor_retry_exhausted,omitempty"`
	CritiquePassed          *bool                      `json:"critique_passed,omitempty"`
	CritiqueCachePolicy     string                     `json:"critique_cache_policy,omitempty"`
	CritiqueHardFailures    []string                   `json:"critique_hard_failures,omitempty"`
	CritiqueSoftWarnings    []string                   `json:"critique_soft_warnings,omitempty"`
	BudgetExhausted         bool                       `json:"budget_exhausted,omitempty"`
	FloorNudges             int                        `json:"floor_nudges,omitempty"`
	FloorNudgeReasons       []string                   `json:"floor_nudge_reasons,omitempty"`
	ProviderRequestCap      int                        `json:"provider_request_cap"`
	TraceTruncated          bool                       `json:"trace_truncated,omitempty"`
	CacheGeneration         string                     `json:"cache_generation,omitempty"`
	CacheVerification       benchmarkCacheVerification `json:"cache_verification"`
	Trace                   benchmarkJSONLTrace        `json:"trace"`
	HumanScoreRubricVersion int                        `json:"human_score_rubric_version"`
	HumanScoreMax           int                        `json:"human_score_max"`
	HumanScoreDimensions    []string                   `json:"human_score_dimensions"`
}

const (
	benchmarkHumanScoreRubricVersion = 1
	benchmarkHumanScoreMax           = 10
)

var benchmarkHumanScoreDimensions = []string{
	"diagnosis",
	"artifact_evidence",
	"claim_discipline",
	"remediation",
	"source_grounding",
}

type benchmarkJSONLDraft struct {
	Attempt             int                           `json:"attempt"`
	Phase               string                        `json:"phase"`
	Selected            bool                          `json:"selected"`
	RuleIDs             []string                      `json:"rule_ids,omitempty"`
	MatchedSkillIDs     []string                      `json:"matched_skill_ids,omitempty"`
	MissingGroups       []ai.CritiqueEvidenceGroupRef `json:"missing_groups,omitempty"`
	UnavailableGroups   []ai.CritiqueEvidenceGroupRef `json:"unavailable_groups,omitempty"`
	PublishedRuleIDs    []string                      `json:"published_rule_ids,omitempty"`
	PublishedHardRules  []string                      `json:"published_hard_rules,omitempty"`
	PublishedSoftRules  []string                      `json:"published_soft_rules,omitempty"`
	PublishedHardIssues int                           `json:"published_hard_issues,omitempty"`
	PublishedPuntCount  int                           `json:"published_punt_count,omitempty"`
	PublishedMissing    int                           `json:"published_missing_group_count,omitempty"`
	PuntCount           int                           `json:"punt_count,omitempty"`
	UnreadCitationCount int                           `json:"unread_citation_count,omitempty"`
	CitationIssueCount  int                           `json:"citation_issue_count,omitempty"`
	MissingGroupCount   int                           `json:"missing_group_count,omitempty"`
	TransientConflict   bool                          `json:"transient_conflict,omitempty"`
	ToolCalls           int                           `json:"tool_calls,omitempty"`
	EvidenceReads       int                           `json:"evidence_reads,omitempty"`
}

type benchmarkCacheVerification struct {
	PersistenceAttempted   bool                    `json:"persistence_attempted"`
	PersistenceAccepted    bool                    `json:"persistence_accepted"`
	PolicyRejectionReason  ai.CacheRejectionReason `json:"policy_rejection_reason,omitempty"`
	CacheSaveSucceeded     bool                    `json:"cache_save_succeeded"`
	LookupAttempted        bool                    `json:"lookup_attempted"`
	LookupAccepted         bool                    `json:"lookup_accepted"`
	LookupRejectionReason  ai.CacheRejectionReason `json:"lookup_rejection_reason,omitempty"`
	LookupHit              bool                    `json:"lookup_hit"`
	ProviderRequests       int                     `json:"provider_requests"`
	EvidencePlanCovered    bool                    `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted bool                    `json:"gcs_floor_retry_exhausted,omitempty"`
	CacheGeneration        string                  `json:"cache_generation,omitempty"`
}

type benchmarkJSONLTrace struct {
	ModelRequests     int            `json:"model_requests"`
	ProviderAttempts  int            `json:"provider_attempts"`
	ModelFailures     int            `json:"model_failures"`
	ToolCalls         int            `json:"tool_calls"`
	ToolFailures      int            `json:"tool_failures"`
	InputTokens       int            `json:"input_tokens"`
	CachedInputTokens int            `json:"cached_input_tokens"`
	OutputTokens      int            `json:"output_tokens"`
	Finalize          map[string]int `json:"finalize"`
	FinalizeRecovery  map[string]int `json:"finalize_recovery"`
	Critique          map[string]int `json:"critique"`
}

func writeBenchmarkJSONL(t *testing.T, path string, bc benchCase, repetition int, tc *models.TestCase, outcome benchmarkOutcome, elapsed time.Duration, snapshot ai.AnalysisTraceFile, observations []benchmarkDraftObservation, selectedAttempt int, toolUsage benchmarkToolUsage, traceSummary benchmarkTraceSummary, providerRequestCap int, cacheGeneration string, critiquePolicy ai.CritiqueCachePolicy, cacheVerification benchmarkCacheVerification) {
	t.Helper()
	if path == "" {
		return
	}
	if !benchmarkStableIDRE.MatchString(bc.stableID) {
		t.Fatalf("external benchmark results require a stable case id")
	}
	label := strings.TrimSpace(os.Getenv("BENCH_MODEL_LABEL"))
	if !benchmarkCaseIDRE.MatchString(label) {
		t.Fatalf("BENCH_MODEL_LABEL must be a stable anonymous label when BENCH_RESULTS_JSONL is set")
	}
	result := benchmarkJSONLResult{
		CaseID: bc.name, StableID: bc.stableID, Repetition: repetition, ModelLabel: label,
		JobName: bc.jobName, BuildID: bc.buildID, CheckoutCommit: bc.commit, TestName: bc.testName, TestSource: bc.testSource, ElapsedMS: elapsed.Milliseconds(), Outcome: string(outcome),
		FileLinks: map[string]string{}, SelectedAttempt: selectedAttempt,
		ToolNames: append([]string(nil), toolUsage.names...), ToolCounts: append([]string(nil), toolUsage.counts...),
		FloorNudges: traceSummary.floorNudges, FloorNudgeReasons: append([]string(nil), traceSummary.floorNudgeReasons...),
		ProviderRequestCap: providerRequestCap, TraceTruncated: traceSummary.truncated, CacheGeneration: cacheGeneration, CacheVerification: cacheVerification,
		CritiqueCachePolicy:     string(critiquePolicy),
		Trace:                   benchmarkJSONLTrace{Finalize: map[string]int{}, FinalizeRecovery: map[string]int{}, Critique: map[string]int{}},
		HumanScoreRubricVersion: benchmarkHumanScoreRubricVersion, HumanScoreMax: benchmarkHumanScoreMax,
		HumanScoreDimensions: append([]string(nil), benchmarkHumanScoreDimensions...),
	}
	for _, observation := range observations {
		result.Drafts = append(result.Drafts, benchmarkJSONLDraft{
			Attempt: observation.Attempt, Phase: observation.Phase, Selected: observation.Attempt == selectedAttempt,
			RuleIDs: append([]string(nil), observation.RuleIDs...), MatchedSkillIDs: append([]string(nil), observation.MatchedSkillIDs...),
			MissingGroups: append([]ai.CritiqueEvidenceGroupRef(nil), observation.MissingGroups...), UnavailableGroups: append([]ai.CritiqueEvidenceGroupRef(nil), observation.UnavailableGroups...),
			PublishedRuleIDs: append([]string(nil), observation.PublishedRuleIDs...), PublishedHardRules: append([]string(nil), observation.PublishedHardRules...), PublishedSoftRules: append([]string(nil), observation.PublishedSoftRules...),
			PublishedHardIssues: observation.PublishedHardIssues, PublishedPuntCount: observation.PublishedPuntCount, PublishedMissing: observation.PublishedMissing,
			PuntCount: observation.PuntCount, UnreadCitationCount: observation.UnreadCitationCount,
			CitationIssueCount: observation.CitationIssueCount, MissingGroupCount: observation.MissingGroupCount,
			TransientConflict: observation.TransientConflict, ToolCalls: observation.ToolCalls, EvidenceReads: observation.EvidenceReads,
		})
	}
	build := models.BuildInfo{Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs)}
	if source, ok := ai.ResolveBuildSource(build, bc.sourceRepo[0], bc.sourceRepo[1]); ok {
		result.SourceRevision = source.Revision
	} else {
		result.SourceUnavailable = true
	}
	if tc != nil && tc.AISummary != nil {
		result.Summary = tc.AISummary.Summary
		result.IsTransient = new(bool)
		*result.IsTransient = tc.AISummary.IsTransient
	}
	if tc != nil && tc.AIAnalysis != nil && tc.AISummary != nil {
		result.Usable = true
		result.RootCause, result.SuggestedFix, result.Severity = tc.AIAnalysis.RootCause, tc.AIAnalysis.SuggestedFix, tc.AIAnalysis.Severity
		result.GCSBytes = tc.AIAnalysis.GCSBytes
		result.EvidencePlanCovered = tc.AIAnalysis.EvidencePlanCovered
		result.GCSFloorRetryExhausted = tc.AIAnalysis.GCSFloorRetryExhausted
		result.CritiquePassed = new(bool)
		*result.CritiquePassed = tc.AIAnalysis.CritiquePassed
		result.CritiqueHardFailures = append([]string(nil), tc.AIAnalysis.CritiqueHardFailures...)
		result.CritiqueSoftWarnings = append([]string(nil), tc.AIAnalysis.CritiqueSoftWarnings...)
		result.BudgetExhausted = tc.AIAnalysis.BudgetExhausted
		result.Evidence = append([]models.EvidenceCitation(nil), tc.AIAnalysis.EvidenceCitations...)
		for key, value := range tc.AIAnalysis.FileLinks {
			result.FileLinks[key] = value
		}
		assessment := assessBenchmarkCase(bc, tc)
		result.SignalHits, result.SignalTotal = assessment.hits, assessment.total
		result.MissingMust = append(result.MissingMust, assessment.missingMust...)
	} else {
		result.SignalTotal = len(bc.signals) + len(bc.forbidden)
		if bc.expectedTransient != nil {
			result.SignalTotal++
		}
	}
	for _, trace := range snapshot.Traces {
		for _, event := range trace.Events {
			switch event.Kind {
			case "model_request":
				result.Trace.ModelRequests++
				result.Trace.ProviderAttempts += max(event.Attempts, 1)
				if event.Outcome == "error" {
					result.Trace.ModelFailures++
				}
				result.Trace.InputTokens += event.InputTokens
				result.Trace.CachedInputTokens += event.CachedInputTokens
				result.Trace.OutputTokens += event.OutputTokens
			case "tool_call":
				result.Trace.ToolCalls++
				if event.Outcome == "error" {
					result.Trace.ToolFailures++
				}
			case "finalize":
				result.Trace.Finalize[event.Outcome+":"+event.ErrorCode]++
			case "finalize_recovery":
				result.Trace.FinalizeRecovery[event.Outcome]++
			case "critique":
				result.Trace.Critique["outcome:"+event.Outcome]++
				result.Trace.Critique["punts"] += event.CritiquePunts
				result.Trace.Critique["unread"] += event.CritiqueUnread
				result.Trace.Critique["citations"] += event.CritiqueCitations
				result.Trace.Critique["skills"] += event.CritiqueSkills
				result.Trace.Critique["groups"] += event.CritiqueGroups
				result.Trace.Critique["transient"] += event.CritiqueTransient
				for _, rule := range event.CritiqueRules {
					result.Trace.Critique["rule:"+rule]++
				}
			case "draft_selection":
				if event.DraftDecision != nil {
					result.DraftDecisions = append(result.DraftDecisions, *event.DraftDecision)
				}
			}
		}
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open BENCH_RESULTS_JSONL: %v", err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(result); err != nil {
		t.Fatalf("write BENCH_RESULTS_JSONL: %v", err)
	}
}

func TestLoadBenchmarkManifest(t *testing.T) {
	valid := `{
  "version": 1,
  "cases": [{
    "id": "case-one",
    "stable_id": "0123456789abcdef0123",
    "bucket": "kubernetes-ci-logs",
    "job_type": "periodic",
    "job_name": "periodic-example",
    "build_id": "123456789",
    "web_url": "https://example.invalid/build/123456789/",
    "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "repo_version": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "repo_refs": {"example/project":"main"},
    "source_owner": "example",
    "source_name": "project",
    "test_name": "Example test",
    "junit_file": "junit.xml",
    "failure_message": "failed",
    "consecutive_failures": 2,
    "signals": [{"name":"cause","pattern":"(?i)root cause","must":true}]
  }]
}`
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cases, err := loadBenchmarkManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].name != "case-one" || cases[0].stableID != "0123456789abcdef0123" || !cases[0].signals[0].must {
		t.Fatalf("cases=%+v", cases)
	}

	buildValue := strings.Replace(valid, `"junit_file": "junit.xml",`, `"test_source": "build",`, 1)
	buildPath := filepath.Join(t.TempDir(), "build-manifest.json")
	if err := os.WriteFile(buildPath, []byte(buildValue), 0o600); err != nil {
		t.Fatal(err)
	}
	buildCases, err := loadBenchmarkManifest(buildPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(buildCases) != 1 || buildCases[0].testSource != models.TestCaseSourceBuild || buildCases[0].junitFile != "" {
		t.Fatalf("build cases = %+v", buildCases)
	}

	for name, mutate := range map[string]func(string) string{
		"unknown field": func(value string) string {
			return strings.Replace(value, `"version": 1`, `"version": 1, "extra": true`, 1)
		},
		"bad stable id": func(value string) string { return strings.Replace(value, "0123456789abcdef0123", "model-name", 1) },
		"bad regexp":    func(value string) string { return strings.Replace(value, "(?i)root cause", "[", 1) },
		"bad test source": func(value string) string {
			return strings.Replace(value, `"test_name": "Example test"`, `"test_name": "Example test", "test_source": "junit"`, 1)
		},
		"build source with junit": func(value string) string {
			return strings.Replace(value, `"junit_file": "junit.xml"`, `"test_source": "build", "junit_file": "junit.xml"`, 1)
		},
		"second object": func(value string) string { return value + `{}` },
	} {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(bad, []byte(mutate(valid)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadBenchmarkManifest(bad); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestWriteBenchmarkJSONLIsBlindedAndPrivate(t *testing.T) {
	t.Setenv("BENCH_MODEL_LABEL", "model-a")
	path := filepath.Join(t.TempDir(), "results.jsonl")
	bc := benchCase{
		name: "case-one", stableID: "0123456789abcdef0123", jobName: "job", buildID: "123", testName: "test", testSource: models.TestCaseSourceBuild,
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": "main"},
		sourceRepo: [2]string{"example", "project"},
		signals:    []benchSignal{{name: "cause", re: regexp.MustCompile(`root cause`), must: true}},
	}
	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "summary"},
		AIAnalysis: &models.AIAnalysis{
			Model: "PRIVATE_MODEL", RootCause: "root cause", SuggestedFix: "fix", Severity: "High",
			FileLinks: map[string]string{"file.go": "https://example.invalid/file.go"}, ToolCalls: 3, GCSBytes: 42,
			EvidencePlanCovered: true, GCSFloorRetryExhausted: true, CritiquePassed: true, BudgetExhausted: true,
		},
	}
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Events: []ai.TraceEvent{
		{Kind: "model_request", Outcome: "success", Attempts: 2, InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2},
		{Kind: "tool_call", Outcome: "success"},
		{Kind: "finalize", Outcome: "empty", ErrorCode: "unexpected_tool_call"},
		{Kind: "finalize_recovery", Outcome: "retained_draft"},
		{Kind: "critique", Outcome: "objected", CritiquePunts: 1},
		{Kind: "draft_selection", Outcome: "accepted", Status: "best", DraftDecision: &ai.DraftDecisionTrace{
			Target: "best", CurrentAttempt: 1, CandidateAttempt: 2,
			CurrentPublishedSoftRules: []string{"remediation.punt"}, CandidatePublishedSoftRules: []string{"evidence.available_unread"},
			CurrentEvidenceRevision: 3, CandidateEvidenceRevision: 7, RootCauseMateriallyChanged: true,
			PublishedStrictDominance: true, ReplacementAccepted: true, ReplacementReason: "candidate_published_dominates",
		}},
	}}}}
	cacheVerification := benchmarkCacheVerification{
		PersistenceAttempted: true, PersistenceAccepted: true, CacheSaveSucceeded: true,
		LookupAttempted: true, LookupAccepted: true, LookupHit: true,
		EvidencePlanCovered: true, GCSFloorRetryExhausted: true, CacheGeneration: "generation",
	}
	observations := []benchmarkDraftObservation{{DraftObservation: ai.DraftObservation{
		Attempt: 1, Phase: "initial", RuleIDs: []string{"remediation.punt"},
		MatchedSkillIDs: []string{"skill-a"}, MissingGroups: []ai.CritiqueEvidenceGroupRef{{SkillID: "skill-a", GroupID: "group-a"}}, PuntCount: 1,
	}}}
	writeBenchmarkJSONL(t, path, bc, 2, tc, benchmarkOutcomeUsable, 3*time.Second, snapshot, observations, 1,
		benchmarkToolUsage{names: []string{"read_artifact"}, counts: []string{"read_artifact=1"}},
		benchmarkTraceSummary{floorNudges: 1, floorNudgeReasons: []string{"gcs_bytes"}}, 17, "generation", ai.CritiqueCachePolicyHard, cacheVerification)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE_MODEL") {
		t.Fatalf("JSONL leaked model identity: %s", data)
	}
	var result benchmarkJSONLResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ModelLabel != "model-a" || result.Repetition != 2 || result.Outcome != string(benchmarkOutcomeUsable) || result.IsTransient == nil || *result.IsTransient || result.SignalHits != 1 || result.SourceRevision != strings.Repeat("a", 40) || result.SourceUnavailable || result.TestSource != models.TestCaseSourceBuild ||
		result.Trace.Finalize["empty:unexpected_tool_call"] != 1 || result.Trace.Critique["punts"] != 1 || result.GCSBytes != 42 ||
		!result.EvidencePlanCovered || !result.GCSFloorRetryExhausted || result.CritiquePassed == nil || !*result.CritiquePassed || !result.BudgetExhausted ||
		result.FloorNudges != 1 || !slices.Equal(result.FloorNudgeReasons, []string{"gcs_bytes"}) ||
		!slices.Equal(result.ToolNames, []string{"read_artifact"}) || !slices.Equal(result.ToolCounts, []string{"read_artifact=1"}) ||
		!result.CacheVerification.LookupAccepted || !result.CacheVerification.LookupHit || result.CacheGeneration != "generation" ||
		result.ProviderRequestCap != 17 || result.Trace.ProviderAttempts != 2 || result.TraceTruncated || result.CritiqueCachePolicy != string(ai.CritiqueCachePolicyHard) ||
		result.HumanScoreRubricVersion != 1 || result.HumanScoreMax != 10 || len(result.Drafts) != 1 ||
		len(result.DraftDecisions) != 1 || result.DraftDecisions[0].ReplacementReason != "candidate_published_dominates" ||
		!slices.Equal(result.HumanScoreDimensions, benchmarkHumanScoreDimensions) ||
		!slices.Equal(result.Drafts[0].RuleIDs, []string{"remediation.punt"}) || !result.Drafts[0].Selected {
		t.Fatalf("result=%+v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode=%o", info.Mode().Perm())
	}
}

func TestWriteBenchmarkJSONLRecordsGroundedUnavailableOutcome(t *testing.T) {
	t.Setenv("BENCH_MODEL_LABEL", "model-a")
	path := filepath.Join(t.TempDir(), "results.jsonl")
	bc := benchCase{
		name: "case-unavailable", stableID: "abcdef0123456789abcd", jobName: "job", buildID: "123", testName: "test",
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": strings.Repeat("a", 40)},
		sourceRepo: [2]string{"example", "project"}, allowUnavailable: true,
	}
	tc := &models.TestCase{AISummary: &models.AISummary{Summary: "AI analysis unavailable: no validated artifact citation supports the analysis"}}
	writeBenchmarkJSONL(t, path, bc, 1, tc, benchmarkOutcomeGroundedPolicyUnavailable, time.Second, ai.AnalysisTraceFile{}, nil, 0, benchmarkToolUsage{}, benchmarkTraceSummary{}, 1, "", ai.CritiqueCachePolicyHard, benchmarkCacheVerification{})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result benchmarkJSONLResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != string(benchmarkOutcomeGroundedPolicyUnavailable) || result.Usable || result.IsTransient == nil || *result.IsTransient || result.Summary != tc.AISummary.Summary {
		t.Fatalf("result = %+v", result)
	}
}
