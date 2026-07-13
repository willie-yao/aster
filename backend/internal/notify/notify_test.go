package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestStateSaveLoad(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	n := NewNotifier("http://example.com", stateFile, "https://dash.example.com", "https://prow.example.com/view/")
	if len(n.state.Notified) != 0 {
		t.Fatal("expected empty state on first load")
	}

	n.state.Notified["job1::test1"] = NotifiedFailure{
		FirstNotifiedAt:  "2024-01-01T00:00:00Z",
		ConsecutiveCount: 5,
		ErrorHash:        "abc123",
		JobName:          "job1",
		TestName:         "test1",
	}
	if err := n.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	n2 := NewNotifier("http://example.com", stateFile, "https://dash.example.com", "https://prow.example.com/view/")
	if len(n2.state.Notified) != 1 {
		t.Fatalf("expected 1 notified entry, got %d", len(n2.state.Notified))
	}
	nf := n2.state.Notified["job1::test1"]
	if nf.ErrorHash != "abc123" || nf.ConsecutiveCount != 5 {
		t.Fatalf("unexpected state: %+v", nf)
	}
}

func TestStateLoadMissingFile(t *testing.T) {
	n := NewNotifier("http://example.com", "/nonexistent/state.json", "https://dash.example.com", "https://prow.example.com/view/")
	if len(n.state.Notified) != 0 {
		t.Fatal("expected empty state when file doesn't exist")
	}
}

func makeReport(failures []models.TestFlakiness) models.FlakinessReport {
	return models.FlakinessReport{
		GeneratedAt:        "2024-01-01T00:00:00Z",
		PersistentFailures: failures,
	}
}

func TestNewPersistentFailureDetection(t *testing.T) {
	var received []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestSomething",
			JobID:               "my-job",
			JobName:             "my-job",
			ConsecutiveFailures: 5,
			LastFailure: &models.TestFailureInfo{
				BuildID:        "100",
				FailureMessage: "context deadline exceeded",
				ErrorHash:      "hash1",
			},
		},
	})

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	if stats.NewAlerts != 1 {
		t.Fatalf("expected 1 new alert, got %d", stats.NewAlerts)
	}
	if stats.Recoveries != 0 {
		t.Fatalf("expected 0 recoveries, got %d", stats.Recoveries)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 webhook call, got %d", len(received))
	}

	nf, ok := n.state.Notified["my-job::TestSomething"]
	if !ok {
		t.Fatal("expected state entry for my-job::TestSomething")
	}
	if nf.ErrorHash != "hash1" {
		t.Fatalf("expected error hash 'hash1', got %q", nf.ErrorHash)
	}
}

func TestRecoveryDetection(t *testing.T) {
	var received []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	n := NewNotifier(srv.URL, stateFile, "https://dash.example.com", "https://prow.example.com/view/")
	n.state.Notified["my-job::TestRecovered"] = NotifiedFailure{
		FirstNotifiedAt:  "2024-01-01T00:00:00Z",
		ConsecutiveCount: 4,
		ErrorHash:        "oldhash",
		JobName:          "my-job",
		TestName:         "TestRecovered",
	}

	// Report with no persistent failures triggers recovery.
	report := makeReport(nil)
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	if stats.Recoveries != 1 {
		t.Fatalf("expected 1 recovery, got %d", stats.Recoveries)
	}
	if stats.NewAlerts != 0 {
		t.Fatalf("expected 0 new alerts, got %d", stats.NewAlerts)
	}
	if len(n.state.Notified) != 0 {
		t.Fatal("expected state entry to be removed after recovery")
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 webhook call, got %d", len(received))
	}
}

func TestErrorHashChangeDetection(t *testing.T) {
	var received []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")
	n.state.Notified["my-job::TestChanged"] = NotifiedFailure{
		FirstNotifiedAt:  "2024-01-01T00:00:00Z",
		ConsecutiveCount: 3,
		ErrorHash:        "oldhash",
		JobName:          "my-job",
		TestName:         "TestChanged",
	}

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestChanged",
			JobID:               "my-job",
			JobName:             "my-job",
			ConsecutiveFailures: 4,
			LastFailure: &models.TestFailureInfo{
				BuildID:        "200",
				FailureMessage: "new error message",
				ErrorHash:      "newhash",
			},
		},
	})

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	if stats.NewAlerts != 1 {
		t.Fatalf("expected 1 new alert (hash change), got %d", stats.NewAlerts)
	}
	if n.state.Notified["my-job::TestChanged"].ErrorHash != "newhash" {
		t.Fatal("expected state to be updated with new hash")
	}
}

func TestDeduplication(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")
	n.state.Notified["my-job::TestSame"] = NotifiedFailure{
		FirstNotifiedAt:  "2024-01-01T00:00:00Z",
		ConsecutiveCount: 3,
		ErrorHash:        "samehash",
		JobName:          "my-job",
		TestName:         "TestSame",
	}

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestSame",
			JobID:               "my-job",
			JobName:             "my-job",
			ConsecutiveFailures: 5,
			LastFailure: &models.TestFailureInfo{
				ErrorHash: "samehash",
			},
		},
	})

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	if stats.NewAlerts != 0 {
		t.Fatalf("expected 0 alerts (de-duplicated), got %d", stats.NewAlerts)
	}
	if stats.Recoveries != 0 {
		t.Fatalf("expected 0 recoveries, got %d", stats.Recoveries)
	}
	if callCount != 0 {
		t.Fatalf("expected 0 webhook calls, got %d", callCount)
	}
}

