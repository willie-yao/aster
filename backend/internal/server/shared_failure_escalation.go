package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/prescalation"
)

// SharedFailureEscalationRunner starts and reads on-demand analysis of a
// failure shared across several open pull requests.
type SharedFailureEscalationRunner interface {
	Start(context.Context, prescalation.ClusterRef, string, string) (prescalation.ClusterView, error)
	Get(prescalation.ClusterRef) (prescalation.ClusterView, error)
}

func startSharedFailureEscalationHandler(timeout time.Duration, runner SharedFailureEscalationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !sharedFailureEscalationBodyEmpty(w, r) {
			return
		}
		ref, ok := sharedFailureEscalationRefFromPath(w, r)
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
			writeSharedFailureEscalationError(w, ref, err)
			return
		}
		status := http.StatusAccepted
		if pullRequestEscalationTerminal(view.State) {
			status = http.StatusOK
		}
		writeAnalysisChatJSON(w, status, view)
	})
}

func getSharedFailureEscalationHandler(runner SharedFailureEscalationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := sharedFailureEscalationRefFromPath(w, r)
		if !ok {
			return
		}
		view, err := runner.Get(ref)
		if err != nil {
			writeSharedFailureEscalationError(w, ref, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, view)
	})
}

// sharedFailureEscalationRefFromPath builds a ClusterRef from the route. The
// published id carries the whole correlation key, so nothing else is needed.
func sharedFailureEscalationRefFromPath(w http.ResponseWriter, r *http.Request) (prescalation.ClusterRef, bool) {
	ref := prescalation.ClusterRef{ID: r.PathValue("id")}
	if strings.TrimSpace(ref.ID) == "" {
		http.Error(w, "missing shared failure id", http.StatusBadRequest)
		return prescalation.ClusterRef{}, false
	}
	return ref, true
}

// sharedFailureEscalationBodyEmpty rejects a request that carries a body. The
// subject comes entirely from the path, so any content is a client sending a
// request this endpoint would silently misread.
func sharedFailureEscalationBodyEmpty(w http.ResponseWriter, r *http.Request) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxPullRequestEscalationBodyBytes+1))
	if err != nil || len(data) > maxPullRequestEscalationBodyBytes {
		http.Error(w, "invalid escalation request", http.StatusBadRequest)
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	// An empty JSON object is accepted so a client that always sends one keeps
	// working; anything with content is rejected.
	var body struct{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid escalation request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeSharedFailureEscalationError(w http.ResponseWriter, ref prescalation.ClusterRef, err error) {
	status, message := sharedFailureEscalationErrorDetails(err)
	if status >= 500 {
		log.Printf("shared failure escalation %s: %s", ref.ID, safePullRequestEscalationError(err))
	}
	http.Error(w, message, status)
}

// sharedFailureEscalationErrorDetails reuses the pull request mapping, except
// for ineligibility: a shared failure is refused because it can be analyzed
// from one of its pull requests, not because nothing is left to explain.
func sharedFailureEscalationErrorDetails(err error) (int, string) {
	if errors.Is(err, prescalation.ErrNotEligible) {
		return http.StatusConflict, "this failure can be analyzed from an affected pull request"
	}
	return pullRequestEscalationErrorDetails(err)
}
