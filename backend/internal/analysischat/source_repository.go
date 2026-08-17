package analysischat

import (
	"fmt"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// ConfigureSourceRepository binds chat sessions to one immutable build source.
func (s *Service) ConfigureSourceRepository(repo sourceinvestigation.Repository) error {
	repo.Owner = strings.TrimSpace(repo.Owner)
	repo.Name = strings.TrimSpace(repo.Name)
	repo.Revision = ""
	if repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf("analysis chat source repository owner and name are required")
	}
	s.sourceRepo = repo
	return nil
}

func sourceRepositoryName(repo sourceinvestigation.Repository) string {
	return strings.ToLower(strings.TrimSpace(repo.Owner) + "/" + strings.TrimSpace(repo.Name))
}

func extendSessionExpiry(current *persistedSession, expires time.Time) {
	if current.ExpiresAt.After(expires) {
		expires = current.ExpiresAt
	}
	current.ExpiresAt = expires
	current.View.ExpiresAt = expires.Format(time.RFC3339)
}
