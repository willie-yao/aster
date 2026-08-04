package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
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
		string, string, string, string, string, string, string, string,
	) (actions.PreviewResult, error)
}

func previewChatFixHandler(timeout time.Duration, run ChatFixRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			PatternID       string `json:"pattern_id"`
			PatternHash     string `json:"pattern_hash"`
			SourceRequestID string `json:"source_request_id"`
			Instruction     string `json:"instruction"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, maxChatFixBodyBytes); err != nil {
			http.Error(w, "invalid chat fix request", http.StatusBadRequest)
			return
		}
		body.PatternID = strings.TrimSpace(body.PatternID)
		body.PatternHash = strings.TrimSpace(body.PatternHash)
		body.SourceRequestID = strings.TrimSpace(body.SourceRequestID)
		body.Instruction = strings.TrimSpace(body.Instruction)
		if body.PatternID == "" || len(body.PatternID) > maxChatFixPatternBytes ||
			body.PatternHash == "" || len(body.PatternHash) > maxChatFixPatternHash ||
			body.SourceRequestID == "" || len(body.SourceRequestID) > maxChatFixRequestIDBytes || len(body.Instruction) > maxChatFixInputBytes {
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
			body.SourceRequestID,
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
	case errors.Is(err, analysischat.ErrAnalysisChanged):
		status, message = http.StatusConflict, analysischat.ErrAnalysisChanged.Error()
	case errors.Is(err, analysischat.ErrPatternChanged):
		status, message = http.StatusConflict, analysischat.ErrPatternChanged.Error()
	case errors.Is(err, analysischat.ErrRequestPending):
		status, message = http.StatusConflict, "source investigation is pending"
	case errors.Is(err, analysischat.ErrRequestOutcomeUnknown):
		status, message = http.StatusConflict, "source investigation outcome unknown"
	case errors.Is(err, analysischat.ErrInvalidRequest):
		status, message = http.StatusBadRequest, "invalid fix proposal request"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "fix proposal timed out"
	case errors.Is(err, context.Canceled):
		status, message = 499, "fix proposal cancelled"
	case errors.Is(err, sourceinvestigation.ErrInvalidResult), errors.Is(err, sourceinvestigation.ErrUnavailable),
		errors.Is(err, analysischat.ErrRequestFailed):
		status, message = http.StatusUnprocessableEntity, "selected source investigation is not usable"
	case errors.Is(err, actions.ErrPreviewRejected):
		status, message = http.StatusUnprocessableEntity, "fix proposal could not be generated"
	}
	if status >= 500 || status == http.StatusUnprocessableEntity {
		log.Printf("chat fix preview failed for %s (by %s): %s", sessionID, login, safeOperatorError(err))
	}
	http.Error(w, message, status)
}
