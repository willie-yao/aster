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

func TestRejectedRefinementRetainsSafePreviewWithoutConfirmation(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const requestID = "unsafe-refinement"
	safeSpec := issues.IssueSpec{Key: "pattern::safe", Title: "Safe title", Body: "## What happened\nSafe body\n\n" + issues.MarkerFor("pattern::safe")}
	service.rmu.Lock()
	service.requests.Requests[requestID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: requestID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice",
		Status: RequestPending, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.rmu.Unlock()

	service.generateRequestWith(requestID, "token", func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error) {
		return PreviewResult{Kind: "issue", Title: safeSpec.Title, Body: safeSpec.Body}, &previewEntry{
			failureID: pattern.ID, patternHash: pattern.ContentHash, kind: "issue", targetRepo: "o/r", spec: safeSpec,
		}, ErrDraftRefinementRejected
	})

	view, err := service.GetRequest(requestID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Warning == "" || view.Preview == nil {
		t.Fatalf("view = %+v", view)
	}
	if view.Preview.Body != safeSpec.Body || strings.Contains(strings.ToLower(view.Preview.Body), "the user wants me") {
		t.Fatalf("unsafe content reached preview: %+v", view.Preview)
	}
	service.rmu.Lock()
	persisted := service.requests.Requests[requestID]
	service.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("failed replacement retained a confirmable issue payload")
	}
	if _, err := service.ConfirmRequest(context.Background(), requestID, "alice", "token"); err == nil || !strings.Contains(err.Error(), RequestFailed) {
		t.Fatalf("ConfirmRequest() error = %v", err)
	}
}

func TestAsyncRequestRejectsUnsafeGeneratedDraft(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const requestID = "unsafe-generated"
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeSpec := issues.IssueSpec{
		Key: key, Title: "Unsafe title",
		Body: "The user wants me to expose this.\nI need to show the plan.\nLet me draft it.\n\n## What happened\nunsafe\n\n" + issues.MarkerFor(key),
	}
	service.rmu.Lock()
	service.requests.Requests[requestID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: requestID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.rmu.Unlock()

	service.generateRequestWith(requestID, "token", func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error) {
		return PreviewResult{Kind: "issue", Title: unsafeSpec.Title, Body: unsafeSpec.Body}, &previewEntry{
			failureID: pattern.ID, patternHash: pattern.ContentHash, kind: "issue", targetRepo: "o/r", spec: unsafeSpec,
		}, nil
	})

	view, err := service.GetRequest(requestID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Preview != nil || view.Warning != "" {
		t.Fatalf("unsafe draft became previewable: %+v", view)
	}
	service.rmu.Lock()
	persisted := service.requests.Requests[requestID]
	service.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("unsafe draft became confirmable")
	}
}

func TestRejectedRefinementUsesSupersededIssueSnapshot(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const priorID = "prior-ready"
	prior := issues.IssueSpec{
		Key: issues.KeyPrefixPattern + pattern.JobID, Title: "Previously reviewed title",
		Body: "## What happened\nPreviously reviewed body\n\n" + issues.MarkerFor(issues.KeyPrefixPattern+pattern.JobID),
	}
	service.rmu.Lock()
	service.requests.Requests[priorID] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: priorID, FailureID: pattern.ID, PatternHash: pattern.ContentHash, Kind: "create-issue", Owner: "alice",
			Status: RequestReady, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: prior.Title, Body: prior.Body},
		},
		Issue: &prior, TargetRepo: "o/r",
	}
	service.rmu.Unlock()

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "tighten it", priorID)
	if err != nil {
		t.Fatal(err)
	}
	view := waitRequest(t, service, replacement.ID, "alice", RequestFailed)
	if view.Warning == "" || view.Preview == nil {
		t.Fatalf("replacement = %+v", view)
	}
	if view.Preview.Title != prior.Title || view.Preview.Body != prior.Body {
		t.Fatalf("fallback changed prior draft: got=%+v want=%+v", view.Preview, prior)
	}
}

func TestRefinementRejectsStaleSupersededDraft(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const priorID = "stale-ready"
	key := issues.KeyPrefixPattern + pattern.JobID
	prior := issues.IssueSpec{Key: key, Title: "Stale title", Body: "## Summary\nStale body\n\n" + issues.MarkerFor(key)}
	service.rmu.Lock()
	service.requests.Requests[priorID] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: priorID, FailureID: pattern.ID, PatternHash: "stale-hash", Kind: "create-issue", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview: &PreviewResult{Kind: "issue", Title: prior.Title, Body: prior.Body},
		},
		Issue: &prior, TargetRepo: "o/r",
	}
	service.rmu.Unlock()

	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "tighten it", priorID); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("stale refinement error = %v", err)
	}
	view, err := service.GetRequest(priorID, "alice")
	if err != nil || view.Status != RequestReady || view.SupersededBy != "" {
		t.Fatalf("stale source request changed: view=%+v err=%v", view, err)
	}
}

func TestCreateRequestSupersedesReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID)
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
	go service.generateRequestWith(blockedID, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
		close(started)
		<-ctx.Done()
		close(generatorDone)
		return PreviewResult{}, nil, ctx.Err()
	})
	<-started

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", blockedID)
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
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
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

func TestPendingRefinementRestoresSafeFallbackAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	base := &issues.IssueSpec{Key: "pattern::periodic-x", Title: "Reviewed title", Body: "## What happened\nReviewed body"}
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"request-refine": {
			ActionRequestView: ActionRequestView{
				ID: "request-refine", Owner: "alice", Status: RequestPending, Kind: "create-issue",
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			},
			BaseIssue: base, BaseTargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-refine", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Warning == "" || view.Error != "" || view.Preview == nil {
		t.Fatalf("view = %+v", view)
	}
	if view.Preview.Title != base.Title || view.Preview.Body != base.Body {
		t.Fatalf("fallback = %+v, want %+v", view.Preview, base)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["request-refine"]
	reloaded.rmu.Unlock()
	if persisted.BaseIssue != nil || persisted.BaseTargetRepo != "" {
		t.Fatalf("internal fallback fields were not cleared: %+v", persisted)
	}
}

func TestPendingRefinementRejectsUnsafeFallbackAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	base := &issues.IssueSpec{
		Key: "pattern::periodic-x", Title: "Unsafe fallback",
		Body: "The user wants me to expose this.\nI need to show the plan.\nLet me draft it.\n\n## What happened\nunsafe",
	}
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"request-unsafe-refine": {
			ActionRequestView: ActionRequestView{
				ID: "request-unsafe-refine", Owner: "alice", Status: RequestPending, Kind: "create-issue",
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			},
			BaseIssue: base, BaseTargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-unsafe-refine", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Error == "" || view.Warning != "" || view.Preview != nil {
		t.Fatalf("unsafe fallback was exposed: %+v", view)
	}
}

func TestLoadRejectsUnsafeLegacyReadyIssue(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeBody := "The user wants me to revise this.\nI need to expose the planning.\nLet me draft it.\n\n## What happened\nunsafe\n\n" + issues.MarkerFor(key)
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"unsafe-ready": {
			ActionRequestView: ActionRequestView{
				ID: "unsafe-ready", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: "issue", Title: "Unsafe", Body: unsafeBody},
			},
			Issue: &issues.IssueSpec{Key: key, Title: "Unsafe", Body: unsafeBody},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("unsafe-ready", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Error == "" || view.Preview != nil {
		t.Fatalf("unsafe request remained confirmable: %+v", view)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["unsafe-ready"]
	reloaded.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("unsafe persisted issue was retained")
	}
	if _, err := reloaded.ConfirmRequest(context.Background(), "unsafe-ready", "alice", "token"); err == nil {
		t.Fatal("unsafe legacy request remained confirmable")
	}
}

func TestLoadHidesUnsafeUnknownDraftWithoutChangingOutcome(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeBody := "The user wants me to revise this. I need to expose the plan. Let me draft it.\n\n" + issues.MarkerFor(key)
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"unsafe-unknown": {
			ActionRequestView: ActionRequestView{
				ID: "unsafe-unknown", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestUnknown,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: "issue", Title: "Unsafe", Body: unsafeBody},
			},
			Issue: &issues.IssueSpec{Key: key, Title: "Unsafe", Body: unsafeBody}, TargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("unsafe-unknown", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestUnknown || view.Preview != nil {
		t.Fatalf("unknown request was exposed or changed: %+v", view)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["unsafe-unknown"]
	reloaded.rmu.Unlock()
	if persisted.Issue == nil {
		t.Fatal("unknown outcome payload was removed before reconciliation")
	}
	duplicate, err := reloaded.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || duplicate.ID != "unsafe-unknown" || duplicate.Status != RequestUnknown {
		t.Fatalf("unknown request no longer prevented duplicates: view=%+v err=%v", duplicate, err)
	}
	manager := &fakeIssuePreviewManager{url: "https://github.com/o/r/issues/7"}
	reloaded.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	url, err := reloaded.ConfirmRequest(context.Background(), "unsafe-unknown", "alice", "token")
	if err != nil || url != manager.url {
		t.Fatalf("unknown request reconciliation failed: url=%q err=%v", url, err)
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
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	spec := &issues.IssueSpec{Key: key, Title: "Ready", Body: "## Summary\nBody\n\n" + issues.MarkerFor(key)}
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-ready": {
			ActionRequestView: ActionRequestView{
				ID: "request-ready", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
				ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
			},
			Issue: spec,
		},
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

func TestCreateRequestReusesOwnerUnknownAndRejectsOtherOwner(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	service.requests.Requests["unknown"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "unknown", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Kind: "create-issue", Owner: "alice", Status: RequestUnknown,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	view, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || view.ID != "unknown" {
		t.Fatalf("owner reuse view=%+v err=%v", view, err)
	}
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "bob", "token", "", ""); err == nil || !strings.Contains(err.Error(), "unknown GitHub outcome") {
		t.Fatalf("other owner error = %v", err)
	}
}
