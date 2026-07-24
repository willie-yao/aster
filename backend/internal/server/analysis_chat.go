package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
)

// AnalysisChatRunner manages authenticated conversations about published analyses.
type AnalysisChatRunner interface {
	Create(analysischat.AnalysisRef, string, string) (analysischat.SessionView, error)
	Get(string, string) (analysischat.SessionView, error)
	Send(context.Context, string, string, string, string) (analysischat.SessionView, error)
}

const (
	analysisChatIdempotencyHeader     = "Idempotency-Key"
	defaultAnalysisChatTimeout        = 2 * time.Minute
	maxAnalysisChatReferenceBodyBytes = 128 << 10
	maxAnalysisChatMessageBodyBytes   = 32 << 10
)

func createAnalysisChatSessionHandler(run AnalysisChatRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var ref analysischat.AnalysisRef
		if err := decodeAnalysisChatBody(w, r, &ref, maxAnalysisChatReferenceBodyBytes); err != nil {
			http.Error(w, "invalid analysis reference", http.StatusBadRequest)
			return
		}
		requestID := strings.TrimSpace(r.Header.Get(analysisChatIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		session, err := run.Create(ref, identity.Login, requestID)
		if err != nil {
			writeAnalysisChatError(w, "create", identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusCreated, session)
	})
}

func getAnalysisChatSessionHandler(run AnalysisChatRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		session, err := run.Get(r.PathValue("id"), identity.Login)
		if err != nil {
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, session)
	})
}

func sendAnalysisChatMessageHandler(timeout time.Duration, run AnalysisChatRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Message string `json:"message"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxAnalysisChatMessageBodyBytes); err != nil || strings.TrimSpace(body.Message) == "" {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		requestID := strings.TrimSpace(r.Header.Get(analysisChatIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		session, err := run.Send(ctx, r.PathValue("id"), identity.Login, requestID, body.Message)
		if err != nil {
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, session)
	})
}

func decodeAnalysisChatBody(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body contains trailing data")
	}
	return nil
}

func writeAnalysisChatJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAnalysisChatError(w http.ResponseWriter, id, login string, err error) {
	status := http.StatusBadGateway
	message := "analysis chat could not complete the request"
	switch {
	case errors.Is(err, analysischat.ErrAnalysisNotFound):
		status, message = http.StatusNotFound, "analysis not found"
	case errors.Is(err, analysischat.ErrSessionNotFound):
		status, message = http.StatusNotFound, "analysis chat session not found"
	case errors.Is(err, analysischat.ErrAnalysisChanged), errors.Is(err, analysischat.ErrSessionBusy),
		errors.Is(err, analysischat.ErrIdempotencyConflict), errors.Is(err, analysischat.ErrRequestOutcomeUnknown):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, analysischat.ErrInvalidRequest):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, analysischat.ErrSessionLimit), errors.Is(err, analysischat.ErrTurnLimit):
		status, message = http.StatusTooManyRequests, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "analysis chat request timed out"
	case errors.Is(err, context.Canceled):
		status, message = 499, "analysis chat request cancelled"
	}
	if status >= 500 {
		log.Printf("analysis chat %s for %s: %s", id, login, safeAnalysisChatError(err))
	}
	http.Error(w, message, status)
}

func safeAnalysisChatError(err error) string {
	if err == nil {
		return "unknown error"
	}
	reason := redact.URLs(strings.TrimSpace(err.Error()))
	lower := strings.ToLower(reason)
	if strings.Contains(lower, "chat returned") || strings.Contains(lower, "responses returned") ||
		strings.Contains(lower, "responses status") || strings.Contains(lower, "decode response") ||
		strings.Contains(lower, "body=") || strings.Contains(lower, "status code") ||
		strings.Contains(lower, "unauthorized") {
		return "model request failed"
	}
	const maxReasonBytes = 300
	if len(reason) > maxReasonBytes {
		reason = reason[:maxReasonBytes] + "..."
	}
	return reason
}
