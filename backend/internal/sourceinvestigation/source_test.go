package sourceinvestigation

import (
	"errors"
	"testing"
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
	unsafe := base
	unsafe.Citations = []Citation{{Path: "../secret", LineStart: 1, LineEnd: 1, Quote: "x"}}
	if err := ValidateResult(unsafe); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("ValidateResult(unsafe) = %v", err)
	}
}
