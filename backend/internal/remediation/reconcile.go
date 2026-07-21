package remediation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	StatusOpen           = "open"
	StatusClosedUnmerged = "closed_unmerged"
	StatusMerged         = "merged"
	StatusInconclusive   = "inconclusive"
)

// FixReference is a dashboard-created fix recorded by the fix-PR manager.
type FixReference struct {
	URL      string
	OpenedAt string
	Pattern  *models.PatternAnalysis
}

// PullRequestClient is the GitHub lifecycle subset used by reconciliation.
type PullRequestClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (ghpr.PullRequest, error)
}

// PullRequestSearchClient recovers dashboard-created pull requests by marker.
type PullRequestSearchClient interface {
	SearchPullRequests(ctx context.Context, owner, repo, queryToken, confirmMarker string) ([]ghpr.PullRequestSearchResult, error)
}

// Reconciler updates the remediation ledger from GitHub state.
type Reconciler struct {
	client      PullRequestClient
	dataDir     string
	backend     storage.Backend
	catalog     *jobconfig.Catalog
	coverage    *CoverageCatalog
	compare     CompareClient
	issues      map[string]IssueRef
	issueClient IssueLifecycleClient
	issueRepo   string
	targetRepo  string
	search      PullRequestSearchClient
}

// NewReconciler builds a lifecycle reconciler.
func NewReconciler(client PullRequestClient, dataDir string) *Reconciler {
	return &Reconciler{client: client, dataDir: dataDir}
}

// SetVerification enables Prow observations after GitHub lifecycle updates.
func (r *Reconciler) SetVerification(backend storage.Backend, catalog *jobconfig.Catalog, coverage *CoverageCatalog, compare CompareClient) {
	r.backend, r.catalog, r.coverage, r.compare = backend, catalog, coverage, compare
}

// SetIssues links pattern job IDs to tracked issues and enables lifecycle updates.
func (r *Reconciler) SetIssues(repo string, issues map[string]IssueRef, client IssueLifecycleClient) {
	r.issueRepo, r.issues, r.issueClient = repo, issues, client
}

// SetRecovery enables marker-based pull request adoption.
func (r *Reconciler) SetRecovery(targetRepo string, search PullRequestSearchClient) {
	r.targetRepo, r.search = targetRepo, search
}

