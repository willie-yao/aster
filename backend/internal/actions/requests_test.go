package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
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
	service := NewService(cfg, dataDir, AIConfig{})
	service.sourceVerifier = nil
	return service, pattern
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
	deadline := time.Now().Add(time.Second)
	for !ready.EmailSent && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		ready = waitRequest(t, service, created.ID, "alice", RequestReady)
	}
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
	old := waitRequest(t, service, created.ID, "alice", RequestCancelled)
	if old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
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
	old := waitRequest(t, service, blockedID, "alice", RequestCancelled)
	if old.Preview != nil || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
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
	old := waitRequest(t, service, created.ID, "alice", RequestCancelled)
	if old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
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
	if _, err := service.CancelRequest(context.Background(), created.ID, "alice"); err != nil {
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
	state := actionRequestState{Version: 4, Requests: map[string]*actionRequest{
		"request-ready": {
			ActionRequestView: ActionRequestView{
				ID: "request-ready", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
				ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
			},
			Issue: spec, VerificationVersion: sourceVerificationVersion,
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
			view, cancelErr := service.CancelRequest(context.Background(), status, "alice")
			if status == RequestCancelled {
				if cancelErr != nil || view.Status != RequestCancelled {
					t.Fatalf("idempotent cancellation view=%+v err=%v", view, cancelErr)
				}
			} else if cancelErr == nil || !strings.Contains(cancelErr.Error(), status) {
				t.Fatalf("CancelRequest() err=%v, want status %q", cancelErr, status)
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

	if _, err := service.CancelRequest(context.Background(), "request-ready", "alice"); err == nil || !strings.Contains(err.Error(), "being confirmed") {
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

type fakeManagedAgentRuntime struct {
	mu      sync.Mutex
	refs    []runtime.WorkRef
	started chan struct{}
	release chan struct{}
	err     error
	errs    []error
	once    sync.Once
}

func (f *fakeManagedAgentRuntime) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{}, nil
}

func (f *fakeManagedAgentRuntime) Cleanup(ctx context.Context, ref runtime.WorkRef) error {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return f.err
}

func TestCancelRequestWaitsForRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{started: make(chan struct{}), release: make(chan struct{})}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "runtime-request"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}

	type result struct {
		view ActionRequestView
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		view, err := service.CancelRequest(context.Background(), id, "alice")
		resultCh <- result{view: view, err: err}
	}()
	<-fake.started
	view, err := service.GetRequest(id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("during cleanup view=%+v err=%v", view, err)
	}
	close(fake.release)
	got := <-resultCh
	if got.err != nil || got.view.Status != RequestCancelled {
		t.Fatalf("cancellation result=%+v err=%v", got.view, got.err)
	}
	fake.mu.Lock()
	refs := append([]runtime.WorkRef(nil), fake.refs...)
	fake.mu.Unlock()
	if len(refs) != 1 || refs[0].UID != "uid-one" || refs[0].Name != "fix-task" {
		t.Fatalf("cleanup refs = %+v", refs)
	}
}

func TestCancelRequestFailsWhenIdentityChanges(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{err: runtime.ErrWorkIdentityChanged}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "identity-changed"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "old-uid", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestFailed || view.Error == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if _, err = service.CancelRequest(context.Background(), id, "alice"); err == nil {
		t.Fatal("failed identity-change cleanup was reported as cancelled")
	}
}

func TestRestartResumesRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 4, Requests: map[string]*actionRequest{
		"restart-runtime": {
			ActionRequestView: ActionRequestView{ID: "restart-runtime", FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
			Runtime: &runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: "restart-runtime"},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	fake := &fakeManagedAgentRuntime{}
	reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	reloaded.ConfigureAsyncRequests(time.Minute, nil)
	view := waitRequest(t, reloaded, "restart-runtime", "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("restart cleanup result = %+v", view)
	}
}

func TestCreateRequestDeduplicatesEquivalentActiveRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate request IDs: first=%s second=%s", first.ID, second.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	third, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || third.ID != first.ID {
		t.Fatalf("ready request was not deduplicated: view=%+v err=%v", third, err)
	}
}

func TestRequestTimeoutUsesRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	service.requestTimeout = 5 * time.Millisecond
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "timeout-runtime"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: id}); err != nil {
				return PreviewResult{}, nil, err
			}
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	view := waitRequest(t, service, id, "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("timeout view = %+v", view)
	}
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls == 0 {
		t.Fatal("timeout did not clean runtime work")
	}
}

func TestCancelPendingRequestWaitsForGenerator(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const id = "pending-cancel"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	started := make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			close(started)
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	<-started
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("pending cancellation view=%+v err=%v", view, err)
	}
}

