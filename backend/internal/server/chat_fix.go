package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	maxChatFixBodyBytes      = 16 << 10
	maxChatFixPatternBytes   = 512
	maxChatFixPatternHash    = 128
	maxChatFixRequestIDBytes = 128
	maxChatFixInputBytes     = 4096
)

// ChatFixRunner generates a fix preview from one selected chat response.
type ChatFixRunner interface {
	PreviewChatFix(
		context.Context,
		string, string, string, string, string, string, string,
	) (actions.PreviewResult, error)
}

// ChatFixRequestRunner admits exact JUnit chat-to-fix previews for durable
// asynchronous generation.
type ChatFixRequestRunner interface {
	CreateAnalysisFixRequest(context.Context, string, string, string, string, string, ...string) (actions.ActionRequestView, error)
}

func createAnalysisChatFixRequestHandler(timeout time.Duration, run ChatFixRequestRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Instruction       string `json:"instruction"`
			ReplacesRequestID string `json:"replaces_request_id"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxChatFixBodyBytes); err != nil {
			http.Error(w, "invalid chat fix request", http.StatusBadRequest)
			return
		}
		body.Instruction = strings.TrimSpace(body.Instruction)
		body.ReplacesRequestID = strings.TrimSpace(body.ReplacesRequestID)
		if len(body.Instruction) > maxChatFixInputBytes || len(body.ReplacesRequestID) > maxChatFixRequestIDBytes {
			http.Error(w, "invalid chat fix request", http.StatusBadRequest)
			return
		}
		// Pinning the source makes network calls, so bound them like every other
		// action rather than letting the client dictate how long they run.
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		view, err := run.CreateAnalysisFixRequest(
			ctx,
			r.PathValue("id"), identity.Login, r.PathValue("requestID"), identity.Token, body.Instruction, body.ReplacesRequestID,
		)
		if err != nil {
			writeChatFixRequestError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusAccepted, view)
	})
}

func writeChatFixRequestError(w http.ResponseWriter, sessionID, login string, err error) {
	switch {
	case errors.Is(err, analysischat.ErrAnalysisNotFound), errors.Is(err, analysischat.ErrPatternNotFound),
		errors.Is(err, analysischat.ErrSessionNotFound), errors.Is(err, analysischat.ErrRequestNotFound),
		errors.Is(err, analysischat.ErrAnalysisChanged), errors.Is(err, analysischat.ErrPatternChanged),
		errors.Is(err, analysischat.ErrRequestPending), errors.Is(err, analysischat.ErrRequestOutcomeUnknown),
		errors.Is(err, analysischat.ErrInvalidRequest), errors.Is(err, analysischat.ErrRequestFailed),
		errors.Is(err, sourceinvestigation.ErrInvalidResult), errors.Is(err, sourceinvestigation.ErrUnavailable):
		writeChatFixError(w, sessionID, login, err)
	default:
		writeActionError(w, sessionID, login, err)
	}
}

func previewChatFixHandler(timeout time.Duration, run ChatFixRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			PatternID   string `json:"pattern_id"`
			PatternHash string `json:"pattern_hash"`
			Instruction string `json:"instruction"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxChatFixBodyBytes); err != nil {
			http.Error(w, "invalid chat fix request", http.StatusBadRequest)
			return
		}
		body.PatternID = strings.TrimSpace(body.PatternID)
		body.PatternHash = strings.TrimSpace(body.PatternHash)
		body.Instruction = strings.TrimSpace(body.Instruction)
		legacyFields := body.PatternID != "" || body.PatternHash != ""
		legacyComplete := body.PatternID != "" && body.PatternHash != ""
		if legacyFields && !legacyComplete || len(body.PatternID) > maxChatFixPatternBytes ||
			len(body.PatternHash) > maxChatFixPatternHash || len(body.Instruction) > maxChatFixInputBytes {
			http.Error(w, "invalid chat fix request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		preview, err := run.PreviewChatFix(
			ctx,
			r.PathValue("id"),
			identity.Login,
			r.PathValue("requestID"),
			body.PatternID,
			body.PatternHash,
			identity.Token,
			body.Instruction,
		)
		if err != nil {
			writeChatFixError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		auth.SetPrivateResponseHeaders(w.Header())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(preview)
	})
}

func writeChatFixError(w http.ResponseWriter, sessionID, login string, err error) {
	status, message := http.StatusInternalServerError, "fix proposal could not be generated"
	switch {
	case errors.Is(err, actions.ErrNotFound), errors.Is(err, analysischat.ErrAnalysisNotFound),
		errors.Is(err, analysischat.ErrPatternNotFound), errors.Is(err, analysischat.ErrSessionNotFound),
		errors.Is(err, analysischat.ErrRequestNotFound):
		status, message = http.StatusNotFound, "not found"
	case errors.Is(err, actions.ErrPatternMismatch):
		status, message = http.StatusConflict, actions.ErrPatternMismatch.Error()
	case errors.Is(err, actions.ErrPreviewTargetChanged), errors.Is(err, actions.ErrPreviewPending):
		status, message = http.StatusConflict, "fix preview state changed; generate a new preview"
	case errors.Is(err, analysischat.ErrAnalysisChanged):
		status, message = http.StatusConflict, analysischat.ErrAnalysisChanged.Error()
	case errors.Is(err, analysischat.ErrPatternChanged):
		status, message = http.StatusConflict, analysischat.ErrPatternChanged.Error()
	case errors.Is(err, analysischat.ErrRequestPending):
		status, message = http.StatusConflict, "analysis chat request is pending"
	case errors.Is(err, analysischat.ErrRequestOutcomeUnknown):
		status, message = http.StatusConflict, "analysis chat outcome unknown"
	// The specific causes come first: source pinning wraps its failures with
	// ErrInvalidRequest, so a generic 400 would otherwise shadow a timeout, a
	// cancellation, or an unusable source.
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "fix proposal timed out"
	case errors.Is(err, context.Canceled):
		status, message = 499, "fix proposal cancelled"
	case errors.Is(err, sourceinvestigation.ErrInvalidResult), errors.Is(err, sourceinvestigation.ErrUnavailable),
		errors.Is(err, analysischat.ErrRequestFailed):
		status, message = http.StatusUnprocessableEntity, "verified source input is not usable"
	case errors.Is(err, actions.ErrPreviewRejected):
		status, message = http.StatusUnprocessableEntity, "fix proposal could not be generated"
	case errors.Is(err, analysischat.ErrInvalidRequest):
		status, message = http.StatusBadRequest, "invalid fix proposal request"
	}
	if status >= 500 || status == http.StatusUnprocessableEntity {
		log.Printf("chat fix preview failed for %s (by %s): %s", sessionID, login, safeOperatorError(err))
	}
	http.Error(w, message, status)
}
