package benchmarks

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
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
)

const benchmarkManifestVersion = 5

var benchmarkCaseIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var benchmarkStableIDRE = regexp.MustCompile(`^[0-9a-f]{20}$`)
var benchmarkSourceIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var benchmarkCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var benchmarkRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
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
	expectedEvidenceGroups := map[string][]string{
		"secrets-store-csi-image-scan":        {"security-gate", "trivy-findings"},
		"kueue-was-podgroup-api-mismatch":     {"apiserver-runtime-config", "competing-readiness-noise", "podgroup-api-response", "scheduler-handler-readiness", "terminal-deployment-timeout"},
		"gcp-pd-csi-windows-mount-visibility": {"node-publish-success", "windows-mount-missing"},
	}
	expectedEvidenceModes := map[string]string{
		"secrets-store-csi-image-scan":        benchmarkEvidenceModeArtifactOnly,
		"kueue-was-podgroup-api-mismatch":     benchmarkEvidenceModeArtifactOnly,
		"gcp-pd-csi-windows-mount-visibility": benchmarkEvidenceModeArtifactAndSource,
	}
	evidenceModeCounts := map[string]int{}
	for _, bc := range cases {
		if bc.evidenceMode != expectedEvidenceModes[bc.name] {
			t.Fatalf("case %q evidence mode = %q, want %q", bc.name, bc.evidenceMode, expectedEvidenceModes[bc.name])
		}
		if bc.evidenceMode == benchmarkEvidenceModeArtifactAndSource {
			if len(bc.sourceRanges) != 1 || bc.sourceRanges[0].Path != "test/k8s-integration/main.go" || bc.sourceRanges[0].LineStart != 638 || bc.sourceRanges[0].LineEnd != 671 || len(bc.sourceSignals) != 1 || bc.sourceSignals[0].name != "identifies Windows snapshot test selection gap" {
				t.Fatalf("case %q source expectations = ranges %v signals %+v", bc.name, bc.sourceRanges, bc.sourceSignals)
			}
		} else if len(bc.sourceRanges) != 0 || len(bc.sourceSignals) != 0 {
			t.Fatalf("case %q artifact-only source expectations = ranges %v signals %+v", bc.name, bc.sourceRanges, bc.sourceSignals)
		}
		evidenceModeCounts[bc.evidenceMode]++
		if bc.fixtureAsset == "" || len(bc.fixtureSHA256) != 64 {
			t.Fatalf("case %q has incomplete fixture identity", bc.name)
		}
		if bc.consumerCommit == "" || bc.projectSHA256 == "" || bc.promptSHA256 == "" || bc.expectedTransient == nil || bc.referenceDiagnosis == "" {
			t.Fatalf("case %q has incomplete consumer or transient identity", bc.name)
		}
		var evidenceGroupIDs []string
		for _, group := range bc.evidenceGroups {
			evidenceGroupIDs = append(evidenceGroupIDs, group.id)
		}
		sort.Strings(evidenceGroupIDs)
		if !slices.Equal(evidenceGroupIDs, expectedEvidenceGroups[bc.name]) {
			t.Fatalf("case %q evidence groups = %v, want %v", bc.name, evidenceGroupIDs, expectedEvidenceGroups[bc.name])
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
			var mountSignal, uncertaintySignal *benchSignal
			for i := range bc.signals {
				switch bc.signals[i].name {
				case "identifies missing Windows mount path":
					mountSignal = &bc.signals[i]
				case "preserves ownership uncertainty":
					uncertaintySignal = &bc.signals[i]
				}
			}
			if mountSignal == nil || uncertaintySignal == nil {
				t.Fatal("GCP benchmark signals are incomplete")
			}
			for _, text := range []string{
				`The helper could not resolve a volumeID for mountPath "/mnt/volume1".`,
				`The helper failed to map the volume ID to C:\mnt\volume1.`,
			} {
				if !mountSignal.matches(text) {
					t.Errorf("missing-mount signal rejects equivalent wording %q", text)
				}
			}
			for _, text := range []string{
				"The owning component boundary is not established by the evidence.",
				"The remaining ownership boundary remains unresolved.",
			} {
				if !uncertaintySignal.matches(text) {
					t.Errorf("ownership-uncertainty signal rejects equivalent wording %q", text)
				}
			}
			for _, text := range []string{
				`The helper resolved volumeID "disk-1" for /mnt/volume1.`,
				`The helper could not resolve the kubelet hostname. It then mapped volumeID "disk-1" to /mnt/volume1 successfully.`,
			} {
				if mountSignal.matches(text) {
					t.Errorf("missing-mount signal accepted unrelated or successful resolution %q", text)
				}
			}
			if uncertaintySignal.matches("The owning component boundary is established by the evidence.") {
				t.Error("ownership-uncertainty signal accepted established ownership")
			}
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
	if evidenceModeCounts[benchmarkEvidenceModeArtifactOnly] != 2 || evidenceModeCounts[benchmarkEvidenceModeArtifactAndSource] != 1 {
		t.Fatalf("evidence mode counts = %v, want two artifact-only and one artifact-and-source", evidenceModeCounts)
	}
}

type benchmarkManifestCase struct {
	ID                   string                           `json:"id"`
	StableID             string                           `json:"stable_id"`
	Bucket               string                           `json:"bucket"`
	FixtureAsset         string                           `json:"fixture_asset,omitempty"`
	FixtureSHA256        string                           `json:"fixture_sha256,omitempty"`
	JobType              string                           `json:"job_type"`
	Repo                 string                           `json:"repo,omitempty"`
	JobName              string                           `json:"job_name"`
	BuildID              string                           `json:"build_id"`
	PullNumber           string                           `json:"pull_number,omitempty"`
	WebURL               string                           `json:"web_url"`
	Commit               string                           `json:"commit"`
	RepoVersion          string                           `json:"repo_version"`
	RepoRefs             map[string]string                `json:"repo_refs"`
	SourceRefs           []benchmarkSourceRef             `json:"source_refs"`
	PrimarySourceID      string                           `json:"primary_source_id"`
	TestName             string                           `json:"test_name"`
	TestSource           string                           `json:"test_source,omitempty"`
	JUnitFile            string                           `json:"junit_file,omitempty"`
	FailureMessage       string                           `json:"failure_message"`
	EvidenceMode         string                           `json:"evidence_mode"`
	ConsecutiveFailures  int                              `json:"consecutive_failures,omitempty"`
	OppositeDiagnosis    string                           `json:"opposite_diagnosis,omitempty"`
	OppositeTransient    bool                             `json:"opposite_is_transient,omitempty"`
	ReferenceDiagnosis   string                           `json:"reference_diagnosis,omitempty"`
	ReferenceTransient   bool                             `json:"reference_is_transient,omitempty"`
	AllowUnavailable     bool                             `json:"allow_unavailable,omitempty"`
	ExpectedTransient    *bool                            `json:"expected_transient,omitempty"`
	Forbidden            []benchmarkManifestSignal        `json:"forbidden,omitempty"`
	ConsumerCommit       string                           `json:"consumer_commit,omitempty"`
	ProjectSHA256        string                           `json:"project_sha256,omitempty"`
	PromptSHA256         string                           `json:"prompt_sha256,omitempty"`
	Signals              []benchmarkManifestSignal        `json:"signals"`
	ExpectedSourceRanges []benchmarkManifestSourceRange   `json:"expected_source_ranges,omitempty"`
	SourceSignals        []benchmarkManifestSignal        `json:"source_signals,omitempty"`
	EvidenceGroups       []benchmarkManifestEvidenceGroup `json:"evidence_groups,omitempty"`
	OracleEvidenceSHA256 string                           `json:"oracle_evidence_sha256,omitempty"`
}

type benchmarkManifestSourceRange struct {
	SourceID  string `json:"source_id"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

type benchmarkManifestSignal struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Negated string `json:"negated,omitempty"`
	Must    bool   `json:"must,omitempty"`
}

type benchmarkManifestEvidenceGroup struct {
	ID                 string                    `json:"id"`
	Paths              []string                  `json:"paths"`
	Content            []string                  `json:"content,omitempty"`
	Causal             []benchmarkManifestSignal `json:"causal,omitempty"`
	OracleContextLines *int                      `json:"oracle_context_lines,omitempty"`
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
		if !validBenchmarkEvidenceMode(item.EvidenceMode) {
			return nil, fmt.Errorf("benchmark manifest case %q has invalid evidence_mode", item.ID)
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
		sourceRefs, err := canonicalBenchmarkSourceRefs(item.SourceRefs)
		if err != nil {
			return nil, fmt.Errorf("benchmark manifest case %q source refs: %w", item.ID, err)
		}
		primarySource, ok := benchmarkSourceRefByID(benchCase{sourceRefs: sourceRefs}, item.PrimarySourceID)
		if !ok {
			return nil, fmt.Errorf("benchmark manifest case %q primary_source_id is not configured", item.ID)
		}
		if revision, ok := item.RepoRefs[primarySource.Repository]; !ok || revision != primarySource.Revision {
			return nil, fmt.Errorf("benchmark manifest case %q primary source must match repo_refs", item.ID)
		}
		for repo, ref := range item.RepoRefs {
			if repo == "" || ref == "" || len(repo) > 256 || len(ref) > 256 || strings.ContainsAny(repo+ref, "\r\n\x00") {
				return nil, fmt.Errorf("benchmark manifest case %q has invalid repo_refs", item.ID)
			}
		}
		if item.Bucket == "" || item.JobName == "" || item.WebURL == "" || item.TestName == "" || item.FailureMessage == "" {
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
			"primary_source_id": item.PrimarySourceID, "junit_file": item.JUnitFile,
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
		if item.EvidenceMode == benchmarkEvidenceModeArtifactOnly && (len(item.ExpectedSourceRanges) > 0 || len(item.SourceSignals) > 0) {
			return nil, fmt.Errorf("benchmark manifest artifact-only case %q cannot declare source expectations", item.ID)
		}
		if item.EvidenceMode == benchmarkEvidenceModeArtifactAndSource && (len(item.ExpectedSourceRanges) == 0 || len(item.ExpectedSourceRanges) > 8 || len(item.SourceSignals) == 0 || len(item.SourceSignals) > 8) {
			return nil, fmt.Errorf("benchmark manifest source-required case %q has incomplete source expectations", item.ID)
		}
		sourceRanges := make([]benchmarkSourceRange, 0, len(item.ExpectedSourceRanges))
		for _, value := range item.ExpectedSourceRanges {
			source, ok := benchmarkSourceRefByID(benchCase{sourceRefs: sourceRefs}, value.SourceID)
			if !ok {
				return nil, fmt.Errorf("benchmark manifest case %q source range source_id %q is not configured", item.ID, value.SourceID)
			}
			sourceRanges = append(sourceRanges, benchmarkSourceRange{Repository: source.Repository, Revision: source.Revision, Path: value.Path, LineStart: value.LineStart, LineEnd: value.LineEnd})
		}
		sourceRanges, err = canonicalBenchmarkExpectedSourceRanges(sourceRanges)
		if err != nil {
			return nil, fmt.Errorf("benchmark manifest case %q source ranges: %w", item.ID, err)
		}
		sourceSignals := make([]benchSignal, 0, len(item.SourceSignals))
		for signalIndex, signal := range item.SourceSignals {
			if signal.Name == "" || signal.Pattern == "" || !signal.Must || len(signal.Pattern) > 2048 || len(signal.Negated) > 2048 {
				return nil, fmt.Errorf("benchmark manifest case %q source signal %d is invalid", item.ID, signalIndex)
			}
			positive, err := regexp.Compile(signal.Pattern)
			if err != nil {
				return nil, fmt.Errorf("benchmark manifest case %q source signal %d pattern: %w", item.ID, signalIndex, err)
			}
			var negative *regexp.Regexp
			if signal.Negated != "" {
				negative, err = regexp.Compile(signal.Negated)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q source signal %d negated: %w", item.ID, signalIndex, err)
				}
			}
			sourceSignals = append(sourceSignals, benchSignal{name: signal.Name, re: positive, negated: negative, must: true})
		}
		if len(item.Forbidden) > 16 {
			return nil, fmt.Errorf("benchmark manifest case %q forbidden count exceeds 16", item.ID)
		}
		if len(item.EvidenceGroups) > 16 {
			return nil, fmt.Errorf("benchmark manifest case %q evidence group count exceeds 16", item.ID)
		}
		if item.OracleEvidenceSHA256 != "" && !benchmarkSHA256RE.MatchString(item.OracleEvidenceSHA256) {
			return nil, fmt.Errorf("benchmark manifest case %q oracle evidence SHA-256 is invalid", item.ID)
		}
		evidenceGroups := make([]benchmarkEvidenceGroup, 0, len(item.EvidenceGroups))
		seenEvidenceGroups := map[string]bool{}
		oracleGroups := 0
		for groupIndex, group := range item.EvidenceGroups {
			if !benchmarkCaseIDRE.MatchString(group.ID) || seenEvidenceGroups[group.ID] || len(group.Paths) == 0 || len(group.Paths) > 8 || len(group.Content) > 8 || len(group.Causal) > 8 {
				return nil, fmt.Errorf("benchmark manifest case %q evidence group %d is invalid", item.ID, groupIndex)
			}
			seenEvidenceGroups[group.ID] = true
			if group.OracleContextLines != nil && (*group.OracleContextLines < 0 || *group.OracleContextLines > 5 || len(group.Content) == 0) {
				return nil, fmt.Errorf("benchmark manifest case %q evidence group %q oracle context is invalid", item.ID, group.ID)
			}
			compiled := benchmarkEvidenceGroup{id: group.ID, oracleContextLines: group.OracleContextLines}
			if group.OracleContextLines != nil {
				oracleGroups++
			}
			for patternIndex, pattern := range group.Paths {
				if pattern == "" || len(pattern) > 1024 {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q path %d is invalid", item.ID, group.ID, patternIndex)
				}
				re, err := regexp.Compile(pattern)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q path %d: %w", item.ID, group.ID, patternIndex, err)
				}
				compiled.pathREs = append(compiled.pathREs, re)
			}
			for patternIndex, pattern := range group.Content {
				if pattern == "" || len(pattern) > 1024 {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q content %d is invalid", item.ID, group.ID, patternIndex)
				}
				re, err := regexp.Compile(pattern)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q content %d: %w", item.ID, group.ID, patternIndex, err)
				}
				compiled.contentREs = append(compiled.contentREs, re)
			}
			for signalIndex, signal := range group.Causal {
				if signal.Name == "" || signal.Pattern == "" || signal.Must || len(signal.Pattern) > 2048 || len(signal.Negated) > 2048 {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q causal %d is invalid", item.ID, group.ID, signalIndex)
				}
				positive, err := regexp.Compile(signal.Pattern)
				if err != nil {
					return nil, fmt.Errorf("benchmark manifest case %q evidence group %q causal %d pattern: %w", item.ID, group.ID, signalIndex, err)
				}
				var negative *regexp.Regexp
				if signal.Negated != "" {
					negative, err = regexp.Compile(signal.Negated)
					if err != nil {
						return nil, fmt.Errorf("benchmark manifest case %q evidence group %q causal %d negated: %w", item.ID, group.ID, signalIndex, err)
					}
				}
				compiled.causalSignals = append(compiled.causalSignals, benchSignal{name: signal.Name, re: positive, negated: negative})
			}
			evidenceGroups = append(evidenceGroups, compiled)
		}
		if (oracleGroups > 0) != (item.OracleEvidenceSHA256 != "") {
			return nil, fmt.Errorf("benchmark manifest case %q oracle evidence identity is incomplete", item.ID)
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
			name: item.ID, stableID: item.StableID, bucket: item.Bucket, evidenceMode: item.EvidenceMode, fixtureAsset: item.FixtureAsset,
			fixtureSHA256: item.FixtureSHA256, jobType: item.JobType, repo: item.Repo, jobName: item.JobName,
			buildID: item.BuildID, pullNumber: item.PullNumber, webURL: item.WebURL,
			commit: item.Commit, repoVersion: item.RepoVersion, repoRefs: maps.Clone(item.RepoRefs),
			sourceRefs: sourceRefs, primarySourceID: item.PrimarySourceID,
			sourceRepo: [2]string{strings.SplitN(primarySource.Repository, "/", 2)[0], strings.SplitN(primarySource.Repository, "/", 2)[1]}, testName: item.TestName, testSource: item.TestSource,
			junitFile: item.JUnitFile, failureMsg: item.FailureMessage, consecutiveFailures: item.ConsecutiveFailures,
			oppositeDiagnosis: item.OppositeDiagnosis, oppositeTransient: item.OppositeTransient,
			referenceDiagnosis: item.ReferenceDiagnosis, referenceTransient: item.ReferenceTransient,
			allowUnavailable: item.AllowUnavailable, expectedTransient: item.ExpectedTransient, forbidden: forbidden,
			consumerCommit: item.ConsumerCommit, projectSHA256: item.ProjectSHA256, promptSHA256: item.PromptSHA256,
			signals: signals, sourceRanges: sourceRanges, sourceSignals: sourceSignals,
			evidenceGroups: evidenceGroups, oracleEvidenceSHA256: item.OracleEvidenceSHA256,
		})
	}
	return out, nil
}

type benchmarkJSONLResult struct {
	CaseID                    string                         `json:"case_id"`
	StableID                  string                         `json:"stable_id"`
	Repetition                int                            `json:"repetition"`
	ModelLabel                string                         `json:"model_label"`
	Arm                       string                         `json:"arm"`
	EngineCommit              string                         `json:"engine_commit"`
	BenchmarkManifestSHA256   string                         `json:"benchmark_manifest_sha256"`
	FixtureSHA256             string                         `json:"fixture_sha256,omitempty"`
	BaselineConsumerCommit    string                         `json:"baseline_consumer_commit,omitempty"`
	BaselinePromptSHA256      string                         `json:"baseline_prompt_sha256,omitempty"`
	ProjectSHA256             string                         `json:"project_sha256,omitempty"`
	EffectivePromptSHA256     string                         `json:"effective_prompt_sha256"`
	SkillSetHash              string                         `json:"skill_set_hash"`
	EffectiveInputSHA256      string                         `json:"effective_input_sha256"`
	ComparisonInputSHA256     string                         `json:"comparison_input_sha256"`
	APIMode                   string                         `json:"api_mode"`
	ReasoningEffort           string                         `json:"reasoning_effort,omitempty"`
	ProviderPath              string                         `json:"provider_path,omitempty"`
	ProviderConfigSHA256      string                         `json:"provider_config_sha256"`
	TransportID               string                         `json:"transport_id,omitempty"`
	ModelContextTokens        int                            `json:"model_context_tokens"`
	ModelOutputTokens         int                            `json:"model_output_tokens"`
	Pricing                   benchmarkPricingIdentity       `json:"pricing"`
	EvidenceTelemetryVersion  int                            `json:"evidence_telemetry_version"`
	EvidenceCondition         string                         `json:"evidence_condition"`
	EvidenceMode              string                         `json:"evidence_mode"`
	SourceExpectationSHA256   string                         `json:"source_expectation_sha256"`
	ExpectedSourceRanges      []benchmarkSourceRange         `json:"expected_source_ranges"`
	SourceReadCoverageHits    int                            `json:"source_read_coverage_hits"`
	SourceReadCoverageTotal   int                            `json:"source_read_coverage_total"`
	SourceReadCoveredLines    int                            `json:"source_read_covered_lines"`
	SourceReadExpectedLines   int                            `json:"source_read_expected_lines"`
	SourceReadPartialRatio    float64                        `json:"source_read_partial_coverage_ratio"`
	SourceReadRangeCoverage   []benchmarkSourceRangeCoverage `json:"source_read_range_coverage"`
	SourceSignalHits          int                            `json:"source_signal_hits"`
	SourceSignalTotal         int                            `json:"source_signal_total"`
	SourceEvidenceToolCalls   int                            `json:"source_evidence_tool_calls"`
	SourceReadRanges          []benchmarkSourceRead          `json:"source_read_ranges"`
	SourceReadCount           int                            `json:"source_read_count"`
	SourceCitations           []benchmarkSourceCitation      `json:"source_citations"`
	SourceCitationEmitted     int                            `json:"source_citation_emitted_count"`
	SourceCitationVerified    int                            `json:"source_citation_verified_count"`
	FrozenEvidenceSHA256      string                         `json:"frozen_evidence_sha256,omitempty"`
	EvidenceStageSHA256       string                         `json:"evidence_stage_sha256"`
	EvidenceStageIDs          []string                       `json:"evidence_stage_ids"`
	ModelRequestMade          bool                           `json:"model_request_made"`
	TrialStatus               string                         `json:"trial_status"`
	ContractViolation         bool                           `json:"contract_violation"`
	AnalysisDisposition       string                         `json:"analysis_disposition,omitempty"`
	DispositionWarnings       []string                       `json:"disposition_warnings,omitempty"`
	StructuredValid           bool                           `json:"structured_valid"`
	Displayable               bool                           `json:"displayable"`
	Grounded                  bool                           `json:"grounded"`
	EvidenceStages            []benchmarkEvidenceStage       `json:"evidence_stages"`
	EvidenceRevisions         []benchmarkEvidenceRevision    `json:"evidence_revisions"`
	EvidenceGroupsSelected    []string                       `json:"evidence_groups_selected,omitempty"`
	EvidenceGroupsHit         []string                       `json:"evidence_groups_hit,omitempty"`
	EvidenceGroupsMissed      []string                       `json:"evidence_groups_missed,omitempty"`
	EvidenceGroupSources      map[string][]string            `json:"evidence_group_sources,omitempty"`
	JobName                   string                         `json:"job_name"`
	BuildID                   string                         `json:"build_id"`
	CheckoutCommit            string                         `json:"checkout_commit"`
	SourceRevision            string                         `json:"source_revision,omitempty"`
	SourceUnavailable         bool                           `json:"source_unavailable,omitempty"`
	TestName                  string                         `json:"test_name"`
	TestSource                string                         `json:"test_source,omitempty"`
	ElapsedMS                 int64                          `json:"elapsed_ms"`
	Outcome                   string                         `json:"outcome"`
	Usable                    bool                           `json:"usable"`
	IsTransient               *bool                          `json:"is_transient,omitempty"`
	Summary                   string                         `json:"summary,omitempty"`
	RootCause                 string                         `json:"root_cause,omitempty"`
	SuggestedFix              string                         `json:"suggested_fix,omitempty"`
	Severity                  string                         `json:"severity,omitempty"`
	Evidence                  []models.EvidenceCitation      `json:"evidence_citations,omitempty"`
	RelevantFiles             []string                       `json:"relevant_files,omitempty"`
	FileLinks                 map[string]string              `json:"file_links,omitempty"`
	SignalHits                int                            `json:"signal_hits"`
	SignalTotal               int                            `json:"signal_total"`
	DiagnosisSignalHits       int                            `json:"diagnosis_signal_hits"`
	DiagnosisSignalTotal      int                            `json:"diagnosis_signal_total"`
	TransientCorrect          *bool                          `json:"transient_classification_correct,omitempty"`
	ForbiddenChecksPassed     int                            `json:"forbidden_checks_passed"`
	ForbiddenChecksTotal      int                            `json:"forbidden_checks_total"`
	MissingMust               []string                       `json:"missing_must,omitempty"`
	SelectedAttempt           int                            `json:"selected_attempt,omitempty"`
	Drafts                    []benchmarkJSONLDraft          `json:"drafts,omitempty"`
	DraftDecisions            []ai.DraftDecisionTrace        `json:"draft_decisions,omitempty"`
	SemanticJudgeOutcomes     []string                       `json:"semantic_judge_outcomes"`
	SemanticFindingClasses    []string                       `json:"semantic_finding_classes"`
	SemanticRevisionAttempted bool                           `json:"semantic_revision_attempted"`
	SemanticRevisionSelected  bool                           `json:"semantic_revision_selected"`
	SemanticRevisionRejected  bool                           `json:"semantic_revision_rejected"`
	SupportedFactsRetained    int                            `json:"supported_facts_retained"`
	SupportedFactsAdded       int                            `json:"supported_facts_added"`
	SupportedFactsDropped     int                            `json:"supported_facts_dropped"`
	ToolNames                 []string                       `json:"tool_names,omitempty"`
	ToolCounts                []string                       `json:"tool_counts,omitempty"`
	GCSBytes                  int                            `json:"gcs_bytes,omitempty"`
	EvidencePlanCovered       bool                           `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted    bool                           `json:"gcs_floor_retry_exhausted,omitempty"`
	CritiquePassed            *bool                          `json:"critique_passed,omitempty"`
	CritiqueCachePolicy       string                         `json:"critique_cache_policy,omitempty"`
	CritiqueHardFailures      []string                       `json:"critique_hard_failures,omitempty"`
	CritiqueSoftWarnings      []string                       `json:"critique_soft_warnings,omitempty"`
	BudgetExhausted           bool                           `json:"budget_exhausted,omitempty"`
	FloorNudges               int                            `json:"floor_nudges,omitempty"`
	FloorNudgeReasons         []string                       `json:"floor_nudge_reasons,omitempty"`
	ProviderRequestCap        int                            `json:"provider_request_cap"`
	TraceTruncated            bool                           `json:"trace_truncated,omitempty"`
	CacheGeneration           string                         `json:"cache_generation,omitempty"`
	CacheVerification         benchmarkCacheVerification     `json:"cache_verification"`
	Trace                     benchmarkJSONLTrace            `json:"trace"`
	HumanScoreRubricVersion   int                            `json:"human_score_rubric_version"`
	HumanScoreMax             int                            `json:"human_score_max"`
	HumanScoreDimensions      []string                       `json:"human_score_dimensions"`
}

const (
	benchmarkHumanScoreRubricVersion = 2
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
	UnavailableCooldownHit bool                    `json:"unavailable_cooldown_hit"`
	ProviderRequests       int                     `json:"provider_requests"`
	EvidencePlanCovered    bool                    `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted bool                    `json:"gcs_floor_retry_exhausted,omitempty"`
	CacheGeneration        string                  `json:"cache_generation,omitempty"`
}

func benchmarkSourceEvidenceToolCalls(usage benchmarkToolUsage) int {
	calls := 0
	for _, count := range usage.counts {
		name, raw, ok := strings.Cut(count, "=")
		if !ok || (name != "read_repo_file" && name != "grep_repo") {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err == nil && value > 0 {
			calls += value
		}
	}
	return calls
}

type benchmarkJSONLTrace struct {
	ModelRequests         int            `json:"model_requests"`
	ReportedRequests      int            `json:"reported_requests"`
	ProviderAttempts      int            `json:"provider_attempts"`
	ProviderAttemptsKnown bool           `json:"provider_attempts_known"`
	ModelFailures         int            `json:"model_failures"`
	ToolCalls             int            `json:"tool_calls"`
	ToolFailures          int            `json:"tool_failures"`
	InputTokens           int            `json:"input_tokens"`
	CachedInputTokens     int            `json:"cached_input_tokens"`
	OutputTokens          int            `json:"output_tokens"`
	ReasoningTokens       int            `json:"reasoning_tokens"`
	Finalize              map[string]int `json:"finalize"`
	FinalizeRecovery      map[string]int `json:"finalize_recovery"`
	Critique              map[string]int `json:"critique"`
}

func writeBenchmarkJSONL(t *testing.T, path string, bc benchCase, repetition int, tc *models.TestCase, outcome benchmarkOutcome, elapsed time.Duration, snapshot ai.AnalysisTraceFile, observations []benchmarkDraftObservation, selectedAttempt int, toolUsage benchmarkToolUsage, traceSummary benchmarkTraceSummary, providerRequestCap int, cacheGeneration string, critiquePolicy ai.CritiqueCachePolicy, cacheVerification benchmarkCacheVerification, identity benchmarkRunIdentity, evidenceCoverage benchmarkEvidenceCoverage, stageReport benchmarkEvidenceStageReport) {
	t.Helper()
	if path == "" {
		return
	}
	if !benchmarkStableIDRE.MatchString(bc.stableID) {
		t.Fatalf("external benchmark results require a stable case id")
	}
	if err := validateBenchmarkRunIdentity(identity); err != nil {
		t.Fatalf("benchmark run identity: %v", err)
	}
	if identity.ModelContextTokens != 0 && (!benchmarkSHA256RE.MatchString(identity.ProviderConfigSHA256) || validateBenchmarkPricingIdentity(identity.Pricing) != nil) {
		t.Fatal("benchmark provider or pricing identity is incomplete")
	}
	if err := validateBenchmarkEvidenceStageReport(bc, stageReport); err != nil {
		t.Fatalf("benchmark evidence stages: %v", err)
	}
	if identity.EvidenceStageSHA256 != benchmarkEvidenceStageSHA256(bc.evidenceGroups) {
		t.Fatal("benchmark evidence stage identity does not match the case")
	}
	label := strings.TrimSpace(os.Getenv("BENCH_MODEL_LABEL"))
	if !benchmarkCaseIDRE.MatchString(label) {
		t.Fatalf("BENCH_MODEL_LABEL must be a stable anonymous label when BENCH_RESULTS_JSONL is set")
	}
	recordedOutcome := outcome
	if stageReport.TrialStatus == "no_result" || stageReport.TrialStatus == "timeout" || stageReport.TrialStatus == "runtime_failure" || (stageReport.TrialStatus == "contract_violation" && (tc == nil || tc.AIAnalysis == nil)) {
		recordedOutcome = benchmarkOutcomeUnknown
	}
	result := benchmarkJSONLResult{
		CaseID: bc.name, StableID: bc.stableID, Repetition: repetition, ModelLabel: label,
		Arm: identity.Arm, EngineCommit: identity.EngineCommit, BenchmarkManifestSHA256: identity.BenchmarkManifestSHA256, FixtureSHA256: identity.FixtureSHA256,
		BaselineConsumerCommit: identity.BaselineConsumerCommit, BaselinePromptSHA256: identity.BaselinePromptSHA256,
		ProjectSHA256: identity.ProjectSHA256, EffectivePromptSHA256: identity.EffectivePromptSHA256,
		SkillSetHash: identity.SkillSetHash, EffectiveInputSHA256: identity.EffectiveInputSHA256, ComparisonInputSHA256: identity.ComparisonInputSHA256,
		APIMode: identity.APIMode, ReasoningEffort: string(identity.ReasoningEffort), ProviderPath: identity.ProviderPath, ProviderConfigSHA256: identity.ProviderConfigSHA256, TransportID: identity.TransportID,
		ModelContextTokens: identity.ModelContextTokens, ModelOutputTokens: identity.ModelOutputTokens, Pricing: identity.Pricing,
		EvidenceTelemetryVersion: 2, EvidenceCondition: stageReport.Condition, EvidenceMode: bc.evidenceMode,
		SourceExpectationSHA256: benchmarkSourceExpectationSHA256(bc), ExpectedSourceRanges: append([]benchmarkSourceRange{}, bc.sourceRanges...), SourceReadCoverageTotal: len(bc.sourceRanges), SourceSignalTotal: len(bc.sourceSignals),
		SourceEvidenceToolCalls: benchmarkSourceEvidenceToolCalls(toolUsage), FrozenEvidenceSHA256: stageReport.FrozenSHA256,
		EvidenceStageSHA256: identity.EvidenceStageSHA256, EvidenceStageIDs: benchmarkEvidenceStageIDs(bc.evidenceGroups), ModelRequestMade: stageReport.ModelRequestMade,
		TrialStatus: stageReport.TrialStatus, EvidenceStages: append([]benchmarkEvidenceStage{}, stageReport.Stages...),
		ContractViolation:      benchmarkTraceHasContractViolation(snapshot),
		EvidenceRevisions:      append([]benchmarkEvidenceRevision{}, stageReport.Revisions...),
		EvidenceGroupsSelected: append([]string(nil), evidenceCoverage.selected...), EvidenceGroupsHit: append([]string(nil), evidenceCoverage.hit...), EvidenceGroupsMissed: append([]string(nil), evidenceCoverage.missed...),
		EvidenceGroupSources: cloneBenchmarkEvidenceSources(evidenceCoverage.sources),
		JobName:              bc.jobName, BuildID: bc.buildID, CheckoutCommit: bc.commit, TestName: bc.testName, TestSource: bc.testSource, ElapsedMS: elapsed.Milliseconds(), Outcome: string(recordedOutcome),
		FileLinks: map[string]string{}, SourceReadRanges: []benchmarkSourceRead{}, SourceCitations: []benchmarkSourceCitation{}, SelectedAttempt: selectedAttempt,
		ToolNames: append([]string(nil), toolUsage.names...), ToolCounts: append([]string(nil), toolUsage.counts...),
		FloorNudges: traceSummary.floorNudges, FloorNudgeReasons: append([]string(nil), traceSummary.floorNudgeReasons...),
		SemanticJudgeOutcomes:  append([]string{}, traceSummary.semanticJudgeOutcomes...),
		SemanticFindingClasses: append([]string{}, traceSummary.semanticFindingClasses...),
		ProviderRequestCap:     providerRequestCap, TraceTruncated: traceSummary.truncated, CacheGeneration: cacheGeneration, CacheVerification: cacheVerification,
		CritiqueCachePolicy: string(critiquePolicy),
		Trace: benchmarkJSONLTrace{
			ProviderAttemptsKnown: true,
			Finalize:              map[string]int{}, FinalizeRecovery: map[string]int{}, Critique: map[string]int{},
		},
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
	if source, ok := benchmarkPrimarySourceRef(bc); ok {
		result.SourceRevision = source.Revision
	} else {
		result.SourceUnavailable = true
	}
	sourceReads, sourceReadErr := benchmarkSourceReadsFromInProcess(bc, toolUsage.sourceObservations)
	if sourceReadErr != nil {
		t.Fatal(sourceReadErr)
	}
	result.SourceReadRanges = sourceReads
	result.SourceReadCount = len(sourceReads)
	coverage := benchmarkExpectedSourceReadCoverage(bc.sourceRanges, sourceReads)
	result.SourceReadCoverageHits, result.SourceReadCoverageTotal = coverage.Hits, coverage.Total
	result.SourceReadCoveredLines, result.SourceReadExpectedLines = coverage.CoveredLines, coverage.ExpectedLines
	result.SourceReadPartialRatio, result.SourceReadRangeCoverage = coverage.CoverageRatio, coverage.Ranges
	if tc != nil && tc.AISummary != nil {
		result.Summary = tc.AISummary.Summary
		result.IsTransient = new(bool)
		*result.IsTransient = tc.AISummary.IsTransient
	}
	if tc != nil && tc.AIAnalysis != nil && tc.AISummary != nil {
		result.Usable = true
		result.AnalysisDisposition = tc.AIAnalysis.Disposition
		result.DispositionWarnings = append([]string(nil), tc.AIAnalysis.DispositionWarnings...)
		result.StructuredValid = result.AnalysisDisposition != ""
		result.Displayable = result.StructuredValid
		result.Grounded = result.AnalysisDisposition == models.AnalysisDispositionGrounded
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
		result.RelevantFiles = append([]string(nil), tc.AIAnalysis.RelevantFiles...)
		for key, value := range tc.AIAnalysis.FileLinks {
			result.FileLinks[key] = value
		}
		assessment := assessBenchmarkCase(bc, tc)
		result.SignalHits, result.SignalTotal = assessment.hits, assessment.total
		result.DiagnosisSignalHits, result.DiagnosisSignalTotal = assessment.diagnosisHits, assessment.diagnosisTotal
		result.SourceSignalHits, result.SourceSignalTotal = assessment.sourceHits, assessment.sourceTotal
		result.TransientCorrect = assessment.transientCorrect
		result.ForbiddenChecksPassed, result.ForbiddenChecksTotal = assessment.forbiddenPassed, assessment.forbiddenTotal
		result.MissingMust = append(result.MissingMust, assessment.missingMust...)
	} else {
		assessment := assessBenchmarkCase(bc, tc)
		result.SignalTotal = assessment.total
		result.DiagnosisSignalTotal = assessment.diagnosisTotal
		result.SourceSignalTotal = assessment.sourceTotal
		result.TransientCorrect = assessment.transientCorrect
		result.ForbiddenChecksTotal = assessment.forbiddenTotal
	}
	for _, trace := range snapshot.Traces {
		for _, event := range trace.Events {
			switch event.Kind {
			case "model_request":
				result.Trace.ModelRequests++
				if event.Attempts > 0 {
					result.Trace.ProviderAttempts += event.Attempts
				} else {
					result.Trace.ProviderAttempts++
					result.Trace.ProviderAttemptsKnown = false
				}
				if event.Outcome == "error" {
					result.Trace.ModelFailures++
				}
				if event.UsageReported {
					result.Trace.ReportedRequests++
				}
				result.Trace.InputTokens += event.InputTokens
				result.Trace.CachedInputTokens += event.CachedInputTokens
				result.Trace.OutputTokens += event.OutputTokens
				result.Trace.ReasoningTokens += event.ReasoningTokens
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
					if event.DraftDecision.Target == "best" {
						result.SupportedFactsRetained = event.DraftDecision.SupportedFactsRetained
						result.SupportedFactsAdded = event.DraftDecision.SupportedFactsAdded
						result.SupportedFactsDropped = event.DraftDecision.SupportedFactsDropped
					}
				}
			case "semantic_judge":
				recordBenchmarkSemanticRevision(&result, event)
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

func recordBenchmarkSemanticRevision(result *benchmarkJSONLResult, event ai.TraceEvent) {
	if result == nil || event.Status != "revision" {
		return
	}
	result.SemanticRevisionAttempted = true
	switch event.Outcome {
	case "revised":
		result.SemanticRevisionSelected = true
	case "revision_denied", "revision_unparseable", "revision_rejected", "revision_not_selected":
		result.SemanticRevisionRejected = true
	}
}

func TestLoadBenchmarkManifest(t *testing.T) {
	valid := `{
  "version": 5,
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
    "repo_refs": {"example/project":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "source_refs": [{"id":"primary","repository":"example/project","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
    "primary_source_id": "primary",
    "test_name": "Example test",
    "junit_file": "junit.xml",
    "failure_message": "failed",
    "evidence_mode": "artifact_only",
    "consecutive_failures": 2,
    "signals": [{"name":"cause","pattern":"(?i)root cause","must":true}],
    "evidence_groups": [{"id":"initiating-error","paths":["build-log\\.txt$"],"content":["root cause"]}]
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
	if len(cases) != 1 || cases[0].name != "case-one" || cases[0].stableID != "0123456789abcdef0123" || cases[0].evidenceMode != benchmarkEvidenceModeArtifactOnly || !cases[0].signals[0].must || len(cases[0].evidenceGroups) != 1 || cases[0].evidenceGroups[0].id != "initiating-error" {
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

	multiRef := strings.Replace(valid,
		`"source_refs": [{"id":"primary","repository":"example/project","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`,
		`"source_refs": [{"id":"server","repository":"example/project","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"id":"primary","repository":"example/project","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, 1)
	multiRef = strings.Replace(multiRef, `"evidence_mode": "artifact_only"`, `"evidence_mode": "artifact_and_source", "expected_source_ranges": [{"source_id":"server","path":"source.go","line_start":10,"line_end":20}], "source_signals": [{"name":"source","pattern":"source","must":true}]`, 1)
	multiRefPath := filepath.Join(t.TempDir(), "multi-ref-manifest.json")
	if err := os.WriteFile(multiRefPath, []byte(multiRef), 0o600); err != nil {
		t.Fatal(err)
	}
	multiRefCases, err := loadBenchmarkManifest(multiRefPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(multiRefCases) != 1 || len(multiRefCases[0].sourceRefs) != 2 || multiRefCases[0].sourceRefs[0].ID != "primary" || multiRefCases[0].sourceRefs[1].ID != "server" || multiRefCases[0].sourceRanges[0].Revision != strings.Repeat("b", 40) {
		t.Fatalf("multi-ref cases = %+v", multiRefCases)
	}

	for name, mutate := range map[string]func(string) string{
		"unknown field": func(value string) string {
			return strings.Replace(value, `"version": 5`, `"version": 5, "extra": true`, 1)
		},
		"bad stable id": func(value string) string { return strings.Replace(value, "0123456789abcdef0123", "model-name", 1) },
		"unknown primary source": func(value string) string {
			return strings.Replace(value, `"primary_source_id": "primary"`, `"primary_source_id": "missing"`, 1)
		},
		"duplicate source id": func(value string) string {
			return strings.Replace(value, `"source_refs": [{"id":"primary","repository":"example/project","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, `"source_refs": [{"id":"primary","repository":"example/project","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"id":"primary","repository":"example/project","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`, 1)
		},
		"unknown range source": func(value string) string {
			return strings.Replace(value, `"evidence_mode": "artifact_only"`, `"evidence_mode": "artifact_and_source", "expected_source_ranges": [{"source_id":"missing","path":"source.go","line_start":1,"line_end":1}], "source_signals": [{"name":"source","pattern":"source","must":true}]`, 1)
		},
		"bad evidence mode": func(value string) string {
			return strings.Replace(value, `"evidence_mode": "artifact_only"`, `"evidence_mode": "all"`, 1)
		},
		"artifact only source expectations": func(value string) string {
			return strings.Replace(value, `"evidence_mode": "artifact_only"`, `"evidence_mode": "artifact_only", "expected_source_ranges": [{"source_id":"primary","path":"source.go","line_start":1,"line_end":1}], "source_signals": [{"name":"source","pattern":"source","must":true}]`, 1)
		},
		"source required without expectations": func(value string) string {
			return strings.Replace(value, `"evidence_mode": "artifact_only"`, `"evidence_mode": "artifact_and_source"`, 1)
		},
		"bad regexp": func(value string) string { return strings.Replace(value, "(?i)root cause", "[", 1) },
		"bad evidence regexp": func(value string) string {
			return strings.Replace(value, `"paths":["build-log\\.txt$"]`, `"paths":["["]`, 1)
		},
		"duplicate evidence id": func(value string) string {
			return strings.Replace(value, `"evidence_groups": [{"id":"initiating-error","paths":["build-log\\.txt$"],"content":["root cause"]}]`, `"evidence_groups": [{"id":"initiating-error","paths":["build-log\\.txt$"]},{"id":"initiating-error","paths":["other"]}]`, 1)
		},
		"bad causal regexp": func(value string) string {
			return strings.Replace(value, `"content":["root cause"]`, `"content":["root cause"],"causal":["["]`, 1)
		},
		"oracle context without hash": func(value string) string {
			return strings.Replace(value, `"content":["root cause"]`, `"content":["root cause"],"oracle_context_lines":1`, 1)
		},
		"oracle hash without context": func(value string) string {
			return strings.Replace(value, `"signals": [{"name":"cause","pattern":"(?i)root cause","must":true}]`, `"signals": [{"name":"cause","pattern":"(?i)root cause","must":true}],"oracle_evidence_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, 1)
		},
		"oracle context too large": func(value string) string {
			return strings.Replace(value, `"content":["root cause"]`, `"content":["root cause"],"oracle_context_lines":6`, 1)
		},
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
		name: "case-one", stableID: "0123456789abcdef0123", evidenceMode: benchmarkEvidenceModeArtifactAndSource, jobName: "job", buildID: "123", testName: "test", testSource: models.TestCaseSourceBuild,
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": strings.Repeat("a", 40)},
		sourceRefs: []benchmarkSourceRef{{ID: "primary", Repository: "example/project", Revision: strings.Repeat("a", 40)}}, primarySourceID: "primary",
		sourceRepo:    [2]string{"example", "project"},
		signals:       []benchSignal{{name: "cause", re: regexp.MustCompile(`root cause`), must: true}},
		sourceRanges:  []benchmarkSourceRange{{Repository: "example/project", Revision: strings.Repeat("a", 40), Path: "file.go", LineStart: 1, LineEnd: 2}},
		sourceSignals: []benchSignal{{name: "source cause", re: regexp.MustCompile(`root cause`), must: true}},
	}
	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "summary"},
		AIAnalysis: &models.AIAnalysis{
			Model: "PRIVATE_MODEL", RootCause: "root cause", SuggestedFix: "fix", Severity: "High",
			RelevantFiles: []string{"file.go"}, FileLinks: map[string]string{"file.go": "https://example.invalid/file.go"}, ToolCalls: 3, GCSBytes: 42,
			EvidencePlanCovered: true, GCSFloorRetryExhausted: true, CritiquePassed: true, BudgetExhausted: true,
		},
	}
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Events: []ai.TraceEvent{
		{Kind: "model_request", Outcome: "success", Attempts: 2, InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2},
		{Kind: "tool_call", Outcome: "success"},
		{Kind: "finalize", Outcome: "empty", ErrorCode: "unexpected_tool_call"},
		{Kind: "finalize_recovery", Outcome: "retained_draft"},
		{Kind: "critique", Outcome: "objected", CritiquePunts: 1},
		{Kind: "semantic_judge", Status: "draft", Outcome: "objected", SemanticFindings: []string{"specific_error_ignored"}},
		{Kind: "semantic_judge", Status: "revision", Outcome: "passed"},
		{Kind: "semantic_judge", Status: "revision", Outcome: "revised"},
		{Kind: "draft_selection", Outcome: "accepted", Status: "best", DraftDecision: &ai.DraftDecisionTrace{
			Target: "best", CurrentAttempt: 1, CandidateAttempt: 2,
			CurrentPublishedSoftRules: []string{"remediation.punt"}, CandidatePublishedSoftRules: []string{"evidence.available_unread"},
			CurrentEvidenceRevision: 3, CandidateEvidenceRevision: 7, RootCauseMateriallyChanged: true,
			CurrentSupportedFacts: 1, CandidateSupportedFacts: 2, SupportedFactsRetained: 1, SupportedFactsAdded: 1,
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
		benchmarkToolUsage{names: []string{"read_artifact", "read_repo_file"}, counts: []string{"read_artifact=1", "read_repo_file=1"}, sourceObservations: []ai.SourceEvidenceObservation{{SourceID: "primary", Tool: "read_repo_file", Path: "file.go", LineStart: 1, LineEnd: 2}}},
		benchmarkTraceSummary{floorNudges: 1, floorNudgeReasons: []string{"gcs_bytes"}, semanticJudgeOutcomes: []string{"draft:objected", "revision:passed", "revision:revised"}, semanticFindingClasses: []string{"specific_error_ignored"}}, 18, "generation", ai.CritiqueCachePolicyHard, cacheVerification,
		benchmarkRunIdentity{Arm: "variant", EngineCommit: strings.Repeat("b", 40), FixtureSHA256: strings.Repeat("c", 64), BaselineConsumerCommit: strings.Repeat("d", 40), BaselinePromptSHA256: strings.Repeat("3", 64), ProjectSHA256: strings.Repeat("e", 64), EffectivePromptSHA256: strings.Repeat("f", 64), SkillSetHash: strings.Repeat("1", 64), EffectiveInputSHA256: strings.Repeat("2", 64), EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceStageSHA256: benchmarkEvidenceStageSHA256(bc.evidenceGroups), APIMode: ai.APIChatCompletions, ProviderPath: "github-copilot/claude-sonnet-4.6", TransportID: "copilot-structural-proxy-v1"}, benchmarkEvidenceCoverage{selected: []string{"initiating-error"}, hit: []string{"initiating-error"}, missed: []string{"secondary-evidence"}, sources: map[string][]string{"initiating-error": {"model_tool"}}}, benchmarkEvidenceStageReport{Condition: benchmarkEvidenceConditionFixture, ModelRequestMade: true, TrialStatus: "contract_violation"})
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
	if !slices.Equal(result.EvidenceGroupsSelected, []string{"initiating-error"}) || !slices.Equal(result.EvidenceGroupsHit, []string{"initiating-error"}) || !slices.Equal(result.EvidenceGroupsMissed, []string{"secondary-evidence"}) || !slices.Equal(result.EvidenceGroupSources["initiating-error"], []string{"model_tool"}) || result.EvidenceTelemetryVersion != 2 || result.EvidenceCondition != benchmarkEvidenceConditionFixture || result.EvidenceStageSHA256 != benchmarkEvidenceStageSHA256(bc.evidenceGroups) || !result.ModelRequestMade || result.TrialStatus != "contract_violation" || result.EvidenceStages == nil || result.EvidenceRevisions == nil || result.ModelLabel != "model-a" || result.Arm != "variant" || result.EngineCommit != strings.Repeat("b", 40) || result.FixtureSHA256 != strings.Repeat("c", 64) || result.BaselineConsumerCommit != strings.Repeat("d", 40) || result.BaselinePromptSHA256 != strings.Repeat("3", 64) || result.ProjectSHA256 != strings.Repeat("e", 64) || result.EffectivePromptSHA256 != strings.Repeat("f", 64) || result.SkillSetHash != strings.Repeat("1", 64) || result.EffectiveInputSHA256 != strings.Repeat("2", 64) || result.APIMode != ai.APIChatCompletions || result.ProviderPath != "github-copilot/claude-sonnet-4.6" || result.TransportID != "copilot-structural-proxy-v1" || result.Repetition != 2 || result.Outcome != string(benchmarkOutcomeUsable) || result.IsTransient == nil || *result.IsTransient || result.SignalHits != 2 || result.SignalTotal != 2 || result.DiagnosisSignalHits != 2 || result.DiagnosisSignalTotal != 2 || result.TransientCorrect != nil || result.ForbiddenChecksPassed != 0 || result.ForbiddenChecksTotal != 0 || result.SourceRevision != strings.Repeat("a", 40) || result.SourceUnavailable || result.TestSource != models.TestCaseSourceBuild ||
		result.Trace.Finalize["empty:unexpected_tool_call"] != 1 || result.Trace.Critique["punts"] != 1 || result.GCSBytes != 42 ||
		!result.EvidencePlanCovered || !result.GCSFloorRetryExhausted || result.CritiquePassed == nil || !*result.CritiquePassed || !result.BudgetExhausted ||
		result.FloorNudges != 1 || !slices.Equal(result.FloorNudgeReasons, []string{"gcs_bytes"}) ||
		!slices.Equal(result.ToolNames, []string{"read_artifact", "read_repo_file"}) || !slices.Equal(result.ToolCounts, []string{"read_artifact=1", "read_repo_file=1"}) ||
		result.SourceEvidenceToolCalls != 1 || result.SourceReadCoverageHits != 1 || result.SourceReadCoverageTotal != 1 || len(result.SourceReadRanges) != 1 || result.SourceReadRanges[0].Path != "file.go" || len(result.SourceCitations) != 0 || result.SourceSignalHits != 1 || result.SourceSignalTotal != 1 ||
		!result.CacheVerification.LookupAccepted || !result.CacheVerification.LookupHit || result.CacheGeneration != "generation" ||
		result.ProviderRequestCap != 18 || result.Trace.ProviderAttempts != 2 || result.TraceTruncated || result.CritiqueCachePolicy != string(ai.CritiqueCachePolicyHard) ||
		result.HumanScoreRubricVersion != benchmarkHumanScoreRubricVersion || result.HumanScoreMax != 10 || len(result.Drafts) != 1 ||
		len(result.DraftDecisions) != 1 || result.DraftDecisions[0].ReplacementReason != "candidate_published_dominates" ||
		!result.SemanticRevisionAttempted || !result.SemanticRevisionSelected || result.SemanticRevisionRejected ||
		!slices.Equal(result.SemanticJudgeOutcomes, []string{"draft:objected", "revision:passed", "revision:revised"}) ||
		!slices.Equal(result.SemanticFindingClasses, []string{"specific_error_ignored"}) || result.SupportedFactsRetained != 1 || result.SupportedFactsAdded != 1 || result.SupportedFactsDropped != 0 ||
		result.EvidenceMode != benchmarkEvidenceModeArtifactAndSource || !slices.Equal(result.HumanScoreDimensions, benchmarkHumanScoreDimensions) ||
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
		name: "case-unavailable", stableID: "abcdef0123456789abcd", evidenceMode: benchmarkEvidenceModeArtifactOnly, jobName: "job", buildID: "123", testName: "test",
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": strings.Repeat("a", 40)},
		sourceRefs: []benchmarkSourceRef{{ID: "primary", Repository: "example/project", Revision: strings.Repeat("a", 40)}}, primarySourceID: "primary",
		sourceRepo: [2]string{"example", "project"}, allowUnavailable: true,
	}
	tc := &models.TestCase{AISummary: &models.AISummary{Summary: "AI analysis unavailable: no validated artifact citation supports the analysis"}}
	writeBenchmarkJSONL(t, path, bc, 1, tc, benchmarkOutcomeGroundedPolicyUnavailable, time.Second, ai.AnalysisTraceFile{}, nil, 0, benchmarkToolUsage{}, benchmarkTraceSummary{}, 1, "", ai.CritiqueCachePolicyHard, benchmarkCacheVerification{}, benchmarkRunIdentity{
		Arm: "baseline", EngineCommit: strings.Repeat("b", 40), EffectivePromptSHA256: strings.Repeat("f", 64), SkillSetHash: strings.Repeat("1", 64), EffectiveInputSHA256: strings.Repeat("2", 64), EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceStageSHA256: benchmarkEvidenceStageSHA256(bc.evidenceGroups), APIMode: ai.APIChatCompletions,
	}, benchmarkEvidenceCoverage{}, benchmarkEvidenceStageReport{Condition: benchmarkEvidenceConditionFixture, TrialStatus: "invalid_result"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result benchmarkJSONLResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != string(benchmarkOutcomeGroundedPolicyUnavailable) || result.TrialStatus != "invalid_result" || result.EvidenceCondition != benchmarkEvidenceConditionFixture || result.Usable || result.IsTransient == nil || *result.IsTransient || result.Summary != tc.AISummary.Summary {
		t.Fatalf("result = %+v", result)
	}
}

func TestWriteBenchmarkJSONLRecordsFailedTrials(t *testing.T) {
	t.Setenv("BENCH_MODEL_LABEL", "model-a")
	group := benchmarkEvidenceGroup{id: "stage"}
	bc := benchCase{
		name: "case-failed", stableID: "abcdef0123456789abcd", jobName: "job", buildID: "123", testName: "test",
		commit: strings.Repeat("a", 40), repoVersion: strings.Repeat("a", 40), repoRefs: map[string]string{"example/project": strings.Repeat("a", 40)},
		sourceRefs: []benchmarkSourceRef{{ID: "primary", Repository: "example/project", Revision: strings.Repeat("a", 40)}}, primarySourceID: "primary",
		sourceRepo: [2]string{"example", "project"}, evidenceGroups: []benchmarkEvidenceGroup{group},
		fixtureSHA256: strings.Repeat("f", 64), consumerCommit: strings.Repeat("1", 40), promptSHA256: strings.Repeat("2", 64), projectSHA256: strings.Repeat("3", 64),
		signals: []benchSignal{{name: "cause", re: regexp.MustCompile(`cause`)}},
	}
	identity := benchmarkRunIdentity{
		Arm: "baseline", EngineCommit: strings.Repeat("b", 40), FixtureSHA256: bc.fixtureSHA256,
		BaselineConsumerCommit: bc.consumerCommit, BaselinePromptSHA256: bc.promptSHA256, ProjectSHA256: bc.projectSHA256,
		EffectivePromptSHA256: strings.Repeat("c", 64), SkillSetHash: strings.Repeat("d", 64), EffectiveInputSHA256: strings.Repeat("e", 64),
		EvidenceCondition: benchmarkEvidenceConditionFixture, EvidenceStageSHA256: benchmarkEvidenceStageSHA256(bc.evidenceGroups),
		APIMode: ai.APIChatCompletions, ProviderPath: "local/model", TransportID: "chat-v1",
	}
	valid := &models.TestCase{AISummary: &models.AISummary{Summary: "summary"}, AIAnalysis: &models.AIAnalysis{RootCause: "cause"}}
	invalid := &models.TestCase{AISummary: &models.AISummary{Summary: "unavailable"}}
	for _, tc := range []struct {
		name         string
		status       string
		outcome      benchmarkOutcome
		result       *models.TestCase
		modelRequest bool
		wantOutcome  benchmarkOutcome
		wantUsable   bool
	}{
		{name: "no result", status: "no_result", outcome: benchmarkOutcomeUsable, wantOutcome: benchmarkOutcomeUnknown},
		{name: "invalid result", status: "invalid_result", outcome: benchmarkOutcomeGroundedPolicyUnavailable, result: invalid, modelRequest: true, wantOutcome: benchmarkOutcomeGroundedPolicyUnavailable},
		{name: "timeout", status: "timeout", outcome: benchmarkOutcomeUnknown, modelRequest: true, wantOutcome: benchmarkOutcomeUnknown},
		{name: "runtime failure", status: "runtime_failure", outcome: benchmarkOutcomeUnknown, modelRequest: true, wantOutcome: benchmarkOutcomeUnknown},
		{name: "contract violation", status: "contract_violation", outcome: benchmarkOutcomeUsable, result: valid, modelRequest: true, wantOutcome: benchmarkOutcomeUsable, wantUsable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "results.jsonl")
			stage := benchmarkEvidenceStage{GroupID: group.id}
			report := benchmarkEvidenceStageReport{
				Condition: benchmarkEvidenceConditionFixture, ModelRequestMade: tc.modelRequest,
				Stages: []benchmarkEvidenceStage{stage}, TrialStatus: tc.status,
			}
			writeBenchmarkJSONL(t, path, bc, 1, tc.result, tc.outcome, time.Second, ai.AnalysisTraceFile{}, nil, 0,
				benchmarkToolUsage{}, benchmarkTraceSummary{}, 1, "", ai.CritiqueCachePolicyHard,
				benchmarkCacheVerification{}, identity, benchmarkEvidenceCoverage{}, report)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var result benchmarkJSONLResult
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.TrialStatus != tc.status || result.Outcome != string(tc.wantOutcome) || result.Usable != tc.wantUsable || result.ModelRequestMade != tc.modelRequest {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestRecordBenchmarkSemanticRevisionCountsUnparseableAndDeniedAsRejected(t *testing.T) {
	for _, outcome := range []string{"revision_unparseable", "revision_denied"} {
		t.Run(outcome, func(t *testing.T) {
			result := benchmarkJSONLResult{}
			recordBenchmarkSemanticRevision(&result, ai.TraceEvent{Kind: "semantic_judge", Status: "revision", Outcome: outcome})
			if !result.SemanticRevisionAttempted || !result.SemanticRevisionRejected || result.SemanticRevisionSelected {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestBenchmarkSourceExpectationSHA256(t *testing.T) {
	revision := strings.Repeat("a", 40)
	base := benchCase{
		primarySourceID: "primary",
		sourceRefs:      []benchmarkSourceRef{{ID: "primary", Repository: "owner/repo", Revision: revision}},
		sourceRanges:    []benchmarkSourceRange{{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 1, LineEnd: 2}},
		sourceSignals:   []benchSignal{{name: "source", re: regexp.MustCompile(`source claim`), must: true}},
	}
	want := benchmarkSourceExpectationSHA256(base)
	for _, changed := range []benchCase{
		{primarySourceID: base.primarySourceID, sourceRefs: base.sourceRefs, sourceRanges: []benchmarkSourceRange{{Repository: "owner/repo", Revision: revision, Path: "pkg/other.go", LineStart: 1, LineEnd: 2}}, sourceSignals: base.sourceSignals},
		{primarySourceID: base.primarySourceID, sourceRefs: base.sourceRefs, sourceRanges: base.sourceRanges, sourceSignals: []benchSignal{{name: "other", re: regexp.MustCompile(`source claim`), must: true}}},
		{primarySourceID: base.primarySourceID, sourceRefs: base.sourceRefs, sourceRanges: base.sourceRanges, sourceSignals: []benchSignal{{name: "source", re: regexp.MustCompile(`other claim`), must: true}}},
		{primarySourceID: "other", sourceRefs: base.sourceRefs, sourceRanges: base.sourceRanges, sourceSignals: base.sourceSignals},
		{primarySourceID: base.primarySourceID, sourceRefs: []benchmarkSourceRef{{ID: "primary", Repository: "owner/repo", Revision: strings.Repeat("b", 40)}}, sourceRanges: base.sourceRanges, sourceSignals: base.sourceSignals},
	} {
		if got := benchmarkSourceExpectationSHA256(changed); got == want {
			t.Fatalf("source expectation hash did not change: %s", got)
		}
	}
	if !benchmarkSHA256RE.MatchString(want) {
		t.Fatalf("source expectation hash = %q", want)
	}
	reordered := base
	reordered.sourceRefs = []benchmarkSourceRef{
		{ID: "secondary", Repository: "other/repo", Revision: strings.Repeat("c", 40)},
		base.sourceRefs[0],
	}
	ordered := reordered
	ordered.sourceRefs = []benchmarkSourceRef{base.sourceRefs[0], reordered.sourceRefs[0]}
	if benchmarkSourceExpectationSHA256(reordered) != benchmarkSourceExpectationSHA256(ordered) {
		t.Fatal("source expectation hash depends on source ref order")
	}
}

func TestKubectlSkewEvaluationManifest(t *testing.T) {
	cases, err := loadBenchmarkManifest("testdata/benchmarks/kubectl-skew-eval.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases=%d", len(cases))
	}
	bc := cases[0]
	if bc.name != "kubectl-skew-stable2-latest-kubeproxy-version" || len(bc.sourceRefs) != 2 || len(bc.sourceRanges) != 3 || bc.primarySourceID != "latest-client" || bc.evidenceMode != benchmarkEvidenceModeArtifactAndSource {
		t.Fatalf("case=%+v", bc)
	}
	if bc.sourceRefs[0].Repository != "kubernetes/kubernetes" || bc.sourceRefs[1].Repository != "kubernetes/kubernetes" || bc.sourceRefs[0].Revision == bc.sourceRefs[1].Revision {
		t.Fatalf("source refs=%+v", bc.sourceRefs)
	}
	reference := &models.TestCase{AISummary: &models.AISummary{Summary: bc.referenceDiagnosis, IsTransient: false}, AIAnalysis: &models.AIAnalysis{RootCause: bc.referenceDiagnosis}}
	if assessment := assessBenchmarkCase(bc, reference); len(assessment.missingMust) > 0 {
		t.Fatalf("reference rejected: %v", assessment.missingMust)
	}
	opposite := &models.TestCase{AISummary: &models.AISummary{Summary: bc.oppositeDiagnosis, IsTransient: false}, AIAnalysis: &models.AIAnalysis{RootCause: bc.oppositeDiagnosis}}
	if assessment := assessBenchmarkCase(bc, opposite); assessment.forbiddenPassed == assessment.forbiddenTotal {
		t.Fatalf("opposite diagnosis passed: %+v", assessment)
	}
	data, err := os.ReadFile("testdata/benchmarks/agent-sandbox-causal-references.json")
	if err != nil {
		t.Fatal(err)
	}
	var references struct {
		Cases map[string]struct {
			ReferenceDiagnosis string `json:"reference_diagnosis"`
			RequiredChain      []struct {
				ID string `json:"id"`
			} `json:"required_chain"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &references); err != nil {
		t.Fatal(err)
	}
	causal, ok := references.Cases[bc.name]
	if !ok || strings.TrimSpace(causal.ReferenceDiagnosis) == "" {
		t.Fatal("kubectl skew causal reference is missing")
	}
	wantChain := []string{"mixed-version-job", "stable-test-requires-field", "latest-client-omits-field", "version-skew-assertion", "health-not-primary", "persistent-not-flake"}
	gotChain := make([]string, 0, len(causal.RequiredChain))
	for _, link := range causal.RequiredChain {
		gotChain = append(gotChain, link.ID)
	}
	if !slices.Equal(gotChain, wantChain) {
		t.Fatalf("causal chain=%v", gotChain)
	}
}

func TestCAPZAgentSandboxEvaluationManifest(t *testing.T) {
	cases, err := loadBenchmarkManifest("testdata/benchmarks/capz-agent-sandbox-eval.json")
	if err != nil {
		t.Fatal(err)
	}
	wantCommits := map[string]string{
		"ccm-dualstack-control-plane-routetable":      "1cefef491698494b19d2fac9895e37c94cab5d4e",
		"flatcar-worker-dns-providerid":               "d8eb84399896fc136eb7aa703bfdeb3686d1bad1",
		"apiversion-upgrade-clusterctl-aso-ratelimit": "7d3a0e1020ad88b7bd095fdf2c3802984af08002",
	}
	wantSources := map[string]string{
		"ccm-dualstack-control-plane-routetable":      "c4a02ed36c22af00a2b228e6a9f02d1f2b2a42e0",
		"flatcar-worker-dns-providerid":               "d8eb84399896fc136eb7aa703bfdeb3686d1bad1",
		"apiversion-upgrade-clusterctl-aso-ratelimit": "7d3a0e1020ad88b7bd095fdf2c3802984af08002",
	}
	wantEvidenceModes := map[string]string{
		"ccm-dualstack-control-plane-routetable":      benchmarkEvidenceModeArtifactAndSource,
		"flatcar-worker-dns-providerid":               benchmarkEvidenceModeArtifactOnly,
		"apiversion-upgrade-clusterctl-aso-ratelimit": benchmarkEvidenceModeArtifactOnly,
	}
	if len(cases) != len(wantCommits) {
		t.Fatalf("cases = %d", len(cases))
	}
	for _, bc := range cases {
		if bc.consumerCommit != "cabcef8e03b510467dac52682fa7e9b0f3e6692f" || bc.projectSHA256 != "1fc01fa1d2590c26b66a98faadb9daff567a4d325b6af3a14c765ed9f72d2e24" || bc.promptSHA256 != "817cea07bd6b4621ac99b362af20f899caeeb336e86c355fd6870792c6afa9b5" {
			t.Fatalf("case %s consumer identity is invalid", bc.name)
		}
		source, ok := benchmarkPrimarySourceRef(bc)
		if bc.commit != wantCommits[bc.name] || bc.repoVersion != bc.commit || !ok || source.Revision != wantSources[bc.name] || bc.evidenceMode != wantEvidenceModes[bc.name] || bc.referenceDiagnosis == "" || bc.expectedTransient == nil {
			t.Fatalf("case %s identity = %+v source=%+v", bc.name, bc, source)
		}
		if bc.evidenceMode == benchmarkEvidenceModeArtifactAndSource {
			if len(bc.sourceRanges) != 2 || bc.sourceRanges[0].Path != "api/v1beta1/azurecluster_default.go" || bc.sourceRanges[0].LineStart != 197 || bc.sourceRanges[1].Path != "azure/scope/cluster.go" || bc.sourceRanges[1].LineStart != 378 || len(bc.sourceSignals) != 2 {
				t.Fatalf("case %s source expectations = ranges %v signals %+v", bc.name, bc.sourceRanges, bc.sourceSignals)
			}
		} else if len(bc.sourceRanges) != 0 || len(bc.sourceSignals) != 0 {
			t.Fatalf("case %s artifact-only source expectations = ranges %v signals %+v", bc.name, bc.sourceRanges, bc.sourceSignals)
		}
	}
}
