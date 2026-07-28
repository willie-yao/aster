package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
)

const fetchStatusStaleAfter = 2 * time.Minute

type fetchStatusResponse struct {
	Available            bool                        `json:"available"`
	State                string                      `json:"state"`
	Stale                bool                        `json:"stale,omitempty"`
	Status               *fetchprogress.Status       `json:"status,omitempty"`
	HistorySchemaVersion int                         `json:"history_schema_version,omitempty"`
	History              []fetchprogress.PassSummary `json:"history,omitempty"`
}

func fetchStatusHandler(dataDir string) http.Handler {
	return fetchStatusHandlerWithClock(dataDir, time.Now, fetchStatusStaleAfter)
}

func fetchStatusHandlerWithClock(dataDir string, now func() time.Time, staleAfter time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
				response.History = history.Passes
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
