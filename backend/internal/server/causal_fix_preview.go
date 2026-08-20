package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/causalfixpreview"
	"github.com/willie-yao/aster/backend/internal/remediationinvestigation"
)

func causalFixPreviewHandler(timeout time.Duration, runner CausalFixPreviewRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			PatternHash     string `json:"pattern_hash"`
			CausalGroupHash string `json:"causal_group_hash"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxCausalRemediationBodyBytes+1))
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err != nil || len(raw) > maxCausalRemediationBodyBytes || decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid causal fix preview request", http.StatusBadRequest)
			return
		}
		requestID := strings.TrimSpace(r.Header.Get(causalRemediationIdempotencyHeader))
		if requestID == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		ref := remediationinvestigation.OperationRef{JobID: r.PathValue("jobID"), PatternID: r.PathValue("patternID"), CausalGroupID: r.PathValue("groupID"), PatternHash: body.PatternHash, CausalGroupHash: body.CausalGroupHash}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		preview, err := runner.Preview(ctx, ref, identity.Login, requestID)
		if err != nil {
			writeCausalFixPreviewError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(preview)
	})
}
func writeCausalFixPreviewError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "fix preview generation failed"
	switch {
	case errors.Is(err, causalfixpreview.ErrInvalid):
		status, message = http.StatusBadRequest, "invalid causal fix preview request"
	case errors.Is(err, causalfixpreview.ErrConflict):
		status, message = http.StatusConflict, "idempotency key conflicts with another preview"
	case errors.Is(err, causalfixpreview.ErrNotActionable), errors.Is(err, remediationinvestigation.ErrOperationNotActionable):
		status, message = http.StatusConflict, "target is no longer exactly actionable"
	case errors.Is(err, causalfixpreview.ErrRejected):
		status, message = http.StatusUnprocessableEntity, "generated patch was rejected"
	case errors.Is(err, causalfixpreview.ErrValidation):
		status, message = http.StatusUnprocessableEntity, "generated patch failed validation"
	case errors.Is(err, remediationinvestigation.ErrOperationStale), errors.Is(err, remediationinvestigation.ErrOperationInactive):
		status, message = http.StatusConflict, "the displayed cause is stale"
	}
	http.Error(w, message, status)
}
