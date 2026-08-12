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
	}
	input.RelevantFiles = []string{"controllers/reconcile.go"}
	proposal := &ActionableProposal{
		TargetKind: TargetAddRequiredCall,
		Repository: input.InvestigationSource,
		Target: models.RemediationTarget{
			Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile",
			RequiredCall: "applyFix", Path: "controllers/reconcile.go",
		},
		ExpectedBehavior:          "invoke applyFix before returning",
		RelationshipProof:         "the recurring failure path executes reconcile before the missing transition",
		CurrentSource:             CurrentSourceAbsent,
		VerificationRequirements:  []string{"verify the call is missing", "run controller tests"},
		AllowedChangedPaths:       []string{"controllers/reconcile.go"},
		AllowedValidationCommands: []ValidationCommand{{Argv: []string{"go", "test", "./controllers/..."}, Timeout: "10m"}},
	}
	result := Result{
		Version: ResultVersion, Classification: ClassificationActionable,
		Reason: "the reconcile path omits applyFix", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "source and recurring build evidence agree", Proposal: proposal,
		Evidence: []EvidenceCitation{
			{Kind: EvidenceSource, Path: "controllers/reconcile.go", LineStart: 2, LineEnd: 2, Quote: "func reconcile"},
			{Kind: EvidenceArtifact, BuildID: "1", Path: "builds/1/log.txt", LineStart: 1, LineEnd: 1, Quote: "reconcile missing applyFix transition"},
			{Kind: EvidenceArtifact, BuildID: "2", Path: "builds/2/log.txt", LineStart: 1, LineEnd: 1, Quote: "reconcile missing applyFix transition"},
		},
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
	provenance := NewProvenance(input, "model", "chat_completions", EvidenceStats{
		ToolCalls: 3, SourceReads: 1, ArtifactReads: 2,
	}, Metrics{ModelRequests: 2}, time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC))
	entry := CacheEntry{Key: key, Result: result, ResultDigest: ResultDigest(result), Provenance: provenance}
	browser := fakeBrowser{files: map[string]string{
		"builds/1/log.txt": "reconcile missing applyFix transition\n", "builds/2/log.txt": "reconcile missing applyFix transition\n",
	}}
	return verifier, input, entry, browser
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

func TestVerifierRejectsTargetAlreadyPresentAtFailureRevision(t *testing.T) {
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

func TestVerifierRejectsFabricatedSymbolAndUnlinkedTarget(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	entry.Result.Proposal.Target.Symbol = "fabricatedFix"
	entry.ResultDigest = ResultDigest(entry.Result)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("verified=%+v", verified)
	}

	entry.Result.Proposal.Target.Symbol = "reconcile"
	entry.Result.Proposal.Target.Path = "controllers/other.go"
	entry.Result.Proposal.AllowedChangedPaths = []string{"controllers/other.go"}
	if _, err := verifier.Verify(t.Context(), input, entry, browser); err == nil {
		t.Fatal("cache entry mutated after acceptance was not rejected")
	}
}

