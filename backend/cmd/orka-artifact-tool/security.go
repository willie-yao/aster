package main

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func loadAuthToken() (string, error) {
	token := strings.TrimSpace(os.Getenv("AUTH_TOKEN"))
	if file := strings.TrimSpace(os.Getenv("AUTH_TOKEN_FILE")); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read AUTH_TOKEN_FILE: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return "", fmt.Errorf("AUTH_TOKEN or AUTH_TOKEN_FILE is required")
	}
	return token, nil
}

func requireBearer(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, provided, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		provided = strings.TrimSpace(provided)
		if !ok || !strings.EqualFold(scheme, "Bearer") || len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