func TestCreateRequestAllowsDifferentOwner(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "bob", "token-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("different owners shared request %q", first.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	waitRequest(t, service, second.ID, "bob", RequestReady)
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRetriesTransientFailure(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{errs: []error{runtime.ErrCleanupPending, nil}}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "retry-cleanup"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || (view.Status != RequestCancelling && view.Status != RequestCancelled) {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestCancelled)
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls < 2 {
		t.Fatalf("cleanup calls = %d, want retry", calls)
	}
}

func TestCleanupPendingGenerationTransitionsThroughCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "cleanup-pending-generation"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: id}); err != nil {
				return PreviewResult{}, nil, err
			}
			return PreviewResult{}, nil, runtime.ErrCleanupPending
		})
	}()
	view := waitRequest(t, service, id, "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("cleanup-pending result = %+v", view)
	}
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls == 0 {
		t.Fatal("cleanup-pending generation was not reconciled")
	}
}

func TestExpiredPendingRequestCleansBeforeExpiring(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const id = "expired-pending"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	started := make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			close(started)
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	<-started
	view, err := service.GetRequest(id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("expired active request view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestExpired)
}

func TestCreateRequestDoesNotDeduplicateDifferentInstruction(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "mention IPv6", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("different instructions shared request %q", first.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	waitRequest(t, service, second.ID, "alice", RequestFailed)
}

func TestCleanupRetriesAfterRuntimeBecomesAvailable(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	var available atomic.Bool
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) {
		if !available.Load() {
			return nil, nil
		}
		return fake, nil
	}
	now := time.Now().UTC()
	const id = "runtime-unavailable"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	available.Store(true)
	waitRequest(t, service, id, "alice", RequestCancelled)
}

func TestOverlappingCleanupWaitsForGenerationExit(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "overlapping-cleanup"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestCancelling,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Cleanup: &actionCleanupState{FinalStatus: RequestCancelled, RequestedAt: now.Format(time.RFC3339)},
	}
	service.requestDone[id] = make(chan struct{})
	firstDone := make(chan ActionRequestView, 1)
	go func() {
		view, _ := service.cleanupRequest(context.Background(), id)
		firstDone <- view
	}()
	time.Sleep(20 * time.Millisecond)
	service.rmu.Lock()
	service.requests.Requests[id].Runtime = &runtime.WorkRef{Backend: "orka", Name: "fix-task", UID: "uid-one", ExecutionID: id}
	service.rmu.Unlock()
	second, err := service.cleanupRequest(context.Background(), id)
	if err != nil || second.Status != RequestCancelled {
		t.Fatalf("second cleanup view=%+v err=%v", second, err)
	}
	select {
	case <-firstDone:
		t.Fatal("first cleanup returned before generation exited")
	case <-time.After(20 * time.Millisecond):
	}
	service.finishGeneration(id)
	select {
	case first := <-firstDone:
		if first.Status != RequestCancelled {
			t.Fatalf("first cleanup view=%+v", first)
		}
	case <-time.After(time.Second):
		t.Fatal("first cleanup did not finish after generation exit")
	}
}

func TestWaitTracksShutdownWatcher(t *testing.T) {
	service, _ := requestTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
	waitDone := make(chan error, 1)
	go func() { waitDone <- service.Wait(context.Background()) }()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before shutdown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not observe shutdown watcher completion")
	}
}

func TestCleanupRetriesFinalStateWriteFailure(t *testing.T) {
	service, pattern := requestTestService(t)
	var writes atomic.Int32
	service.requestStateWriter = func(path string, value any) error {
		if writes.Add(1) == 2 {
			return errors.New("transient state write failure")
		}
		return statefile.WritePrivateJSONDurable(path, value)
	}
	now := time.Now().UTC()
	const id = "final-write-retry"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestCancelled)
	if writes.Load() < 3 {
		t.Fatalf("state writes = %d, want retry", writes.Load())
	}
}

func TestSupersedingRequestCancelsReadyNotification(t *testing.T) {
	service, pattern := requestTestService(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	service.ConfigureAsyncRequests(time.Minute, func(ctx context.Context, _ ActionRequestView) error {
		if calls.Add(1) != 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	<-started
	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("superseded ready notification was not cancelled")
	}
	waitRequest(t, service, replacement.ID, "alice", RequestReady)
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRequestDoesNotReuseStaleReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	service.rmu.Lock()
	service.requests.Requests[first.ID].PatternHash = "stale"
	service.rmu.Unlock()
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("stale ready request %q was reused", first.ID)
	}
	waitRequest(t, service, second.ID, "alice", RequestReady)
}

func TestShutdownRejectsNewRequests(t *testing.T) {
	service, pattern := requestTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.rmu.Lock()
		stopping := service.stopping
		service.rmu.Unlock()
		if stopping {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", ""); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("CreateRequest() error = %v", err)
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoadActionRequestsInvalidatesLegacyVerifiedPreview(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	spec := &issues.IssueSpec{Key: key, Title: "Ready", Body: "## Summary\nBody\n\n" + issues.MarkerFor(key)}
	state := actionRequestState{Version: 3, Requests: map[string]*actionRequest{
		"legacy": {
			ActionRequestView: ActionRequestView{
				ID: "legacy", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
			},
			Issue: spec, VerificationVersion: sourceVerificationVersion - 1,
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("legacy", "alice")
	if err != nil || view.Status != RequestFailed || view.Preview != nil {
		t.Fatalf("legacy view = %+v, %v", view, err)
	}
}
