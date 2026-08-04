package analysischat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeSourceInvestigator struct {
	mu      sync.Mutex
	calls   []sourceinvestigation.Request
	result  sourceinvestigation.Result
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeSourceInvestigator) Investigate(ctx context.Context, request sourceinvestigation.Request) (sourceinvestigation.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	started, release, result, err := f.started, f.release, f.result, f.err
	f.mu.Unlock()
	request.ReportProgress(sourceinvestigation.PhaseInvestigating)
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return sourceinvestigation.Result{}, ctx.Err()
		}
	}
	return result, err
}

func sourceResult() sourceinvestigation.Result {
	return sourceinvestigation.Result{
		State:        sourceinvestigation.StateActionableCodeChange,
		Target:       &models.RemediationTarget{Intent: models.RemediationIntentModifySymbol, Path: "pkg/retry.go", Symbol: "retry"},
		Finding:      "The retry loop returns only after the terminal condition.",
		Confidence:   sourceinvestigation.ConfidenceHigh,
		Relationship: sourceinvestigation.RelationshipSupports,
		Direction:    "Inspect the terminal retry branch.",
		Citations: []sourceinvestigation.Citation{{
			Path: "pkg/retry.go", LineStart: 10, LineEnd: 12, Quote: "terminal", Verified: true,
		}},
	}
}

func sourceReadyService(t *testing.T, dir string, sourceRunner *fakeSourceInvestigator) (*Service, SessionView, string) {
	t.Helper()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{
		"example/repo": "main:0123456789abcdef0123456789abcdef01234567",
	}
	writeJobDetail(t, dir, detail)
	chatRunner := &fakeRunner{reply: Reply{Answer: "The retry loop is consistent with the artifacts.", Assessment: "supports"}}
	service, err := NewService(t.Context(), dir, chatRunner, Options{
		StateDir: filepath.Join(dir, ".private-chat"), PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceInvestigation(sourceRunner, sourceinvestigation.Repository{Owner: "example", Name: "repo"}, SourceInvestigationOptions{
		Timeout: time.Second, LeaseTTL: 2 * time.Second, MaxPerSession: 2, MaxActivePerOwner: 1,
	}); err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
		AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	chatRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), created.ID, "Alice", chatRequestID, "Could the retry loop be responsible?"); err != nil {
		t.Fatal(err)
	}
	return service, created, chatRequestID
}

func TestServiceSourceInvestigationPersistsPinnedSubjectAndResult(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeSourceInvestigator{result: sourceResult()}
	service, session, chatRequestID := sourceReadyService(t, dir, runner)
	requestID := testRequestID(t)
	view, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", requestID, chatRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != sourceinvestigation.StatusSucceeded || view.Result == nil || !view.Result.Citations[0].Verified {
		t.Fatalf("view = %+v", view)
	}
	runner.mu.Lock()
	calls := append([]sourceinvestigation.Request(nil), runner.calls...)
	runner.mu.Unlock()
	if len(calls) != 1 || calls[0].Subject.Repository.Revision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Subject.Question != "Could the retry loop be responsible?" || calls[0].Subject.Answer == "" {
		t.Fatalf("subject chat context = %+v", calls[0].Subject)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		record := state.Sessions[session.ID].Investigations[requestID]
		if record.Subject.SessionID != "" || record.LeaseID != "" || !record.LeaseExpires.IsZero() {
			t.Fatalf("terminal record retained private execution state: %+v", record)
		}
		if record.Revision != "0123456789abcdef0123456789abcdef01234567" {
			t.Fatalf("persisted revision = %q", record.Revision)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "jobs", models.JobDataFilename("periodic-demo"))); err != nil {
		t.Fatal(err)
	}

	repeated, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", requestID, chatRequestID)
	if err != nil || repeated.Status != sourceinvestigation.StatusSucceeded {
		t.Fatalf("repeated = %+v, %v", repeated, err)
	}
	runner.mu.Lock()
	if len(runner.calls) != 1 {
		runner.mu.Unlock()
		t.Fatalf("idempotent calls = %d", len(runner.calls))
	}
	runner.mu.Unlock()

	restarted, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: service.opts.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureSourceInvestigation(runner, sourceinvestigation.Repository{Owner: "example", Name: "repo"}, SourceInvestigationOptions{}); err != nil {
		t.Fatal(err)
	}
	persisted, err := restarted.GetSourceInvestigation(session.ID, "Alice", requestID)
	if err != nil || persisted.Status != sourceinvestigation.StatusSucceeded || persisted.Result == nil {
		t.Fatalf("persisted view = %+v, %v", persisted, err)
	}
	restartCtx, restartCancel := restarted.store.context()
	defer restartCancel()
	if err := restarted.store.update(restartCtx, func(state *persistedState) (bool, error) {
		if revision := state.Sessions[session.ID].Investigations[requestID].Revision; revision != "0123456789abcdef0123456789abcdef01234567" {
			t.Fatalf("restarted revision = %q", revision)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoRevisionAcceptsOnlyExactUnambiguousCommits(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "bare", value: sha, want: true},
		{name: "single ref", value: "main:" + sha, want: true},
		{name: "branch", value: "main"},
		{name: "composite presubmit", value: "main:" + sha + ",123:" + sha},
		{name: "short hash", value: "main:01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repoRevision(map[string]string{"Example/Repo": tc.value}, "example", "repo")
			if ok != tc.want || tc.want && got != sha {
				t.Fatalf("repoRevision(%q) = %q, %v", tc.value, got, ok)
			}
		})
	}
}

