package remediationinvestigation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

func testFrozenInput() FrozenInput {
	group := models.PatternCausalGroup{
		Builds: []string{"2", "1"}, RootCause: "a required call is missing", Confidence: "high",
	}
	group.ContentHash = models.PatternCausalGroupHash(group)
	group.ID = models.PatternCausalGroupID("pattern-id", group)
	return FrozenInput{
		PatternID: "pattern-id", PatternHash: strings.Repeat("b", 64),
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
		JobID: "periodic-test", JobName: "periodic-test", Recurrence: models.PatternRecurrenceSharedCause, Group: group,
		Builds: []BuildReference{
			{BuildID: "2", BuildPrefix: "jobs/test/2/", Source: &sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision}},
			{BuildID: "1", BuildPrefix: "jobs/test/1/", Source: &sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision}},
		},
		Analyses: []AnalysisReference{
			{BuildID: "2", TestName: "test", GeneratedAt: "2026-08-12T00:00:00Z", RootCause: "a required call is missing", Severity: "High"},
			{BuildID: "1", TestName: "test", GeneratedAt: "2026-08-12T00:00:00Z", RootCause: "a required call is missing", Severity: "High"},
		},
		RelevantFiles:       []string{"controllers/reconcile.go"},
		InvestigationSource: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
		DestinationPolicy: DestinationPolicy{Project: "test-project", Repositories: []RepositoryPolicy{{
			Repository: "example/repo", AllowedPaths: []string{"controllers/"}, AllowedCommands: []ValidationCommand{{Argv: []string{"go", "test", "./controllers/..."}, Timeout: "10m"}},
		}}},
		ConsumerPrompt: "Project context.", ConsumerPromptHash: HashText("Project context."),
		SkillHash: strings.Repeat("c", 64), ProviderFingerprint: strings.Repeat("d", 16), Versions: CurrentVersions(),
	}
}

func TestFrozenInputDigestCanonicalizesOrderAndSealsSemanticFields(t *testing.T) {
	base := testFrozenInput()
	if err := ValidateFrozenInput(base); err != nil {
		t.Fatal(err)
	}
	key, err := CacheKey(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := testFrozenInput()
	reordered.Group.Builds[0], reordered.Group.Builds[1] = reordered.Group.Builds[1], reordered.Group.Builds[0]
	reordered.Builds[0], reordered.Builds[1] = reordered.Builds[1], reordered.Builds[0]
	reordered.Analyses[0], reordered.Analyses[1] = reordered.Analyses[1], reordered.Analyses[0]
	got, err := CacheKey(reordered)
	if err != nil || got != key {
		t.Fatalf("reordered key=%q err=%v want=%q", got, err, key)
	}

	mutations := []struct {
		name string
		edit func(*FrozenInput)
	}{
		{"pattern hash", func(input *FrozenInput) { input.PatternHash = strings.Repeat("e", 64) }},
		{"job name", func(input *FrozenInput) { input.JobName = "different-job" }},
		{"group hash", func(input *FrozenInput) {
			input.CausalGroupHash = strings.Repeat("f", 64)
			input.Group.ContentHash = input.CausalGroupHash
		}},
		{"source revision", func(input *FrozenInput) { input.InvestigationSource.Revision = strings.Repeat("1", 40) }},
		{"provider", func(input *FrozenInput) { input.ProviderFingerprint = strings.Repeat("2", 16) }},
		{"prompt", func(input *FrozenInput) {
			input.ConsumerPrompt = "changed"
			input.ConsumerPromptHash = HashText("changed")
		}},
		{"skills", func(input *FrozenInput) { input.SkillHash = strings.Repeat("3", 64) }},
		{"policy", func(input *FrozenInput) { input.DestinationPolicy.Repositories[0].AllowedPaths = []string{"pkg/"} }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := testFrozenInput()
			test.edit(&changed)
			changedKey, err := CacheKey(changed)
			if err == nil && changedKey == key {
				t.Fatalf("mutation did not change cache key")
			}
		})
	}
}

