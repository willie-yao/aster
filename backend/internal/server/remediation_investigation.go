package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/remediationinvestigation"
)

const (
	causalRemediationIdempotencyHeader = "Idempotency-Key"
	maxCausalRemediationBodyBytes      = 4096
)

// CausalRemediationInvestigationRunner starts and reads safe causal remediation state.
type CausalRemediationInvestigationRunner interface {
	Start(context.Context, remediationinvestigation.OperationRef, string, string, bool) (models.PatternRemediationInvestigationSummary, error)
	Get(context.Context, remediationinvestigation.OperationRef) (models.PatternRemediationInvestigationSummary, error)
}

func startCausalRemediationInvestigationHandler(timeout time.Duration, runner CausalRemediationInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ref, refresh, ok := decodeCausalRemediationStart(w, r)
		if !ok {
			return
		}
		requestID := strings.TrimSpace(r.Header.Get(causalRemediationIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		view, err := runner.Start(ctx, ref, identity.Login, requestID, refresh)
		if err != nil {
			writeCausalRemediationError(w, ref, err)
			return
		}
		status := http.StatusAccepted
		if causalRemediationTerminal(view.State) {
			status = http.StatusOK
		}
		writeCausalRemediationJSON(w, status, view)
	})
}

func getCausalRemediationInvestigationHandler(timeout time.Duration, runner CausalRemediationInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := remediationinvestigation.OperationRef{
			JobID: r.PathValue("jobID"), PatternID: r.PathValue("patternID"), CausalGroupID: r.PathValue("groupID"),
			PatternHash: r.URL.Query().Get("pattern_hash"), CausalGroupHash: r.URL.Query().Get("causal_group_hash"),
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		view, err := runner.Get(ctx, ref)
		if err != nil {
			writeCausalRemediationError(w, ref, err)
			return
		}
		writeCausalRemediationJSON(w, http.StatusOK, view)
	})
}

func decodeCausalRemediationStart(w http.ResponseWriter, r *http.Request) (remediationinvestigation.OperationRef, bool, bool) {
	var body struct {
		PatternHash     string `json:"pattern_hash"`
		CausalGroupHash string `json:"causal_group_hash"`
		Refresh         bool   `json:"refresh"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxCausalRemediationBodyBytes+1))
	if err != nil || len(data) > maxCausalRemediationBodyBytes {
		http.Error(w, "invalid remediation investigation request", http.StatusBadRequest)
		return remediationinvestigation.OperationRef{}, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid remediation investigation request", http.StatusBadRequest)
		return remediationinvestigation.OperationRef{}, false, false
	}
	ref := remediationinvestigation.OperationRef{
		JobID: r.PathValue("jobID"), PatternID: r.PathValue("patternID"), CausalGroupID: r.PathValue("groupID"),
		PatternHash: body.PatternHash, CausalGroupHash: body.CausalGroupHash,
	}
	return ref, body.Refresh, true
}

func writeCausalRemediationError(w http.ResponseWriter, ref remediationinvestigation.OperationRef, err error) {
	status, message := causalRemediationErrorDetails(err)
	if status >= 500 {
		log.Printf("causal remediation investigation %s/%s: %s", ref.PatternID, ref.CausalGroupID, safeCausalRemediationError(err))
	}
	http.Error(w, message, status)
}

func causalRemediationErrorDetails(err error) (int, string) {
	switch {
	case errors.Is(err, remediationinvestigation.ErrOperationInvalid):
		return http.StatusBadRequest, "invalid remediation investigation request"
	case errors.Is(err, remediationinvestigation.ErrOperationNotFound):
		return http.StatusNotFound, "remediation investigation subject not found"
	case errors.Is(err, remediationinvestigation.ErrOperationStale):
		return http.StatusConflict, "the displayed recurring cause is stale"
	case errors.Is(err, remediationinvestigation.ErrOperationInactive):
		return http.StatusConflict, "the recurring cause is no longer active"
	case errors.Is(err, remediationinvestigation.ErrOperationRefreshRunning):
		return http.StatusConflict, "dashboard refresh is in progress"
	case errors.Is(err, remediationinvestigation.ErrOperationIdempotencyConflict):
		return http.StatusConflict, "remediation investigation idempotency key conflict"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "remediation investigation request timed out"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "remediation investigation request cancelled"
	default:
		return http.StatusServiceUnavailable, "remediation investigation is unavailable"
	}
}

func safeCausalRemediationError(err error) string {
	status, message := causalRemediationErrorDetails(err)
	return fmt.Sprintf("status=%d reason=%s", status, message)
}

func writeCausalRemediationJSON(w http.ResponseWriter, status int, value models.PatternRemediationInvestigationSummary) {
	auth.SetPrivateResponseHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func causalRemediationTerminal(state models.PatternRemediationInvestigationState) bool {
	switch state {
	case models.PatternRemediationActionable, models.PatternRemediationAlreadyFixed,
		models.PatternRemediationExternalDependency, models.PatternRemediationEnvironmentOrInfrastructure,
		models.PatternRemediationMitigationOnly, models.PatternRemediationInsufficientEvidence,
		models.PatternRemediationInvestigationFailed, models.PatternRemediationStale:
		return true
	default:
		return false
	}
}
