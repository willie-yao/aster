package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportErrorsExcludeProviderResponseBody(t *testing.T) {
	const sentinel = "PRIVATE_TRANSPORT_RESPONSE_SENTINEL"
	for _, testCase := range []struct {
		name   string
		api    string
		status int
		body   string
	}{
		{name: "chat HTTP error", api: APIChatCompletions, status: http.StatusBadRequest, body: sentinel},
		{name: "chat decode error", api: APIChatCompletions, status: http.StatusOK, body: sentinel},
		{name: "responses HTTP error", api: APIResponses, status: http.StatusBadRequest, body: sentinel},
		{name: "responses decode error", api: APIResponses, status: http.StatusOK, body: sentinel},
		{name: "responses incomplete", api: APIResponses, status: http.StatusOK, body: `{"id":"r","status":"incomplete","private":"` + sentinel + `"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Request-Id", "request-123")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			client := NewClientWithOptions(Options{API: testCase.api, Endpoint: server.URL, Model: "model"})
			_, err := client.callModel(context.Background(), nil, nil, nil)
			if err == nil {
				t.Fatal("provider failure returned nil error")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("provider response body leaked in error: %v", err)
			}
			if testCase.status != http.StatusOK && (!strings.Contains(err.Error(), "returned 400") || !strings.Contains(err.Error(), "request_id=request-123")) {
				t.Fatalf("HTTP error lost safe metadata: %v", err)
			}
		})
	}
}
