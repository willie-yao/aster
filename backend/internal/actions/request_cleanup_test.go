package actions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

func savedCleanupRequest(id, failureID, owner string) *actionRequest {
	now := time.Now().UTC().Format(time.RFC3339)
	return &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: id, FailureID: failureID, Owner: owner, Kind: "propose-fix", Status: RequestCancelling,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "fix-" + id, UID: "uid-" + id, ExecutionID: id},
		Cleanup: &actionCleanupState{FinalStatus: RequestCancelled, RequestedAt: now},
	}
}

func assertCleanupDebt(t *testing.T, service *Service, id string) {
	t.Helper()
	service.rmu.Lock()
	defer service.rmu.Unlock()
	request := service.requests.Requests[id]
	if request == nil || request.Status != RequestCancelling || request.Runtime == nil || request.Runtime.UID != "uid-"+id || request.Cleanup == nil {
		t.Fatalf("cleanup obligation = %+v", request)
	}
}

func waitForCleanupWorkers(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.Wait(ctx); err != nil {
		t.Fatalf("cleanup workers did not stop: %v", err)
	}
}

func TestCleanupWorkerStopsWhenIdentityBecomesBlocked(t *testing.T) {
	for _, phase := range []string{"backoff", "in-flight success"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, pattern := requestTestService(t)
				const id = "blocked-worker"
				request := savedCleanupRequest(id, pattern.ID, "alice")
				request.Cleanup.FinalStatus = RequestFailed
				request.Cleanup.Reason = "draft generation failed"
				service.requests.Requests[id] = request
				work := *request.Runtime
				cleanup := *request.Cleanup
				fake := &fakeManagedAgentRuntime{err: runtime.ErrUnavailable}
				if phase == "in-flight success" {
					fake.err = nil
					fake.release = make(chan struct{})
				}
				service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
				defer func() {
					service.stopCleanup()
					_ = service.Wait(context.Background())
				}()
				service.startCleanup(id)
				synctest.Wait()
				fake.mu.Lock()
				calls := len(fake.refs)
				fake.mu.Unlock()
				if calls != 1 {
					t.Fatalf("cleanup attempts before identity block = %d", calls)
				}
				if phase == "backoff" && service.currentRequestView(id).Error == "" {
					t.Fatal("cleanup did not reach transient retry backoff")
				}
				service.markCleanupBlocked(id)
				if fake.release != nil {
					close(fake.release)
				}
				waitForCleanupWorkers(t, service)
				service.rmu.Lock()
				_, running := service.requestCleanups[id]
				service.rmu.Unlock()
				if running {
					t.Fatal("blocked cleanup retained its single-flight slot")
				}
				service.startCleanup(id)
				waitForCleanupWorkers(t, service)
				reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
				for _, current := range []*Service{service, reloaded} {
					assertCleanupDebt(t, current, id)
					retained := current.requests.Requests[id]
					if *retained.Runtime != work || !retained.Cleanup.Blocked ||
						retained.Cleanup.FinalStatus != cleanup.FinalStatus || retained.Cleanup.Reason != cleanup.Reason ||
						retained.Cleanup.RequestedAt != cleanup.RequestedAt {
						t.Fatalf("blocked cleanup lost its obligation: runtime=%+v cleanup=%+v", retained.Runtime, retained.Cleanup)
					}
				}
				reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
				reloaded.ConfigureAsyncRequests(time.Minute, nil)
				waitForCleanupWorkers(t, reloaded)
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if len(fake.refs) != 1 || fake.refs[0] != work {
					t.Fatalf("identity-blocked cleanup retried: %+v", fake.refs)
				}
			})
		})
	}
}

func TestSavedFixCleanupDebtDoesNotBlockIssueAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		owner func(int) string
	}{
		{"owner limit", maxPendingPerOwner, func(int) string { return "alice" }},
		{"global limit", maxActiveRequests, func(i int) string { return fmt.Sprintf("owner-%d", i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, pattern := requestTestService(t)
			state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{}}
			for i := 0; i < tc.count; i++ {
				id := fmt.Sprintf("cleanup-%d", i)
				state.Requests[id] = savedCleanupRequest(id, "old-failure", tc.owner(i))
			}
			writeActionRequestState(t, original.dataDir, state, 0o600)
			service := NewService(original.cfg, original.dataDir, AIConfig{})
			service.sourceVerifier = nil
			service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return nil, runtime.ErrUnavailable }
			for id := range state.Requests {
				if _, err := service.cleanupRequest(t.Context(), id); !errors.Is(err, runtime.ErrUnavailable) {
					t.Fatalf("unavailable cleanup error = %v", err)
				}
			}
			issue, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
			if err != nil {
				t.Fatalf("issue admission: %v", err)
			}
			waitRequest(t, service, issue.ID, "alice", RequestReady)
			if _, err := service.CreateRequest(pattern.ID, "propose-fix", "alice", "token", "", ""); !errors.Is(err, ErrRequestLimit) {
				t.Fatalf("Fix admission bypassed cleanup bound: %v", err)
			}
			waitForCleanupWorkers(t, service)
			for id := range state.Requests {
				assertCleanupDebt(t, service, id)
			}
		})
	}
}

