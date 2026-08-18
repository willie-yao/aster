package prattribution

import (
	"sort"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

// Clusters groups failing cases reported by several open pull requests into one
// object per correlation key, so a failure that is nobody's fault in particular
// has somewhere to be investigated. The grouping is the same one attribution
// consults when it issues a widespread verdict; publishing it is what turns
// that verdict from a dead end into a link.
//
// It must run after Annotate, because a member carries the verdict it received
// and the cluster's escalatability is derived from every member's verdict.
func Clusters(details []models.PullRequestDetail) []models.SharedFailure {
	groups := map[failureKey]*models.SharedFailure{}
	// Escalatability is tracked outside the published shape because it is a
	// property of the members, and a member's own escalatability is not
	// published: the pull request page derives it from data it already has.
	individuallyEscalatable := map[failureKey]bool{}

	for _, detail := range details {
		for _, check := range detail.Checks {
			for _, failure := range check.Failures {
				key := failureKey{
					baseRef: detail.BaseRef, jobName: check.JobName, testName: failure.Name,
				}
				group := groups[key]
				if group == nil {
					group = &models.SharedFailure{
						ID:         models.SharedFailureID(key.baseRef, key.jobName, key.testName),
						BaseRef:    key.baseRef,
						JobName:    key.jobName,
						JobID:      check.JobID,
						TestName:   key.testName,
						BuildLevel: failure.Source == models.TestCaseSourceBuild,
					}
					groups[key] = group
				}
				if escalatableOnPull(check, failure) {
					individuallyEscalatable[key] = true
				}
				addMember(group, detail, check, failure)
			}
		}
	}

	out := make([]models.SharedFailure, 0, len(groups))
	for key, group := range groups {
		if len(group.PullRequests) < models.SharedFailureMinPulls {
			continue
		}
		sort.Slice(group.PullRequests, func(i, j int) bool {
			return group.PullRequests[i].Number < group.PullRequests[j].Number
		})
		group.OldestBuildStarted, group.NewestBuildStarted = buildWindow(group.PullRequests)
		// A shared failure is only the last resort when no member offers the
		// cheaper per-pull-request path to the same analysis.
		group.Escalatable = !individuallyEscalatable[key]
		out = append(out, *group)
	}
	sortClusters(out)
	return out
}

// addMember records one pull request's observation of a failure. A pull request
// is recorded once per cluster, keeping the newest build, so a repeated job name
// cannot make one pull request look like several.
func addMember(group *models.SharedFailure, detail models.PullRequestDetail, check models.PullRequestCheck, failure models.PullRequestFailure) {
	member := models.SharedFailureMember{
		Number:   detail.Number,
		Title:    detail.Title,
		Author:   detail.Author,
		HTMLURL:  detail.HTMLURL,
		BuildID:  check.BuildID,
		Started:  check.Started,
		Finished: check.Finished,
		WebURL:   check.WebURL,
		Stale:    check.Stale,
	}
	if failure.Attribution != nil {
		member.Verdict = failure.Attribution.Verdict
	}
	for i, existing := range group.PullRequests {
		if existing.Number != member.Number {
			continue
		}
		if member.Started.After(existing.Started) {
			group.PullRequests[i] = member
		}
		return
	}
	group.PullRequests = append(group.PullRequests, member)
}

// escalatableOnPull reports whether this failure can already be analyzed from
// its own pull request, which is exactly what the pull request page offers: a
// verdict that leaves room for analysis, on a build that tested the current
// head.
func escalatableOnPull(check models.PullRequestCheck, failure models.PullRequestFailure) bool {
	return !check.Stale && failure.Attribution.NeedsInvestigation()
}

// buildWindow returns the earliest and latest member build start. Zero times
// are ignored so a build with no recorded start cannot report the window as
// beginning at the zero value.
func buildWindow(members []models.SharedFailureMember) (oldest, newest time.Time) {
	for _, member := range members {
		if member.Started.IsZero() {
			continue
		}
		if oldest.IsZero() || member.Started.Before(oldest) {
			oldest = member.Started
		}
		if newest.IsZero() || member.Started.After(newest) {
			newest = member.Started
		}
	}
	return oldest, newest
}

// sortClusters orders the widest failures first, then the most recently
// observed. The remaining comparisons are on the correlation key, so a pass
// that observes nothing new republishes the same order.
func sortClusters(clusters []models.SharedFailure) {
	sort.Slice(clusters, func(i, j int) bool {
		a, b := clusters[i], clusters[j]
		if len(a.PullRequests) != len(b.PullRequests) {
			return len(a.PullRequests) > len(b.PullRequests)
		}
		if !a.NewestBuildStarted.Equal(b.NewestBuildStarted) {
			return a.NewestBuildStarted.After(b.NewestBuildStarted)
		}
		if a.BaseRef != b.BaseRef {
			return a.BaseRef < b.BaseRef
		}
		if a.JobName != b.JobName {
			return a.JobName < b.JobName
		}
		return a.TestName < b.TestName
	})
}
