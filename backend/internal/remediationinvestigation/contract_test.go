package remediationinvestigation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
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

func TestDecodeTwoStageContracts(t *testing.T) {
	evidenceID := "analysis:" + strings.Repeat("a", 64)
	extraction := `{"version":1,"hypotheses":[{"target":{"kind":"required_call","path":"controllers/reconcile.go","containing_symbol":"reconcile","required_call":"applyFix"},"evidence_ids":["` + evidenceID + `"],"relationship_reason":"the recurring path omits applyFix"}]}`
	decoded, err := DecodeTargetExtraction(json.RawMessage(extraction))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Hypotheses) != 1 {
		t.Fatalf("hypotheses=%d", len(decoded.Hypotheses))
	}
	if _, ok := decoded.Hypotheses[0].Target.(*RequiredCallCandidate); !ok {
		t.Fatalf("target type=%T", decoded.Hypotheses[0].Target)
	}
	assessment := `{"version":1,"cause_assessment":"inconclusive","reason":"the source relationship is ambiguous","evidence_ids":["` + evidenceID + `"],"non_actionable_reason":"insufficient_evidence"}`
	if _, err := DecodeNonActionableAssessment(json.RawMessage(assessment)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"too many hypotheses":    `{"version":1,"hypotheses":[` + strings.Repeat(`{"target":{"kind":"symbol_addition","path":"p.go","symbol":"S"},"evidence_ids":["`+evidenceID+`"],"relationship_reason":"reason"},`, 3) + `{"target":{"kind":"symbol_addition","path":"q.go","symbol":"Q"},"evidence_ids":["` + evidenceID + `"],"relationship_reason":"reason"}]}`,
		"target in stage two":    strings.Replace(assessment, `"reason":`, `"target":null,"reason":`, 1),
		"lifecycle in stage one": strings.Replace(extraction, `"hypotheses":`, `"classification":"actionable","hypotheses":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(name, "stage two") {
				if _, err := DecodeNonActionableAssessment(json.RawMessage(raw)); err == nil {
					t.Fatal("invalid assessment accepted")
				}
				return
			}
			if _, err := DecodeTargetExtraction(json.RawMessage(raw)); err == nil {
				t.Fatal("invalid extraction accepted")
			}
		})
	}
}

func TestDecodeTargetExtractionPreservesLegacyCandidateKinds(t *testing.T) {
	evidenceID := "analysis:" + strings.Repeat("a", 64)
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{
			name: "symbol addition",
			raw:  `{"version":1,"hypotheses":[{"target":{"kind":"symbol_addition","path":"controllers/helpers.go","symbol":"applyFix"},"evidence_ids":["` + evidenceID + `"],"relationship_reason":"legacy diagnostic target"}]}`,
			want: &SymbolAdditionCandidate{},
		},
		{
			name: "configuration field",
			raw:  `{"version":1,"hypotheses":[{"target":{"kind":"configuration_field","path":"config/defaults.yaml","field_path":["feature","enabled"],"value":"true"},"evidence_ids":["` + evidenceID + `"],"relationship_reason":"legacy diagnostic target"}]}`,
			want: &ConfigurationFieldCandidate{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := DecodeTargetExtraction(json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded.Hypotheses) != 1 || decoded.Hypotheses[0].Target == nil {
				t.Fatalf("decoded=%+v", decoded)
			}
			switch test.want.(type) {
			case *SymbolAdditionCandidate:
				if _, ok := decoded.Hypotheses[0].Target.(*SymbolAdditionCandidate); !ok {
					t.Fatalf("target type=%T", decoded.Hypotheses[0].Target)
				}
			case *ConfigurationFieldCandidate:
				if _, ok := decoded.Hypotheses[0].Target.(*ConfigurationFieldCandidate); !ok {
					t.Fatalf("target type=%T", decoded.Hypotheses[0].Target)
				}
			}
		})
	}
}

func TestTargetHypothesisVariantsDoNotRequireIrrelevantFields(t *testing.T) {
	evidenceIDs := []string{"source:" + strings.Repeat("a", 64), "analysis:" + strings.Repeat("b", 64)}
	candidates := []CandidateTarget{
		&RequiredCallCandidate{Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix"},
		&SymbolAdditionCandidate{Kind: CandidateSymbolAddition, Path: "controllers/helpers.go", Symbol: "applyFix"},
		&ProwEnvironmentEntryCandidate{Kind: CandidateProwEnvironmentEntry, ConfigPath: "config/jobs/example/periodics.yaml", Job: "periodic-test", Container: "test", Name: "FEATURE_FLAG", Value: "enabled"},
		&ConfigurationFieldCandidate{Kind: CandidateConfigurationField, Path: "config/defaults.yaml", FieldPath: []string{"feature", "enabled"}, Value: "true"},
	}
	for _, candidate := range candidates {
		extraction := TargetExtraction{Version: TargetExtractionVersion, Hypotheses: []TargetHypothesis{{
			Target: candidate, EvidenceIDs: evidenceIDs, RelationshipReason: "bounded evidence identifies one target",
		}}}
		if err := ValidateTargetExtraction(extraction); err != nil {
			t.Fatalf("%T: %v", candidate, err)
		}
		encoded, err := json.Marshal(extraction)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`"repository"`, `"revision"`, `"allowed_changed_paths"`, `"classification"`} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("target extraction contains engine-owned field %s: %s", forbidden, encoded)
			}
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

func TestTwoStageFormatsExcludeEngineOwnedAndCrossStageFields(t *testing.T) {
	extraction, err := json.Marshal(targetExtractionFormat().Schema)
	if err != nil {
		t.Fatal(err)
	}
	extractionText := string(extraction)
	for _, forbidden := range []string{`"classification"`, `"repository"`, `"revision"`, `"current_source"`, `"non_actionable_reason"`, `"cause_assessment"`, `"allowed_changed_paths"`, `"commands"`, string(CandidateSymbolAddition), string(CandidateConfigurationField)} {
		if strings.Contains(extractionText, forbidden) {
			t.Fatalf("target extraction schema contains forbidden field %s", forbidden)
		}
	}
	for _, required := range []string{string(CandidateRequiredCall), string(CandidateProwEnvironmentEntry), `"hypotheses"`, `"relationship_reason"`, `"evidence_ids"`} {
		if !strings.Contains(extractionText, required) {
			t.Fatalf("target extraction schema is missing %s", required)
		}
	}
	assessment, err := json.Marshal(nonActionableAssessmentFormat().Schema)
	if err != nil {
		t.Fatal(err)
	}
	assessmentText := string(assessment)
	for _, forbidden := range []string{`"target"`, `"hypotheses"`, `"repository"`, `"revision"`, `"classification"`, `"commands"`} {
		if strings.Contains(assessmentText, forbidden) {
			t.Fatalf("non-actionable schema contains forbidden field %s", forbidden)
		}
	}
	for _, required := range []string{`"cause_assessment"`, `"non_actionable_reason"`, `"reason"`, `"evidence_ids"`} {
		if !strings.Contains(assessmentText, required) {
			t.Fatalf("non-actionable schema is missing %s", required)
		}
	}
}

func TestTargetCandidateSchemaOffersOnlyDeterministicallyVerifiableKinds(t *testing.T) {
	variants, ok := targetCandidateSchema()["anyOf"].([]any)
	if !ok || len(variants) != 2 {
		t.Fatalf("candidate variants=%T %+v", targetCandidateSchema()["anyOf"], targetCandidateSchema()["anyOf"])
	}
	seen := map[string]bool{}
	for _, rawVariant := range variants {
		variant, ok := rawVariant.(map[string]any)
		if !ok {
			t.Fatalf("candidate variant type=%T", rawVariant)
		}
		properties, ok := variant["properties"].(map[string]any)
		if !ok {
			t.Fatalf("candidate properties type=%T", variant["properties"])
		}
		kindSchema, ok := properties["kind"].(map[string]any)
		if !ok {
			t.Fatalf("candidate kind schema type=%T", properties["kind"])
		}
		values, ok := kindSchema["enum"].([]string)
		if !ok || len(values) != 1 {
			t.Fatalf("candidate kind enum=%T %+v", kindSchema["enum"], kindSchema["enum"])
		}
		seen[values[0]] = true
	}
	for _, kind := range []CandidateKind{CandidateRequiredCall, CandidateProwEnvironmentEntry} {
		if !seen[string(kind)] {
			t.Fatalf("candidate schema is missing %q", kind)
		}
	}
	for _, kind := range []CandidateKind{CandidateSymbolAddition, CandidateConfigurationField} {
		if seen[string(kind)] {
			t.Fatalf("candidate schema offers unsupported kind %q", kind)
		}
	}
}

func TestEvidencePromptRequiresContentBearingSourceRead(t *testing.T) {
	prompt := evidenceSystemPrompt("Project context.")
	for _, anchor := range []string{"MUST call read_repo_file", "non-empty pinned source content", "then call grep_repo", "content-bearing repository grep is discarded", "Relevant files are hints, not proven targets"} {
		if !strings.Contains(prompt, anchor) {
			t.Fatalf("evidence prompt is missing %q", anchor)
		}
	}
}

func TestTargetExtractionPromptTreatsHypothesisAsVerificationSubject(t *testing.T) {
	prompt := targetExtractionSystemPrompt()
	for _, anchor := range []string{
		"target hypothesis is a verification subject, not authorization to modify source",
		"including when it already appears present",
		"derives actionable, already_fixed, insufficient_evidence, or ambiguous",
		"zero to three hypotheses",
	} {
		if !strings.Contains(strings.ToLower(prompt), strings.ToLower(anchor)) {
			t.Fatalf("target extraction prompt is missing %q", anchor)
		}
	}
	for _, kind := range []CandidateKind{CandidateRequiredCall, CandidateProwEnvironmentEntry} {
		if !strings.Contains(prompt, string(kind)) {
			t.Fatalf("target extraction prompt is missing supported kind %q", kind)
		}
	}
	for _, kind := range []CandidateKind{CandidateSymbolAddition, CandidateConfigurationField} {
		if strings.Contains(prompt, string(kind)) {
			t.Fatalf("target extraction prompt offers unsupported kind %q", kind)
		}
	}
}

func TestNonActionablePromptCannotIntroduceTarget(t *testing.T) {
	prompt := nonActionableSystemPrompt()
	for _, anchor := range []string{"only because dashboard code found no deterministically verified target hypothesis", "Do not introduce, suggest, or encode a target"} {
		if !strings.Contains(prompt, anchor) {
			t.Fatalf("non-actionable prompt is missing %q", anchor)
		}
	}
}

// TestValidateFrozenInputAcceptsSingleBuildCause verifies a cause observed in
// one build can be investigated. A cause seen once can still be a real defect,
// so eligibility follows the cause rather than how many builds happened to hit
// it. The pattern-level recurrence requirement is unchanged.
func TestValidateFrozenInputAcceptsSingleBuildCause(t *testing.T) {
	input := testFrozenInput()
	group := models.PatternCausalGroup{
		Builds: []string{"2"}, RootCause: "a required call is missing", Confidence: "high",
	}
	group.ContentHash = models.PatternCausalGroupHash(group)
	group.ID = models.PatternCausalGroupID(input.PatternID, group)
	input.Group = group
	input.CausalGroupID, input.CausalGroupHash = group.ID, group.ContentHash
	input.Builds = input.Builds[:1]
	input.Analyses = input.Analyses[:1]

	if err := ValidateFrozenInput(input); err != nil {
		t.Fatalf("a single-build cause was rejected: %v", err)
	}

	// A cause with no builds at all still has nothing to investigate.
	empty := input
	empty.Group.Builds = nil
	empty.Group.ContentHash = models.PatternCausalGroupHash(empty.Group)
	empty.Group.ID = models.PatternCausalGroupID(empty.PatternID, empty.Group)
	empty.CausalGroupID, empty.CausalGroupHash = empty.Group.ID, empty.Group.ContentHash
	if err := ValidateFrozenInput(empty); err == nil {
		t.Error("a cause with no builds was accepted")
	}
}