func TestIssueAdmissionStillCountsActiveFixGeneration(t *testing.T) {
	service, pattern := requestTestService(t)
	for i := 0; i < maxPendingPerOwner; i++ {
		id := fmt.Sprintf("generation-%d", i)
		service.requests.Requests[id] = savedCleanupRequest(id, "old-failure", "alice")
		service.requestDone[id] = make(chan struct{})
	}
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", ""); !errors.Is(err, ErrRequestLimit) {
		t.Fatalf("active generation bypassed admission bound: %v", err)
	}
}

func TestCleanupShutdownPreservesDebtForRestart(t *testing.T) {
	for _, phase := range []string{"attempt", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			service, pattern := requestTestService(t)
			const id = "shutdown"
			service.requests.Requests[id] = savedCleanupRequest(id, pattern.ID, "alice")
			if err := service.saveRequestsLocked(); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			var once sync.Once
			if phase == "attempt" {
				fake := &fakeManagedAgentRuntime{started: started, release: make(chan struct{})}
				service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
			} else {
				service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return nil, runtime.ErrUnavailable }
				service.requestStateWriter = func(path string, value any) error {
					if err := statefile.WritePrivateJSONDurable(path, value); err != nil {
						return err
					}
					if value.(*actionRequestState).Requests[id].Error != "" {
						once.Do(func() { close(started) })
					}
					return nil
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("cleanup did not start")
			}
			cancel()
			waitForCleanupWorkers(t, service)
			assertCleanupDebt(t, service, id)
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			assertCleanupDebt(t, reloaded, id)
			fake := &fakeManagedAgentRuntime{}
			reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
			reloaded.ConfigureAsyncRequests(time.Minute, nil)
			waitRequest(t, reloaded, id, "alice", RequestCancelled)
			waitForCleanupWorkers(t, reloaded)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.refs) != 1 || fake.refs[0].UID != "uid-"+id || fake.refs[0].ExecutionID != id {
				t.Fatalf("recovered cleanup refs = %+v", fake.refs)
			}
		})
	}
}

func TestCleanupFinalSaveFailureRetainsRuntime(t *testing.T) {
	service, pattern := requestTestService(t)
	const id = "final-save"
	service.requests.Requests[id] = savedCleanupRequest(id, pattern.ID, "alice")
	if err := service.saveRequestsLocked(); err != nil {
		t.Fatal(err)
	}
	failed := make(chan struct{})
	var once sync.Once
	service.requestStateWriter = func(path string, value any) error {
		if value.(*actionRequestState).Requests[id].Status != RequestCancelling {
			once.Do(func() { close(failed) })
			return errors.New("final state is unavailable")
		}
		return statefile.WritePrivateJSONDurable(path, value)
	}
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	ctx, cancel := context.WithCancel(t.Context())
	service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
	view, err := service.CancelRequest(t.Context(), id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("cancellation view=%+v err=%v", view, err)
	}
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("final state save was not attempted")
	}
	cancel()
	waitForCleanupWorkers(t, service)
	assertCleanupDebt(t, service, id)
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	assertCleanupDebt(t, reloaded, id)
	reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	reloaded.ConfigureAsyncRequests(time.Minute, nil)
	waitRequest(t, reloaded, id, "alice", RequestCancelled)
	waitForCleanupWorkers(t, reloaded)
}

func TestBlockedCleanupSurvivesExpiry(t *testing.T) {
	service, pattern := requestTestService(t)
	const id = "blocked"
	request := savedCleanupRequest(id, pattern.ID, "alice")
	request.Cleanup.Blocked = true
	request.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	service.requests.Requests[id] = request
	if err := service.saveRequestsLocked(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	assertCleanupDebt(t, reloaded, id)
	if !reloaded.requests.Requests[id].Cleanup.Blocked {
		t.Fatal("expiry cleared the cleanup block")
	}
	reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { t.Error("blocked cleanup retried"); return nil, nil }
	reloaded.ConfigureAsyncRequests(time.Minute, nil)
	waitForCleanupWorkers(t, reloaded)
}

func TestCleanupPreservesFailedFixWarnings(t *testing.T) {
	for _, testCase := range []struct {
		name, kind, finalStatus string
		retain                  bool
	}{
		{"manual fix failure", "propose-fix", RequestFailed, true},
		{"exact fix failure", requestKindAnalysisFix, RequestFailed, true},
		{"issue failure", "create-issue", RequestFailed, false},
		{"cancelled fix", "propose-fix", RequestCancelled, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, pattern := requestTestService(t)
			now := time.Now().UTC()
			const warning = "The original analysis is an investigation hypothesis."
			service.requests.Requests["request"] = &actionRequest{
				ActionRequestView: ActionRequestView{
					ID: "request", FailureID: pattern.ID, Owner: "alice",
					Kind: testCase.kind, Status: RequestCancelling, Warnings: plainWarnings(warning),
					CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
					ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				},
				Cleanup: &actionCleanupState{FinalStatus: testCase.finalStatus, Reason: "no patch"},
			}
			view, err := service.finalizeCleanup("request")
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if testCase.retain {
				want = warning
			}
			if view.Status != testCase.finalStatus || warningText(view.Warnings) != want {
				t.Fatalf("view=%+v", view)
			}
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			restored, err := reloaded.GetRequest("request", "alice")
			if err != nil || warningText(restored.Warnings) != want {
				t.Fatalf("restored=%+v err=%v", restored, err)
			}
		})
	}
}