// Reconcile attaches tracked fixes to findings and refreshes every known pull request.
func (r *Reconciler) Reconcile(ctx context.Context, patterns []models.PatternAnalysis, details []models.JobDetail, fixes map[string]FixReference, keyFor func(models.PatternAnalysis) string) (*State, error) {
	state, err := LoadForRepo(r.dataDir, r.targetRepo)
	if err != nil {
		return nil, err
	}
	var errs []error

	combined := append([]models.PatternAnalysis(nil), patterns...)
	seenKeys := map[string]bool{}
	for _, pattern := range combined {
		seenKeys[keyFor(pattern)] = true
	}
	for key, fix := range fixes {
		if fix.Pattern != nil && !seenKeys[key] {
			combined = append(combined, *fix.Pattern)
			seenKeys[key] = true
		}
	}

	for _, pattern := range combined {
		currentID := pattern.ID
		if currentID == "" {
			currentID = models.PatternID(pattern)
		}
		currentEvidence := EvidenceForPattern(pattern, details)
		entry := state.Remediations[currentID]
		if entry == nil {
			for _, existing := range state.Remediations {
				if existing != nil && existing.JobID == pattern.JobID && evidenceOverlaps(existing.Evidence, currentEvidence) {
					entry = existing
					entry.FindingID = currentID
					entry.UpdatedAt = nowString()
					break
				}
			}
		}
		if entry != nil {
			refreshRemediationMetadata(entry, pattern, details)
		}
		if entry != nil && len(entry.Attempts) > 0 {
			latest := &entry.Attempts[len(entry.Attempts)-1]
			if entry.JobType == models.JobTypePresubmit && latest.Status == StatusVerifiedFixed && evidenceAdvanced(entry.Evidence, currentEvidence) {
				if evidenceOverlaps(entry.Evidence, currentEvidence) {
					entry.Evidence = currentEvidence
					mergeObservations(latest, recurrenceObservations(entry, currentEvidence, details))
					transitionAttempt(entry, latest, StatusStillFailingSameCause, OutcomeSameCause,
						"failure recurred in a newer Prow build after verification")
				} else {
					entry.Evidence = currentEvidence
					transitionAttempt(entry, latest, StatusInconclusive, OutcomeInconclusive,
						"newer failure does not match the previously verified test evidence")
				}
			}
		}
		if entry != nil {
			syncLinkedIssue(entry, r.issues[pattern.JobID])
		}
		key := keyFor(pattern)
		fix, ok := fixes[key]
		if (!ok || strings.TrimSpace(fix.URL) == "") && r.search != nil && r.targetRepo != "" {
			owner, repo, valid := strings.Cut(r.targetRepo, "/")
			if valid {
				results, err := r.search.SearchPullRequests(ctx, owner, repo, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
				if err != nil {
					errs = append(errs, fmt.Errorf("recover remediation pull request: %w", err))
				} else if len(results) > 0 {
					result := results[0]
					if entry == nil || findAttempt(entry, result.HTMLURL) == nil {
						copy := pattern
						fix, ok = FixReference{URL: result.HTMLURL, OpenedAt: nowString(), Pattern: &copy}, true
					}
				}
			}
		}
		if !ok || strings.TrimSpace(fix.URL) == "" {
			continue
		}
		id := currentID
		if entry == nil {
			now := nowString()
			entry = &Remediation{
				ID: id, FindingID: id, Subject: pattern.Subject, JobID: pattern.JobID,
				JobName: pattern.Subject, Classification: classificationForPattern(pattern, details),
				Evidence: currentEvidence, CreatedAt: now, UpdatedAt: now,
			}
			refreshRemediationMetadata(entry, pattern, details)
			state.Remediations[id] = entry
		}
		syncLinkedIssue(entry, r.issues[pattern.JobID])
		if findAttempt(entry, fix.URL) == nil {
			owner, repo, number, err := ParsePullRequestURL(fix.URL)
			if err != nil {
				errs = append(errs, fmt.Errorf("remediation %s: %w", id, err))
				continue
			}
			entry.Attempts = append(entry.Attempts, Attempt{
				Number: len(entry.Attempts) + 1, PRNumber: number, URL: fix.URL,
				TargetRepo: owner + "/" + repo, OpenedAt: fix.OpenedAt, Status: StatusOpen,
			})
		}
	}

	ids := make([]string, 0, len(state.Remediations))
	for id := range state.Remediations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := state.Remediations[id]
		if entry == nil || len(entry.Attempts) == 0 {
			continue
		}
		attempt := &entry.Attempts[len(entry.Attempts)-1]
		if attempt.PRState != StatusMerged {
			owner, repo, number, err := ParsePullRequestURL(attempt.URL)
			if err != nil {
				errs = append(errs, fmt.Errorf("remediation %s: %w", id, err))
				continue
			}
			pull, err := r.client.GetPullRequest(ctx, owner, repo, number)
			if err != nil {
				errs = append(errs, fmt.Errorf("remediation %s pull request: %w", id, err))
				continue
			}
			applyPullRequest(entry, attempt, pull)
		}
		if entry.SourceRepo == "" {
			entry.SourceRepo = testedRepoFor(entry, attempt.TargetRepo, r.catalog)
		}
		if entry.SourceRepo == "" {
			if detail := findJobDetail(entry, details); detail != nil {
				entry.SourceRepo = sourceRepoFromDetail(*detail, attempt.TargetRepo)
			}
		}
		if entry.CommitRepo == "" {
			entry.CommitRepo = commitRepoFor(entry, r.catalog, details)
		}
		if r.backend != nil && attempt.PRState == StatusOpen {
			if err := ObservePresubmits(ctx, r.backend, entry, attempt, r.coverage); err != nil {
				errs = append(errs, fmt.Errorf("remediation %s presubmit: %w", id, err))
			}
		}
		if attempt.PRState == StatusMerged && entry.JobType == models.JobTypePresubmit {
			finalizeMergedPresubmit(entry, attempt)
		}
		if r.compare != nil && attempt.PRState == StatusMerged {
			minCleanRuns := 2
			if entry.Classification == string(models.ClassificationFlaky) {
				minCleanRuns = 10
			}
			if err := ObservePeriodic(ctx, r.compare, entry, attempt, details, minCleanRuns); err != nil {
				errs = append(errs, fmt.Errorf("remediation %s periodic: %w", id, err))
			}
		}
	}
	pendingIssueJobs := map[string]bool{}
	for _, pattern := range UntrackedPatterns(state, patterns, details) {
		if pattern.JobID != "" {
			pendingIssueJobs[pattern.JobID] = true
		}
	}
	if err := reconcileLinkedIssues(ctx, r.issueClient, r.issueRepo, state, pendingIssueJobs); err != nil {
		errs = append(errs, fmt.Errorf("reconcile remediation issues: %w", err))
	}
	if err := state.Save(r.dataDir); err != nil {
		errs = append(errs, err)
	}
	return state, errors.Join(errs...)
}

