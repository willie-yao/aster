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
