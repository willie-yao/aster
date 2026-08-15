package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
)

func TestPatternDiagnosticsEndpointIsAuthenticatedAndSanitized(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	cache := map[string]any{
		"pattern-failure:kubernetes:g:generation:abcdef": map[string]any{
			"key":        "pattern-failure:kubernetes:g:generation:abcdef",
			"created_at": now.Add(-time.Minute),
			"data": map[string]any{
				"version": 1, "job_id": "periodic-safe", "category": "schema",
				"failed_at": now.Add(-time.Minute), "retry_after": now.Add(time.Hour),
				"stage": "extraction", "validation_category": "schema", "validation_code": "unsafe_conversion_remediation",
				"candidate_count": 1, "contract_like_rejected_count": 1,
				"repair_stage": "validation", "repair_validation_code": "privatecredentialvalue", "repair_count": 1,
			},
		},
		"unrelated": map[string]any{
			"key": "unrelated", "created_at": now.Add(-time.Minute), "data": map[string]any{"private": "PRIVATE_MODEL_PROSE"},
		},
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ai.CacheFilename), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/pattern-diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/pattern-diagnostics", nil)
	req.Header.Set("Authorization", "ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("status=%d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var snapshot ai.PatternFailureDiagnosticsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].JobID != "periodic-safe" || snapshot.Entries[0].ValidationCode != "unsafe_conversion_remediation" || snapshot.Entries[0].RepairValidationCode != "" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "PRIVATE_MODEL_PROSE") || strings.Contains(string(encoded), "unrelated") {
		t.Fatalf("private cache content leaked: %s", encoded)
	}

	resp, err = http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Features.PatternDiagnostics {
		t.Fatalf("capabilities=%+v", caps.Features)
	}
}

func TestPatternDiagnosticsHandlerMissingAndMalformed(t *testing.T) {
	now := func() time.Time { return time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC) }
	for _, testCase := range []struct {
		name   string
		write  string
		status int
	}{
		{name: "missing", status: http.StatusNotFound},
		{name: "malformed", write: `{`, status: http.StatusInternalServerError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			if testCase.write != "" {
				if err := os.WriteFile(filepath.Join(dir, ai.CacheFilename), []byte(testCase.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			recorder := httptest.NewRecorder()
			patternDiagnosticsHandler(dir, now).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/pattern-diagnostics", nil))
			if recorder.Code != testCase.status || testCase.write != "" && strings.Contains(recorder.Body.String(), testCase.write) {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}
