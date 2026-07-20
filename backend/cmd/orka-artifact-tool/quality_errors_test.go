package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type qualityErrorBrowser struct{ artifacts.Browser }

func (*qualityErrorBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return nil, -1, errors.New("unreadable")
}

func (*qualityErrorBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return nil, errors.New("unreadable")
}

func TestQualityToolErrorsUseNonSuccessStatus(t *testing.T) {
	env := &toolEnv{browser: &qualityErrorBrowser{}}
	tests := []struct {
		name    string
		body    string
		handler func(*toolEnv, http.ResponseWriter, *http.Request)
		status  int
	}{
		{name: "timeline unreadable", body: `{"path":"missing.log"}`, handler: verifyTimeline, status: http.StatusUnprocessableEntity},
		{name: "transient unreadable", body: `{"paths":["missing.log"]}`, handler: checkTransientSignatures, status: http.StatusUnprocessableEntity},
		{name: "recurrence missing test", body: `{}`, handler: recurrence, status: http.StatusBadRequest},
		{name: "diff missing path", body: `{}`, handler: diffLastPassing, status: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/tool", strings.NewReader(tc.body))
			recorder := httptest.NewRecorder()
			tc.handler(env, recorder, req)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.status, recorder.Body.String())
			}
		})
	}
}