func TestWebhookPOSTFormat(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %s", ct)
		}
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestWebhook",
			JobName:             "webhook-job",
			ConsecutiveFailures: 3,
			LastFailure: &models.TestFailureInfo{
				BuildID:        "999",
				FailureMessage: "something broke",
				ErrorHash:      "webhookhash",
			},
		},
	})

	_, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}

	blocks, ok := receivedBody["blocks"].([]interface{})
	if !ok || len(blocks) == 0 {
		t.Fatal("expected blocks array")
	}

	header := blocks[0].(map[string]interface{})
	if header["type"] != "header" {
		t.Fatalf("expected header type, got %v", header["type"])
	}
	headerText := header["text"].(map[string]interface{})
	if !strings.Contains(headerText["text"].(string), "Persistent Test Failure") {
		t.Fatalf("expected failure header, got %v", headerText["text"])
	}

	raw, _ := json.Marshal(receivedBody)
	rawStr := string(raw)
	if !contains(rawStr, "View on Dashboard") {
		t.Fatal("expected 'View on Dashboard' button")
	}
}

func TestGracefulEmptyWebhookURL(t *testing.T) {
	dir := t.TempDir()
	n := NewNotifier("", filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestNoWebhook",
			JobName:             "some-job",
			ConsecutiveFailures: 5,
			LastFailure: &models.TestFailureInfo{
				ErrorHash: "hash",
			},
		},
	})

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	// Empty webhook still records a new alert because postWebhook returns nil.
	if stats.NewAlerts != 1 {
		t.Fatalf("expected 1 new alert, got %d", stats.NewAlerts)
	}
}

func TestBelowThresholdIgnored(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")

	// ConsecutiveFailures = 2, below threshold of 3.
	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestFlaky",
			JobName:             "flaky-job",
			ConsecutiveFailures: 2,
			LastFailure: &models.TestFailureInfo{
				ErrorHash: "hash",
			},
		},
	})

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}
	if stats.NewAlerts != 0 {
		t.Fatalf("expected 0 alerts for below-threshold failure, got %d", stats.NewAlerts)
	}
	if callCount != 0 {
		t.Fatalf("expected 0 webhook calls, got %d", callCount)
	}
}

func TestAILookup(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")

	report := makeReport([]models.TestFlakiness{
		{
			TestName:            "TestWithAI",
			JobName:             "ai-job",
			ConsecutiveFailures: 4,
			LastFailure: &models.TestFailureInfo{
				BuildID:        "300",
				FailureMessage: "node not ready",
				ErrorHash:      "aihash",
			},
		},
	})

	jobDetails := []models.JobDetail{
		{
			Name: "ai-job",
			Runs: []models.BuildResult{
				{
					TestCases: []models.TestCase{
						{
							Name:   "TestWithAI",
							Status: "failed",
							AISummary: &models.AISummary{
								Summary: "Node readiness check timed out",
							},
							AIAnalysis: &models.AIAnalysis{
								RootCause: "Azure VM provisioning delay",
							},
						},
					},
				},
			},
		},
	}

	_, err := n.ProcessFailures(context.Background(), report, jobDetails)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}

	raw, _ := json.Marshal(receivedBody)
	body := string(raw)
	if !contains(body, "Azure VM provisioning delay") {
		t.Fatal("expected AI root cause in webhook body")
	}
}

func TestRecoveryCardFormat(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	n := NewNotifier(srv.URL, filepath.Join(dir, "state.json"), "https://dash.example.com", "https://prow.example.com/view/")
	n.state.Notified["recovery-job::TestRecoveryCard"] = NotifiedFailure{
		FirstNotifiedAt:  "2024-01-01T00:00:00Z",
		ConsecutiveCount: 7,
		ErrorHash:        "old",
		JobName:          "recovery-job",
		TestName:         "TestRecoveryCard",
	}

	report := makeReport(nil) // no persistent failures, so recovery
	_, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatalf("ProcessFailures: %v", err)
	}

	blocks := receivedBody["blocks"].([]interface{})
	header := blocks[0].(map[string]interface{})
	headerText := header["text"].(map[string]interface{})
	if !strings.Contains(headerText["text"].(string), "Recovery") {
		t.Fatalf("expected recovery header, got %v", headerText["text"])
	}

	raw, _ := json.Marshal(receivedBody)
	if !contains(string(raw), "7 consecutive") {
		t.Fatal("expected consecutive count in recovery card")
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	state := NotificationState{
		Notified: map[string]NotifiedFailure{
			"j1::t1": {
				FirstNotifiedAt:  "2024-06-01T00:00:00Z",
				ConsecutiveCount: 10,
				ErrorHash:        "xyz",
				JobName:          "j1",
				TestName:         "t1",
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(stateFile, data, 0644)

	n := NewNotifier("", stateFile, "https://dash.example.com", "https://prow.example.com/view/")
	if len(n.state.Notified) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(n.state.Notified))
	}

	n.state.Notified["j2::t2"] = NotifiedFailure{
		ErrorHash: "new",
		JobName:   "j2",
		TestName:  "t2",
	}
	if err := n.SaveState(); err != nil {
		t.Fatal(err)
	}

	n2 := NewNotifier("", stateFile, "https://dash.example.com", "https://prow.example.com/view/")
	if len(n2.state.Notified) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(n2.state.Notified))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchContains(s, substr)
}

func searchContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
