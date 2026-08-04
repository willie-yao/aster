package sourceinvestigation

import (
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
		Target:  &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "retry"},
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