func refreshRemediationMetadata(entry *Remediation, pattern models.PatternAnalysis, details []models.JobDetail) {
	if entry == nil {
		return
	}
	changed := false
	if entry.Subject == "" && pattern.Subject != "" {
		entry.Subject = pattern.Subject
		changed = true
	}
	if entry.JobID == "" && pattern.JobID != "" {
		entry.JobID = pattern.JobID
		changed = true
	}
	classification := classificationForPattern(pattern, details)
	if entry.Classification == "" || (entry.Classification == "pattern" && classification != "pattern") {
		entry.Classification = classification
		changed = true
	}
	for _, detail := range details {
		if !patternMatchesDetail(pattern, detail) {
			continue
		}
		if entry.JobType == "" {
			entry.JobType = detail.JobType
			entry.JobName = detail.Name
			changed = true
		} else if entry.JobName == "" {
			entry.JobName = detail.Name
			changed = true
		}
		if entry.SourceRepo == "" {
			if repo := sourceRepoFromDetail(detail, ""); repo != "" {
				entry.SourceRepo = repo
				changed = true
			}
		}
		break
	}
	if changed {
		entry.UpdatedAt = nowString()
	}
}

func syncLinkedIssue(entry *Remediation, issue IssueRef) {
	if entry == nil || issue.Number == 0 {
		return
	}
	if entry.Issue != nil && entry.Issue.Number == issue.Number && entry.Issue.Repo == issue.Repo {
		entry.Issue.URL = issue.URL
		return
	}
	copy := issue
	entry.Issue = &copy
}

func finalizeMergedPresubmit(remediation *Remediation, attempt *Attempt) {
	if attempt.Status == StatusVerifiedFixed || attempt.Status == StatusStillFailingSameCause || attempt.Status == StatusFailingDifferentCause {
		return
	}
	switch attempt.Outcome {
	case OutcomePassed:
		transitionAttempt(remediation, attempt, StatusVerifiedFixed, OutcomePassed,
			"current-head presubmit passed before merge")
	case OutcomeSameCause:
		transitionAttempt(remediation, attempt, StatusStillFailingSameCause, OutcomeSameCause,
			"pull request merged after reproducing the same presubmit failure")
	case OutcomeDifferentCause:
		transitionAttempt(remediation, attempt, StatusFailingDifferentCause, OutcomeDifferentCause,
			"pull request merged with a different presubmit failure")
	default:
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeInconclusive,
			"pull request merged without successful current-head presubmit evidence")
	}
}

