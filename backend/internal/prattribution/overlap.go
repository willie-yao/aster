package prattribution

import (
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/junit"
	"github.com/willie-yao/aster/backend/internal/models"
)

// Repository identifies the source repository whose files a pull request
// changes. Failure sites outside it never count as overlap.
type Repository struct {
	Owner string
	Name  string
}

// FullRepo returns the repository as "owner/name".
func (r Repository) FullRepo() string {
	if r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Owner + "/" + r.Name
}

// PullChanges is the set of repository-relative files one pull request changes.
type PullChanges struct {
	// Paths holds every changed file, keyed for lookup. Keys are the paths
	// GitHub reported, unmodified, because a git path may legitimately contain
	// leading or trailing spaces.
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
		if path != "" {
			set[path] = true
		}
	}
	return PullChanges{Paths: set, Truncated: truncated}
}

// Known reports whether any changed file was observed.
func (c PullChanges) Known() bool { return len(c.Paths) > 0 }

// failureSitePaths returns the repository-relative source files a failing test
// points at, restricted to repo.
//
// Two sources are combined. The failure body is scanned for every location, so
// a stack that passes through another repository's framework before reaching
// this one is still attributed correctly. The pre-extracted location link is
// also parsed, because it was derived from the untruncated body at ingest time
// and may name a frame the stored body no longer contains.
func failureSitePaths(tc models.TestCase, repo Repository) []string {
	full := repo.FullRepo()
	if full == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(paths []string) {
		for _, path := range paths {
			if path != "" && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	add(junit.RepoFailurePaths(tc.FailureBody, full))
	// A version-qualified location names a tagged dependency copy rather than
	// the checked-out tree, so it is not a site in this pull request's code.
	if url := strings.TrimSpace(tc.FailureLocURL); url != "" && !strings.Contains(tc.FailureLocation, "@") {
		add(buildsource.VerifiedPaths(
			map[string]string{tc.FailureLocation: url},
			buildsource.Source{Owner: repo.Owner, Name: repo.Name},
		))
	}
	sort.Strings(out)
	return out
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
//
// stale reports that the build tested a different head than the changed-file
// list describes. Neither overlap nor its absence is claimed then, because the
// two sides would describe different revisions.
func changedCodeAttribution(base *models.FailureAttribution, tc models.TestCase, repo Repository, changes PullChanges, stale bool) *models.FailureAttribution {
	if stale {
		return nil
	}
	sites := failureSitePaths(tc, repo)
	if len(sites) == 0 || !changes.Known() {
		return nil
	}
	if matched := overlap(sites, changes); len(matched) > 0 {
		return &models.FailureAttribution{
			Verdict:    models.AttributionTouchesChangedCode,
			Confidence: models.AttributionConfidenceMedium,
			Summary:    "This test fails in a file this pull request changes, so review the change first. Overlap is not proof that the change is responsible.",
			Evidence: appendEvidence(base, models.AttributionEvidence{
				Kind:     models.AttributionEvidenceChangedCode,
				Detail:   "The reported failure location is in " + humanList(matched) + ", which this pull request modifies.",
				TestName: tc.Name,
				Paths:    matched,
			}),
		}
	}
	// Absence of overlap is only meaningful when the changed-file list is
	// complete. A truncated list cannot rule the change out.
	if changes.Truncated {
		return nil
	}
	// Overlap says nothing about baseline coverage, so the baseline's own
	// confidence and evidence are carried forward rather than replaced. A test
	// the base branch never ran stays low confidence with its reason intact.
	return &models.FailureAttribution{
		Verdict:    models.AttributionUnexplained,
		Confidence: baseConfidence(base),
		Summary:    "This test fails in a file this pull request does not change, so the change is unlikely to be the reason. It still needs investigation.",
		Evidence: appendEvidence(base, models.AttributionEvidence{
			Kind:     models.AttributionEvidenceUnchangedCode,
			Detail:   "The reported failure location is in " + humanList(sites) + ", which this pull request does not modify.",
			TestName: tc.Name,
			Paths:    sites,
		}),
	}
}

func baseConfidence(base *models.FailureAttribution) string {
	if base == nil || base.Confidence == "" {
		return models.AttributionConfidenceLow
	}
	return base.Confidence
}

// appendEvidence keeps the baseline's evidence ahead of the overlap finding
// without aliasing the baseline's slice.
func appendEvidence(base *models.FailureAttribution, extra models.AttributionEvidence) []models.AttributionEvidence {
	var existing []models.AttributionEvidence
	if base != nil {
		existing = base.Evidence
	}
	out := make([]models.AttributionEvidence, 0, len(existing)+1)
	out = append(out, existing...)
	return append(out, extra)
}
