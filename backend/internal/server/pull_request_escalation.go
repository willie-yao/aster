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
	"strconv"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/prescalation"
)

const (
	pullRequestEscalationIdempotencyHeader = "Idempotency-Key"
	maxPullRequestEscalationBodyBytes      = 4096
)

// PullRequestEscalationRunner starts and reads on-demand pull request analysis.
type PullRequestEscalationRunner interface {
	Start(context.Context, prescalation.Ref, string, string) (prescalation.View, error)
	Get(prescalation.Ref) (prescalation.View, error)
}

func startPullRequestEscalationHandler(timeout time.Duration, runner PullRequestEscalationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ref, ok := decodePullRequestEscalationStart(w, r)
		if !ok {
			return
		}
		requestID := strings.TrimSpace(r.Header.Get(pullRequestEscalationIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		view, err := runner.Start(ctx, ref, identity.Login, requestID)
		if err != nil {
			writePullRequestEscalationError(w, ref, err)
			return
		}
		status := http.StatusAccepted
		if pullRequestEscalationTerminal(view.State) {
			status = http.StatusOK
		}
		writeAnalysisChatJSON(w, status, view)
	})
}

func getPullRequestEscalationHandler(runner PullRequestEscalationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := pullRequestEscalationRefFromPath(w, r, r.URL.Query().Get("test"))
		if !ok {
			return
		}
		view, err := runner.Get(ref)
		if err != nil {
			writePullRequestEscalationError(w, ref, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, view)
	})
}

// pullRequestEscalationRefFromPath builds a Ref from the route values plus the
// test name, which is carried in the body or query rather than the path because
// Ginkgo test names contain slashes and spaces.
func pullRequestEscalationRefFromPath(w http.ResponseWriter, r *http.Request, testName string) (prescalation.Ref, bool) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		http.Error(w, "invalid pull request number", http.StatusBadRequest)
		return prescalation.Ref{}, false
	}
	ref := prescalation.Ref{
		PullNumber: number,
		JobID:      r.PathValue("jobID"),
		BuildID:    r.PathValue("buildID"),
		TestName:   testName,
	}
	if strings.TrimSpace(ref.TestName) == "" {
		http.Error(w, "missing test name", http.StatusBadRequest)
		return prescalation.Ref{}, false
	}
	return ref, true
}

func decodePullRequestEscalationStart(w http.ResponseWriter, r *http.Request) (prescalation.Ref, bool) {
	var body struct {
		TestName string `json:"test_name"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxPullRequestEscalationBodyBytes+1))
	if err != nil || len(data) > maxPullRequestEscalationBodyBytes {
		http.Error(w, "invalid escalation request", http.StatusBadRequest)
		return prescalation.Ref{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Silently dropping an unknown field would change which failure is
	// analyzed, so a malformed body is rejected instead.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid escalation request", http.StatusBadRequest)
		return prescalation.Ref{}, false
	}
	return pullRequestEscalationRefFromPath(w, r, body.TestName)
}

func pullRequestEscalationTerminal(state string) bool {
	return state == prescalation.StateComplete || state == prescalation.StateFailed
}

func writePullRequestEscalationError(w http.ResponseWriter, ref prescalation.Ref, err error) {
	status, message := pullRequestEscalationErrorDetails(err)
	if status >= 500 {
		log.Printf("pull request escalation #%d %s: %s", ref.PullNumber, ref.BuildID, safePullRequestEscalationError(err))
	}
	http.Error(w, message, status)
}

func pullRequestEscalationErrorDetails(err error) (int, string) {
	switch {
	case errors.Is(err, prescalation.ErrInvalid):
		return http.StatusBadRequest, "invalid escalation request"
	case errors.Is(err, prescalation.ErrNotEligible):
		return http.StatusConflict, "this failure is already explained without analysis"
	case errors.Is(err, prescalation.ErrIdempotencyConflict):
		return http.StatusConflict, "escalation idempotency key conflict"
	case errors.Is(err, prescalation.ErrBusy):
		return http.StatusConflict, "another escalation is already running"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "escalation request timed out"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "escalation request cancelled"
	default:
		return http.StatusServiceUnavailable, "escalation is unavailable"
	}
}

func safePullRequestEscalationError(err error) string {
	status, message := pullRequestEscalationErrorDetails(err)
	return fmt.Sprintf("status=%d reason=%s", status, message)
}
