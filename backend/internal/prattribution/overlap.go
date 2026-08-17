package prattribution

import (
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
)

// Repository identifies the source repository whose files a pull request
// changes. Failure sites outside it never count as overlap.
type Repository struct {
	Owner string
	Name  string
}

// PullChanges is the set of repository-relative files one pull request changes.
type PullChanges struct {
	// Paths holds every changed file, keyed for lookup.
	Paths map[string]bool
	// Truncated reports that the changed-file list is incomplete. Absence of
	// overlap cannot be claimed when it is set, because an unobserved file may
	// be the one that matters.
	Truncated bool
}

// NewPullChanges builds a lookup set from a changed-file listing.
func NewPullChanges(paths []string, truncated bool) PullChanges {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		if cleaned := strings.TrimSpace(path); cleaned != "" {
			set[cleaned] = true
		}
	}
	return PullChanges{Paths: set, Truncated: truncated}
}

// Known reports whether any changed file was observed.
func (c PullChanges) Known() bool { return len(c.Paths) > 0 }

// failureSitePaths returns the repository-relative source files a failing test
// points at. It reuses the verified-link parser, which rejects links outside
// the configured repository, so a failure inside a dependency such as
// cluster-api never matches a cluster-api-provider-azure pull request.
func failureSitePaths(tc models.TestCase, repo Repository) []string {
	url := strings.TrimSpace(tc.FailureLocURL)
	if url == "" || repo.Owner == "" || repo.Name == "" {
		return nil
	}
	// An empty revision accepts the link's own ref, which is the base branch or
	// a module version rather than the pull request head.
	return buildsource.VerifiedPaths(
		map[string]string{tc.FailureLocation: url},
		buildsource.Source{Owner: repo.Owner, Name: repo.Name},
	)
}

// overlap returns the changed files that the failure site points at.
func overlap(sites []string, changes PullChanges) []string {
	var matched []string
	for _, site := range sites {
		if changes.Paths[site] {
			matched = append(matched, site)
		}
	}
	sort.Strings(matched)
	return matched
}

// changedCodeAttribution refines an unexplained failure with observed overlap
// between the failure site and the pull request's changes. It returns nil when
// there is nothing to add, leaving the baseline verdict unchanged.
func changedCodeAttribution(tc models.TestCase, repo Repository, changes PullChanges) *models.FailureAttribution {
	sites := failureSitePaths(tc, repo)
	if len(sites) == 0 || !changes.Known() {
		return nil
	}
	if matched := overlap(sites, changes); len(matched) > 0 {
		return &models.FailureAttribution{
			Verdict:    models.AttributionTouchesChangedCode,
			Confidence: models.AttributionConfidenceMedium,
			Summary:    "This test fails in a file this pull request changes, so review the change first. Overlap is not proof that the change is responsible.",
			Evidence: []models.AttributionEvidence{{
				Kind:     models.AttributionEvidenceChangedCode,
				Detail:   "The reported failure location is in " + humanList(matched) + ", which this pull request modifies.",
				TestName: tc.Name,
				Paths:    matched,
			}},
		}
	}
	// Absence of overlap is only meaningful when the changed-file list is
	// complete. A truncated list cannot rule the change out.
	if changes.Truncated {
		return nil
	}
	return &models.FailureAttribution{
		Verdict:    models.AttributionUnexplained,
		Confidence: models.AttributionConfidenceHigh,
		Summary:    "This test fails in a file this pull request does not change, and no baseline explains it, so it still needs investigation.",
		Evidence: []models.AttributionEvidence{{
			Kind:     models.AttributionEvidenceUnchangedCode,
			Detail:   "The reported failure location is in " + humanList(sites) + ", which this pull request does not modify.",
			TestName: tc.Name,
			Paths:    sites,
		}},
	}
}
