package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// SourceInvestigationRunner manages owner-bound source requests for chat sessions.
type SourceInvestigationRunner interface {
	SourceInvestigation(context.Context, string, string, string, string) (sourceinvestigation.View, error)
	StreamSourceInvestigation(context.Context, string, string, string, string, func(sourceinvestigation.Progress) error) (sourceinvestigation.View, error)
	GetSourceInvestigation(string, string, string) (sourceinvestigation.View, error)
	CancelSourceInvestigation(string, string, string) error
}

const maxSourceInvestigationBodyBytes = 4096

func sourceInvestigationHandler(timeout time.Duration, run SourceInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		chatRequestID, requestID, ok := decodeSourceInvestigationRequest(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout+15*time.Second)
		defer cancel()
		view, err := run.SourceInvestigation(ctx, r.PathValue("id"), identity.Login, requestID, chatRequestID)
		if err != nil {
			writeSourceInvestigationError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, view)
	})
}

func streamSourceInvestigationHandler(timeout time.Duration, run SourceInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		chatRequestID, requestID, ok := decodeSourceInvestigationRequest(w, r)
		if !ok {
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		auth.SetPrivateResponseHeaders(w.Header())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx, cancel := context.WithTimeout(r.Context(), timeout+15*time.Second)
		defer cancel()
		emit := func(progress sourceinvestigation.Progress) error {
			return writeAnalysisChatSSE(w, flusher, "progress", progress)
		}
		view, err := run.StreamSourceInvestigation(ctx, r.PathValue("id"), identity.Login, requestID, chatRequestID, emit)
		if err != nil {
			status, message, outcome := sourceInvestigationErrorDetails(err)
			if status >= 500 {
				log.Printf("source investigation %s for %s: %s", r.PathValue("id"), identity.Login, safeSourceInvestigationError(err))
			}
			payload := map[string]any{"status": status, "message": message}
			if outcome != "" {
				payload["outcome"] = outcome
			}
			_ = writeAnalysisChatSSE(w, flusher, "error", payload)
			return
		}
		_ = writeAnalysisChatSSE(w, flusher, "investigation", view)
	})
}

func getSourceInvestigationHandler(run SourceInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view, err := run.GetSourceInvestigation(r.PathValue("id"), identity.Login, r.PathValue("requestID"))
		if err != nil {
			writeSourceInvestigationError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, view)
	})
}

func cancelSourceInvestigationHandler(run SourceInvestigationRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := run.CancelSourceInvestigation(r.PathValue("id"), identity.Login, r.PathValue("requestID")); err != nil {
			writeSourceInvestigationError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		auth.SetPrivateResponseHeaders(w.Header())
		w.WriteHeader(http.StatusNoContent)
	})
}

func decodeSourceInvestigationRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var body struct {
		ChatRequestID string `json:"chat_request_id"`
	}
	if err := decodeAnalysisChatBody(w, r, &body, maxSourceInvestigationBodyBytes); err != nil || strings.TrimSpace(body.ChatRequestID) == "" {
		http.Error(w, "invalid source investigation request", http.StatusBadRequest)
		return "", "", false
	}
	requestID := strings.TrimSpace(r.Header.Get(analysisChatIdempotencyHeader))
	if requestID == "" {
		http.Error(w, "missing idempotency key", http.StatusBadRequest)
		return "", "", false
	}
	return strings.TrimSpace(body.ChatRequestID), requestID, true
}

func writeSourceInvestigationError(w http.ResponseWriter, id, login string, err error) {
	status, message, outcome := sourceInvestigationErrorDetails(err)
	if outcome != "" {
		w.Header().Set(analysisChatOutcomeHeader, outcome)
	}
	if status >= 500 {
		log.Printf("source investigation %s for %s: %s", id, login, safeSourceInvestigationError(err))
	}
	http.Error(w, message, status)
}

func sourceInvestigationErrorDetails(err error) (int, string, string) {
	status, message, outcome := analysisChatErrorDetails(err)
	if message == "analysis chat could not complete the request" {
		message = "source investigation could not complete the request"
	}
	switch {
	case errors.Is(err, analysischat.ErrRequestNotFound):
		message = "source investigation not found"
	case errors.Is(err, analysischat.ErrRequestPending):
		message = "source investigation is pending"
	case errors.Is(err, analysischat.ErrIdempotencyConflict):
		message = "source investigation idempotency key conflict"
	case errors.Is(err, analysischat.ErrRequestOutcomeUnknown):
		message = "source investigation outcome unknown"
	case errors.Is(err, analysischat.ErrInvalidRequest):
		message = "invalid source investigation request"
	case errors.Is(err, context.DeadlineExceeded):
		message = "source investigation timed out"
	case errors.Is(err, context.Canceled):
		message = "source investigation cancelled"
	}
	return status, message, outcome
}

func safeSourceInvestigationError(err error) string {
	reason := safeAnalysisChatError(err)
	if reason == "model request failed" {
		return "source investigation failed"
	}
	return reason
}
