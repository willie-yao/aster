package remediation

// PublicFileName is the redacted remediation status consumed by the frontend.
const PublicFileName = "remediations.json"

// PublicState is the redacted remediation projection.
type PublicState struct {
	Remediations map[string]PublicRemediation `json:"remediations"`
}

// PublicRemediation exposes links and status without private generation data.
type PublicRemediation struct {
	ID        string         `json:"id"`
	Subject   string         `json:"subject"`
	JobID     string         `json:"job_id"`
	JobName   string         `json:"job_name"`
	JobType   string         `json:"job_type"`
	Issue     *IssueRef      `json:"issue,omitempty"`
	Attempt   *PublicAttempt `json:"attempt,omitempty"`
	UpdatedAt string         `json:"updated_at"`
}

// PublicAttempt is the latest pull request verification state.
type PublicAttempt struct {
	Number        int                 `json:"number"`
	PRNumber      int                 `json:"pr_number"`
	URL           string              `json:"url"`
	TargetRepo    string              `json:"target_repo"`
	HeadSHA       string              `json:"head_sha,omitempty"`
	MergeSHA      string              `json:"merge_sha,omitempty"`
	Status        string              `json:"status"`
	PRState       string              `json:"pr_state,omitempty"`
	Outcome       string              `json:"outcome,omitempty"`
	OutcomeReason string              `json:"outcome_reason,omitempty"`
	Observations  []PublicObservation `json:"observations,omitempty"`
}

// PublicObservation exposes only fields consumed by the frontend.
type PublicObservation struct {
	BuildID     string `json:"build_id"`
	JobName     string `json:"job_name"`
	JobType     string `json:"job_type"`
	Result      string `json:"result"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	ProwURL     string `json:"prow_url,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

func publicAttemptReason(status string) string {
	switch status {
	case StatusAwaitingPresubmit:
		return "waiting for a matching Prow presubmit"
	case StatusPresubmitRunning:
		return "matching Prow presubmit is still running"
	case StatusPremergeVerified:
		return "matching Prow presubmit passed"
	case StatusPresubmitFailedSameCause:
		return "the original failure recurred in presubmit verification"
	case StatusPresubmitFailedDifferentCause:
		return "presubmit verification failed for a different reason"
	case StatusMerged, StatusObserving:
		return "waiting for post-merge Prow verification"
	case StatusVerifiedFixed:
		return "required post-merge Prow runs passed"
	case StatusStillFailingSameCause:
		return "the original failure recurred after merge"
	case StatusFailingDifferentCause:
		return "post-merge Prow failed for a different reason"
	case StatusInconclusive:
		return "Prow verification did not have enough evidence"
	default:
		return ""
	}
}

func publicObservationReason(outcome string) string {
	switch outcome {
	case OutcomePassed:
		return "expected tests passed"
	case OutcomeSameCause:
		return "the original failure signature recurred"
	case OutcomeDifferentCause:
		return "Prow completed with a different failure"
	case OutcomeInconclusive:
		return "Prow evidence was incomplete"
	case OutcomePending:
		return "Prow is still running"
	default:
		return ""
	}
}

// Public returns the redacted frontend projection.
func (s *State) Public() PublicState {
	out := PublicState{Remediations: map[string]PublicRemediation{}}
	if s == nil {
		return out
	}
	for id, entry := range s.Remediations {
		if entry == nil {
			continue
		}
		public := PublicRemediation{
			ID: entry.ID, Subject: entry.Subject, JobID: entry.JobID,
			JobName: entry.JobName, JobType: entry.JobType, UpdatedAt: entry.UpdatedAt,
		}
		if entry.Issue != nil {
			issue := *entry.Issue
			issue.Repo = ""
			issue.LastTransition = ""
			public.Issue = &issue
		}
		if len(entry.Attempts) > 0 {
			attempt := entry.Attempts[len(entry.Attempts)-1]
			observations := make([]PublicObservation, 0, len(attempt.Observations))
			for _, observation := range attempt.Observations {
				observations = append(observations, PublicObservation{
					BuildID: observation.BuildID, JobName: observation.JobName, JobType: observation.JobType,
					Result: observation.Result, Outcome: observation.Outcome, Reason: publicObservationReason(observation.Outcome),
					ProwURL: observation.ProwURL, StartedAt: observation.StartedAt, CompletedAt: observation.CompletedAt,
				})
			}
			public.Attempt = &PublicAttempt{
				Number: attempt.Number, PRNumber: attempt.PRNumber, URL: attempt.URL,
				TargetRepo: attempt.TargetRepo, HeadSHA: attempt.HeadSHA, MergeSHA: attempt.MergeSHA,
				Status: attempt.Status, PRState: attempt.PRState,
				Outcome: attempt.Outcome, OutcomeReason: publicAttemptReason(attempt.Status),
				Observations: observations,
			}
		}
		key := entry.FindingID
		if key == "" {
			key = id
		}
		out.Remediations[key] = public
	}
	return out
}
