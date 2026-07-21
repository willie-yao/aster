// Package remediation tracks the lifecycle of dashboard-created fixes.
package remediation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// FileName is the private remediation ledger filename.
const FileName = "remediation_state.json"

const currentVersion = 2

// State is the versioned remediation ledger.
type State struct {
	Version      int                     `json:"version"`
	Repo         string                  `json:"repo,omitempty"`
	Remediations map[string]*Remediation `json:"remediations"`
}

// Remediation links one finding to its issues and fix attempts.
type Remediation struct {
	ID             string    `json:"id"`
	FindingID      string    `json:"finding_id"`
	Subject        string    `json:"subject"`
	JobID          string    `json:"job_id"`
	JobName        string    `json:"job_name"`
	JobType        string    `json:"job_type"`
	SourceRepo     string    `json:"source_repo,omitempty"`
	CommitRepo     string    `json:"commit_repo,omitempty"`
	Classification string    `json:"classification,omitempty"`
	Evidence       Evidence  `json:"evidence"`
	Issue          *IssueRef `json:"issue,omitempty"`
	Attempts       []Attempt `json:"attempts,omitempty"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
	LastTransition string    `json:"last_transition,omitempty"`
}

// Evidence is the deterministic failure evidence captured before a fix.
type Evidence struct {
	PatternID       string         `json:"pattern_id"`
	RootCause       string         `json:"root_cause,omitempty"`
	RootCauseHash   string         `json:"root_cause_hash,omitempty"`
	BuildWatermark  string         `json:"build_watermark,omitempty"`
	AffectedBuilds  []string       `json:"affected_builds,omitempty"`
	Tests           []TestEvidence `json:"tests,omitempty"`
	EvidenceCreated string         `json:"created_at"`
}

// TestEvidence identifies one failed JUnit case across affected builds.
type TestEvidence struct {
	Identity   string   `json:"identity"`
	Name       string   `json:"name"`
	SuiteName  string   `json:"suite_name,omitempty"`
	ClassName  string   `json:"class_name,omitempty"`
	ErrorHash  string   `json:"error_hash,omitempty"`
	BuildIDs   []string `json:"build_ids,omitempty"`
	JUnitFiles []string `json:"junit_files,omitempty"`
}

// IssueRef identifies a tracked GitHub issue.
type IssueRef struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	Repo           string `json:"repo,omitempty"`
	State          string `json:"state,omitempty"`
	LastTransition string `json:"last_transition,omitempty"`
}

// Attempt records one draft pull request and its observations.
type Attempt struct {
	Number                     int                `json:"number"`
	PRNumber                   int                `json:"pr_number"`
	URL                        string             `json:"url"`
	TargetRepo                 string             `json:"target_repo"`
	HeadRepo                   string             `json:"head_repo,omitempty"`
	HeadRef                    string             `json:"head_ref,omitempty"`
	HeadSHA                    string             `json:"head_sha,omitempty"`
	BaseRef                    string             `json:"base_ref,omitempty"`
	BaseSHA                    string             `json:"base_sha,omitempty"`
	MergeSHA                   string             `json:"merge_sha,omitempty"`
	OpenedAt                   string             `json:"opened_at"`
	MergedAt                   string             `json:"merged_at,omitempty"`
	Status                     string             `json:"status"`
	PRState                    string             `json:"pr_state,omitempty"`
	Outcome                    string             `json:"outcome,omitempty"`
	OutcomeReason              string             `json:"outcome_reason,omitempty"`
	Observations               []BuildObservation `json:"observations,omitempty"`
	IneligibleCommits          map[string]string  `json:"ineligible_commits,omitempty"`
	LastObservedAt             string             `json:"last_observed_at,omitempty"`
	LastTransition             string             `json:"last_transition,omitempty"`
	LastEmailedTransition      string             `json:"last_emailed_transition,omitempty"`
	LastEmailedTransitionIndex int                `json:"last_emailed_transition_index,omitempty"`
	TransitionIndex            int                `json:"transition_index,omitempty"`
}

// BuildObservation records one Prow result used for verification.
type BuildObservation struct {
	BuildID       string   `json:"build_id"`
	JobName       string   `json:"job_name"`
	JobType       string   `json:"job_type"`
	PullNumber    int      `json:"pull_number,omitempty"`
	SourceRepo    string   `json:"source_repo,omitempty"`
	SourceCommit  string   `json:"source_commit,omitempty"`
	HeadSHA       string   `json:"head_sha,omitempty"`
	Result        string   `json:"result"`
	Outcome       string   `json:"outcome"`
	Reason        string   `json:"reason,omitempty"`
	ProwURL       string   `json:"prow_url,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	CompletedAt   string   `json:"completed_at,omitempty"`
	MatchedTests  []string `json:"matched_tests,omitempty"`
	FailedMatches []string `json:"failed_matches,omitempty"`
}

// NewState returns an initialized empty ledger.
func NewState() *State {
	return NewStateForRepo("")
}

// NewStateForRepo returns an empty ledger scoped to one fix target.
func NewStateForRepo(repo string) *State {
	return &State{Version: currentVersion, Repo: repo, Remediations: map[string]*Remediation{}}
}

// Load reads the private ledger without applying a repository scope.
func Load(dir string) (*State, error) {
	return LoadForRepo(dir, "")
}

// LoadForRepo reads the private ledger and resets state from another target repo.
func LoadForRepo(dir, repo string) (*State, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewStateForRepo(repo), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read remediation state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse remediation state: %w", err)
	}
	if state.Version != currentVersion {
		return NewStateForRepo(repo), nil
	}
	if repo != "" && state.Repo != repo {
		return NewStateForRepo(repo), nil
	}
	if repo != "" {
		state.Repo = repo
	}
	if state.Remediations == nil {
		state.Remediations = map[string]*Remediation{}
	}
	return &state, nil
}

// Save writes the private ledger to dir.
func (s *State) Save(dir string) error {
	if s.Version == 0 {
		s.Version = currentVersion
	}
	if s.Remediations == nil {
		s.Remediations = map[string]*Remediation{}
	}
	if err := statefile.WriteJSON(filepath.Join(dir, FileName), s); err != nil {
		return err
	}
	return statefile.WriteJSON(filepath.Join(dir, PublicFileName), s.Public())
}

func nowString() string {
	return time.Now().UTC().Format(time.RFC3339)
}
