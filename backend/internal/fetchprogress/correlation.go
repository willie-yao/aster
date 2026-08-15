package fetchprogress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const currentTaskLimit = 256

// Correlation identifies the current fetch run and pass with label-safe values.
type Correlation struct {
	RunID    string
	PassID   string
	PassType PassType
}

// TaskMapping correlates one safe work digest with its content-addressed Task.
type TaskMapping struct {
	WorkItem         string `json:"work_item"`
	TaskName         string `json:"task_name"`
	Phase            string `json:"phase,omitempty"`
	Attempts         int    `json:"attempts,omitempty"`
	Adopted          bool   `json:"adopted,omitempty"`
	ResultRetrieved  bool   `json:"result_retrieved,omitempty"`
	CacheDisposition string `json:"cache_disposition,omitempty"`
}

// WorkItemID returns a bounded digest without exposing source identity.
func WorkItemID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:8])
}

// Correlation returns the current run and pass identifiers.
func (t *Tracker) Correlation() (Correlation, bool) {
	if t == nil {
		return Correlation{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status.RunID == "" || t.status.PassID == "" || !validPassType(t.status.PassType) {
		return Correlation{}, false
	}
	return Correlation{RunID: t.status.RunID, PassID: t.status.PassID, PassType: t.status.PassType}, true
}

// RecordTaskPlanned adds a bounded current-pass mapping and classifies new work.
func (t *Tracker) RecordTaskPlanned(workItem, taskName string, hasCacheSeed, buildSubject bool) {
	if t == nil || workItem == "" || taskName == "" {
		return
	}
	t.update(false, func(status *Status) {
		mapping := findTaskMapping(status, workItem)
		if mapping == nil && len(status.CurrentTasks) < currentTaskLimit {
			status.CurrentTasks = append(status.CurrentTasks, TaskMapping{WorkItem: workItem, TaskName: taskName})
			mapping = &status.CurrentTasks[len(status.CurrentTasks)-1]
		}
		if mapping != nil && mapping.TaskName == "" {
			mapping.TaskName = taskName
		}
		if buildSubject {
			t.taskBuildSubjects[workItem] = true
		}
		if !t.plannedTasks[workItem] {
			t.plannedTasks[workItem] = true
			if !t.analysisPlanFinalized && !hasCacheSeed {
				status.Analyses.NewWork++
			}
		}
	})
}

// RecordTaskState updates Task attempts without inflating aggregate retries.
func (t *Tracker) RecordTaskState(workItem, phase string, attempts int, adopted bool) {
	if t == nil || workItem == "" {
		return
	}
	t.update(false, func(status *Status) {
		mapping := findTaskMapping(status, workItem)
		if mapping != nil {
			mapping.Phase = safeTaskPhase(phase)
		}
		if adopted && !t.taskAdopted[workItem] {
			t.taskAdopted[workItem] = true
			status.Analyses.ExistingTasksAdopted++
			if t.taskBuildSubjects[workItem] {
				status.Analyses.BuildSubjects.ExistingTasksAdopted++
			}
		}
		if !adopted && !t.taskCreated[workItem] {
			t.taskCreated[workItem] = true
			status.Analyses.NewTasksCreated++
			if t.taskBuildSubjects[workItem] {
				status.Analyses.BuildSubjects.NewTasksCreated++
			}
		}
		if mapping != nil {
			mapping.Adopted = t.taskAdopted[workItem]
		}
		oldAttempts := t.taskAttempts[workItem]
		if attempts <= oldAttempts {
			return
		}
		oldRetries := max(oldAttempts-1, 0)
		newRetries := max(attempts-1, 0)
		status.Analyses.TaskAttempts += attempts - oldAttempts
		status.Analyses.Retries += newRetries - oldRetries
		t.taskAttempts[workItem] = attempts
		if mapping != nil {
			mapping.Attempts = attempts
		}
	})
}

// RecordTaskOutcome stores a safe terminal phase when no further Task state is available.
func (t *Tracker) RecordTaskOutcome(workItem, phase string) {
	if t == nil || workItem == "" {
		return
	}
	t.update(false, func(status *Status) {
		if mapping := findTaskMapping(status, workItem); mapping != nil {
			mapping.Phase = safeTaskPhase(phase)
		}
	})
}

// RecordResultAttempt tracks retrieval polls and accepted result availability.
func (t *Tracker) RecordResultAttempt(workItem string, retry, retrieved bool) {
	if t == nil || workItem == "" {
		return
	}
	t.update(false, func(status *Status) {
		mapping := findTaskMapping(status, workItem)
		if retry {
			status.Analyses.ResultRetrievalRetries++
		}
		if retrieved && !t.taskResults[workItem] {
			t.taskResults[workItem] = true
			status.Analyses.ResultsRetrieved++
		}
		if mapping != nil {
			mapping.ResultRetrieved = t.taskResults[workItem]
		}
	})
}

// RecordSameFailureReused records logical results shared from one representative Task.
func (t *Tracker) RecordSameFailureReused(count int) {
	if t == nil || count <= 0 {
		return
	}
	t.update(false, func(status *Status) { status.Analyses.SameFailureReused += count })
}

// RecordFreshAnalysisCompleted records one accepted result from a newly created Task.
func (t *Tracker) RecordFreshAnalysisCompleted(workItem string) {
	if t == nil || workItem == "" {
		return
	}
	t.update(false, func(status *Status) {
		if !t.taskCreated[workItem] || t.freshResults[workItem] {
			return
		}
		t.freshResults[workItem] = true
		status.Analyses.FreshAnalysesCompleted++
		if t.taskBuildSubjects[workItem] {
			status.Analyses.BuildSubjects.FreshAnalysesCompleted++
		}
	})
}

// RecordCacheDisposition records whether a seeded cache entry was accepted or stale.
func (t *Tracker) RecordCacheDisposition(workItem string, accepted bool) {
	if t == nil || workItem == "" {
		return
	}
	t.update(false, func(status *Status) {
		mapping := findTaskMapping(status, workItem)
		if t.cacheDisposition[workItem] != "" {
			return
		}
		if accepted {
			t.cacheDisposition[workItem] = "accepted"
			if !t.analysisPlanFinalized {
				status.Analyses.AcceptedCacheHits++
				if t.taskBuildSubjects[workItem] {
					status.Analyses.BuildSubjects.AcceptedCacheHits++
				}
			}
		} else {
			t.cacheDisposition[workItem] = "stale"
			if !t.analysisPlanFinalized {
				status.Analyses.StaleWork++
			}
		}
		if mapping != nil {
			mapping.CacheDisposition = t.cacheDisposition[workItem]
		}
	})
}

func findTaskMapping(status *Status, workItem string) *TaskMapping {
	for i := range status.CurrentTasks {
		if status.CurrentTasks[i].WorkItem == workItem {
			return &status.CurrentTasks[i]
		}
	}
	return nil
}

func safeTaskPhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "pending":
		return "Pending"
	case "running":
		return "Running"
	case "scheduled":
		return "Scheduled"
	case "succeeded":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled", "canceled":
		return "Cancelled"
	case "timedout", "timed-out", "timeout":
		return "TimedOut"
	default:
		return "Unknown"
	}
}
