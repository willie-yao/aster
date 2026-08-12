package remediationinvestigation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const currentRevision = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type revisionSource struct {
	files map[string]map[string]string
}

func (s revisionSource) ListFiles(_ context.Context, repository sourceinvestigation.Repository) ([]string, error) {
	files := s.files[sourceKey(repository)]
	if files == nil {
		return nil, errors.New("revision unavailable")
	}
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s revisionSource) ReadFile(_ context.Context, repository sourceinvestigation.Repository, file string) (string, error) {
	content, ok := s.files[sourceKey(repository)][file]
	if !ok {
		return "", errors.New("file unavailable")
	}
	return content, nil
}

func sourceKey(repository sourceinvestigation.Repository) string {
	return strings.ToLower(repository.Owner + "/" + repository.Name + "@" + repository.Revision)
}

func verificationFixture(t *testing.T, current, failure string) (*Verifier, FrozenInput, CacheEntry, fakeBrowser) {
	t.Helper()
	input := testFrozenInput()
	input.InvestigationSource.Revision = currentRevision
	for index := range input.Builds {
		input.Builds[index].Source = &sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision}
		input.Analyses[index].SourceRepository = input.Builds[index].Source
		input.Analyses[index].RelevantFiles = []string{"controllers/reconcile.go"}
		input.Analyses[index].RootCause = "reconcile is missing the required applyFix call"
	}
	input.RelevantFiles = []string{"controllers/reconcile.go"}
	browser := fakeBrowser{files: map[string]string{
		"builds/1/log.txt": "reconcile missing applyFix transition\n",
		"builds/2/log.txt": "reconcile missing applyFix transition\n",
	}}
	catalog := evidenceCatalogForFixture(input, "controllers/reconcile.go", current, browser.files)
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports,
		Reason: "the reconcile path omits applyFix",
		Candidate: &RequiredCallCandidate{
			Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
		},
		EvidenceIDs: evidenceIDs(catalog),
	}
	reader := revisionSource{files: map[string]map[string]string{
		sourceKey(input.InvestigationSource): {"controllers/reconcile.go": current},
		sourceKey(*input.Builds[0].Source):   {"controllers/reconcile.go": failure},
	}}
	verifier, err := NewVerifier(reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := CacheKey(input)
	if err != nil {
		t.Fatal(err)
	}
	provenance := NewProvenance(input, "model", "chat_completions", "", EvidenceStats{
		ToolCalls: 3, SourceReads: 1, ArtifactReads: 2,
	}, Metrics{ModelRequests: 2}, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	entry := CacheEntry{
		Key: key, Result: result, ResultDigest: ResultDigest(result),
		EvidenceCatalog: catalog, EvidenceCatalogDigest: EvidenceCatalogDigest(catalog), Provenance: provenance,
	}
	return verifier, input, entry, browser
}

func evidenceCatalogForFixture(input FrozenInput, sourcePath, sourceContent string, artifactFiles map[string]string) EvidenceCatalog {
	catalog := EvidenceCatalog{Version: EvidenceCatalogVersion}
	if sourcePath != "" {
		record := EvidenceRecord{
			Kind: EvidenceSource,
			Source: &SourceEvidenceIdentity{
				Repository: input.InvestigationSource, Path: sourcePath, ContentDigest: HashText(sourceContent),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	for _, analysis := range input.Analyses {
		record := EvidenceRecord{
			Kind: EvidenceAnalysis,
			Analysis: &AnalysisEvidenceIdentity{
				BuildID: analysis.BuildID, GeneratedAt: analysis.GeneratedAt, RootCauseDigest: HashText(analysis.RootCause),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	paths := make([]string, 0, len(artifactFiles))
	for file := range artifactFiles {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	for _, file := range paths {
		buildID, ok := artifactBuildID(file, input.Group.Builds)
		if !ok {
			continue
		}
		record := EvidenceRecord{
			Kind: EvidenceArtifact,
			Artifact: &ArtifactEvidenceIdentity{
				BuildID: buildID, Path: file, ContentDigest: HashText(artifactFiles[file]),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	return canonicalEvidenceCatalog(catalog)
}

func evidenceIDs(catalog EvidenceCatalog) []string {
	ids := make([]string, 0, len(catalog.Records))
	for _, record := range catalog.Records {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestVerifierAcceptsOnlyMissingCurrentAndFailureTarget(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationActionable || verified.Proposal == nil || verified.CurrentSource == nil || len(verified.FailureSources) != 1 {
		t.Fatalf("verified=%+v", verified)
	}
	if verified.Proposal.Repository != input.InvestigationSource || len(verified.Proposal.AllowedChangedPaths) != 1 || len(verified.Proposal.AllowedValidationCommands) != 1 || len(verified.Proposal.VerificationRequirements) == 0 {
		t.Fatalf("engine-derived proposal=%+v", verified.Proposal)
	}
}

func TestVerifierConvertsPresentCurrentTargetToAlreadyFixed(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	present := "package controllers\nfunc reconcile() error { applyFix(); return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, present, missing)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationAlreadyFixed || verified.Proposal != nil || verified.CurrentSource == nil {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierAcceptsAndReconstructsSelectedSourceGrepEvidence(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	for index := range entry.EvidenceCatalog.Records {
		record := entry.EvidenceCatalog.Records[index]
		if record.Kind != EvidenceSource {
			continue
		}
		grepRecord := EvidenceRecord{
			Kind: EvidenceSourceGrep,
			SourceGrep: &SourceGrepEvidenceIdentity{
				Repository: input.InvestigationSource, Path: "controllers/reconcile.go", LineStart: 2, LineEnd: 3,
				ContentDigest: HashText(missing), Match: "func reconcile() error { return nil }\nfunc applyFix() {}",
			},
		}
		grepRecord.ID = evidenceRecordID(grepRecord)
		entry.EvidenceCatalog.Records[index] = grepRecord
		for evidenceIndex, id := range entry.Result.EvidenceIDs {
			if id == record.ID {
				entry.Result.EvidenceIDs[evidenceIndex] = grepRecord.ID
			}
		}
		break
	}
	entry.ResultDigest = ResultDigest(entry.Result)
	entry.EvidenceCatalogDigest = EvidenceCatalogDigest(entry.EvidenceCatalog)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationActionable || verified.Proposal == nil {
		t.Fatalf("verified=%+v", verified)
	}

	for index := range entry.EvidenceCatalog.Records {
		record := &entry.EvidenceCatalog.Records[index]
		if record.Kind != EvidenceSourceGrep {
			continue
		}
		oldID := record.ID
		record.SourceGrep.Match = "fabricated match"
		record.ID = evidenceRecordID(*record)
		for evidenceIndex, id := range entry.Result.EvidenceIDs {
			if id == oldID {
				entry.Result.EvidenceIDs[evidenceIndex] = record.ID
			}
		}
	}
	entry.ResultDigest = ResultDigest(entry.Result)
	entry.EvidenceCatalogDigest = EvidenceCatalogDigest(entry.EvidenceCatalog)
	verified, err = verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence || verified.Proposal != nil {
		t.Fatalf("tampered grep verified=%+v", verified)
	}
}

func TestVerifierRejectsTargetAlreadyPresentInFailureRevision(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	present := "package controllers\nfunc reconcile() error { applyFix(); return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, present)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence || verified.Proposal != nil {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierRejectsMutatedResultCatalogAndPolicy(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)

	mutatedResult := entry
	mutatedResult.Result.Reason = "mutated"
	if _, err := verifier.Verify(t.Context(), input, mutatedResult, browser); err == nil {
		t.Fatal("mutated result digest accepted")
	}

	mutatedCatalog := entry
	mutatedCatalog.EvidenceCatalog.Records[0].ID = "source:" + strings.Repeat("0", 64)
	if _, err := verifier.Verify(t.Context(), input, mutatedCatalog, browser); err == nil {
		t.Fatal("mutated catalog digest accepted")
	}

	mutatedPolicy := input
	mutatedPolicy.DestinationPolicy.Repositories[0].AllowedPaths = []string{"pkg/"}
	if _, err := verifier.Verify(t.Context(), mutatedPolicy, entry, browser); err == nil {
		t.Fatal("mutated frozen policy accepted")
	}
}

func TestVerifierRejectsCandidateOutsidePolicyAndUnknownEvidence(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	entry.Result.Candidate = &RequiredCallCandidate{
		Kind: CandidateRequiredCall, Path: "pkg/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
	}
	entry.Result.ResultEvidenceForTestReplaceSource(t, input, &entry.EvidenceCatalog, "pkg/reconcile.go", missing)
	entry.ResultDigest = ResultDigest(entry.Result)
	entry.EvidenceCatalogDigest = EvidenceCatalogDigest(entry.EvidenceCatalog)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("outside-policy verified=%+v", verified)
	}

	_, _, unknownEntry, unknownBrowser := verificationFixture(t, missing, missing)
	unknownEntry.Result.EvidenceIDs[0] = "source:" + strings.Repeat("f", 64)
	unknownEntry.ResultDigest = ResultDigest(unknownEntry.Result)
	verified, err = verifier.Verify(t.Context(), input, unknownEntry, unknownBrowser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("unknown evidence verified=%+v", verified)
	}
}

func (result *Result) ResultEvidenceForTestReplaceSource(t *testing.T, input FrozenInput, catalog *EvidenceCatalog, path, content string) {
	t.Helper()
	for index := range catalog.Records {
		if catalog.Records[index].Kind != EvidenceSource {
			continue
		}
		record := EvidenceRecord{
			Kind: EvidenceSource,
			Source: &SourceEvidenceIdentity{
				Repository: input.InvestigationSource, Path: path, ContentDigest: HashText(content),
			},
		}
		record.ID = evidenceRecordID(record)
		oldID := catalog.Records[index].ID
		catalog.Records[index] = record
		for evidenceIndex, id := range result.EvidenceIDs {
			if id == oldID {
				result.EvidenceIDs[evidenceIndex] = record.ID
			}
		}
		return
	}
	t.Fatal("source record not found")
}

func TestVerifierRejectsUnsafeConversionAndUnsupportedTargetKinds(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	entry.Result.Reason = "Delete conversion webhook configurations to disable conversion."
	entry.ResultDigest = ResultDigest(entry.Result)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("unsafe conversion verified=%+v", verified)
	}

	entry.Result.Reason = "set one exact configuration field"
	entry.Result.Candidate = &ConfigurationFieldCandidate{
		Kind: CandidateConfigurationField, Path: "controllers/reconcile.go", FieldPath: []string{"feature", "enabled"}, Value: "true",
	}
	entry.ResultDigest = ResultDigest(entry.Result)
	verified, err = verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("unsupported configuration field verified=%+v", verified)
	}

	for _, value := range []string{"pkg/mod/example.net/dependency@v1.2.3/fix.go", ".cache/modules/fix.go", "C:\\pkg\\mod\\dependency\\fix.go"} {
		if !suspiciousRepositoryPath(value) {
			t.Fatalf("workspace path %q was not rejected", value)
		}
	}
}

func TestVerifierDerivesNonActionableClassificationsWithoutOwnershipClaims(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	for reason, want := range map[NonActionableReason]Classification{
		NonActionableEnvironmentOrInfrastructure:   ClassificationEnvironmentOrInfrastructure,
		NonActionableMitigationOnly:                ClassificationMitigationOnly,
		NonActionableInsufficientEvidence:          ClassificationInsufficientEvidence,
		NonActionableDependencyOwnershipUnverified: ClassificationInsufficientEvidence,
	} {
		t.Run(string(reason), func(t *testing.T) {
			candidate := cloneCacheEntry(entry)
			candidate.Result.Candidate = nil
			candidate.Result.NonActionableReason = &reason
			candidate.Result.Reason = "no verified repository target"
			candidate.ResultDigest = ResultDigest(candidate.Result)
			verified, err := verifier.Verify(t.Context(), input, candidate, browser)
			if err != nil {
				t.Fatal(err)
			}
			if verified.Classification != want || verified.Proposal != nil {
				t.Fatalf("verified=%+v want=%s", verified, want)
			}
		})
	}
}

func TestVerifierProwTargetRequiresExactFrozenJobID(t *testing.T) {
	input := testFrozenInput()
	input.JobName = "periodic-capz"
	input.JobID = "wrong-job-id"
	input.InvestigationSource = sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: currentRevision}
	const configPath = "config/jobs/example/periodics.yaml"
	input.RelevantFiles = []string{configPath}
	input.DestinationPolicy = DestinationPolicy{Project: "test", Repositories: []RepositoryPolicy{{
		Repository: "kubernetes/test-infra", AllowedPaths: []string{"config/jobs/example/"},
		AllowedCommands: []ValidationCommand{{Argv: []string{"go", "test", "./config/..."}, Timeout: "10m"}},
	}}}
	for index := range input.Builds {
		input.Builds[index].Source = &sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: testRevision}
		input.Analyses[index].SourceRepository = input.Builds[index].Source
		input.Analyses[index].RelevantFiles = []string{configPath}
		input.Analyses[index].RootCause = "periodic-capz is missing FEATURE_FLAG"
	}
	config := "periodics:\n- name: periodic-capz\n  spec:\n    containers:\n    - name: test\n"
	artifacts := map[string]string{
		"builds/1/log.txt": "periodic-capz FEATURE_FLAG is missing\n",
		"builds/2/log.txt": "periodic-capz FEATURE_FLAG is missing\n",
	}
	catalog := evidenceCatalogForFixture(input, configPath, config, artifacts)
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports, Reason: "the job omits FEATURE_FLAG",
		Candidate: &ProwEnvironmentEntryCandidate{
			Kind: CandidateProwEnvironmentEntry, ConfigPath: configPath, Job: "periodic-capz", Container: "test", Name: "FEATURE_FLAG", Value: "enabled",
		},
		EvidenceIDs: evidenceIDs(catalog),
	}
	reader := revisionSource{files: map[string]map[string]string{
		sourceKey(input.InvestigationSource): {configPath: config},
		sourceKey(*input.Builds[0].Source):   {configPath: config},
	}}
	verifier, _ := NewVerifier(reader)
	key, _ := CacheKey(input)
	entry := CacheEntry{
		Key: key, Result: result, ResultDigest: ResultDigest(result),
		EvidenceCatalog: catalog, EvidenceCatalogDigest: EvidenceCatalogDigest(catalog),
		Provenance: NewProvenance(input, "model", "chat_completions", "", EvidenceStats{}, Metrics{}, time.Now()),
	}
	browser := fakeBrowser{files: artifacts}
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierProwEvidenceRequiresExactEnvironmentName(t *testing.T) {
	proposal := ActionableProposal{
		TargetKind: TargetSetJobEnvironment,
		Target:     modelsRemediationTargetForProwTest("FABRICATED_FLAG"),
	}
	input := testFrozenInput()
	input.JobName = "periodic-capz"
	input.RelevantFiles = []string{"config/jobs/example/periodics.yaml"}
	artifacts := map[string]string{
		"builds/1/log.txt": "periodic-capz failed during startup\n",
		"builds/2/log.txt": "periodic-capz failed during startup\n",
	}
	catalog := evidenceCatalogForFixture(input, proposal.Target.Path, "periodic-capz\n", artifacts)
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports, Reason: "the job failed",
		Candidate: &ProwEnvironmentEntryCandidate{
			Kind: CandidateProwEnvironmentEntry, ConfigPath: proposal.Target.Path, Job: "periodic-capz", Container: "test", Name: "FABRICATED_FLAG", Value: "enabled",
		},
		EvidenceIDs: evidenceIDs(catalog),
	}
	if err := verifyStructuralRelationship(t.Context(), fakeBrowser{files: artifacts}, input, result, catalog, proposal); err == nil {
		t.Fatal("job-name-only evidence accepted a fabricated environment variable")
	}
}

func modelsRemediationTargetForProwTest(name string) models.RemediationTarget {
	return models.RemediationTarget{
		Intent: models.RemediationIntentSetJobEnvironment, Path: "config/jobs/example/periodics.yaml",
		Job: "periodic-capz", Container: "test", Name: name, Value: "enabled",
	}
}

func TestVerifierRejectsFabricatedSymbolAdditionDespiteRecurringText(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\n"
	verifier, input, _, _ := verificationFixture(t, missing, missing)
	for index := range input.Analyses {
		input.Analyses[index].RootCause = "analysis suggested fabricatedFix without source support"
	}
	artifacts := map[string]string{
		"builds/1/log.txt": "analysis suggested fabricatedFix without source support\n",
		"builds/2/log.txt": "analysis suggested fabricatedFix without source support\n",
	}
	catalog := evidenceCatalogForFixture(input, "controllers/reconcile.go", missing, artifacts)
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports,
		Reason: "the repeated analysis mentions fabricatedFix",
		Candidate: &SymbolAdditionCandidate{
			Kind: CandidateSymbolAddition, Path: "controllers/reconcile.go", Symbol: "fabricatedFix",
		},
		EvidenceIDs: evidenceIDs(catalog),
	}
	key, err := CacheKey(input)
	if err != nil {
		t.Fatal(err)
	}
	entry := CacheEntry{
		Key: key, Result: result, ResultDigest: ResultDigest(result),
		EvidenceCatalog: catalog, EvidenceCatalogDigest: EvidenceCatalogDigest(catalog),
		Provenance: NewProvenance(input, "model", "chat_completions", "", EvidenceStats{}, Metrics{}, time.Now()),
	}
	verified, err := verifier.Verify(t.Context(), input, entry, fakeBrowser{files: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence || verified.Proposal != nil {
		t.Fatalf("fabricated symbol addition became actionable: %+v", verified)
	}
	if deterministicallyVerifiableTargetKind(TargetAddSymbol) {
		t.Fatal("symbol addition lacks a deterministic behavioral-role predicate")
	}
}
