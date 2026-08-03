package server

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/corrections"
)

// AnalysisCorrectionRunner previews, confirms, and revokes correction overlays.
type AnalysisCorrectionRunner interface {
	Preview(sessionID, requestID, owner string) (corrections.Preview, error)
	Confirm(token, owner string) (corrections.PublicCorrection, error)
	Revoke(id, owner string) (corrections.PublicCorrection, error)
}

func previewAnalysisCorrectionHandler(run AnalysisCorrectionRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		preview, err := run.Preview(r.PathValue("id"), r.PathValue("requestID"), identity.Login)
		if err != nil {
			writeAnalysisCorrectionError(w, "preview", identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, preview)
	})
}

func confirmAnalysisCorrectionHandler(run AnalysisCorrectionRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := decodeAnalysisChatBody(w, r, &body, 4096); err != nil || strings.TrimSpace(body.Token) == "" {
			http.Error(w, "invalid correction confirmation", http.StatusBadRequest)
			return
		}
		correction, err := run.Confirm(body.Token, identity.Login)
		if err != nil {
			writeAnalysisCorrectionError(w, "confirm", identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, correction)
	})
}

func revokeAnalysisCorrectionHandler(run AnalysisCorrectionRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		correction, err := run.Revoke(r.PathValue("id"), identity.Login)
		if err != nil {
			writeAnalysisCorrectionError(w, r.PathValue("id"), identity.Login, err)
			return
		}
		writeAnalysisChatJSON(w, http.StatusOK, correction)
	})
}

func writeAnalysisCorrectionError(w http.ResponseWriter, id, login string, err error) {
	status := http.StatusInternalServerError
	message := "analysis correction request failed"
	switch {
	case errors.Is(err, corrections.ErrPreviewNotFound), errors.Is(err, corrections.ErrCorrectionNotFound),
		errors.Is(err, analysischat.ErrSessionNotFound), errors.Is(err, analysischat.ErrRequestNotFound),
		errors.Is(err, analysischat.ErrAnalysisNotFound):
		status, message = http.StatusNotFound, "analysis correction not found"
	case errors.Is(err, corrections.ErrPreviewExpired), errors.Is(err, corrections.ErrCorrectionState),
		errors.Is(err, analysischat.ErrAnalysisChanged):
		status, message = http.StatusConflict, "analysis correction state changed"
	case errors.Is(err, analysischat.ErrInvalidRequest):
		status, message = http.StatusBadRequest, "invalid analysis correction request"
	case errors.Is(err, corrections.ErrCorrectionLimit):
		status, message = http.StatusTooManyRequests, "analysis correction limit reached"
	}
	if status >= 500 {
		log.Printf("analysis correction %s for %s: %s", id, login, safeOperatorError(err))
	}
	http.Error(w, message, status)
}