func TestDecodeResultUsesMinimalDiscriminatedContract(t *testing.T) {
	evidenceID := "analysis:" + strings.Repeat("a", 64)
	valid := `{"version":3,"cause_assessment":"inconclusive","reason":"the source relationship is ambiguous","candidate":null,"evidence_ids":["` + evidenceID + `"],"non_actionable_reason":"insufficient_evidence"}`
	if _, err := DecodeResult(json.RawMessage(valid)); err != nil {
		t.Fatal(err)
	}
	candidate := `{"version":3,"cause_assessment":"supports","reason":"the recurring path omits applyFix","candidate":{"kind":"required_call","path":"controllers/reconcile.go","containing_symbol":"reconcile","required_call":"applyFix"},"evidence_ids":["` + evidenceID + `"],"non_actionable_reason":null}`
	decoded, err := DecodeResult(json.RawMessage(candidate))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.Candidate.(*RequiredCallCandidate); !ok {
		t.Fatalf("candidate type=%T", decoded.Candidate)
	}
	for name, raw := range map[string]string{
		"unknown top level":       strings.Replace(valid, `"version":3`, `"version":3,"extra":true`, 1),
		"duplicate":               strings.Replace(valid, `"version":3`, `"version":3,"version":3`, 1),
		"irrelevant target field": strings.Replace(candidate, `"required_call":"applyFix"`, `"required_call":"applyFix","job":"periodic-test"`, 1),
		"candidate and reason":    strings.Replace(candidate, `"non_actionable_reason":null`, `"non_actionable_reason":"insufficient_evidence"`, 1),
		"no candidate reason":     strings.Replace(valid, `"non_actionable_reason":"insufficient_evidence"`, `"non_actionable_reason":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeResult(json.RawMessage(raw)); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestCandidateVariantsDoNotRequireIrrelevantFields(t *testing.T) {
	evidenceIDs := []string{"source:" + strings.Repeat("a", 64), "analysis:" + strings.Repeat("b", 64)}
	candidates := []CandidateTarget{
		&RequiredCallCandidate{Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix"},
		&SymbolAdditionCandidate{Kind: CandidateSymbolAddition, Path: "controllers/helpers.go", Symbol: "applyFix"},
		&ProwEnvironmentEntryCandidate{Kind: CandidateProwEnvironmentEntry, ConfigPath: "config/jobs/example/periodics.yaml", Job: "periodic-test", Container: "test", Name: "FEATURE_FLAG", Value: "enabled"},
		&ConfigurationFieldCandidate{Kind: CandidateConfigurationField, Path: "config/defaults.yaml", FieldPath: []string{"feature", "enabled"}, Value: "true"},
	}
	for _, candidate := range candidates {
		result := Result{Version: ResultVersion, CauseAssessment: CauseSupports, Reason: "bounded evidence identifies one target", Candidate: candidate, EvidenceIDs: evidenceIDs}
		if err := ValidateResult(result); err != nil {
			t.Fatalf("%T: %v", candidate, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"repository"`) || strings.Contains(string(encoded), `"revision"`) || strings.Contains(string(encoded), `"allowed_changed_paths"`) {
			t.Fatalf("model result contains engine-owned fields: %s", encoded)
		}
	}
}

func TestEvidenceCatalogIDsBindEngineIssuedIdentity(t *testing.T) {
	record := EvidenceRecord{
		Kind: EvidenceSource,
		Source: &SourceEvidenceIdentity{
			Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
			Path:       "controllers/reconcile.go", ContentDigest: HashText("package controllers\n"),
		},
	}
	record.ID = evidenceRecordID(record)
	catalog := EvidenceCatalog{Version: EvidenceCatalogVersion, Records: []EvidenceRecord{record}}
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	digest := EvidenceCatalogDigest(catalog)
	catalog.Records[0].Source.Path = "controllers/other.go"
	if ValidateEvidenceCatalog(catalog) == nil || EvidenceCatalogDigest(catalog) == digest {
		t.Fatal("mutated evidence identity remained valid")
	}
}

func TestSourceGrepEvidenceIDsBindCanonicalPrivateIdentity(t *testing.T) {
	record := EvidenceRecord{
		Kind: EvidenceSourceGrep,
		SourceGrep: &SourceGrepEvidenceIdentity{
			Repository: sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
			Path:       "controllers/reconcile.go", LineStart: 10, LineEnd: 12,
			ContentDigest: HashText("package controllers\n"), Match: "func reconcile() {\n\tapplyFix()\n}",
		},
	}
	record.ID = evidenceRecordID(record)
	catalog := EvidenceCatalog{Version: EvidenceCatalogVersion, Records: []EvidenceRecord{record}}
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	originalID := record.ID
	catalog.Records[0].SourceGrep.LineStart = 11
	if ValidateEvidenceCatalog(catalog) == nil || evidenceRecordID(catalog.Records[0]) == originalID {
		t.Fatal("mutated source grep range retained a valid engine identity")
	}
}

func TestResultFormatExcludesEngineOwnedFields(t *testing.T) {
	encoded, err := json.Marshal(resultFormat().Schema)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		`"classification"`, `"repository"`, `"revision"`, `"current_source"`,
		`"allowed_changed_paths"`, `"allowed_validation_commands"`, `"verification_requirements"`,
		`"line_start"`, `"line_end"`, `"quote"`, `"build_id"`, `"generated_at"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("model schema contains engine-owned field %s: %s", forbidden, text)
		}
	}
	for _, required := range []string{
		string(CandidateRequiredCall), string(CandidateSymbolAddition),
		string(CandidateProwEnvironmentEntry), string(CandidateConfigurationField),
		`"evidence_ids"`, `"non_actionable_reason"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("model schema is missing %s: %s", required, text)
		}
	}
}

func TestEvidencePromptRequiresContentBearingSourceRead(t *testing.T) {
	prompt := evidenceSystemPrompt("Project context.")
	for _, anchor := range []string{"MUST call read_repo_file", "non-empty pinned source content", "memo without a content-bearing source read is discarded", "Relevant files are hints, not proven targets"} {
		if !strings.Contains(prompt, anchor) {
			t.Fatalf("evidence prompt is missing %q", anchor)
		}
	}
}

func TestFinalPromptTreatsCandidateAsVerificationSubject(t *testing.T) {
	prompt := finalSystemPrompt()
	for _, anchor := range []string{
		"A candidate is a verification subject, not authorization to modify source",
		"including when it already appears present",
		"derives actionable, already_fixed, or insufficient_evidence",
		"Do not author or withhold a candidate based on a lifecycle classification",
	} {
		if !strings.Contains(prompt, anchor) {
			t.Fatalf("final prompt is missing %q", anchor)
		}
	}
}
