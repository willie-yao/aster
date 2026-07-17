// Package prowbuild addresses Prow build artifacts over a storage.Backend.
// It encodes Prow storage-layout conventions and is independent of the storage
// provider backing the bucket.
package prowbuild

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// JobLocation identifies which Prow-convention path a job lives under. For
// periodics, only JobType matters. For presubmits, Repo is required to
// construct the pr-logs prefix; the pull number lives on BuildLocation.
type JobLocation struct {
	JobType string // models.JobTypePeriodic or models.JobTypePresubmit
	Repo    string // org/repo for presubmits; ignored for periodics
}

// BuildLocation extends JobLocation with the per-build identifiers needed to
// address a specific build's artifacts.
type BuildLocation struct {
	JobLocation
	JobName    string
	BuildID    string
	PullNumber string // required for presubmits; ignored for periodics
}

// Build is a single Prow build's identity. PullNumber is the pull request
// number for presubmits and empty for periodics.
type Build struct {
	ID         string
	PullNumber string
}

// jobTypePrefix returns the bucket-relative prefix above the job name, with a
// trailing slash. Examples:
//
//	periodic:  "logs/"
//	presubmit: "pr-logs/pull/<org_repo>/<pull>/"
//
// Panics if JobType is presubmit and Repo or pullNumber is empty; these are
// programming errors at construction time, not runtime user input.
func (l JobLocation) jobTypePrefix(pullNumber string) string {
	switch l.JobType {
	case models.JobTypePeriodic:
		return "logs/"
	case models.JobTypePresubmit:
		if l.Repo == "" {
			panic("prowbuild: presubmit JobLocation requires Repo (org/repo)")
		}
		if pullNumber == "" {
			panic("prowbuild: presubmit JobLocation requires pull number")
		}
		repo := strings.ReplaceAll(l.Repo, "/", "_")
		return "pr-logs/pull/" + repo + "/" + pullNumber + "/"
	default:
		panic(fmt.Sprintf("prowbuild: unknown JobType %q", l.JobType))
	}
}

// jobPath returns the bucket-relative path down to a job name, trailing slash
// included.
func (l JobLocation) jobPath(jobName, pullNumber string) string {
	return l.jobTypePrefix(pullNumber) + jobName + "/"
}

// BuildPath returns the bucket-relative path down to a specific build, trailing
// slash included.
func (l BuildLocation) BuildPath() string {
	return l.jobPath(l.JobName, l.PullNumber) + l.BuildID + "/"
}

// JobIndexPrefix returns the bucket-relative prefix for listing builds without
// a known pull number. Periodics list logs/<job>/. Presubmits list
// pr-logs/directory/<job>/, whose .txt entries point to actual build paths.
func (l JobLocation) JobIndexPrefix(jobName string) string {
	switch l.JobType {
	case models.JobTypePeriodic:
		return "logs/" + jobName + "/"
	case models.JobTypePresubmit:
		return "pr-logs/directory/" + jobName + "/"
	default:
		panic(fmt.Sprintf("prowbuild: unknown JobType %q", l.JobType))
	}
}
