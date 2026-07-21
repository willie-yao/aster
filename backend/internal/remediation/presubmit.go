package remediation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const maxStoredObservations = 30

const (
	StatusAwaitingPresubmit             = "awaiting_presubmit"
	StatusPresubmitRunning              = "presubmit_running"
	StatusPremergeVerified              = "premerge_verified"
	StatusPresubmitFailedSameCause      = "presubmit_failed_same_cause"
	StatusPresubmitFailedDifferentCause = "presubmit_failed_different_cause"

	OutcomePassed         = "passed"
	OutcomeSameCause      = "same_cause"
	OutcomeDifferentCause = "different_cause"
	OutcomeInconclusive   = "inconclusive"
	OutcomePending        = "pending"
)

// ObservePresubmits evaluates verification jobs for the current pull head.
func ObservePresubmits(ctx context.Context, b storage.Backend, remediation *Remediation, attempt *Attempt, coverage *CoverageCatalog) error {
	if remediation == nil || attempt == nil || attempt.PRNumber <= 0 || attempt.HeadSHA == "" {
		return nil
	}
	jobs := verificationJobs(remediation, coverage)
	if len(jobs) == 0 {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			"no applicable Prow presubmit could be discovered")
		return nil
	}
	matching := jobs[:0]
	for _, job := range jobs {
		if job.Repo == attempt.TargetRepo {
			matching = append(matching, job)
		}
	}
	if len(matching) == 0 {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeTargetRepoNotTested,
			fmt.Sprintf("pull request targets %s but verification jobs test another repository", attempt.TargetRepo))
		return nil
	}
	jobs = matching
	applicable := jobs[:0]
	for _, job := range jobs {
		matches, err := verificationJobAppliesToBranch(job, attempt.BaseRef)
		if err != nil {
			transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive, err.Error())
			return nil
		}
		if matches {
			applicable = append(applicable, job)
		}
	}
	if len(applicable) == 0 {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			fmt.Sprintf("no discovered Prow presubmit applies to base branch %s", attempt.BaseRef))
		return nil
	}
	jobs = applicable
	var observations []BuildObservation
	for _, job := range jobs {
		jobEvidence := evidenceForVerificationJob(remediation.Evidence, coverage, job)
		builds, err := prowbuild.ListPullBuilds(ctx, b, job.Repo, strconv.Itoa(attempt.PRNumber), job.JobName, 5)
		if err != nil {
			return fmt.Errorf("list pull builds for %s: %w", job.JobName, err)
		}
		var missingRevision *BuildObservation
		observed := false
		for _, build := range builds {
			loc := prowbuild.BuildLocation{
				JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: job.Repo},
				JobName:     job.JobName, BuildID: build.ID, PullNumber: build.PullNumber,
			}
			metadata, metadataErr := prowbuild.FetchProwJobMetadata(ctx, b, loc)
			if metadataErr != nil && !errors.Is(metadataErr, prowbuild.ErrProwJobNotFound) {
				observations = append(observations, BuildObservation{
					BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
					PullNumber: attempt.PRNumber, SourceRepo: job.Repo, HeadSHA: attempt.HeadSHA,
					Outcome: OutcomeInconclusive, Reason: metadataErr.Error(),
				})
				observed = true
				break
			}
			if metadataErr == nil && metadata.Refs != nil && len(metadata.Refs.Pulls) > 0 {
				headSHA := metadata.Refs.Pulls[0].SHA
				if headSHA != attempt.HeadSHA {
					continue
				}
				observation := BuildObservation{
					BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
					PullNumber: attempt.PRNumber, SourceRepo: job.Repo, HeadSHA: headSHA,
					Result: strings.ToUpper(metadata.State), ProwURL: metadata.URL,
				}
				if !metadata.StartTime.IsZero() {
					observation.StartedAt = metadata.StartTime.UTC().Format(timeFormat)
				}
				if !metadata.Completion.IsZero() {
					observation.CompletedAt = metadata.Completion.UTC().Format(timeFormat)
				}
				if metadata.State == "pending" || metadata.State == "triggered" {
					observation.Outcome = OutcomePending
					observations = append(observations, observation)
					observed = true
					break
				}
				info, cases, err := fetchBuildTests(ctx, b, loc)
				if err != nil {
					observation.Outcome, observation.Reason = OutcomeInconclusive, err.Error()
					observations = append(observations, observation)
					observed = true
					break
				}
				observation.Result = info.Result
				classifyObservation(jobEvidence, cases, info.Passed, &observation)
				observations = append(observations, observation)
				observed = true
				break
			}

			info, cases, err := fetchBuildTests(ctx, b, loc)
			if info == nil {
				continue
			}
			if info.Revision == "" {
				if info.Result != "PENDING" && missingRevision == nil {
					missingRevision = &BuildObservation{
						BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
						PullNumber: attempt.PRNumber, SourceRepo: job.Repo, Result: info.Result,
						ProwURL: info.ProwURL, Outcome: OutcomeInconclusive,
						Reason: "finished.json is missing revision metadata",
					}
				}
				continue
			}
			if info.Revision != attempt.HeadSHA {
				continue
			}
			observation := BuildObservation{
				BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
				PullNumber: attempt.PRNumber, SourceRepo: job.Repo, HeadSHA: info.Revision,
				Result: info.Result, ProwURL: info.ProwURL,
			}
			if err != nil {
				observation.Outcome, observation.Reason = OutcomeInconclusive, err.Error()
			} else {
				classifyObservation(jobEvidence, cases, info.Passed, &observation)
			}
			observations = append(observations, observation)
			observed = true
			break
		}
		if !observed && missingRevision != nil {
			observations = append(observations, *missingRevision)
		}
	}
	mergeObservations(attempt, observations)
	coverageComplete := remediation.JobType == models.JobTypePresubmit || verificationCoverageComplete(remediation.Evidence, jobs, coverage)
	applyPresubmitOutcome(remediation, attempt, jobs, currentPresubmitObservations(attempt, jobs), coverageComplete)
	return nil
}

