// Package prattribution decides whether a pull request's test failure is
// specific to that pull request, using only observed results: the base
// branch's own runs, the same failure on other open pull requests, and the
// test's recorded flakiness history.
//
// No verdict asserts that a pull request caused a failure. Deterministic
// evidence can rule a pull request out, but it cannot rule one in, so the
// strongest judgment here is that nothing in the baseline explains the failure.
package prattribution

import (
	"fmt"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

// widespreadHighConfidencePulls is how many other pull requests must show the
// same failure before the verdict is reported with high confidence.
const widespreadHighConfidencePulls = 3

// widespreadVerdictMinPulls is how many other pull requests must show the same
// failure before it becomes a verdict at all. Two pull requests citing each
// other is mutual and uncorroborated, and evidence that weak must not preempt
// base-branch evidence or take the failure out of escalation.
const widespreadVerdictMinPulls = 2

// Baseline is the observed non-pull-request evidence for one pass.
type Baseline struct {
	// FailingOnBase maps test name to the base-branch jobs currently reporting
	// it as failed.
	FailingOnBase map[string][]string
	// FlakyTests maps test name to the base-branch jobs whose history classifies
	// it flaky.
	FlakyTests map[string][]string
	// KnownTests holds every test name observed on the base branch, which
	// separates "passes on the base branch" from "never runs there".
	KnownTests map[string]bool
	// Observed reports whether any base-branch data was available. When false
	// every verdict is inconclusive rather than falsely pull-request-specific.
	Observed bool
}

// BuildBaseline derives base-branch evidence from the periodic job details and
// flakiness report the dashboard pass already produced. Presubmit jobs are
// excluded from every field so a verdict does not depend on whether the
// dashboard publishes presubmits. Callers should supply a flakiness report
// computed over base-branch jobs only, because a report ranked across every
// published job can drop a base-branch flake before it reaches this filter.
func BuildBaseline(details []models.JobDetail, flakiness models.FlakinessReport) Baseline {
	baseline := Baseline{
		FailingOnBase: map[string][]string{},
		FlakyTests:    map[string][]string{},
		KnownTests:    map[string]bool{},
	}
	// Flakiness entries carry no job type, so presubmit job IDs are collected
	// here to filter the flakiness report below. Matching on the ID rather than
	// the name keeps a periodic and a presubmit that share a name distinct.
	presubmitJobs := map[string]bool{}
	for _, detail := range details {
		// Presubmit job details describe other pull requests, not the base branch.
		if detail.JobType == models.JobTypePresubmit {
			presubmitJobs[detail.JobID] = true
			continue
		}
		if len(detail.Runs) == 0 {
			continue
		}
		baseline.Observed = true
		newest := newestRun(detail.Runs)
		for _, tc := range newest.TestCases {
			if tc.Source == models.TestCaseSourceBuild {
				continue
			}
			baseline.KnownTests[tc.Name] = true
			if tc.Status == "failed" {
				baseline.FailingOnBase[tc.Name] = appendUnique(baseline.FailingOnBase[tc.Name], detail.Name)
			}
		}
	}
	for _, group := range [][]models.TestFlakiness{
		flakiness.MostFlaky, flakiness.PersistentFailures, flakiness.RecentlyBroken,
	} {
		for _, entry := range group {
			if entry.Classification != models.ClassificationFlaky {
				continue
			}
			// Presubmit flakiness is measured across other pull requests, so it
			// is not base-branch history. Peer failures have their own verdict.
			if presubmitJobs[entry.JobID] {
				continue
			}
			baseline.FlakyTests[entry.TestName] = appendUnique(baseline.FlakyTests[entry.TestName], entry.JobName)
		}
	}
	return baseline
}

// newestRun returns the most recently started run.
func newestRun(runs []models.BuildResult) models.BuildResult {
	newest := runs[0]
	for _, run := range runs[1:] {
		if run.Started.After(newest.Started) {
			newest = run
		}
	}
	return newest
}

// failureKey identifies one failing case across pull requests. Job name is part
// of the key because a build-level failure carries the same generic test name on
// every job, so matching on test name alone would correlate unrelated jobs. Base
// ref is part of it because one job often runs on several release branches, and
// matching without it correlates pull requests testing different code.
type failureKey struct {
	baseRef  string
	jobName  string
	testName string
}

// Annotate attaches a deterministic attribution to every failing case in
// details, in place. Overlap between a failure site and a pull request's
// changed files refines an unexplained verdict; changes may be nil.
func Annotate(details []models.PullRequestDetail, baseline Baseline, repo Repository, changes map[int]PullChanges) {
	otherPulls := pullsByFailure(details)
	for i := range details {
		pullChanges := changes[details[i].Number]
		for c := range details[i].Checks {
			check := &details[i].Checks[c]
			for f := range check.Failures {
				failure := &check.Failures[f]
				key := failureKey{
					baseRef: details[i].BaseRef, jobName: check.JobName, testName: failure.Name,
				}
				attribution := attribute(details[i].Number, key, failure.TestCase, baseline, otherPulls)
				// Only the residual set benefits from overlap. A failure already
				// explained by the base branch or by other pull requests is not
				// made more explicable by touching changed code.
				if attribution.Verdict == models.AttributionUnexplained {
					if refined := changedCodeAttribution(attribution, failure.TestCase, repo, pullChanges, check.Stale); refined != nil {
						attribution = refined
					}
				}
				failure.Attribution = attribution
			}
		}
	}
}

// pullsByFailure indexes which pull requests report each failing job and test.
func pullsByFailure(details []models.PullRequestDetail) map[failureKey][]int {
	index := map[failureKey][]int{}
	for _, detail := range details {
		for _, check := range detail.Checks {
			for _, failure := range check.Failures {
				key := failureKey{
					baseRef: detail.BaseRef, jobName: check.JobName, testName: failure.Name,
				}
				index[key] = appendUniqueInt(index[key], detail.Number)
			}
		}
	}
	for key := range index {
		sort.Ints(index[key])
	}
	return index
}

// attribute issues the verdict for one failing case. Evidence that rules the
// pull request out is checked strongest first. Peers too few to carry a verdict
// of their own are recorded on the verdict that does apply, except under
// pre_existing, which returns before peers are consulted because a failure the
// base branch already explains does not need them.
func attribute(number int, key failureKey, tc models.TestCase, baseline Baseline, otherPulls map[failureKey][]int) *models.FailureAttribution {
	buildLevel := tc.Source == models.TestCaseSourceBuild

	// The base branch is the strongest signal: the failure exists without this
	// pull request. Build-level failures are skipped because their generic name
	// matches every failed job.
	if !buildLevel {
		if jobs := baseline.FailingOnBase[tc.Name]; len(jobs) > 0 {
			return &models.FailureAttribution{
				Verdict:    models.AttributionPreExisting,
				Confidence: models.AttributionConfidenceHigh,
				Summary:    fmt.Sprintf("This test is already failing on the base branch in %s, so this pull request did not introduce it.", humanList(jobs)),
				Evidence: []models.AttributionEvidence{{
					Kind:     models.AttributionEvidenceBaseBranch,
					Detail:   fmt.Sprintf("The newest base-branch run of %s reports this test as failed.", humanList(jobs)),
					TestName: tc.Name,
				}},
			}
		}
	}

	others := peers(otherPulls[key], number)
	if len(others) >= widespreadVerdictMinPulls {
		return &models.FailureAttribution{
			Verdict:    models.AttributionWidespread,
			Confidence: widespreadConfidence(len(others)),
			Summary:    fmt.Sprintf("%s is failing the same way on %s as of this pass, so it is not specific to this pull request.", subject(buildLevel, key.jobName), pullList(others)),
			Evidence:   []models.AttributionEvidence{peerEvidence(key, others)},
		}
	}

	attribution := baselineVerdict(key, tc, buildLevel, baseline, others)
	// Peers too few to carry a verdict are still an observation worth weighing,
	// so they are reported rather than dropped.
	if len(others) > 0 {
		attribution.Evidence = append(attribution.Evidence, peerEvidence(key, others))
	}
	return attribution
}

// baselineVerdict judges a failure from base-branch evidence alone, once peers
// have been ruled out as a verdict of their own. others is read only to keep the
// prose honest, because two of these summaries would otherwise deny peers that
// do exist.
func baselineVerdict(key failureKey, tc models.TestCase, buildLevel bool, baseline Baseline, others []int) *models.FailureAttribution {
	if !buildLevel {
		if jobs := baseline.FlakyTests[tc.Name]; len(jobs) > 0 {
			return &models.FailureAttribution{
				Verdict:    models.AttributionKnownFlake,
				Confidence: models.AttributionConfidenceMedium,
				Summary:    fmt.Sprintf("This test is already tracked as flaky in %s, so the failure may not reflect this pull request.", humanList(jobs)),
				Evidence: []models.AttributionEvidence{{
					Kind:     models.AttributionEvidenceFlakiness,
					Detail:   fmt.Sprintf("Flakiness history classifies this test as flaky in %s.", humanList(jobs)),
					TestName: tc.Name,
				}},
			}
		}
	}

	// Nothing observed rules the pull request out. Report how complete the
	// baseline was so the reader can weigh the result.
	if !baseline.Observed {
		return &models.FailureAttribution{
			Verdict:    models.AttributionInconclusive,
			Confidence: models.AttributionConfidenceLow,
			Summary:    "No base-branch results were available in this pass, so this failure could not be compared against one.",
			Evidence: []models.AttributionEvidence{{
				Kind:   models.AttributionEvidenceNoBaseline,
				Detail: "This pass published no base-branch job runs to compare against.",
			}},
		}
	}
	if buildLevel {
		return &models.FailureAttribution{
			Verdict:    models.AttributionUnexplained,
			Confidence: models.AttributionConfidenceLow,
			Summary:    buildLevelSummary(key.jobName, key.baseRef, others),
			Evidence: []models.AttributionEvidence{{
				Kind:   models.AttributionEvidenceBuildFailer,
				Detail: "A build-level failure carries no test identity, so it cannot be compared against the base branch.",
			}},
		}
	}
	if baseline.KnownTests[tc.Name] {
		return &models.FailureAttribution{
			Verdict:    models.AttributionUnexplained,
			Confidence: models.AttributionConfidenceHigh,
			Summary:    basePassingSummary(key.baseRef, others),
			Evidence: []models.AttributionEvidence{{
				Kind:     models.AttributionEvidenceBaseBranch,
				Detail:   "The newest base-branch run reports this test as passing.",
				TestName: tc.Name,
			}},
		}
	}
	return &models.FailureAttribution{
		Verdict:    models.AttributionUnexplained,
		Confidence: models.AttributionConfidenceLow,
		Summary:    "This test does not run on the base branch, so there is no baseline to compare against.",
		Evidence: []models.AttributionEvidence{{
			Kind:     models.AttributionEvidenceNoBaseline,
			Detail:   "No base-branch job observed this test in the current window.",
			TestName: tc.Name,
		}},
	}
}

// basePassingSummary states the residual verdict for a test the base branch runs
// and passes. Peers below the widespread threshold are named rather than denied,
// and the no-peer wording is scoped to the branch the comparison covered.
func basePassingSummary(baseRef string, others []int) string {
	if len(others) == 0 {
		return fmt.Sprintf("This test passes on the base branch and is not failing on other open pull requests%s, so it needs investigation on this pull request.", baseRefScope(baseRef))
	}
	return fmt.Sprintf("This test passes on the base branch. It is also failing on %s, which is not enough to rule this pull request out, so it needs investigation.", pullList(others))
}

// buildLevelSummary states the residual verdict for a job that failed without
// reporting a test. The peer wording drops the build-log claim, because a peer
// is recorded as evidence alongside it.
func buildLevelSummary(jobName, baseRef string, others []int) string {
	if len(others) == 0 {
		return fmt.Sprintf("%s failed without reporting a failing test, and no other open pull request%s hit it. The build log is the only evidence.", jobName, baseRefScope(baseRef))
	}
	return fmt.Sprintf("%s failed without reporting a failing test, and it also failed on %s, which is not enough to rule this pull request out.", jobName, pullList(others))
}

// baseRefScope qualifies a claim about other pull requests with the base branch
// the comparison was bounded to. It is empty when the branch is unknown, which
// is the only case where every open pull request was compared.
func baseRefScope(baseRef string) string {
	if baseRef == "" {
		return ""
	}
	return " targeting " + baseRef
}

// peerEvidence records the other pull requests observed failing the same way.
// The base branch bounding the comparison is named when known, so a reader can
// see the scope of the correlation instead of inferring it.
func peerEvidence(key failureKey, others []int) models.AttributionEvidence {
	detail := fmt.Sprintf("The same %s failure was observed on %s during this pass.", key.jobName, pullList(others))
	if key.baseRef != "" {
		detail += fmt.Sprintf(" Only pull requests targeting %s were compared.", key.baseRef)
	}
	return models.AttributionEvidence{
		Kind:     models.AttributionEvidenceOtherPulls,
		Detail:   detail,
		TestName: key.testName,
	}
}

// subject names what failed, since a build-level failure has no useful test name.
func subject(buildLevel bool, jobName string) string {
	if buildLevel {
		return jobName
	}
	return "This test"
}

// widespreadConfidence grades the verdict by peer count. Callers gate on
// widespreadVerdictMinPulls, so the count never reaches a low tier here.
func widespreadConfidence(others int) string {
	if others >= widespreadHighConfidencePulls {
		return models.AttributionConfidenceHigh
	}
	return models.AttributionConfidenceMedium
}

// peers returns the pull requests other than number.
func peers(numbers []int, number int) []int {
	out := make([]int, 0, len(numbers))
	for _, candidate := range numbers {
		if candidate != number {
			out = append(out, candidate)
		}
	}
	return out
}

func pullList(numbers []int) string {
	labels := make([]string, len(numbers))
	for i, number := range numbers {
		labels[i] = fmt.Sprintf("#%d", number)
	}
	if len(labels) == 1 {
		return "pull request " + labels[0]
	}
	return "pull requests " + humanList(labels)
}

// humanList joins names for prose, truncating long lists so a summary stays readable.
func humanList(names []string) string {
	const max = 3
	if len(names) > max {
		return fmt.Sprintf("%s and %d others", strings.Join(names[:max], ", "), len(names)-max)
	}
	if len(names) == 2 {
		return names[0] + " and " + names[1]
	}
	return strings.Join(names, ", ")
}

func appendUnique(existing []string, value string) []string {
	for _, candidate := range existing {
		if candidate == value {
			return existing
		}
	}
	return append(existing, value)
}

func appendUniqueInt(existing []int, value int) []int {
	for _, candidate := range existing {
		if candidate == value {
			return existing
		}
	}
	return append(existing, value)
}
