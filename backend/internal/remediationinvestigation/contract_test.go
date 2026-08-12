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
			{BuildID: "2", TestName: "test", GeneratedAt: "2026-08-11T00:00:00Z", RootCause: "a required call is missing", Severity: "High"},
			{BuildID: "1", TestName: "test", GeneratedAt: "2026-08-11T00:00:00Z", RootCause: "a required call is missing", Severity: "High"},
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
	reordered.DestinationPolicy.Repositories[0].AllowedCommands = []ValidationCommand{{Argv: []string{"go", "test", "./controllers/..."}, Timeout: "10m"}}
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

func TestDecodeResultRejectsUnknownDuplicateAndNonActionableProposal(t *testing.T) {
	valid := `{"version":2,"classification":"insufficient_evidence","reason":"not enough evidence","cause_assessment":"inconclusive","cause_assessment_reason":"the source relationship is ambiguous","proposal":null,"evidence":[{"kind":"analysis","build_id":"1","path":"","line_start":0,"line_end":0,"quote":"cause","analysis_generated_at":"2026-08-11T00:00:00Z"}]}`
	if _, err := DecodeResult(json.RawMessage(valid)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown":   strings.Replace(valid, `"version":2`, `"version":2,"extra":true`, 1),
		"duplicate": strings.Replace(valid, `"version":2`, `"version":2,"version":2`, 1),
		"proposal":  strings.Replace(valid, `"proposal":null`, `"proposal":{"repository":{"owner":"example","name":"repo","revision":"`+testRevision+`"},"target":{"intent":"modify_symbol","symbol":"reconcile","required_call":"applyFix","path":"controllers/reconcile.go","value":"","repository":"example/repo","revision":"`+testRevision+`","job":"","container":"","name":""},"expected_behavior":"call applyFix","relationship_proof":"the cited function owns the failing path","current_source":"absent","verification_requirements":["run tests"],"allowed_changed_paths":["controllers/reconcile.go"],"allowed_validation_commands":["go test ./controllers/..."]}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeResult(json.RawMessage(raw)); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestValidateActionableResultRequiresTypedBoundedTarget(t *testing.T) {
	result := Result{
		Version: ResultVersion, Classification: ClassificationActionable,
		Reason: "the call is missing", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "source and artifact evidence agree",
		Proposal: &ActionableProposal{
			TargetKind:       TargetAddRequiredCall,
			Repository:       sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
			Target:           models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "applyFix", Path: "controllers/reconcile.go"},
			ExpectedBehavior: "invoke applyFix before returning", RelationshipProof: "the failing path executes reconcile",
			CurrentSource: CurrentSourceAbsent, VerificationRequirements: []string{"go test the controller"},
			AllowedChangedPaths: []string{"controllers/reconcile.go"}, AllowedValidationCommands: []ValidationCommand{{Argv: []string{"go", "test", "./controllers/..."}, Timeout: "10m"}},
		},
		Evidence: []EvidenceCitation{{Kind: EvidenceSource, Path: "controllers/reconcile.go", LineStart: 1, LineEnd: 3, Quote: "func reconcile"}},
	}
	if err := ValidateResult(result); err != nil {
		t.Fatal(err)
	}
	result.Proposal.CurrentSource = CurrentSourcePresent
	if err := ValidateResult(result); err == nil {
		t.Fatal("already-present actionable proposal accepted")
	}
	result.Proposal.CurrentSource = CurrentSourceAbsent
	result.Proposal.Target.Path = "other.go"
	if err := ValidateResult(result); err == nil {
		t.Fatal("target outside allowed paths accepted")
	}
}

func TestValidateActionableResultRejectsUnsafeConversionProposal(t *testing.T) {
	result := Result{
		Version: ResultVersion, Classification: ClassificationActionable,
		Reason: "DeleteWebhookConfigurations so conversion stops calling ASO", CauseAssessment: CauseSupports,
		CauseAssessmentReason: "the conversion request failed",
		Proposal: &ActionableProposal{
			TargetKind:       TargetAddRequiredCall,
			Repository:       sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: testRevision},
			Target:           models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Symbol: "DeleteWebhookConfigurations", Path: "controllers/conversion.go"},
			ExpectedBehavior: "delete webhook configuration to disable conversion", RelationshipProof: "conversion timed out",
			CurrentSource: CurrentSourceAbsent, VerificationRequirements: []string{"run tests"},
			AllowedChangedPaths: []string{"controllers/conversion.go"}, AllowedValidationCommands: []ValidationCommand{{Argv: []string{"go", "test", "./controllers/..."}, Timeout: "10m"}},
		},
		Evidence: []EvidenceCitation{{Kind: EvidenceSource, Path: "controllers/conversion.go", LineStart: 1, LineEnd: 1, Quote: "conversion"}},
	}
	if err := ValidateResult(result); err == nil {
		t.Fatal("unsafe conversion proposal accepted")
	}
}