func recurrenceObservations(entry *Remediation, evidence Evidence, details []models.JobDetail) []BuildObservation {
	builds := map[string]bool{}
	for _, buildID := range evidence.AffectedBuilds {
		builds[buildID] = true
	}
	var observations []BuildObservation
	detail := findJobDetail(entry, details)
	if detail != nil {
		for _, run := range detail.Runs {
			if !builds[run.BuildID] {
				continue
			}
			pullNumber, _ := strconv.Atoi(run.PullNumber)
			observation := BuildObservation{
				BuildID: run.BuildID, JobName: detail.Name, JobType: detail.JobType,
				PullNumber: pullNumber, SourceRepo: entry.SourceRepo, SourceCommit: run.Commit,
				HeadSHA: run.Revision, Result: run.Result, Outcome: OutcomeSameCause,
				Reason: "failure recurred after verification", ProwURL: run.ProwURL,
			}
			if !run.Started.IsZero() {
				observation.StartedAt = run.Started.UTC().Format(timeFormat)
			}
			if !run.Finished.IsZero() {
				observation.CompletedAt = run.Finished.UTC().Format(timeFormat)
			}
			for _, test := range evidence.Tests {
				for _, buildID := range test.BuildIDs {
					if buildID == run.BuildID {
						observation.MatchedTests = appendUnique(observation.MatchedTests, test.Identity)
						observation.FailedMatches = appendUnique(observation.FailedMatches, test.Identity)
						break
					}
				}
			}
			observations = append(observations, observation)
			delete(builds, run.BuildID)
		}
	}
	for buildID := range builds {
		observations = append(observations, BuildObservation{
			BuildID: buildID, JobName: entry.JobName, JobType: entry.JobType,
			SourceRepo: entry.SourceRepo, Outcome: OutcomeSameCause,
			Reason: "failure recurred after verification",
		})
	}
	return observations
}

func evidenceAdvanced(previous, current Evidence) bool {
	if previous.BuildWatermark == "" || current.BuildWatermark == "" {
		return false
	}
	return newerBuild(current.BuildWatermark, previous.BuildWatermark)
}

func evidenceOverlaps(left, right Evidence) bool {
	for _, a := range left.Tests {
		for _, b := range right.Tests {
			if a.Identity == b.Identity && a.ErrorHash != "" && a.ErrorHash == b.ErrorHash {
				return true
			}
		}
	}
	return false
}

// ParsePullRequestURL extracts owner, repository, and number from a GitHub URL.
func ParsePullRequestURL(value string) (string, string, int, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", "", 0, fmt.Errorf("parse pull request URL: %w", err)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("pull request URL %q has unexpected path", value)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("pull request URL %q has invalid number", value)
	}
	return parts[0], parts[1], number, nil
}

func findAttempt(remediation *Remediation, pullURL string) *Attempt {
	for i := range remediation.Attempts {
		if remediation.Attempts[i].URL == pullURL {
			return &remediation.Attempts[i]
		}
	}
	return nil
}

