package sourceinvestigation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestValidateRepositoryRequiresPinnedCommit(t *testing.T) {
	if err := ValidateRepository(Repository{Owner: "example", Name: "repo", Revision: "main"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ValidateRepository(main) = %v", err)
	}
	if err := ValidateRepository(Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"}); err != nil {
		t.Fatalf("ValidateRepository(commit) = %v", err)
	}
}

func TestValidateResultRejectsUnsafeCitations(t *testing.T) {
	base := Result{
		Finding: "The retry loop masks the original error.", Confidence: ConfidenceHigh,
		Relationship: RelationshipRefines, Direction: "Inspect the retry termination path.",
		Citations: []Citation{{Path: "pkg/retry.go", LineStart: 10, LineEnd: 12, Quote: "return err"}},
	}
	if err := ValidateResult(base); err != nil {
		t.Fatalf("ValidateResult(valid) = %v", err)
	}
	for _, value := range []string{"..", "../secret"} {
		unsafe := base
		unsafe.Citations = []Citation{{Path: value, LineStart: 1, LineEnd: 1, Quote: "x"}}
		if err := ValidateResult(unsafe); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("ValidateResult(%q) = %v", value, err)
		}
	}
}

func TestValidateResultRequiresStateTargetAlignment(t *testing.T) {
	base := Result{
		State:   StateActionableCodeChange,
		Target:  &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "retry", RequiredCall: "applyFix"},
		Finding: "The retry loop masks the original error.", Confidence: ConfidenceHigh,
		Relationship: RelationshipRefines, Direction: "Inspect the retry termination path.",
		Citations: []Citation{{Path: "pkg/retry.go", LineStart: 10, LineEnd: 12, Quote: "return err"}},
	}
	if err := ValidateResult(base); err != nil {
		t.Fatalf("ValidateResult(actionable) = %v", err)
	}
	invalid := base
	invalid.State = StateActionableConfigurationChange
	if err := ValidateResult(invalid); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("ValidateResult(mismatched state) = %v", err)
	}
	missingCall := base
	missingTarget := *base.Target
	missingTarget.RequiredCall = ""
	missingCall.Target = &missingTarget
	if err := ValidateResult(missingCall); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("ValidateResult(missing required call) = %v", err)
	}
	inconclusive := base
	inconclusive.State = StateInconclusive
	if err := ValidateResult(inconclusive); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("ValidateResult(inconclusive target) = %v", err)
	}
}

func TestValidateResultBoundsTargetMetadata(t *testing.T) {
	result := Result{
		State:   StateActionableCodeChange,
		Target:  &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "Name" + strings.Repeat("x", 30<<10)},
		Finding: "finding", Confidence: ConfidenceHigh, Relationship: RelationshipRefines, Direction: "direction",
		Citations: []Citation{{Path: "pkg/retry.go", LineStart: 1, LineEnd: 1, Quote: "Name"}},
	}
	if err := ValidateResult(result); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("ValidateResult(oversized target) = %v", err)
	}
}

type countingSourceReader struct {
	files map[string]string
	calls map[string]int
}

func (r *countingSourceReader) ReadFile(_ context.Context, _ Repository, path string) (string, error) {
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[path]++
	content, ok := r.files[path]
	if !ok {
		return "", errors.New("missing")
	}
	return content, nil
}

