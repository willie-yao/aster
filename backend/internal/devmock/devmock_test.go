package devmock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/prescalation"
	"github.com/willie-yao/aster/backend/internal/project"
)

// testClock advances only when a test says so, so the request and escalation
// state machines are exercised without sleeping.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestActions(t *testing.T) (*Actions, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	opts := Options{DataDir: t.TempDir(), Latency: time.Minute, Now: clock.Now}
	return newActions(&project.Config{}, opts), clock
}

func TestActionsRequestReachesReadyThenConfirms(t *testing.T) {
	service, clock := newTestActions(t)

	view, err := service.CreateRequest("job/build/test", actionFix, "Mock-Admin", "", "", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if view.Status != actions.RequestPending || view.Stage != actions.RequestStageDrafting {
		t.Fatalf("new request = %q/%q, want pending/drafting", view.Status, view.Stage)
	}
	if view.Owner != "mock-admin" {
		t.Fatalf("owner = %q, want the lowercased login", view.Owner)
	}

	clock.advance(40 * time.Second)
	if view, err = service.GetRequest(view.ID, "mock-admin"); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if view.Stage != actions.RequestStageVerifying {
		t.Fatalf("late pending stage = %q, want %q", view.Stage, actions.RequestStageVerifying)
	}

	clock.advance(30 * time.Second)
	if view, err = service.GetRequest(view.ID, "mock-admin"); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if view.Status != actions.RequestReady {
		t.Fatalf("status = %q, want ready", view.Status)
	}
	if view.Preview == nil || view.Preview.Kind != "fix" || view.Preview.Diff == "" {
		t.Fatalf("ready fix request has no fix preview with a diff: %+v", view.Preview)
	}
	if view.Verification == nil {
		t.Fatal("ready fix request has no verification verdict")
	}

	// The preview token has to survive further polls, or confirming a request
	// the operator has been watching would fail.
	before := view.Preview.Token
	clock.advance(time.Minute)
	if view, err = service.GetRequest(view.ID, "mock-admin"); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if view.Preview.Token != before {
		t.Fatalf("preview token changed across polls: %q -> %q", before, view.Preview.Token)
	}

	url, err := service.ConfirmRequest(context.Background(), view.ID, "mock-admin", "")
	if err != nil {
		t.Fatalf("ConfirmRequest: %v", err)
	}
	if url == "" {
		t.Fatal("ConfirmRequest returned no result URL")
	}
	if view, err = service.GetRequest(view.ID, "mock-admin"); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if view.Status != actions.RequestConfirmed || view.ResultURL != url {
		t.Fatalf("after confirm = %q/%q, want confirmed/%s", view.Status, view.ResultURL, url)
	}
}

func TestActionsRequestSupersedeAndExpiry(t *testing.T) {
	service, clock := newTestActions(t)

	first, err := service.CreateRequest("job/build/test", actionIssue, "mock-admin", "", "", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	second, err := service.CreateRequest("job/build/test", actionIssue, "mock-admin", "", "tighten it", first.ID)
	if err != nil {
		t.Fatalf("CreateRequest superseding: %v", err)
	}
	replaced, err := service.GetRequest(first.ID, "mock-admin")
	if err != nil {
		t.Fatalf("GetRequest superseded: %v", err)
	}
	if replaced.Status != actions.RequestCancelled || replaced.SupersededBy != second.ID {
		t.Fatalf("superseded request = %q/%q, want cancelled and superseded by %s",
			replaced.Status, replaced.SupersededBy, second.ID)
	}

	clock.advance(requestTTL + time.Minute)
	expired, err := service.GetRequest(second.ID, "mock-admin")
	if err != nil {
		t.Fatalf("GetRequest expired: %v", err)
	}
	if expired.Status != actions.RequestExpired || expired.Preview != nil {
		t.Fatalf("expired request = %q with preview %v, want expired and no preview", expired.Status, expired.Preview)
	}
}

func TestActionsRequestRejectsUnknownActionAndOwner(t *testing.T) {
	service, _ := newTestActions(t)

	if _, err := service.CreateRequest("job/build/test", "delete-repo", "mock-admin", "", "", ""); err == nil {
		t.Fatal("CreateRequest accepted an action the routes never produce")
	}
	view, err := service.CreateRequest("job/build/test", actionIssue, "mock-admin", "", "", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if _, err := service.GetRequest(view.ID, "someone-else"); err == nil {
		t.Fatal("GetRequest returned another operator's request")
	}
}

func TestActionsPreviewConfirmConsumesToken(t *testing.T) {
	service, _ := newTestActions(t)
	service.opts.Latency = time.Millisecond

	preview, err := service.PreviewIssue(context.Background(), "job/build/test", "mock-admin", "", "")
	if err != nil {
		t.Fatalf("PreviewIssue: %v", err)
	}
	if _, err := service.Confirm(context.Background(), preview.Token, "mock-admin", ""); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if _, err := service.Confirm(context.Background(), preview.Token, "mock-admin", ""); err == nil {
		t.Fatal("Confirm accepted a token that was already used")
	}
}

func newTestChat() (*Chat, *testClock) {
	clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	return newChat(Options{DataDir: ".", Latency: 5 * time.Millisecond, Now: clock.Now}), clock
}

func TestChatTurnRecordsSucceededAttempt(t *testing.T) {
	chat, _ := newTestChat()
	ref := analysischat.AnalysisRef{JobID: "job", BuildID: "build", TestName: "test"}

	session, err := chat.Create(ref, "mock-admin", "req-0")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	view, err := chat.Send(context.Background(), session.ID, "mock-admin", "req-1", "why did it fail?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if view.TurnsUsed != 1 || view.Active != nil {
		t.Fatalf("after one turn: turns=%d active=%v, want 1 and no active turn", view.TurnsUsed, view.Active)
	}
	if len(view.Messages) != 2 || view.Messages[1].Role != "assistant" {
		t.Fatalf("transcript = %d messages ending in %q, want a user question and an assistant answer",
			len(view.Messages), view.Messages[len(view.Messages)-1].Role)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].Outcome != attemptSucceeded {
		t.Fatalf("attempt outcome = %q, want %q; the transcript only folds an attempt into its answer for that value",
			view.Attempts[0].Outcome, attemptSucceeded)
	}
	if view.Attempts[0].RequestID != "req-1" {
		t.Fatalf("attempt request id = %q, want req-1", view.Attempts[0].RequestID)
	}
}

func TestChatRejectsSecondSessionLookupAndUnknownOwner(t *testing.T) {
	chat, _ := newTestChat()
	ref := analysischat.AnalysisRef{JobID: "job", BuildID: "build", TestName: "test"}

	session, err := chat.Create(ref, "mock-admin", "req-0")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	found, err := chat.Find(ref, "mock-admin")
	if err != nil || found.ID != session.ID {
		t.Fatalf("Find = %q, %v; want the session just created", found.ID, err)
	}
	if _, err := chat.Find(ref, "someone-else"); err != analysischat.ErrSessionNotFound {
		t.Fatalf("Find for another operator = %v, want ErrSessionNotFound", err)
	}
	if _, err := chat.Get(session.ID, "someone-else"); err != analysischat.ErrSessionNotFound {
		t.Fatalf("Get for another operator = %v, want ErrSessionNotFound", err)
	}
}

func TestChatCancelEndsTheTurn(t *testing.T) {
	chat, _ := newTestChat()
	chat.opts.Latency = 2 * time.Second
	ref := analysischat.AnalysisRef{JobID: "job", BuildID: "build", TestName: "test"}
	session, err := chat.Create(ref, "mock-admin", "req-0")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, sendErr := chat.Send(context.Background(), session.ID, "mock-admin", "req-1", "why?")
		done <- sendErr
	}()

	// Wait for the turn to register before cancelling it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		view, getErr := chat.Get(session.ID, "mock-admin")
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if view.Active != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never became active")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := chat.Cancel(session.ID, "mock-admin", "req-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("Send returned an answer for a cancelled turn")
	}
	view, err := chat.Get(session.ID, "mock-admin")
	if err != nil {
		t.Fatalf("Get after cancel: %v", err)
	}
	if view.Active != nil {
		t.Fatalf("session still has an active turn after cancel: %+v", view.Active)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].Outcome != attemptCancelled {
		t.Fatalf("attempts after cancel = %+v, want one cancelled attempt", view.Attempts)
	}
}

func TestPullRequestEscalationProgressesToComplete(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	service := newPullRequestEscalation(Options{DataDir: t.TempDir(), Latency: time.Minute, Now: clock.Now})
	ref := prescalation.Ref{PullNumber: 7, JobID: "job", BuildID: "build", TestName: "test"}

	view, err := service.Get(ref)
	if err != nil {
		t.Fatalf("Get before start: %v", err)
	}
	if view.State != prescalation.StateNotStarted {
		t.Fatalf("state before start = %q, want %q", view.State, prescalation.StateNotStarted)
	}
	if _, err := service.Start(context.Background(), ref, "mock-admin", ""); err == nil {
		t.Fatal("Start accepted a request with no idempotency key")
	}
	if view, err = service.Start(context.Background(), ref, "mock-admin", "key-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.State != prescalation.StateQueued {
		t.Fatalf("state at start = %q, want queued", view.State)
	}

	clock.advance(40 * time.Second)
	if view, err = service.Get(ref); err != nil {
		t.Fatalf("Get while running: %v", err)
	}
	if view.State != prescalation.StateRunning {
		t.Fatalf("state mid-run = %q, want running", view.State)
	}

	clock.advance(time.Minute)
	if view, err = service.Get(ref); err != nil {
		t.Fatalf("Get after completion: %v", err)
	}
	if view.State != prescalation.StateComplete || view.RootCause == "" {
		t.Fatalf("completed view = %q with root cause %q, want complete with a root cause", view.State, view.RootCause)
	}
	if view.CompletedAt.IsZero() || view.StartedAt.IsZero() {
		t.Fatalf("completed view has no timestamps: %+v", view)
	}

	// A replayed start must not restart the analysis it already answered.
	replayed, err := service.Start(context.Background(), ref, "mock-admin", "key-1")
	if err != nil {
		t.Fatalf("Start replay: %v", err)
	}
	if replayed.State != prescalation.StateComplete {
		t.Fatalf("replayed start = %q, want the completed result", replayed.State)
	}
}

func TestSharedFailureEscalationRejectsEmptyID(t *testing.T) {
	service := newSharedFailureEscalation(Options{DataDir: t.TempDir()})
	if _, err := service.Start(context.Background(), prescalation.ClusterRef{ID: "  "}, "mock-admin", "key"); err != prescalation.ErrInvalid {
		t.Fatalf("Start with a blank id = %v, want ErrInvalid", err)
	}
}

// TestSeedWritesFilesTheRealReadersAccept guards the fabricated operational
// files against the validation the authenticated read-only endpoints apply.
// Those readers reject unknown enum values, so a fixture that drifts from them
// would leave the operator pages empty with no obvious cause.
func TestSeedWritesFilesTheRealReadersAccept(t *testing.T) {
	dir := t.TempDir()
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	status, err := fetchprogress.Read(fetchprogress.Path(dir))
	if err != nil {
		t.Fatalf("fetchprogress.Read: %v", err)
	}
	if status.Outcome != fetchprogress.OutcomeSucceeded {
		t.Fatalf("seeded outcome = %q, want succeeded", status.Outcome)
	}

	diagnostics, err := ai.ReadPatternFailureDiagnostics(filepath.Join(dir, ai.CacheFilename), time.Now().UTC())
	if err != nil {
		t.Fatalf("ReadPatternFailureDiagnostics: %v", err)
	}
	if len(diagnostics.Entries) == 0 {
		t.Fatal("seeded AI cache produced no pattern diagnostics; the records failed validation")
	}

	var traces ai.AnalysisTraceFile
	readJSON(t, filepath.Join(dir, output.AITraceFilename), &traces)
	if len(traces.Traces) == 0 {
		t.Fatal("seeded trace file holds no traces")
	}

	for _, name := range []string{output.AIUsageFetcherFilename, output.AIUsageServerFilename} {
		var ledger aiusage.UsageLedger
		readJSON(t, filepath.Join(dir, name), &ledger)
		if ledger.Version != aiusage.LedgerVersion {
			t.Fatalf("%s version = %d, want %d", name, ledger.Version, aiusage.LedgerVersion)
		}
		if len(ledger.Days) == 0 {
			t.Fatalf("%s holds no usage days", name)
		}
	}
}

func TestSeedLeavesExistingFilesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, output.AITraceFilename)
	if err := os.WriteFile(path, []byte(`{"version":1,"generated_at":"","traces":[]}`), 0o600); err != nil {
		t.Fatalf("writing existing traces: %v", err)
	}
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	var traces ai.AnalysisTraceFile
	readJSON(t, path, &traces)
	if len(traces.Traces) != 0 {
		t.Fatal("Seed overwrote a trace file that was already there")
	}
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}
