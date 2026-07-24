package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

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
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
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
		w.Header().Set("Cache-Control", "no-store")
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
			status, message, outcome := analysisChatErrorDetails(err)
			if status >= 500 {
				log.Printf("source investigation %s for %s: %s", r.PathValue("id"), identity.Login, safeAnalysisChatError(err))
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
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
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
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
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