func TestVerifyCitationsReturnsVerifiedClone(t *testing.T) {
	reader := &countingSourceReader{files: map[string]string{"pkg/retry.go": "first\r\nreturn err\nthird\n"}}
	input := []Citation{
		{Path: "pkg/retry.go", LineStart: 2, LineEnd: 2, Quote: "return err"},
		{Path: "pkg/retry.go", LineStart: 1, LineEnd: 2, Quote: "first"},
	}
	got, err := VerifyCitations(t.Context(), reader, Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Verified || !got[1].Verified || input[0].Verified || reader.calls["pkg/retry.go"] != 1 {
		t.Fatalf("verified=%+v input=%+v calls=%v", got, input, reader.calls)
	}
}

func TestVerifyCitationsDoesNotPartiallyVerifyOnFailure(t *testing.T) {
	reader := &countingSourceReader{files: map[string]string{"pkg/retry.go": "first\nreturn err\n"}}
	input := []Citation{
		{Path: "pkg/retry.go", LineStart: 2, LineEnd: 2, Quote: "return err"},
		{Path: "pkg/retry.go", LineStart: 1, LineEnd: 1, Quote: "missing"},
	}
	if _, err := VerifyCitations(t.Context(), reader, Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, input); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("error = %v", err)
	}
	if input[0].Verified || input[1].Verified {
		t.Fatalf("input was mutated: %+v", input)
	}
}

func (r *countingSourceReader) ListFiles(_ context.Context, _ Repository) ([]string, error) {
	files := make([]string, 0, len(r.files))
	for file := range r.files {
		files = append(files, file)
	}
	return files, nil
}

func TestVerifyResultTargetProvesRequiredCall(t *testing.T) {
	repo := Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	for _, testCase := range []struct {
		name   string
		result Result
		files  map[string]string
		wantOK bool
	}{
		{
			name: "same package missing call",
			result: Result{State: StateActionableCodeChange, Target: &models.RemediationTarget{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "applyFix", Path: "controllers/reconcile.go",
			}},
			files: map[string]string{
				"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
				"controllers/fix.go":       "package controllers\nfunc applyFix() {}\n",
			},
			wantOK: true,
		},
		{
			name: "same package fabricated call",
			result: Result{State: StateActionableCodeChange, Target: &models.RemediationTarget{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "fabricatedFix", Path: "controllers/reconcile.go",
			}},
			files: map[string]string{"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n"},
		},
		{
			name: "nested module imported helper",
			result: Result{State: StateActionableCodeChange, Target: &models.RemediationTarget{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/tools/migration.ApplyFix", Path: "tools/controllers/reconcile.go",
			}},
			files: map[string]string{
				"go.mod":                         "module example.com/root\n",
				"tools/go.mod":                   "module example.com/tools\n",
				"tools/controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
				"tools/migration/fix.go":         "package migration\nfunc ApplyFix() {}\n",
			},
			wantOK: true,
		},
		{
			name: "existing call",
			result: Result{State: StateAlreadyPresent, Target: &models.RemediationTarget{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "applyFix", Path: "controllers/reconcile.go",
			}},
			files: map[string]string{
				"controllers/reconcile.go": "package controllers\nfunc reconcile() { applyFix() }\n",
				"controllers/fix.go":       "package controllers\nfunc applyFix() {}\n",
			},
			wantOK: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := VerifyResultTarget(t.Context(), &countingSourceReader{files: testCase.files}, repo, &testCase.result)
			if (err == nil) != testCase.wantOK {
				t.Fatalf("VerifyResultTarget error = %v, wantOK=%t", err, testCase.wantOK)
			}
			if err != nil && !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("VerifyResultTarget error = %v", err)
			}
		})
	}
}

func TestValidateVerifiedResultRequiresTargetVerification(t *testing.T) {
	result := Result{
		State:   StateActionableCodeChange,
		Target:  &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "applyFix", Path: "controllers/reconcile.go"},
		Finding: "finding", Confidence: ConfidenceHigh, Relationship: RelationshipSupports, Direction: "direction",
		Citations: []Citation{{Path: "controllers/reconcile.go", LineStart: 1, LineEnd: 1, Quote: "reconcile", Verified: true}},
	}
	if err := ValidateVerifiedResult(result); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("unverified target error = %v", err)
	}
	result.TargetVerificationVersion = targetVerificationVersion
	if err := ValidateVerifiedResult(result); err != nil {
		t.Fatalf("verified target error = %v", err)
	}
}