func TestBoundedRepoRefsRetainsConfiguredSource(t *testing.T) {
	refs := map[string]string{}
	for i := 0; i < 25; i++ {
		refs[fmt.Sprintf("example/repo-%02d", i)] = fmt.Sprintf("main:%040x", i)
	}
	const source = "zzzz/source"
	refs[source] = "main:0123456789abcdef0123456789abcdef01234567"
	got := boundedRepoRefs(refs, source)
	if len(got) != 20 || got[source] == "" {
		t.Fatalf("bounded refs omitted configured source: len=%d refs=%+v", len(got), got)
	}
	refs["ZZZZ/Source"] = "main:fedcba9876543210fedcba9876543210fedcba98"
	got = boundedRepoRefs(refs, source)
	if len(got) != 20 {
		t.Fatalf("ambiguous bounded refs len = %d", len(got))
	}
	if revision, ok := repoRevision(got, "zzzz", "source"); ok {
		t.Fatalf("ambiguous bounded refs resolved to %q", revision)
	}
}

func TestServiceSourceInvestigationExpiredLeaseRetainsUnknownOutcome(t *testing.T) {
	dir := t.TempDir()
	service, session, chatRequestID := sourceReadyService(t, dir, &fakeSourceInvestigator{result: sourceResult()})
	requestID := testRequestID(t)
	now := time.Now().UTC().Truncate(time.Second)
	service.opts.Now = func() time.Time { return now }
	ctx, cancel := service.store.context()
	err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		stamp := now.Add(-time.Minute)
		state.Sessions[session.ID].ExpiresAt = stamp
		state.Sessions[session.ID].View.ExpiresAt = stamp.Format(time.RFC3339)
		if state.Sessions[session.ID].Investigations == nil {
			state.Sessions[session.ID].Investigations = map[string]persistedInvestigation{}
		}
		state.Sessions[session.ID].Investigations[requestID] = persistedInvestigation{
			View: sourceinvestigation.View{
				ID: requestID, SessionID: session.ID, ChatRequestID: chatRequestID,
				Status: sourceinvestigation.StatusPending, Phase: sourceinvestigation.PhaseInvestigating,
			},
			Subject: sourceinvestigation.Subject{SessionID: session.ID},
			LeaseID: "expired", LeaseExpires: stamp,
		}
		return true, nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.GetSourceInvestigation(session.ID, "Alice", requestID)
	expectedExpiry := now.Add(service.opts.SessionTTL).Format(time.RFC3339)
	if err != nil || view.Status != sourceinvestigation.StatusUnknown || view.Phase != "" || view.ExpiresAt != expectedExpiry {
		t.Fatalf("expired view = %+v, %v", view, err)
	}
	ctx, cancel = service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		record := state.Sessions[session.ID].Investigations[requestID]
		if record.Subject.SessionID != "" || record.LeaseID != "" || !record.LeaseExpires.IsZero() {
			t.Fatalf("expired record retained private execution state: %+v", record)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSourceInvestigationRefreshesLegacySnapshot(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeSourceInvestigator{result: sourceResult()}
	service, session, chatRequestID := sourceReadyService(t, dir, runner)
	ctx, cancel := service.store.context()
	err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		state.Sessions[session.ID].Resolved.Build.RepoRefs = nil
		return true, nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", testRequestID(t), chatRequestID); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 1 || runner.calls[0].Subject.Repository.Revision == "" {
		t.Fatalf("legacy refresh calls = %+v", runner.calls)
	}
}

func TestExtendSessionExpiryPreservesLatestRetention(t *testing.T) {
	later := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	current := &persistedSession{
		ExpiresAt: later,
		View:      SessionView{ExpiresAt: later.Add(-time.Hour).Format(time.RFC3339)},
		Investigations: map[string]persistedInvestigation{
			"source": {View: sourceinvestigation.View{ExpiresAt: later.Add(-time.Hour).Format(time.RFC3339)}},
		},
	}
	extendSessionExpiry(current, later.Add(-time.Minute))
	want := later.Format(time.RFC3339)
	if !current.ExpiresAt.Equal(later) || current.View.ExpiresAt != want || current.Investigations["source"].View.ExpiresAt != want {
		t.Fatalf("expiry moved backward: %+v", current)
	}
}

func TestSourceProgressDueIncludesHeartbeat(t *testing.T) {
	now := time.Now()
	if sourceProgressDue(sourceinvestigation.PhaseInvestigating, sourceinvestigation.PhaseInvestigating, now) {
		t.Fatal("unchanged recent progress was due")
	}
	if !sourceProgressDue(sourceinvestigation.PhaseInvestigating, sourceinvestigation.PhaseInvestigating, now.Add(-progressHeartbeat-time.Second)) {
		t.Fatal("unchanged stale progress was not due")
	}
	if !sourceProgressDue(sourceinvestigation.PhaseVerifying, sourceinvestigation.PhaseInvestigating, now) {
		t.Fatal("changed progress was not due")
	}
}

func TestServiceSourceInvestigationExpiryTracksSession(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeSourceInvestigator{result: sourceResult()}
	service, session, firstChatRequestID := sourceReadyService(t, dir, runner)
	firstSourceRequestID := testRequestID(t)
	if _, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", firstSourceRequestID, firstChatRequestID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	service.opts.Now = func() time.Time { return now }
	secondChatRequestID := testRequestID(t)
	updatedSession, err := service.Send(t.Context(), session.ID, "Alice", secondChatRequestID, "Does the source support another explanation?")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.GetSourceInvestigation(session.ID, "Alice", firstSourceRequestID)
	if err != nil || first.ExpiresAt != updatedSession.ExpiresAt {
		t.Fatalf("expiry after chat turn: investigation=%q session=%q err=%v", first.ExpiresAt, updatedSession.ExpiresAt, err)
	}

	now = now.Add(time.Minute)
	second, err := service.SourceInvestigation(t.Context(), session.ID, "Alice", testRequestID(t), secondChatRequestID)
	if err != nil {
		t.Fatal(err)
	}
	first, err = service.GetSourceInvestigation(session.ID, "Alice", firstSourceRequestID)
	if err != nil || first.ExpiresAt != second.ExpiresAt {
		t.Fatalf("expiry after source request: first=%q second=%q err=%v", first.ExpiresAt, second.ExpiresAt, err)
	}
}

func TestServiceSourceInvestigationRejectsMutableRevision(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeSourceInvestigator{result: sourceResult()}
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-07-24T12:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main"}
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{Answer: "answer", Assessment: "supports"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceInvestigation(runner, sourceinvestigation.Repository{Owner: "example", Name: "repo"}, SourceInvestigationOptions{}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	chatRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", chatRequestID, "question"); err != nil {
		t.Fatal(err)
	}
	_, err = service.SourceInvestigation(t.Context(), session.ID, "Alice", testRequestID(t), chatRequestID)
	if !errors.Is(err, sourceinvestigation.ErrUnavailable) {
		t.Fatalf("SourceInvestigation = %v", err)
	}
}

func TestServiceSourceInvestigationCancellationAcrossReplicas(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeSourceInvestigator{result: sourceResult(), started: make(chan struct{}, 1), release: make(chan struct{})}
	service, session, chatRequestID := sourceReadyService(t, dir, runner)
	canceller, err := NewService(t.Context(), dir, &fakeRunner{}, Options{
		StateDir: service.opts.StateDir, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	done := make(chan error, 1)
	go func() {
		_, err := service.SourceInvestigation(context.Background(), session.ID, "Alice", requestID, chatRequestID)
		done <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("investigation did not start")
	}
	if err := canceller.CancelSourceInvestigation(session.ID, "Alice", requestID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("investigation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("investigation did not cancel")
	}
	view, err := canceller.GetSourceInvestigation(session.ID, "Alice", requestID)
	if err != nil || view.Status != sourceinvestigation.StatusFailed {
		t.Fatalf("persisted cancelled view = %+v, %v", view, err)
	}
}
