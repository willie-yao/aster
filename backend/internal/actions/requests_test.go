package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func requestTestService(t *testing.T) (*Service, models.PatternAnalysis) {
	t.Helper()
	dataDir := t.TempDir()
	pattern := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{
		JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pattern},
	})
	cfg := &project.Config{
		Branding: project.Branding{SiteURL: "https://dash.example.com", SourceRepo: project.SourceRepo{Owner: "o", Name: "r"}},
		Issues:   &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}},
	}
	return NewService(cfg, dataDir, AIConfig{}), pattern
}

func waitRequest(t *testing.T, service *Service, id, owner string, want ...string) ActionRequestView {
	t.Helper()
	allowed := map[string]bool{}
	for _, status := range want {
		allowed[status] = true
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := service.GetRequest(id, owner)
		if err == nil && allowed[view.Status] {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := service.GetRequest(id, owner)
	t.Fatalf("request did not reach %v: view=%+v err=%v", want, view, err)
	return ActionRequestView{}
}

func TestAsyncIssueRequestPersistsAndNotifies(t *testing.T) {
	service, pattern := requestTestService(t)
	notified := make(chan ActionRequestView, 1)
	service.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})

	created, err := service.CreateRequest(pattern.ID, "create-issue", "Alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != RequestPending || created.Owner != "alice" {
		t.Fatalf("created = %+v", created)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Kind != "issue" || ready.Preview.Title == "" {
		t.Fatalf("ready = %+v", ready)
	}
	select {
	case got := <-notified:
		if got.ID != created.ID || got.Status != RequestReady {
			t.Fatalf("notification = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("draft-ready notifier was not called")
	}
	ready = waitRequest(t, service, created.ID, "alice", RequestReady)
	if !ready.EmailSent {
		t.Fatalf("email status not persisted: %+v", ready)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	persisted, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || persisted.Status != RequestReady || persisted.Preview == nil {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if _, err := reloaded.GetRequest(created.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-owner lookup err=%v", err)
	}
}

func TestCreateRequestSupersedesReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "mention IPv6", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == created.ID || replacement.Status != RequestPending {
		t.Fatalf("replacement = %+v", replacement)
	}
	old, err := service.GetRequest(created.ID, "alice")
	if err != nil || old.Status != RequestCancelled || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v err=%v", old, err)
	}
	waitRequest(t, service, replacement.ID, "alice", RequestReady)

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	old, err = reloaded.GetRequest(created.ID, "alice")
	if err != nil || old.Status != RequestCancelled || old.SupersededBy != replacement.ID {
		t.Fatalf("persisted superseded=%+v err=%v", old, err)
	}
	next, err := reloaded.GetRequest(replacement.ID, "alice")
	if err != nil || next.Status != RequestReady {
		t.Fatalf("persisted replacement=%+v err=%v", next, err)
	}
}

func TestCreateRequestSupersedesPendingRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	notified := make(chan string, 2)
	service.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view.ID
		return nil
	})
	now := time.Now().UTC()
	const blockedID = "blocked-request"
	service.rmu.Lock()
	service.requests.Requests[blockedID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: blockedID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice",
		Status: RequestPending, CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	if err := service.saveRequestsLocked(); err != nil {
		service.rmu.Unlock()
		t.Fatal(err)
	}
	service.rmu.Unlock()

	started := make(chan struct{})
	generatorDone := make(chan struct{})
	go service.generateRequestWith(blockedID, "token", func(ctx context.Context, _, _, _, _ string) (PreviewResult, *previewEntry, error) {
		close(started)
		<-ctx.Done()
		close(generatorDone)
		return PreviewResult{}, nil, ctx.Err()
	})
	<-started

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "replacement", blockedID)
	if err != nil {
		t.Fatal(err)
	}
	<-generatorDone
	ready := waitRequest(t, service, replacement.ID, "alice", RequestReady)
	if ready.Preview == nil {
		t.Fatalf("replacement=%+v", ready)
	}
	old, err := service.GetRequest(blockedID, "alice")
	if err != nil || old.Status != RequestCancelled || old.Preview != nil || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v err=%v", old, err)
	}
	select {
	case id := <-notified:
		if id != replacement.ID {
			t.Fatalf("notification request=%q, want %q", id, replacement.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement notification was not sent")
	}
	select {
	case id := <-notified:
		t.Fatalf("unexpected notification for %q", id)
	case <-time.After(100 * time.Millisecond):
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.rmu.Lock()
		_, running := service.requestCancels[blockedID]
		service.rmu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("superseded generator did not stop")
}

func TestCreateRequestSupersedesDifferentAction(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	replacement, err := service.CreateRequest(pattern.ID, "propose-fix", "alice", "token", "", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := service.GetRequest(created.ID, "alice")
	if err != nil || old.Status != RequestCancelled || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v err=%v", old, err)
	}
	if replacement.Kind != "propose-fix" || replacement.Status != RequestPending {
		t.Fatalf("replacement=%+v", replacement)
	}
	waitRequest(t, service, replacement.ID, "alice", RequestFailed, RequestReady)
}

func TestCreateRequestRejectsDifferentFailureSupersede(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	service.rmu.Lock()
	service.requests.Requests[created.ID].FailureID = "another-failure"
	service.rmu.Unlock()

	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID); err == nil || !strings.Contains(err.Error(), "does not match failure") {
		t.Fatalf("CreateRequest() err=%v", err)
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestReady {
		t.Fatalf("original=%+v err=%v", view, err)
	}
}

func TestPendingRequestBecomesFailedAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-1": {ActionRequestView: ActionRequestView{
			ID: "request-1", Owner: "alice", Status: RequestPending,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-1", "alice")
	if err != nil || view.Status != RequestFailed || view.Error == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestCancelReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	if err := service.CancelRequest(created.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestConfigureAsyncRequestsRetriesPersistedReadyEmail(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-ready": {ActionRequestView: ActionRequestView{
			ID: "request-ready", Owner: "alice", Kind: "create-issue", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview:   &PreviewResult{Kind: "issue", Title: "Ready", Body: "Body"},
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	notified := make(chan ActionRequestView, 1)
	reloaded.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})
	select {
	case view := <-notified:
		if view.ID != "request-ready" {
			t.Fatalf("notification = %+v", view)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted ready request was not retried")
	}
	view := waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	deadline := time.Now().Add(time.Second)
	for !view.EmailSent && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		view = waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	}
	if !view.EmailSent {
		t.Fatalf("email status = %+v", view)
	}
}

func TestConfigureAsyncRequestsSkipsExpiredReadyEmail(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-expired": {ActionRequestView: ActionRequestView{
			ID: "request-expired", Owner: "alice", Kind: "create-issue", Status: RequestReady,
			CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
			ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339),
			Preview:   &PreviewResult{Kind: "issue", Title: "Expired", Body: "Body"},
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	notified := make(chan ActionRequestView, 1)
	reloaded.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})
	select {
	case view := <-notified:
		t.Fatalf("expired request was notified: %+v", view)
	case <-time.After(100 * time.Millisecond):
	}
	view, err := reloaded.GetRequest("request-expired", "alice")
	if err != nil || view.Status != RequestExpired || view.Preview != nil {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestCancelRequestPreservesTerminalStatus(t *testing.T) {
	for _, status := range []string{RequestFailed, RequestConfirmed, RequestCancelled, RequestExpired} {
		t.Run(status, func(t *testing.T) {
			service, _ := requestTestService(t)
			now := time.Now().UTC()
			service.requests.Requests[status] = &actionRequest{ActionRequestView: ActionRequestView{
				ID: status, Owner: "alice", Status: status,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			}}
			if err := service.CancelRequest(status, "alice"); err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("CancelRequest() err=%v, want status %q", err, status)
			}
			view, err := service.GetRequest(status, "alice")
			if err != nil || view.Status != status {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}

func TestCancelRequestRejectsConfirmationInProgress(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	service.requests.Requests["request-ready"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "request-ready", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestConfirms["request-ready"] = struct{}{}

	if err := service.CancelRequest("request-ready", "alice"); err == nil || !strings.Contains(err.Error(), "being confirmed") {
		t.Fatalf("CancelRequest() err=%v", err)
	}
	view, err := service.GetRequest("request-ready", "alice")
	if err != nil || view.Status != RequestReady {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestConfirmedRequestExpiresAndClearsDraft(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-confirmed": {
			ActionRequestView: ActionRequestView{
				ID: "request-confirmed", Owner: "alice", Kind: "propose-fix", Status: RequestConfirmed,
				CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339),
				ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339), ResultURL: "https://github.com/o/r/pull/1",
				Preview: &PreviewResult{Kind: "fix", Title: "Fix", Body: "Description", Diff: "secret diff"},
			},
			Instruction: "private instruction",
			Issue:       &issues.IssueSpec{Key: "key", Title: "Issue", Body: "private issue body"},
			Fix:         &fixpr.GeneratedFixSnapshot{Title: "Fix", Diff: "private diff", Files: map[string]string{"main.go": "private source"}},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-confirmed", "alice")
	if err != nil || view.Status != RequestExpired || view.Preview != nil || view.ResultURL == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["request-confirmed"]
	if persisted.Instruction != "" || persisted.Issue != nil || persisted.Fix != nil {
		t.Fatalf("expired draft payload retained: %+v", persisted)
	}
	reloaded.rmu.Unlock()
}