func applyPullRequest(remediation *Remediation, attempt *Attempt, pull ghpr.PullRequest) {
	previousStatus := attempt.Status
	previousHead := attempt.HeadSHA
	attempt.PRNumber = pull.Number
	attempt.URL = pull.HTMLURL
	attempt.TargetRepo = pull.Base.Repo
	attempt.HeadRepo, attempt.HeadRef, attempt.HeadSHA = pull.Head.Repo, pull.Head.Ref, pull.Head.SHA
	attempt.BaseRef, attempt.BaseSHA = pull.Base.Ref, pull.Base.SHA
	attempt.MergeSHA = pull.MergeCommitSHA
	if !pull.MergedAt.IsZero() {
		attempt.MergedAt = pull.MergedAt.UTC().Format(timeFormat)
	}
	switch {
	case pull.Merged:
		attempt.PRState = StatusMerged
	case pull.State == "closed":
		attempt.PRState = StatusClosedUnmerged
	default:
		attempt.PRState = StatusOpen
	}
	if previousHead != "" && previousHead != attempt.HeadSHA {
		attempt.Observations = nil
		attempt.IneligibleCommits = nil
		attempt.Outcome, attempt.OutcomeReason = "", ""
		attempt.Status = attempt.PRState
	} else {
		switch attempt.PRState {
		case StatusMerged:
			if !postMergeStatus(attempt.Status) {
				attempt.Status = StatusMerged
			}
		case StatusClosedUnmerged:
			attempt.Status = StatusClosedUnmerged
		case StatusOpen:
			if !preMergeStatus(attempt.Status) {
				attempt.Status = StatusOpen
			}
		}
	}
	if previousStatus != attempt.Status {
		attempt.LastTransition = previousStatus + "->" + attempt.Status
		attempt.TransitionIndex++
		remediation.LastTransition = attempt.LastTransition
	}
	remediation.UpdatedAt = nowString()
}

func preMergeStatus(status string) bool {
	switch status {
	case StatusAwaitingPresubmit, StatusPresubmitRunning, StatusPremergeVerified,
		StatusPresubmitFailedSameCause, StatusPresubmitFailedDifferentCause, StatusInconclusive:
		return true
	default:
		return false
	}
}

func postMergeStatus(status string) bool {
	switch status {
	case StatusObserving, StatusVerifiedFixed, StatusStillFailingSameCause,
		StatusFailingDifferentCause, StatusInconclusive:
		return true
	default:
		return false
	}
}

func commitRepoFor(remediation *Remediation, catalog *jobconfig.Catalog, details []models.JobDetail) string {
	if remediation.JobType == models.JobTypePresubmit {
		return remediation.SourceRepo
	}
	if catalog != nil {
		if definition, ok := catalog.Jobs[remediation.JobID]; ok && len(definition.Refs) > 0 {
			return definition.Refs[0].FullRepo()
		}
		if remediation.JobID == "" {
			for _, definition := range catalog.Jobs {
				if definition.Name == remediation.JobName && definition.JobType == remediation.JobType && len(definition.Refs) > 0 {
					return definition.Refs[0].FullRepo()
				}
			}
		}
	}
	if detail := findJobDetail(remediation, details); detail != nil {
		repos := map[string]bool{}
		for _, run := range detail.Runs {
			for repo := range run.RepoRefs {
				repos[repo] = true
			}
		}
		if len(repos) == 1 {
			for repo := range repos {
				return repo
			}
		}
	}
	return ""
}

func sourceRepoFromDetail(detail models.JobDetail, targetRepo string) string {
	if detail.Repo != "" {
		return detail.Repo
	}
	repos := map[string]struct{}{}
	for _, run := range detail.Runs {
		for repo := range run.RepoRefs {
			if repo == targetRepo && targetRepo != "" {
				return repo
			}
			if strings.Contains(repo, "/") {
				repos[repo] = struct{}{}
			}
		}
	}
	if len(repos) == 1 {
		for repo := range repos {
			return repo
		}
	}
	return ""
}

func testedRepoFor(remediation *Remediation, targetRepo string, catalog *jobconfig.Catalog) string {
	if catalog == nil {
		return remediation.SourceRepo
	}
	definition, ok := catalog.Jobs[remediation.JobID]
	if !ok && remediation.JobID == "" {
		for _, candidate := range catalog.Jobs {
			if candidate.Name == remediation.JobName && candidate.JobType == remediation.JobType {
				definition, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		return remediation.SourceRepo
	}
	if definition.TestsRepo(targetRepo) {
		return targetRepo
	}
	if definition.JobType == models.JobTypePresubmit {
		return definition.Repo
	}
	for _, ref := range definition.Refs {
		if repo := ref.FullRepo(); repo != "" {
			return repo
		}
	}
	return remediation.SourceRepo
}

const timeFormat = "2006-01-02T15:04:05Z07:00"
