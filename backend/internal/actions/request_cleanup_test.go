package actions

import (
	"testing"
	"time"
)

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
					Kind: testCase.kind, Status: RequestCancelling, Warning: warning,
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
			if view.Status != testCase.finalStatus || view.Warning != want {
				t.Fatalf("view=%+v", view)
			}
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			restored, err := reloaded.GetRequest("request", "alice")
			if err != nil || restored.Warning != want {
				t.Fatalf("restored=%+v err=%v", restored, err)
			}
		})
	}
}
