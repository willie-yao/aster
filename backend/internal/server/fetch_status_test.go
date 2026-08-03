package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
)

func serverFetchStatus(now time.Time) fetchprogress.Status {
	return fetchprogress.Status{
		SchemaVersion: fetchprogress.SchemaVersion,
		RunID:         "safe-run", PassID: "safe-pass", PassType: fetchprogress.PassLightweightWatch,
		EngineVersion: "sha-safe", Phase: fetchprogress.PhaseAnalysis,
		RunStartedAt: now.Add(-time.Hour), PassStartedAt: now.Add(-time.Minute),
		PhaseStartedAt: now.Add(-30 * time.Second), LastProgressAt: now,
		Outcome: fetchprogress.OutcomeRunning,
		Jobs:    fetchprogress.JobProgress{Total: 28, Completed: 28},
		Builds:  fetchprogress.BuildProgress{Cached: 241, Fetched: 29},
		Analyses: fetchprogress.AnalysisProgress{
			LogicalTotal: 61, CompatibleResultsReused: 4, ExactResultsReused: 2,
			SameFailureGroups: 3, SameFailureCandidates: 9, PotentialTasksSaved: 6, LargestSameFailureGroup: 4, SameFailureReused: 5,
			NewWork: 20, StaleWork: 3, Queued: 35, Running: 2, Completed: 24, Retries: 3,
			NewTasksCreated: 4, FreshAnalysesCompleted: 2,
			CacheRejections: fetchprogress.CacheRejectionProgress{Missing: 20, Critique: 3},
		},
		Patterns: fetchprogress.PatternProgress{
			Eligible: 2, Completed: 1, Failed: 1, Attempts: 3, Retries: 1,
			FailureCategory: fetchprogress.PatternFailureAmbiguous,
		},
		PatternPhase:     fetchprogress.StagePending,
		PublicationPhase: fetchprogress.StagePending,
		SideEffectPhase:  fetchprogress.StagePending,
	}
}

