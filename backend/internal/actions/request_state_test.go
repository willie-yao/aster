package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	fixpr "github.com/willie-yao/aster/backend/internal/fix/pr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/project"
)

func TestReadyFixRequestIgnoresUnusedJSONFields(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	var calls atomic.Int32
	installHandoffTestGenerator(t, service, &calls)
	input := exactAnalysisRequestInput()
	input.UserRequest = "Select a compatible published version."
	created, err := service.CreateAnalysisFixRequest(t.Context(), input, "alice", "write-token", "Keep exact version pins strict.")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if err := service.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	requestData, err := os.ReadFile(service.requestStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var requests map[string]any
	if err := json.Unmarshal(requestData, &requests); err != nil {
		t.Fatal(err)
	}
	record := requests["requests"].(map[string]any)[created.ID].(map[string]any)
	record["review"] = map[string]any{"status": "unavailable", "metadata": "unused"}
	record["preview"].(map[string]any)["review"] = map[string]any{"status": "concerns"}
	requestData, err = json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.requestStatePath(), requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	previewData, err := os.ReadFile(service.previewStore.path)
	if err != nil {
		t.Fatal(err)
	}
	var previews map[string]any
	if err := json.Unmarshal(previewData, &previews); err != nil {
		t.Fatal(err)
	}
	previewRecord := previews["previews"].(map[string]any)[tokenHash(ready.Preview.Token)].(map[string]any)
	previewRecord["fix"].(map[string]any)["review"] = map[string]any{"status": "passed", "metadata": "unused"}
	previewData, err = json.Marshal(previews)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.previewStore.path, previewData, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	restored, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || restored.Status != RequestReady || restored.Preview == nil ||
		restored.Preview.Token != ready.Preview.Token || restored.Preview.Diff != ready.Preview.Diff || calls.Load() != 1 {
		t.Fatalf("restored request = %+v, %v; generations = %d", restored, err, calls.Load())
	}
	handoff := reloaded.requests.Requests[created.ID].AnalysisFix
	if handoff == nil || handoff.UserRequest != input.UserRequest || handoff.Instruction != "Keep exact version pins strict." {
		t.Fatalf("restored task = %+v", handoff)
	}
	entry, _, _, _, err := reloaded.beginConfirm("alice", restored.Preview.Token, time.Minute)
	if err != nil || entry.fix == nil || entry.fix.Preview.Diff != ready.Preview.Diff {
		t.Fatalf("restored preview = %+v, %v", entry, err)
	}
	encoded, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"review":`)) {
		t.Fatal("unused model review metadata remained in the request API")
	}
}

func validUnknownRequestState(now time.Time) actionRequestState {
	return actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{
		"unknown": {ActionRequestView: ActionRequestView{
			ID: "unknown", FailureID: "failure", Kind: "create-issue", Owner: "alice", Status: RequestUnknown,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}, Issue: &issues.IssueSpec{Key: "pattern::unknown", Title: "Failure", Body: "## What happened\nThe operation failed."}},
	}}
}

func TestActionRequestStateRejectsInvalidStructure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		mutate func(*actionRequestState)
	}{
		{name: "truncated", body: `{"version":8,"requests":{"unknown":`},
		{name: "null", body: `null`},
		{name: "empty object", body: `{}`},
		{name: "null requests", body: `{"version":8,"requests":null}`},
		{name: "nil record", mutate: func(s *actionRequestState) { s.Requests["broken"] = nil }},
		{name: "id mismatch", mutate: func(s *actionRequestState) { s.Requests["unknown"].ID = "other" }},
		{name: "missing owner", mutate: func(s *actionRequestState) { s.Requests["unknown"].Owner = "" }},
		{name: "missing failure", mutate: func(s *actionRequestState) { s.Requests["unknown"].FailureID = "" }},
		{name: "missing analysis request identity", mutate: func(s *actionRequestState) { s.Requests["unknown"].Kind = requestKindAnalysisFix }},
		{name: "unknown kind", mutate: func(s *actionRequestState) { s.Requests["unknown"].Kind = "future-action" }},
		{name: "unknown status", mutate: func(s *actionRequestState) { s.Requests["unknown"].Status = "future-status" }},
		{name: "invalid time", mutate: func(s *actionRequestState) { s.Requests["unknown"].ExpiresAt = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := []byte(tc.body)
			if tc.mutate != nil {
				state := validUnknownRequestState(time.Now().UTC())
				tc.mutate(&state)
				var err error
				before, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "action_request_state.json")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			service := NewService(&project.Config{}, dir, AIConfig{})
			if !errors.Is(service.InitializationError(), ErrActionStateUnavailable) {
				t.Fatalf("initialization error = %v", service.InitializationError())
			}
			if _, err := service.CreateRequest("failure", "create-issue", "alice", "token", "", ""); !errors.Is(err, ErrActionStateUnavailable) {
				t.Fatalf("admission error = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("invalid state changed: err=%v", err)
			}
		})
	}
}

func TestActionStateReadFailuresPreserveState(t *testing.T) {
	for _, filename := range []string{"action_request_state.json", "action_preview_state.json"} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			service := NewService(&project.Config{}, dir, AIConfig{})
			if !errors.Is(service.InitializationError(), ErrActionStateUnavailable) {
				t.Fatalf("initialization error = %v", service.InitializationError())
			}
			if _, err := service.Confirm(t.Context(), "token", "alice", "write-token"); !errors.Is(err, ErrActionStateUnavailable) {
				t.Fatalf("Confirm error = %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() {
				t.Fatalf("unreadable state changed: info=%v err=%v", info, err)
			}
		})
	}
}

func TestActionStateInitializationErrorsBlockWritePaths(t *testing.T) {
	for _, filename := range []string{"action_request_state.json", "action_preview_state.json"} {
		t.Run(filename, func(t *testing.T) {
			original, pattern := requestTestService(t)
			preview, err := original.PreviewIssue(t.Context(), pattern.ID, "alice", "token", "")
			if err != nil {
				t.Fatal(err)
			}
			writeActionRequestState(t, original.dataDir, validUnknownRequestState(time.Now().UTC()), 0o600)
			if err := os.WriteFile(filepath.Join(original.dataDir, filename), []byte(`{"broken":`), 0o600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, name := range []string{"action_request_state.json", "action_preview_state.json"} {
				before[name], err = os.ReadFile(filepath.Join(original.dataDir, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			service := NewService(original.cfg, original.dataDir, AIConfig{})
			if !errors.Is(service.InitializationError(), ErrActionStateUnavailable) {
				t.Fatalf("initialization error = %v", service.InitializationError())
			}
			service.issueManagerFactory = func(_, _, _ string) issuePreviewManager { t.Fatal("GitHub manager constructed"); return nil }
			for _, tc := range []struct {
				name string
				run  func() error
			}{
				{"create issue", func() error {
					_, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
					return err
				}},
				{"create fix", func() error {
					_, err := service.CreateRequest(pattern.ID, "propose-fix", "alice", "token", "", "")
					return err
				}},
				{"create analysis fix", func() error {
					_, err := service.CreateAnalysisFixRequest(t.Context(), exactAnalysisRequestInput(), "alice", "token", "")
					return err
				}},
				{"find analysis fix", func() error {
					_, _, err := service.FindAnalysisFixRequest("session", "request", "alice", FixBranchTested, "")
					return err
				}},
				{"preview issue", func() error {
					_, err := service.PreviewIssue(t.Context(), pattern.ID, "alice", "token", "")
					return err
				}},
				{"preview fix", func() error { _, err := service.PreviewFix(t.Context(), pattern.ID, "alice", "token", ""); return err }},
				{"pattern chat preview", func() error {
					_, err := service.PreviewFixWithContext(t.Context(), pattern, "alice", "token", "", FixTarget{}, fixpr.GenerationContext{})
					return err
				}},
				{"analysis chat preview", func() error {
					_, err := service.PreviewAnalysisFix(t.Context(), exactAnalysisRequestInput(), "alice", "token", "")
					return err
				}},
				{"confirm request", func() error { _, err := service.ConfirmRequest(t.Context(), "unknown", "alice", "token"); return err }},
				{"confirm preview", func() error { _, err := service.Confirm(t.Context(), preview.Token, "alice", "token"); return err }},
				{"confirm entry", func() error {
					_, err := service.confirmEntry(t.Context(), &previewEntry{kind: "issue"}, "token")
					return err
				}},
				{"cancel request", func() error { _, err := service.CancelRequest(t.Context(), "unknown", "alice"); return err }},
				{"get request", func() error { _, err := service.GetRequest("unknown", "alice"); return err }},
				{"save requests", func() error { return service.saveRequestsLocked() }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := tc.run(); !errors.Is(err, ErrActionStateUnavailable) {
						t.Fatalf("error = %v", err)
					}
				})
			}
			service.ConfigureAsyncRequestsWithContext(t.Context(), time.Minute, func(context.Context, ActionRequestView) error { t.Error("notification started"); return nil })
			if err := service.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			for name, data := range before {
				after, err := os.ReadFile(filepath.Join(service.dataDir, name))
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("%s changed: err=%v", name, err)
				}
			}
		})
	}
}

func TestPreviewStateFailurePrecedesRequestRecovery(t *testing.T) {
	dir := t.TempDir()
	state := validUnknownRequestState(time.Now().UTC())
	state.Requests["unknown"].Status = RequestPending
	writeActionRequestState(t, dir, state, 0o600)
	path := filepath.Join(dir, "action_request_state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action_preview_state.json"), []byte(`null`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(&project.Config{}, dir, AIConfig{})
	if !errors.Is(service.InitializationError(), ErrActionStateUnavailable) {
		t.Fatalf("initialization error = %v", service.InitializationError())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("request recovery ran: err=%v", err)
	}
}

func TestActionRequestRecoveryReportsPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	state := validUnknownRequestState(time.Now().UTC())
	state.Requests["unknown"].Status = RequestPending
	writeActionRequestState(t, dir, state, 0o600)
	writeErr := errors.New("disk unavailable")
	service := &Service{dataDir: dir, requestStateWriter: func(string, any) error { return writeErr }}
	if err := service.loadActionRequests(); !errors.Is(err, writeErr) {
		t.Fatalf("recovery error = %v", err)
	}
}

func TestMissingActionStateAllowsFreshAdmission(t *testing.T) {
	service, pattern := requestTestService(t)
	if err := service.InitializationError(); err != nil {
		t.Fatal(err)
	}
	view, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, view.ID, "alice", RequestReady)
	if err := service.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownActionRequestRetainsExpiryPolicy(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "active"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := validUnknownRequestState(time.Now().UTC())
			if expired {
				state.Requests["unknown"].ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			}
			writeActionRequestState(t, dir, state, 0o600)
			service := NewService(&project.Config{}, dir, AIConfig{})
			view, err := service.GetRequest("unknown", "alice")
			want := RequestUnknown
			if expired {
				want = RequestExpired
			}
			if err != nil || view.Status != want {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}