func TestVerifierDowngradesUntargetedExternalDependencyClaim(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	entry.Result = Result{
		Version: ResultVersion, Classification: ClassificationExternalDependency,
		Reason: "the failing implementation is dependency-owned", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "both analyses point to the dependency boundary",
		Evidence: []EvidenceCitation{
			{Kind: EvidenceAnalysis, BuildID: "1", AnalysisGeneratedAt: input.Analyses[0].GeneratedAt, Quote: input.Analyses[0].RootCause},
			{Kind: EvidenceAnalysis, BuildID: "2", AnalysisGeneratedAt: input.Analyses[1].GeneratedAt, Quote: input.Analyses[1].RootCause},
		},
	}
	entry.ResultDigest = ResultDigest(entry.Result)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence || verified.Proposal != nil {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierAcceptsExactProwJobEnvironmentTarget(t *testing.T) {
	input := testFrozenInput()
	input.JobName = "periodic-capz"
	input.JobID = "periodic-capz"
	input.InvestigationSource = sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: currentRevision}
	const configPath = "config/jobs/example/periodics.yaml"
	input.RelevantFiles = []string{configPath}
	input.DestinationPolicy = DestinationPolicy{Project: "test", Repositories: []RepositoryPolicy{{
		Repository: "kubernetes/test-infra", AllowedPaths: []string{"config/jobs/example/"}, AllowedCommands: []ValidationCommand{{Argv: []string{"go", "test", "./config/..."}, Timeout: "10m"}},
	}}}
	for index := range input.Builds {
		input.Builds[index].Source = &sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: testRevision}
		input.Analyses[index].SourceRepository = input.Builds[index].Source
		input.Analyses[index].RelevantFiles = []string{configPath}
	}
	target := models.RemediationTarget{
		Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra",
		Revision: currentRevision, Path: configPath, Job: "periodic-capz", Container: "test",
		Name: "FEATURE_FLAG", Value: "enabled",
	}
	result := Result{
		Version: ResultVersion, Classification: ClassificationActionable,
		Reason: "the job omits FEATURE_FLAG", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "both builds fail before the feature-gated test starts",
		Proposal: &ActionableProposal{
			TargetKind: TargetSetJobEnvironment, Repository: input.InvestigationSource, Target: target,
			ExpectedBehavior: "set FEATURE_FLAG=enabled in the test container", RelationshipProof: "the exact Prow job launches both failing builds",
			CurrentSource: CurrentSourceAbsent, VerificationRequirements: []string{"verify the env value", "render Prow config"},
			AllowedChangedPaths: []string{configPath}, AllowedValidationCommands: []ValidationCommand{{Argv: []string{"go", "test", "./config/..."}, Timeout: "10m"}},
		},
		Evidence: []EvidenceCitation{
			{Kind: EvidenceSource, Path: configPath, LineStart: 1, LineEnd: 6, Quote: "name: periodic-capz"},
			{Kind: EvidenceArtifact, BuildID: "1", Path: "builds/1/log.txt", LineStart: 1, LineEnd: 1, Quote: "periodic-capz FEATURE_FLAG is missing"},
			{Kind: EvidenceArtifact, BuildID: "2", Path: "builds/2/log.txt", LineStart: 1, LineEnd: 1, Quote: "periodic-capz FEATURE_FLAG is missing"},
		},
	}
	config := "periodics:\n- name: periodic-capz\n  spec:\n    containers:\n    - name: test\n"
	reader := revisionSource{files: map[string]map[string]string{
		sourceKey(input.InvestigationSource): {configPath: config},
		sourceKey(*input.Builds[0].Source):   {configPath: config},
	}}
	verifier, err := NewVerifier(reader)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := CacheKey(input)
	entry := CacheEntry{
		Key: key, Result: result, ResultDigest: ResultDigest(result),
		Provenance: NewProvenance(input, "model", "chat_completions", EvidenceStats{SourceReads: 1, ArtifactReads: 2}, Metrics{}, time.Now()),
	}
	browser := fakeBrowser{files: map[string]string{"builds/1/log.txt": "periodic-capz FEATURE_FLAG is missing\n", "builds/2/log.txt": "periodic-capz FEATURE_FLAG is missing\n"}}
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationActionable || verified.Proposal == nil {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierNonActionableControlCategories(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	for _, classification := range []Classification{
		ClassificationEnvironmentOrInfrastructure,
		ClassificationMitigationOnly,
		ClassificationInsufficientEvidence,
	} {
		t.Run(string(classification), func(t *testing.T) {
			verifier, input, entry, browser := verificationFixture(t, missing, missing)
			entry.Result = Result{
				Version: ResultVersion, Classification: classification,
				Reason: "evidence-backed non-actionable result", CauseAssessment: CauseSupports,
				CauseAssessmentReason: "both frozen analyses support this terminal category",
				Evidence: []EvidenceCitation{
					{Kind: EvidenceAnalysis, BuildID: "1", AnalysisGeneratedAt: input.Analyses[0].GeneratedAt, Quote: input.Analyses[0].RootCause},
					{Kind: EvidenceAnalysis, BuildID: "2", AnalysisGeneratedAt: input.Analyses[1].GeneratedAt, Quote: input.Analyses[1].RootCause},
				},
			}
			entry.ResultDigest = ResultDigest(entry.Result)
			verified, err := verifier.Verify(t.Context(), input, entry, browser)
			if err != nil {
				t.Fatal(err)
			}
			if verified.Classification != classification || verified.Proposal != nil {
				t.Fatalf("verified=%+v", verified)
			}
		})
	}
}

func TestVerifierDowngradesUntargetedAlreadyFixedClaim(t *testing.T) {
	missing := "package controllers\nfunc reconcile() error { return nil }\nfunc applyFix() {}\n"
	verifier, input, entry, browser := verificationFixture(t, missing, missing)
	entry.Result = Result{
		Version: ResultVersion, Classification: ClassificationAlreadyFixed,
		Reason: "the model claims the fix is present", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "source appears changed",
		Evidence: []EvidenceCitation{
			{Kind: EvidenceAnalysis, BuildID: "1", AnalysisGeneratedAt: input.Analyses[0].GeneratedAt, Quote: input.Analyses[0].RootCause},
			{Kind: EvidenceAnalysis, BuildID: "2", AnalysisGeneratedAt: input.Analyses[1].GeneratedAt, Quote: input.Analyses[1].RootCause},
		},
	}
	entry.ResultDigest = ResultDigest(entry.Result)
	verified, err := verifier.Verify(t.Context(), input, entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence {
		t.Fatalf("verified=%+v", verified)
	}
}

func TestVerifierRejectsUndecidableTargetKindsAndWorkspacePaths(t *testing.T) {
	for _, kind := range []TargetKind{TargetModifySymbol, TargetSetConfiguration, TargetRemoveConfiguration} {
		if deterministicallyVerifiableTargetKind(kind) {
			t.Fatalf("target kind %q unexpectedly verifiable", kind)
		}
	}
	for _, value := range []string{"pkg/mod/example.net/dependency@v1.2.3/fix.go", ".cache/modules/fix.go", "C:\\pkg\\mod\\dependency\\fix.go"} {
		if !suspiciousRepositoryPath(value) {
			t.Fatalf("workspace path %q was not rejected", value)
		}
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
	}
	target := models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: currentRevision, Path: configPath, Job: "periodic-capz", Container: "test", Name: "FEATURE_FLAG", Value: "enabled"}
	result := Result{
		Version: ResultVersion, Classification: ClassificationActionable,
		Reason: "the job omits FEATURE_FLAG", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "the exact job fails before test startup",
		Proposal: &ActionableProposal{
			TargetKind: TargetSetJobEnvironment, Repository: input.InvestigationSource, Target: target,
			ExpectedBehavior: "set FEATURE_FLAG", RelationshipProof: "the Prow job launches the failing builds",
			CurrentSource: CurrentSourceAbsent, VerificationRequirements: []string{"render config"},
			AllowedChangedPaths: []string{configPath}, AllowedValidationCommands: []ValidationCommand{{Argv: []string{"go", "test", "./config/..."}, Timeout: "10m"}},
		},
		Evidence: []EvidenceCitation{
			{Kind: EvidenceSource, Path: configPath, LineStart: 1, LineEnd: 6, Quote: "name: periodic-capz"},
			{Kind: EvidenceArtifact, BuildID: "1", Path: "builds/1/log.txt", LineStart: 1, LineEnd: 1, Quote: "periodic-capz FEATURE_FLAG is missing"},
			{Kind: EvidenceArtifact, BuildID: "2", Path: "builds/2/log.txt", LineStart: 1, LineEnd: 1, Quote: "periodic-capz FEATURE_FLAG is missing"},
		},
	}
	config := "periodics:\n- name: periodic-capz\n  spec:\n    containers:\n    - name: test\n"
	reader := revisionSource{files: map[string]map[string]string{sourceKey(input.InvestigationSource): {configPath: config}, sourceKey(*input.Builds[0].Source): {configPath: config}}}
	verifier, _ := NewVerifier(reader)
	key, _ := CacheKey(input)
	entry := CacheEntry{Key: key, Result: result, ResultDigest: ResultDigest(result), Provenance: NewProvenance(input, "model", "chat_completions", EvidenceStats{}, Metrics{}, time.Now())}
	browser := fakeBrowser{files: map[string]string{"builds/1/log.txt": "periodic-capz FEATURE_FLAG is missing\n", "builds/2/log.txt": "periodic-capz FEATURE_FLAG is missing\n"}}
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
		Target:     models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Job: "periodic-capz", Name: "FABRICATED_FLAG"},
	}
	result := Result{
		Proposal: &proposal,
		Evidence: []EvidenceCitation{
			{Kind: EvidenceArtifact, BuildID: "1", Quote: "periodic-capz failed during startup"},
			{Kind: EvidenceArtifact, BuildID: "2", Quote: "periodic-capz failed during startup"},
		},
	}
	input := testFrozenInput()
	input.JobName = "periodic-capz"
	input.RelevantFiles = []string{"config/jobs/example/periodics.yaml"}
	result.Proposal.Target.Path = "config/jobs/example/periodics.yaml"
	result.Evidence = append(result.Evidence, EvidenceCitation{Kind: EvidenceSource, Path: result.Proposal.Target.Path, Quote: "periodic-capz"})
	if err := verifyStructuralRelationship(input, result); err == nil {
		t.Fatal("job-name-only evidence accepted a fabricated environment variable")
	}
}
