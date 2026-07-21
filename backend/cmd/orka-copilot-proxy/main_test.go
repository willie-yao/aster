package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRejectsOversizedBody(t *testing.T) {
	h := &handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", 8<<20+1)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestNormalizeCopilotErrorResponse(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantCode    string
		wantWrapped bool
	}{
		{
			name: "bare Copilot error", status: http.StatusBadRequest,
			body:     `{"message":"The requested model is not supported.","code":"model_not_supported","type":"invalid_request_error"}`,
			wantCode: "model_not_supported", wantWrapped: true,
		},
		{
			name: "existing OpenAI envelope", status: http.StatusBadRequest,
			body:     `{"error":{"message":"bad request","code":"invalid_request"}}`,
			wantCode: "invalid_request", wantWrapped: true,
		},
		{
			name: "successful response", status: http.StatusOK,
			body: `{"id":"resp-1"}`, wantWrapped: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			if err := normalizeCopilotErrorResponse(resp); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			errorPayload, wrapped := decoded["error"].(map[string]any)
			if wrapped != tc.wantWrapped {
				t.Fatalf("response = %s, wrapped = %v", raw, wrapped)
			}
			if tc.wantCode != "" && errorPayload["code"] != tc.wantCode {
				t.Fatalf("response = %s, code = %v", raw, errorPayload["code"])
			}
		})
	}
}
