// Package sourceinvestigation defines read-only source investigation contracts.
package sourceinvestigation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnavailable means the source runtime cannot run in this deployment.
var ErrUnavailable = errors.New("source investigation unavailable")

var fullCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Repository identifies the exact source checkout to investigate.
type Repository struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// ValidateRepository rejects mutable or ambiguous source revisions.
func ValidateRepository(repo Repository) error {
	repo.Owner = strings.TrimSpace(repo.Owner)
	repo.Name = strings.TrimSpace(repo.Name)
	repo.Revision = strings.TrimSpace(repo.Revision)
	if repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf("%w: source repository owner and name are required", ErrUnavailable)
	}
	if !fullCommitPattern.MatchString(repo.Revision) {
		return fmt.Errorf("%w: source revision must be a full commit SHA", ErrUnavailable)
	}
	return nil
}