func verificationJobs(remediation *Remediation, coverage *CoverageCatalog) []VerificationJob {
	byID := map[string]VerificationJob{}
	if remediation.JobType == models.JobTypePresubmit && remediation.SourceRepo != "" && remediation.JobName != "" {
		job := VerificationJob{
			JobID:   models.JobIDFor(models.JobTypePresubmit, remediation.SourceRepo, remediation.JobName),
			JobName: remediation.JobName, Repo: remediation.SourceRepo,
			RerunCommand: "/test " + remediation.JobName,
		}
		if coverage != nil {
			if definition, ok := coverage.Jobs[job.JobID]; ok {
				job = definition
			} else {
				for _, evidence := range remediation.Evidence.Tests {
					for _, candidate := range coverage.Tests[evidence.Identity] {
						if candidate.JobID == job.JobID {
							job = candidate
							break
						}
					}
				}
			}
		}
		byID[job.JobID] = job
	}
	if remediation.JobType != models.JobTypePresubmit && coverage != nil {
		for _, evidence := range remediation.Evidence.Tests {
			for _, job := range coverage.Tests[evidence.Identity] {
				byID[job.JobID] = job
			}
		}
	}
	out := make([]VerificationJob, 0, len(byID))
	for _, job := range byID {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

func verificationJobAppliesToBranch(job VerificationJob, branch string) (bool, error) {
	for _, pattern := range job.SkipBranches {
		matches, err := regexp.MatchString(pattern, branch)
		if err != nil {
			return false, fmt.Errorf("invalid skip_branches selector for %s: %w", job.JobName, err)
		}
		if matches {
			return false, nil
		}
	}
	if len(job.Branches) == 0 {
		return true, nil
	}
	for _, pattern := range job.Branches {
		matches, err := regexp.MatchString(pattern, branch)
		if err != nil {
			return false, fmt.Errorf("invalid branches selector for %s: %w", job.JobName, err)
		}
		if matches {
			return true, nil
		}
	}
	return false, nil
}

func verificationCoverageComplete(evidence Evidence, jobs []VerificationJob, coverage *CoverageCatalog) bool {
	if coverage == nil || len(evidence.Tests) == 0 {
		return false
	}
	selected := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		selected[job.JobID] = true
	}
	for _, test := range evidence.Tests {
		covered := false
		for _, job := range coverage.Tests[test.Identity] {
			if selected[job.JobID] {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func evidenceForVerificationJob(evidence Evidence, coverage *CoverageCatalog, job VerificationJob) Evidence {
	if coverage == nil {
		return evidence
	}
	out := evidence
	out.Tests = nil
	for _, test := range evidence.Tests {
		for _, candidate := range coverage.Tests[test.Identity] {
			if candidate.JobID == job.JobID {
				out.Tests = append(out.Tests, test)
				break
			}
		}
	}
	if len(out.Tests) == 0 {
		return evidence
	}
	return out
}

func fetchBuildTests(ctx context.Context, b storage.Backend, loc prowbuild.BuildLocation) (*models.BuildInfo, []models.TestCase, error) {
	info, err := prowbuild.FetchBuildInfo(ctx, b, loc)
	if err != nil {
		return nil, nil, err
	}
	paths, complete, err := prowbuild.DiscoverJUnitPathsWithCompleteness(ctx, b, loc)
	if err != nil {
		return info, nil, err
	}
	var cases []models.TestCase
	for _, path := range paths {
		data, err := storage.ReadAll(ctx, b, path)
		if err != nil {
			return info, nil, err
		}
		parsed, err := junit.ParseFile(data, filepath.Base(path))
		if err != nil {
			return info, nil, err
		}
		cases = append(cases, parsed...)
	}
	if len(cases) == 0 {
		return info, nil, fmt.Errorf("no JUnit tests found")
	}
	if !complete {
		return info, cases, fmt.Errorf("JUnit artifact listing was incomplete")
	}
	return info, cases, nil
}

func classifyObservation(evidence Evidence, cases []models.TestCase, jobPassed bool, observation *BuildObservation) {
	if len(evidence.Tests) == 0 {
		observation.Outcome, observation.Reason = OutcomeInconclusive, "original finding has no test-level evidence"
		return
	}
	byIdentity := make(map[string][]models.TestCase, len(cases))
	for _, test := range cases {
		for _, identity := range junit.Identities(test) {
			byIdentity[identity] = append(byIdentity[identity], test)
		}
	}
	missing := 0
	differentFailure := false
	for _, expected := range evidence.Tests {
		matched := byIdentity[expected.Identity]
		if len(matched) == 0 {
			missing++
			continue
		}
		executed := false
		for _, test := range matched {
			if test.Status == "skipped" {
				continue
			}
			executed = true
			if test.Status != "failed" {
				continue
			}
			hash := aggregator.HashError(aggregator.NormalizeErrorMessage(test.FailureMessage + "\n" + test.FailureBody))
			if expected.ErrorHash != "" && hash == expected.ErrorHash {
				observation.FailedMatches = append(observation.FailedMatches, expected.Identity)
				observation.Outcome, observation.Reason = OutcomeSameCause, "original failure signature recurred"
				return
			}
			differentFailure = true
		}
		if !executed {
			missing++
			continue
		}
		observation.MatchedTests = append(observation.MatchedTests, expected.Identity)
	}
	sort.Strings(observation.MatchedTests)
	if missing > 0 {
		observation.Outcome = OutcomeInconclusive
		observation.Reason = fmt.Sprintf("%d expected test(s) did not execute", missing)
		return
	}
	if differentFailure {
		observation.Outcome, observation.Reason = OutcomeDifferentCause, "expected test failed with a different signature"
		return
	}
	if !jobPassed {
		observation.Outcome, observation.Reason = OutcomeDifferentCause, "target tests passed but the Prow job failed elsewhere"
		return
	}
	observation.Outcome = OutcomePassed
}

func mergeObservations(attempt *Attempt, observations []BuildObservation) {
	for _, observation := range observations {
		found := false
		for i := range attempt.Observations {
			if attempt.Observations[i].BuildID == observation.BuildID && attempt.Observations[i].JobName == observation.JobName {
				attempt.Observations[i] = observation
				found = true
				break
			}
		}
		if !found {
			attempt.Observations = append(attempt.Observations, observation)
		}
	}
	sort.SliceStable(attempt.Observations, func(i, j int) bool {
		left, right := attempt.Observations[i].BuildID, attempt.Observations[j].BuildID
		if newerBuild(left, right) {
			return false
		}
		if newerBuild(right, left) {
			return true
		}
		if left == right {
			return attempt.Observations[i].JobName < attempt.Observations[j].JobName
		}
		return left < right
	})
	if len(attempt.Observations) > maxStoredObservations {
		attempt.Observations = append([]BuildObservation(nil), attempt.Observations[len(attempt.Observations)-maxStoredObservations:]...)
	}
}

func everyEvidenceTestPassed(evidence Evidence, observations []BuildObservation) bool {
	if len(evidence.Tests) == 0 {
		return false
	}
	passed := map[string]bool{}
	for _, observation := range observations {
		if observation.Outcome != OutcomePassed {
			continue
		}
		for _, identity := range observation.MatchedTests {
			passed[identity] = true
		}
	}
	for _, test := range evidence.Tests {
		if !passed[test.Identity] {
			return false
		}
	}
	return true
}

func currentPresubmitObservations(attempt *Attempt, jobs []VerificationJob) []BuildObservation {
	selected := map[string]bool{}
	for _, job := range jobs {
		selected[job.JobName] = true
	}
	latest := map[string]BuildObservation{}
	for _, observation := range attempt.Observations {
		if observation.JobType != models.JobTypePresubmit || !selected[observation.JobName] {
			continue
		}
		if observation.HeadSHA != attempt.HeadSHA && (observation.HeadSHA != "" || observation.Outcome != OutcomeInconclusive) {
			continue
		}
		current, exists := latest[observation.JobName]
		if !exists || newerBuild(observation.BuildID, current.BuildID) {
			latest[observation.JobName] = observation
		}
	}
	out := make([]BuildObservation, 0, len(latest))
	for _, observation := range latest {
		out = append(out, observation)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobName < out[j].JobName })
	return out
}

func applyPresubmitOutcome(remediation *Remediation, attempt *Attempt, jobs []VerificationJob, observations []BuildObservation, coverageComplete bool) {
	previous := attempt.Status
	attempt.Outcome, attempt.OutcomeReason = "", ""
	if len(observations) == 0 {
		if !coverageComplete {
			attempt.Status, attempt.Outcome = StatusInconclusive, OutcomeInconclusive
			attempt.OutcomeReason = "applicable Prow presubmits do not cover every affected test"
		} else {
			attempt.Status = StatusAwaitingPresubmit
			attempt.OutcomeReason = "waiting for Prow presubmit"
			for _, job := range jobs {
				if job.RerunCommand != "" {
					attempt.OutcomeReason = "waiting for " + job.RerunCommand
					break
				}
			}
		}
	} else {
		passed := false
		pending := false
		inconclusive := false
		for _, observation := range observations {
			switch observation.Outcome {
			case OutcomeSameCause:
				attempt.Outcome, attempt.OutcomeReason = OutcomeSameCause, observation.Reason
			case OutcomeDifferentCause:
				if attempt.Outcome != OutcomeSameCause {
					attempt.Outcome, attempt.OutcomeReason = OutcomeDifferentCause, observation.Reason
				}
			case OutcomePending:
				pending = true
			case OutcomePassed:
				passed = true
			case OutcomeInconclusive:
				inconclusive = true
				if attempt.Outcome == "" {
					attempt.Outcome, attempt.OutcomeReason = OutcomeInconclusive, observation.Reason
				}
			default:
				inconclusive = true
			}
		}
		allEvidencePassed := everyEvidenceTestPassed(remediation.Evidence, observations)
		if attempt.Outcome == OutcomeSameCause {
			attempt.Status = StatusPresubmitFailedSameCause
		} else if attempt.Outcome == OutcomeDifferentCause {
			attempt.Status = StatusPresubmitFailedDifferentCause
		} else if inconclusive {
			attempt.Status, attempt.Outcome = StatusInconclusive, OutcomeInconclusive
			if attempt.OutcomeReason == "" {
				attempt.OutcomeReason = "completed Prow result lacked usable test evidence"
			}
		} else if pending {
			attempt.Status, attempt.Outcome, attempt.OutcomeReason = StatusPresubmitRunning, OutcomePending, "matching Prow presubmit is still running"
		} else if !coverageComplete {
			attempt.Status, attempt.Outcome = StatusInconclusive, OutcomeInconclusive
			attempt.OutcomeReason = "applicable Prow presubmits do not cover every affected test"
		} else if allEvidencePassed {
			attempt.Status, attempt.Outcome, attempt.OutcomeReason = StatusPremergeVerified, OutcomePassed, "matching Prow presubmit passed"
		} else if attempt.Status != StatusPresubmitRunning {
			attempt.Status = StatusAwaitingPresubmit
			if passed {
				attempt.OutcomeReason = "waiting for presubmits covering every affected test"
			} else {
				attempt.OutcomeReason = "waiting for a matching Prow presubmit"
			}
		}
	}
	if previous != attempt.Status {
		attempt.LastTransition = previous + "->" + attempt.Status
		attempt.TransitionIndex++
		remediation.LastTransition = attempt.LastTransition
	}
	attempt.LastObservedAt = nowString()
	remediation.UpdatedAt = nowString()
}
