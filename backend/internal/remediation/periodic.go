package remediation

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	StatusObserving             = "observing"
	StatusVerifiedFixed         = "verified_fixed"
	StatusStillFailingSameCause = "still_failing_same_cause"
	StatusFailingDifferentCause = "failing_different_cause"

	OutcomeTargetRepoNotTested = "target_repo_not_tested"
)

// CompareClient is the GitHub ancestry subset used by periodic verification.
type CompareClient interface {
	CompareCommits(ctx context.Context, owner, repo, base, head string) (bool, string, error)
}

// ObservePeriodic evaluates post-merge runs of the originating periodic job.
func ObservePeriodic(ctx context.Context, client CompareClient, remediation *Remediation, attempt *Attempt, details []models.JobDetail, minCleanRuns int) error {
	if remediation == nil || attempt == nil || attempt.PRState != StatusMerged || attempt.MergeSHA == "" || remediation.JobType != models.JobTypePeriodic {
		return nil
	}
	if minCleanRuns <= 0 {
		minCleanRuns = 2
	}
	if attempt.Status == StatusStillFailingSameCause {
		return nil
	}
	if remediation.SourceRepo == "" || attempt.TargetRepo != remediation.SourceRepo {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeTargetRepoNotTested,
			fmt.Sprintf("pull request targets %s but Prow tests %s", attempt.TargetRepo, remediation.SourceRepo))
		return nil
	}
	if remediation.CommitRepo == "" {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			"Prow did not identify which repository owns the decorated source commit")
		return nil
	}
	if remediation.CommitRepo != remediation.SourceRepo {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			fmt.Sprintf("Prow source commit belongs to %s, not fix target %s", remediation.CommitRepo, remediation.SourceRepo))
		return nil
	}
	owner, repo, ok := strings.Cut(remediation.SourceRepo, "/")
	if !ok || owner == "" || repo == "" {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive, "invalid tested repository")
		return nil
	}
	detail := findJobDetail(remediation, details)
	if detail == nil {
		if attempt.Status == StatusVerifiedFixed {
			return nil
		}
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			"originating Prow job is missing from the current dataset")
		return nil
	}
	currentBuilds := make(map[string]bool, len(detail.Runs))
	for _, run := range detail.Runs {
		currentBuilds[run.BuildID] = true
	}
	for buildID := range attempt.IneligibleCommits {
		if !currentBuilds[buildID] {
			delete(attempt.IneligibleCommits, buildID)
		}
	}
	var errs []error
	missingCommit := false
	for _, run := range detail.Runs {
		if run.Result == "PENDING" || !newerBuild(run.BuildID, remediation.Evidence.BuildWatermark) {
			continue
		}
		if run.Commit == "" {
			missingCommit = true
			continue
		}
		if attempt.IneligibleCommits[run.BuildID] == run.Commit {
			continue
		}
		if hasObservation(attempt, models.JobTypePeriodic, detail.Name, run.BuildID, run.Commit) {
			continue
		}
		contains, status, err := client.CompareCommits(ctx, owner, repo, attempt.MergeSHA, run.Commit)
		if err != nil {
			errs = append(errs, fmt.Errorf("compare merge %s to build %s: %w", attempt.MergeSHA, run.BuildID, err))
			continue
		}
		if !contains {
			if attempt.IneligibleCommits == nil {
				attempt.IneligibleCommits = map[string]string{}
			}
			attempt.IneligibleCommits[run.BuildID] = run.Commit
			continue
		}
		delete(attempt.IneligibleCommits, run.BuildID)
		observation := BuildObservation{
			BuildID: run.BuildID, JobName: detail.Name, JobType: models.JobTypePeriodic,
			SourceRepo: remediation.SourceRepo, SourceCommit: run.Commit,
			Result: run.Result, ProwURL: run.ProwURL,
			StartedAt: run.Started.UTC().Format(timeFormat), CompletedAt: run.Finished.UTC().Format(timeFormat),
			Reason: "merge ancestry " + status,
		}
		if !run.JUnitComplete {
			observation.Outcome = OutcomeInconclusive
			observation.Reason = "periodic JUnit ingestion was incomplete"
			mergeObservations(attempt, []BuildObservation{observation})
			continue
		}
		classifyObservation(remediation.Evidence, run.TestCases, run.Passed, &observation)
		mergeObservations(attempt, []BuildObservation{observation})
		switch observation.Outcome {
		case OutcomeSameCause:
			transitionAttempt(remediation, attempt, StatusStillFailingSameCause, OutcomeSameCause, observation.Reason)
			return errors.Join(errs...)
		}
	}
	var periodicObservations []BuildObservation
	for _, observation := range attempt.Observations {
		if observation.JobType == models.JobTypePeriodic {
			periodicObservations = append(periodicObservations, observation)
		}
	}
	sort.Slice(periodicObservations, func(i, j int) bool {
		return newerBuild(periodicObservations[i].BuildID, periodicObservations[j].BuildID)
	})
	clean := 0
	different := false
	inconclusive := false
	inconclusiveReason := ""
	for _, observation := range periodicObservations {
		switch observation.Outcome {
		case OutcomePassed:
			clean++
			continue
		case OutcomeDifferentCause:
			different = true
		case OutcomeInconclusive:
			inconclusive = true
			inconclusiveReason = observation.Reason
		}
		break
	}
	comparable := len(periodicObservations)
	if comparable == 0 && missingCommit {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			"completed post-merge Prow build has no source commit")
		return errors.Join(errs...)
	}
	if attempt.Status == StatusVerifiedFixed && clean < minCleanRuns && !different {
		return errors.Join(errs...)
	}
	if attempt.Status == StatusStillFailingSameCause {
		return errors.Join(errs...)
	}
	switch {
	case clean >= minCleanRuns:
		transitionAttempt(remediation, attempt, StatusVerifiedFixed, OutcomePassed, fmt.Sprintf("%d clean post-merge runs", clean))
	case clean > 0:
		transitionAttempt(remediation, attempt, StatusObserving, OutcomePending,
			fmt.Sprintf("%d/%d clean post-merge runs after the latest non-passing result", clean, minCleanRuns))
	case different:
		transitionAttempt(remediation, attempt, StatusFailingDifferentCause, OutcomeDifferentCause, "original test failed with a different signature")
	case inconclusive:
		if inconclusiveReason == "" {
			inconclusiveReason = "post-merge Prow result lacked usable test evidence"
		}
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive, inconclusiveReason)
	default:
		transitionAttempt(remediation, attempt, StatusObserving, OutcomePending, fmt.Sprintf("%d/%d clean post-merge runs", clean, minCleanRuns))
	}
	attempt.LastObservedAt = nowString()
	return errors.Join(errs...)
}