func TestFetchStatusEndpointAuthenticationMethodsAndPrivacy(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	status := serverFetchStatus(now)
	status.CurrentTasks = []fetchprogress.TaskMapping{{WorkItem: "safe-work", TaskName: "private-task-name", Phase: "Running"}}
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	history := fetchprogress.History{SchemaVersion: fetchprogress.HistorySchemaVersion, Passes: []fetchprogress.PassSummary{{
		RunID: "safe-run", PassID: "previous-pass", PassType: fetchprogress.PassLightweightWatch,
		StartedAt: now.Add(-2 * time.Minute), CompletedAt: now.Add(-time.Minute),
		LogicalCount: 3, CompatibleResultsReused: 1, ExactResultsReused: 1,
		SameFailureGroups: 1, SameFailureCandidates: 2, PotentialTasksSaved: 1, LargestSameFailureGroup: 2, SameFailureReused: 1,
		NewTasksCreated: 1, FreshAnalysesCompleted: 1,
		TaskAttempts: 4, Retries: 1, Outcome: fetchprogress.OutcomeSucceeded, Published: true,
	}}}
	if err := fetchprogress.WriteHistory(fetchprogress.HistoryPath(dataDir), history); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/fetch-status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/fetch-status", nil)
	req.Header.Set("Authorization", "ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("GET status=%d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var got fetchStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !got.Available || got.State != "active" || got.Status == nil || got.Status.Analyses.Retries != 3 || got.Status.Analyses.CompatibleResultsReused != 4 || got.Status.Analyses.ExactResultsReused != 2 || got.Status.Analyses.PotentialTasksSaved != 6 || got.Status.Analyses.LargestSameFailureGroup != 4 || got.Status.Analyses.SameFailureReused != 5 || got.Status.Analyses.NewTasksCreated != 4 || got.Status.Analyses.FreshAnalysesCompleted != 2 || got.Status.Analyses.CacheRejections.Missing != 20 ||
		got.Status.Patterns.Attempts != 3 || got.Status.Patterns.FailureCategory != fetchprogress.PatternFailureAmbiguous {
		t.Fatalf("GET response = %+v", got)
	}
	if len(got.Status.CurrentTasks) != 0 || got.HistorySchemaVersion != fetchprogress.HistorySchemaVersion || len(got.History) != 1 || got.History[0].DurationMS != int64(time.Minute/time.Millisecond) || got.History[0].ExactResultsReused != 1 || got.History[0].PotentialTasksSaved != 1 || got.History[0].LargestSameFailureGroup != 2 || got.History[0].SameFailureReused != 1 || got.History[0].NewTasksCreated != 1 || got.History[0].FreshAnalysesCompleted != 1 {
		t.Fatalf("safe status/history response = %+v", got)
	}
	body, _ := json.Marshal(got)
	for _, sensitive := range []string{"/private/", "token-value", "provider-body", "test-name", "job-name", "build-id", "private-task-name", "previous-pass"} {
		if string(body) != "" && bytes.Contains(body, []byte(sensitive)) {
			t.Fatalf("response exposed %q: %s", sensitive, body)
		}
	}

	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/api/fetch-status", nil)
	req.Header.Set("Authorization", "ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	headBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Fatalf("HEAD status=%d body=%q", resp.StatusCode, headBody)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/fetch-status", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status=%d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	resp, err = http.Get(srv.URL + "/data/.fetch-status/status.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("private status file = %d, want 404", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/data/.fetch-status/history.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("private history file = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !caps.Features.FetchStatus {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestFetchStatusEndpointReturnsOnlyRecentPasses(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 7, 31, 18, 0, 0, 0, time.UTC)
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), serverFetchStatus(now)); err != nil {
		t.Fatal(err)
	}
	history := fetchprogress.History{SchemaVersion: fetchprogress.HistorySchemaVersion}
	for i := 0; i < fetchStatusRecentPassLimit+2; i++ {
		history.Passes = append(history.Passes, fetchprogress.PassSummary{
			RunID: "safe-run", PassID: fmt.Sprintf("pass-%02d", i), PassType: fetchprogress.PassLightweightWatch,
			StartedAt: now.Add(time.Duration(i) * time.Minute), CompletedAt: now.Add(time.Duration(i)*time.Minute + time.Second),
			Outcome: fetchprogress.OutcomeSucceeded,
		})
	}
	if err := fetchprogress.WriteHistory(fetchprogress.HistoryPath(dataDir), history); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	fetchStatusHandlerWithClock(dataDir, func() time.Time { return now }, time.Minute).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/fetch-status", nil))
	var got fetchStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != fetchStatusRecentPassLimit || !got.History[0].StartedAt.Equal(now.Add(2*time.Minute)) || !got.History[len(got.History)-1].StartedAt.Equal(now.Add(11*time.Minute)) {
		t.Fatalf("recent history = %+v", got.History)
	}
}

func TestFetchStatusHandlerMissingCorruptStaleAndInterrupted(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	handler := fetchStatusHandlerWithClock(dataDir, func() time.Time { return now }, time.Minute)
	read := func() fetchStatusResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/fetch-status", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		var response fetchStatusResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	if got := read(); got.Available || got.State != "missing" {
		t.Fatalf("missing response = %+v", got)
	}
	if err := os.MkdirAll(filepath.Dir(fetchprogress.Path(dataDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fetchprogress.Path(dataDir), []byte(`{"schema_version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Available || got.State != "unavailable" {
		t.Fatalf("corrupt response = %+v", got)
	}

	status := serverFetchStatus(now.Add(-2 * time.Minute))
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.Available || got.State != "stale" || !got.Stale {
		t.Fatalf("stale response = %+v", got)
	}

	status.LastProgressAt = now
	status.Phase = fetchprogress.PhaseInterrupted
	status.Outcome = fetchprogress.OutcomeInterrupted
	status.FailureCategory = fetchprogress.FailureInterrupted
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.Available || got.State != "interrupted" || got.Stale {
		t.Fatalf("interrupted response = %+v", got)
	}

	nextWatch := now.Add(-2 * time.Minute)
	status.Phase = fetchprogress.PhaseIdle
	status.Outcome = fetchprogress.OutcomeSucceeded
	status.FailureCategory = fetchprogress.FailureNone
	status.NextWatchAt = &nextWatch
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "stale" || !got.Stale {
		t.Fatalf("overdue idle response = %+v", got)
	}
}

func TestFetchStatusHandlerReadsEachSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	handler := fetchStatusHandlerWithClock(dataDir, func() time.Time { return now }, time.Minute)
	status := serverFetchStatus(now)
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	read := func() fetchStatusResponse {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/fetch-status", nil))
		var response fetchStatusResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if got := read(); got.Status == nil || got.Status.Phase != fetchprogress.PhaseAnalysis {
		t.Fatalf("first read = %+v", got)
	}
	status.Phase = fetchprogress.PhaseIdle
	status.Outcome = fetchprogress.OutcomeSucceeded
	status.LastProgressAt = now
	if err := fetchprogress.Write(fetchprogress.Path(dataDir), status); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "idle" || got.Status == nil || got.Status.Phase != fetchprogress.PhaseIdle {
		t.Fatalf("second read = %+v", got)
	}
}
