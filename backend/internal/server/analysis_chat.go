package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// AnalysisChatRunner manages authenticated conversations about published analyses.
type analysisChatFixPreflighter interface {
	PreflightTestFix(context.Context, string, string, string) error
}

type AnalysisChatRunner interface {
	Create(analysischat.AnalysisRef, string, string) (analysischat.SessionView, error)
	Find(analysischat.AnalysisRef, string) (analysischat.SessionView, error)
	Get(string, string) (analysischat.SessionView, error)
	Send(context.Context, string, string, string, string) (analysischat.SessionView, error)
	Stream(context.Context, string, string, string, string, func(analysischat.Progress) error) (analysischat.SessionView, error)
	Cancel(string, string, string) error
}

func findAnalysisChatSessionHandler(run AnalysisChatRunner) http.Handler {
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
		session, err := run.Find(ref, identity.Login)
		if errors.Is(err, analysischat.ErrSessionNotFound) {
			auth.SetPrivateResponseHeaders(w.Header())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			writeAnalysisChatError(w, "find", identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, session)
	})
}

const (
	analysisChatIdempotencyHeader     = "Idempotency-Key"
	analysisChatOutcomeHeader         = "X-Analysis-Chat-Outcome"
	analysisChatReasonHeader          = "X-Analysis-Chat-Reason"
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
			Message   string `json:"message"`
			FixIntent bool   `json:"fix_intent,omitempty"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxAnalysisChatMessageBodyBytes); err != nil || strings.TrimSpace(body.Message) == "" {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout+15*time.Second)
		defer cancel()
		requestID := strings.TrimSpace(r.Header.Get(analysisChatIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		if err := preflightAnalysisChatFix(r.Context(), run, r.PathValue("id"), identity.Login, requestID, body.FixIntent); err != nil {
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
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

func streamAnalysisChatMessageHandler(timeout time.Duration, run AnalysisChatRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Message   string `json:"message"`
			FixIntent bool   `json:"fix_intent,omitempty"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxAnalysisChatMessageBodyBytes); err != nil || strings.TrimSpace(body.Message) == "" {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		requestID := strings.TrimSpace(r.Header.Get(analysisChatIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		if err := preflightAnalysisChatFix(r.Context(), run, r.PathValue("id"), identity.Login, requestID, body.FixIntent); err != nil {
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
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
		emit := func(progress analysischat.Progress) error {
			return writeAnalysisChatSSE(w, flusher, "progress", progress)
		}
		session, err := run.Stream(ctx, r.PathValue("id"), identity.Login, requestID, body.Message, emit)
		if err != nil {
			status, message, outcome := analysisChatErrorDetails(err)
			if status >= http.StatusBadRequest {
				log.Printf("analysis chat %s for %s: %s", r.PathValue("id"), identity.Login, safeAnalysisChatError(err))
			}
			payload := map[string]any{"status": status, "message": message}
			if outcome != "" {
				payload["outcome"] = outcome
			}
			if reason, ok := analysisChatReasonCode(err); ok {
				payload["reason"] = reason
			}
			_ = writeAnalysisChatSSE(w, flusher, "error", payload)
			return
		}
		_ = writeAnalysisChatSSE(w, flusher, "session", session)
	})
}

func preflightAnalysisChatFix(ctx context.Context, run AnalysisChatRunner, sessionID, owner, requestID string, fixIntent bool) error {
	if !fixIntent {
		return nil
	}
	preflight, ok := run.(analysisChatFixPreflighter)
	if !ok {
		return fmt.Errorf("%w: exact JUnit Fix preflight is unavailable", analysischat.ErrInvalidRequest)
	}
	return preflight.PreflightTestFix(ctx, sessionID, owner, requestID)
}

func cancelAnalysisChatMessageHandler(run AnalysisChatRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := run.Cancel(r.PathValue("id"), identity.Login, r.PathValue("requestID")); err != nil {
			writeAnalysisChatError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		auth.SetPrivateResponseHeaders(w.Header())
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeAnalysisChatSSE(w io.Writer, flusher http.Flusher, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
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
	auth.SetPrivateResponseHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAnalysisChatError(w http.ResponseWriter, id, login string, err error) {
	status, message, outcome := analysisChatErrorDetails(err)
	if outcome != "" {
		w.Header().Set(analysisChatOutcomeHeader, outcome)
	}
	if reason, ok := analysisChatReasonCode(err); ok {
		w.Header().Set(analysisChatReasonHeader, reason)
	}
	// Rejections are as worth an operator line as failures: a 400 with a generic
	// body is otherwise undiagnosable from the server side.
	if status >= http.StatusBadRequest {
		log.Printf("analysis chat %s for %s: %s", id, login, safeAnalysisChatError(err))
	}
	http.Error(w, message, status)
}

// analysisChatReasonCode returns the stable machine-readable reason for a
// failure: an action reason code, or the response gate a validation failure
// tripped.
func analysisChatReasonCode(err error) (string, bool) {
	if code, ok := actions.ReasonCodeFrom(err); ok {
		return string(code), true
	}
	return analysischat.ValidationGateOf(err)
}

func analysisChatErrorDetails(err error) (int, string, string) {
	status := http.StatusBadGateway
	message := "analysis chat could not complete the request"
	outcome := ""
	switch {
	case errors.Is(err, analysischat.ErrAnalysisNotFound), errors.Is(err, analysischat.ErrPatternNotFound):
		status, message, outcome = http.StatusNotFound, "analysis not found", "rejected"
	case errors.Is(err, analysischat.ErrSessionNotFound):
		status, message, outcome = http.StatusNotFound, "analysis chat session not found", "rejected"
	case errors.Is(err, analysischat.ErrRequestNotFound):
		status, message, outcome = http.StatusNotFound, "analysis chat request not found", "rejected"
	case errors.Is(err, analysischat.ErrAnalysisChanged), errors.Is(err, analysischat.ErrPatternChanged):
		status, message, outcome = http.StatusConflict, "analysis changed; start a new chat", "rejected"
	case errors.Is(err, analysischat.ErrSessionBusy):
		status, message, outcome = http.StatusConflict, analysischat.ErrSessionBusy.Error(), "pending"
	case errors.Is(err, analysischat.ErrRequestPending):
		status, message, outcome = http.StatusConflict, analysischat.ErrRequestPending.Error(), "pending"
	case errors.Is(err, analysischat.ErrIdempotencyConflict):
		status, message, outcome = http.StatusConflict, "analysis chat request conflicts with existing input", "rejected"
	case errors.Is(err, analysischat.ErrRequestOutcomeUnknown):
		status, message, outcome = http.StatusConflict, "analysis chat outcome is unknown", "unknown"
	case errors.Is(err, analysischat.ErrInvalidRequest):
		status, message, outcome = http.StatusBadRequest, "invalid analysis chat request", "rejected"
		if code, ok := actions.ReasonCodeFrom(err); ok {
			message = actions.ReasonMessage(code)
		}
	case errors.Is(err, analysischat.ErrSessionLimit), errors.Is(err, analysischat.ErrTurnLimit),
		errors.Is(err, analysischat.ErrActiveTurnLimit), errors.Is(err, analysischat.ErrRateLimit):
		status, message, outcome = http.StatusTooManyRequests, "analysis chat limit reached", "rejected"
	case errors.Is(err, sourceinvestigation.ErrInvalidResult), errors.Is(err, sourceinvestigation.ErrUnavailable):
		status, message, outcome = http.StatusBadGateway, "analysis chat source validation failed", "failed"
	case errors.Is(err, analysischat.ErrRequestFailed):
		outcome = "failed"
	case errors.Is(err, analysischat.ErrProviderRequestFailed):
		status, message, outcome = http.StatusBadGateway, analysischat.ErrProviderRequestFailed.Error(), "failed"
	case errors.Is(err, analysischat.ErrResponseValidationFailed):
		// The provider answered; the content failed a local policy check, so
		// this is not a gateway failure.
		status, message, outcome = http.StatusUnprocessableEntity, analysischat.ErrResponseValidationFailed.Error(), "failed"
		if gate, ok := analysischat.ValidationGateOf(err); ok {
			message = analysischat.GateMessage(gate)
		}
	case errors.Is(err, context.DeadlineExceeded):
		status, message, outcome = http.StatusGatewayTimeout, "analysis chat request timed out", "failed"
	case errors.Is(err, context.Canceled):
		status, message, outcome = 499, "analysis chat request cancelled", "failed"
	}
	return status, message, outcome
}

func safeAnalysisChatError(err error) string {
	if err == nil {
		return "unknown error"
	}
	if errors.Is(err, analysischat.ErrRequestFailed) {
		return "model request failed"
	}
	if errors.Is(err, sourceinvestigation.ErrInvalidResult) || errors.Is(err, sourceinvestigation.ErrUnavailable) {
		return "source validation failed"
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