func hasObservation(attempt *Attempt, jobType, jobName, buildID, commit string) bool {
	for _, observation := range attempt.Observations {
		if observation.JobType == jobType && observation.JobName == jobName &&
			observation.BuildID == buildID && observation.SourceCommit == commit {
			return observation.Outcome != OutcomeInconclusive
		}
	}
	return false
}

func findJobDetail(remediation *Remediation, details []models.JobDetail) *models.JobDetail {
	for i := range details {
		if remediation.JobID != "" {
			if details[i].JobID == remediation.JobID {
				return &details[i]
			}
			continue
		}
		if details[i].Name == remediation.JobName {
			return &details[i]
		}
	}
	return nil
}

func newerBuild(buildID, watermark string) bool {
	if watermark == "" {
		return true
	}
	build, ok := new(big.Int).SetString(strings.TrimSpace(buildID), 10)
	if !ok {
		return false
	}
	mark, ok := new(big.Int).SetString(strings.TrimSpace(watermark), 10)
	return ok && build.Cmp(mark) > 0
}

func transitionAttempt(remediation *Remediation, attempt *Attempt, status, outcome, reason string) {
	previous := attempt.Status
	attempt.Status, attempt.Outcome, attempt.OutcomeReason = status, outcome, reason
	if previous != status {
		attempt.LastTransition = previous + "->" + status
		attempt.TransitionIndex++
		remediation.LastTransition = attempt.LastTransition
	}
	remediation.UpdatedAt = nowString()
}
