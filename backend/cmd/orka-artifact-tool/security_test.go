package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	handler := requireBearer("secret", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name   string
		header string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong", header: "Bearer nope", status: http.StatusUnauthorized},
		{name: "valid", header: "Bearer secret", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/tool/read", nil)
			req.Header.Set("Authorization", tc.header)
			recorder := httptest.NewRecorder()
			handler(recorder, req)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
		})
	}
}

func TestLoadAuthTokenRequiresCredential(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "")
	t.Setenv("AUTH_TOKEN_FILE", "")
	if _, err := loadAuthToken(); err == nil {
		t.Fatal("missing token was accepted")
	}
	t.Setenv("AUTH_TOKEN", "secret")
	if got, err := loadAuthToken(); err != nil || got != "secret" {
		t.Fatalf("token = %q, err = %v", got, err)
	}
}
