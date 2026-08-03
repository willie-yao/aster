package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
)

const (
	fetchStatusStaleAfter      = 2 * time.Minute
	fetchStatusRecentPassLimit = 10
)

type fetchStatusResponse struct {
	Available            bool                     `json:"available"`
	State                string                   `json:"state"`
	Stale                bool                     `json:"stale,omitempty"`
	Status               *fetchprogress.Status    `json:"status,omitempty"`
	HistorySchemaVersion int                      `json:"history_schema_version,omitempty"`
	History              []fetchStatusPassSummary `json:"history,omitempty"`
}

type fetchStatusPassSummary struct {
	PassType                fetchprogress.PassType `json:"pass_type"`
	StartedAt               time.Time              `json:"started_at"`
	CompletedAt             time.Time              `json:"completed_at"`
	DurationMS              int64                  `json:"duration_ms"`
	LogicalCount            int                    `json:"logical_count"`
	CacheHits               int                    `json:"cache_hits"`
	CompatibleResultsReused int                    `json:"compatible_results_reused"`
	ExactResultsReused      int                    `json:"exact_results_reused"`
	SameFailureReused       int                    `json:"same_failure_results_reused"`
	SameFailureGroups       int                    `json:"same_failure_groups"`
	SameFailureCandidates   int                    `json:"same_failure_candidates"`
	PotentialTasksSaved     int                    `json:"potential_tasks_saved"`
	LargestSameFailureGroup int                    `json:"largest_same_failure_group"`
	NewTasksCreated         int                    `json:"new_tasks_created"`
	FreshAnalysesCompleted  int                    `json:"fresh_analyses_completed"`
	Retries                 int                    `json:"retries"`
	Outcome                 fetchprogress.Outcome  `json:"outcome"`
	Published               bool                   `json:"published"`
}

func fetchStatusHandler(dataDir string) http.Handler {
	return fetchStatusHandlerWithClock(dataDir, time.Now, fetchStatusStaleAfter)
}

func fetchStatusHandlerWithClock(dataDir string, now func() time.Time, staleAfter time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.SetPrivateResponseHeaders(w.Header())
		w.Header().Set("Content-Type", "application/json")
		response := fetchStatusResponse{State: "unavailable"}
		status, err := fetchprogress.Read(fetchprogress.Path(dataDir))
		switch {
		case errors.Is(err, os.ErrNotExist):
			response.State = "missing"
		case err != nil:
			response.State = "unavailable"
		default:
			response.Available = true
			publicStatus := status
			publicStatus.CurrentTasks = nil
			response.Status = &publicStatus
			response.State, response.Stale = classifyFetchStatus(status, now().UTC(), staleAfter)
			if history, historyErr := fetchprogress.ReadHistory(fetchprogress.HistoryPath(dataDir)); historyErr == nil {
				response.HistorySchemaVersion = history.SchemaVersion
				passes := history.Passes
				if len(passes) > fetchStatusRecentPassLimit {
					passes = passes[len(passes)-fetchStatusRecentPassLimit:]
				}
				response.History = make([]fetchStatusPassSummary, 0, len(passes))
				for _, pass := range passes {
					response.History = append(response.History, fetchStatusPassSummary{
						PassType: pass.PassType, StartedAt: pass.StartedAt, CompletedAt: pass.CompletedAt,
						DurationMS: pass.CompletedAt.Sub(pass.StartedAt).Milliseconds(), LogicalCount: pass.LogicalCount,
						CacheHits: pass.CacheHits, CompatibleResultsReused: pass.CompatibleResultsReused,
						ExactResultsReused: pass.ExactResultsReused, SameFailureReused: pass.SameFailureReused,
						SameFailureGroups:     pass.SameFailureGroups,
						SameFailureCandidates: pass.SameFailureCandidates, PotentialTasksSaved: pass.PotentialTasksSaved,
						LargestSameFailureGroup: pass.LargestSameFailureGroup, NewTasksCreated: pass.NewTasksCreated,
						FreshAnalysesCompleted: pass.FreshAnalysesCompleted, Retries: pass.Retries,
						Outcome: pass.Outcome, Published: pass.Published,
					})
				}
			}
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})
}

func classifyFetchStatus(status fetchprogress.Status, now time.Time, staleAfter time.Duration) (string, bool) {
	stale := status.Outcome == fetchprogress.OutcomeRunning && staleAfter > 0 && now.Sub(status.LastProgressAt) > staleAfter
	if status.Outcome == fetchprogress.OutcomeSucceeded && status.Phase == fetchprogress.PhaseIdle && staleAfter > 0 {
		next := earliestTime(status.NextWatchAt, status.NextReconcileAt)
		stale = next != nil && now.Sub(*next) > staleAfter
	}
	if stale {
		return "stale", true
	}
	switch status.Outcome {
	case fetchprogress.OutcomeRunning:
		return "active", false
	case fetchprogress.OutcomeFailed:
		return "failed", false
	case fetchprogress.OutcomeCancelled:
		return "cancelled", false
	case fetchprogress.OutcomeInterrupted:
		return "interrupted", false
	case fetchprogress.OutcomeSucceeded:
		if status.Phase == fetchprogress.PhaseIdle {
			return "idle", false
		}
		return "completed", false
	default:
		return "unavailable", false
	}
}

func earliestTime(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right == nil || left.Before(*right) {
		return left
	}
	return right
}
