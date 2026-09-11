package analysischat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func historyFixture(t *testing.T, scope string) (string, AnalysisRef) {
	t.Helper()
	dir := t.TempDir()
	if scope == ScopeTest {
		writeJobDetail(t, dir, testDetail(analyzedTest("TestCluster", "junit.xml", "2026-09-01T00:00:00Z")))
		return dir, AnalysisRef{Scope: scope, JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster"}
	}
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1", "2"}, RootCause: "controller failure", Confidence: "high"}}, nil)
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dir, causalPatternDetail(pattern, "1", "2"))
	ref := AnalysisRef{Scope: scope, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash}
	if scope == ScopeCause {
		ref.CausalGroupID, ref.CausalGroupHash = pattern.CausalGroups[0].ID, pattern.CausalGroups[0].ContentHash
	}
	return dir, ref
}

func TestHistorySurvivesExpiryAndPublicationRemoval(t *testing.T) {
	for _, scope := range []string{ScopeTest, ScopePattern, ScopeCause} {
		t.Run(scope, func(t *testing.T) {
			dir, ref := historyFixture(t, scope)
			var clock atomic.Int64
			start := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			clock.Store(start.UnixNano())
			opts := Options{Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, SessionTTL: time.Hour, HistoryRetention: 24 * time.Hour, MaxSessions: 1, MaxSessionsPerOwner: 1}
			service := newTestService(t, t.Context(), dir, &fakeRunner{reply: Reply{Answer: "saved finding", EvidenceWarnings: []string{"partial evidence"}, Citations: []Citation{{Path: "build-log.txt", Quote: "failure"}}}}, opts)
			created, err := service.Create(ref, "alice", "create")
			if err != nil {
				t.Fatal(err)
			}
			saved, err := service.Send(t.Context(), created.ID, "alice", "ask", "investigate")
			if err != nil {
				t.Fatal(err)
			}
			clock.Store(start.Add(2 * time.Hour).UnixNano())
			if _, err := service.Find(ref, "bob"); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("expired lookup: %v", err)
			}
			if _, err := service.Send(t.Context(), created.ID, "bob", "new-question", "continue"); !errors.Is(err, ErrSessionInactive) {
				t.Fatalf("expired turn: %v", err)
			}
			if replay, err := service.Send(t.Context(), created.ID, "alice", "ask", "investigate"); err != nil || !reflect.DeepEqual(replay.Messages, saved.Messages) {
				t.Fatalf("historical replay: %+v %v", replay, err)
			}
			if replacement, err := service.Create(ref, "alice", "replacement"); err != nil || replacement.ID == created.ID {
				t.Fatalf("history held a live slot: %+v %v", replacement, err)
			}
			writeJobDetail(t, dir, models.JobDetail{JobID: ref.JobID, Name: ref.JobID})
			restarted := newTestService(t, t.Context(), dir, &fakeRunner{}, opts)
			got, err := restarted.Get(created.ID, "bob")
			if err != nil || !got.ReadOnly || !reflect.DeepEqual(got.Messages, saved.Messages) || len(got.BuildIDs) == 0 {
				t.Fatalf("restored history: %+v %v", got, err)
			}
			page, err := restarted.List(HistoryQuery{JobID: ref.JobID, Scope: scope}, "bob")
			if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != created.ID {
				t.Fatalf("history: %+v %v", page, err)
			}
			if got.HistoryExpiresAt != saved.HistoryExpiresAt {
				t.Fatalf("reads changed retention: %s -> %s", saved.HistoryExpiresAt, got.HistoryExpiresAt)
			}
			clock.Store(start.Add(24 * time.Hour).UnixNano())
			if _, err := restarted.Get(created.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("history retention expired: %v", err)
			}
		})
	}
}

func TestArchiveKeepsHistoryAndStartsWithoutContext(t *testing.T) {
	dir, ref := historyFixture(t, ScopeTest)
	runner := &fakeRunner{reply: Reply{Answer: "original finding"}}
	service := newTestService(t, t.Context(), dir, runner, Options{MaxSessions: 1, MaxSessionsPerOwner: 1})
	first, err := service.Create(ref, "alice", "first")
	if err != nil {
		t.Fatal(err)
	}
	first, err = service.Send(t.Context(), first.ID, "alice", "question-one", "original question")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.Archive(first.ID, "bob"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := service.Get(first.ID, "bob")
	if err != nil || !got.Archived || !got.ReadOnly || !reflect.DeepEqual(got.Messages, first.Messages) {
		t.Fatalf("archive: %+v %v", got, err)
	}
	if _, err := service.Send(t.Context(), first.ID, "alice", "new-question", "continue"); !errors.Is(err, ErrSessionInactive) {
		t.Fatalf("archived turn: %v", err)
	}
	second, err := service.Create(ref, "alice", "second")
	if err != nil || second.ID == first.ID {
		t.Fatalf("new conversation: %+v %v", second, err)
	}
	if _, err := service.Send(t.Context(), second.ID, "alice", "question-two", "fresh question"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.turns) != 2 || len(runner.turns[1].History) != 0 {
		t.Fatalf("inherited old context: %+v", runner.turns)
	}
}

func TestHistoryPaginationAndFilters(t *testing.T) {
	dir, ref := historyFixture(t, ScopeTest)
	service := newTestService(t, t.Context(), dir, &fakeRunner{}, Options{})
	for _, id := range []string{"one", "two", "three"} {
		created, err := service.Create(ref, "alice", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Send(t.Context(), created.ID, "alice", id+"-turn", "question"); err != nil {
			t.Fatal(err)
		}
		if err := service.Archive(created.ID, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := service.List(HistoryQuery{JobID: ref.JobID, Limit: 2}, "bob")
	if err != nil || len(first.Sessions) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := service.List(HistoryQuery{JobID: ref.JobID, Limit: 2, Cursor: first.NextCursor}, "bob")
	if err != nil || len(second.Sessions) != 1 || second.NextCursor != "" || second.Sessions[0].ID == first.Sessions[0].ID || second.Sessions[0].ID == first.Sessions[1].ID {
		t.Fatalf("second page: %+v %v", second, err)
	}
	for _, query := range []HistoryQuery{{JobID: "other"}, {Scope: ScopeCause}} {
		page, err := service.List(query, "bob")
		if err != nil || len(page.Sessions) != 0 {
			t.Fatalf("filtered: %+v %v", page, err)
		}
	}
	for _, query := range []HistoryQuery{{Scope: "unknown"}, {Limit: 101}, {Cursor: "bad"}, {Cursor: first.NextCursor}} {
		if _, err := service.List(query, "alice"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid query %+v: %v", query, err)
		}
	}
	if _, err := service.List(HistoryQuery{}, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("anonymous history: %v", err)
	}
}

func TestHistoryRetainsFixFindingButNotPreparedOnly(t *testing.T) {
	dir, ref := historyFixture(t, ScopeTest)
	service := newTestService(t, t.Context(), dir, &fakeRunner{}, Options{})
	created, err := service.Create(ref, "alice", "create")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		session := state.Sessions[created.ID]
		session.Requests = map[string]persistedRequest{"prepared": {Prepared: true, Status: requestSucceeded}}
		session.View.Messages = []Message{{Role: "assistant", RequestID: "prepared", Prepared: true, Content: "prepared finding"}}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := service.List(HistoryQuery{}, "alice")
	if err != nil || len(page.Sessions) != 0 {
		t.Fatalf("prepared-only history: %+v %v", page, err)
	}
	if err := service.RetainForFix(created.ID, "alice", "prepared"); err != nil {
		t.Fatal(err)
	}
	page, err = service.List(HistoryQuery{}, "alice")
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("Fix history: %+v %v", page, err)
	}
}

func TestHistoryMigratesVersionFourBeforeCleanup(t *testing.T) {
	dir, ref := historyFixture(t, ScopeTest)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	opts := Options{Now: func() time.Time { return start.Add(48 * time.Hour) }, HistoryRetention: 180 * 24 * time.Hour}
	stateDir := filepath.Join(dir, ".analysis-chat")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Actor: "alice", RequestID: "ask", Content: "question", CreatedAt: start.Format(time.RFC3339)}, {Role: "assistant", RequestID: "ask", Content: "finding", CreatedAt: start.Add(time.Minute).Format(time.RFC3339)}}
	messages[1].Citations = []Citation{{Path: "build-log.txt", LineStart: 5, LineEnd: 5, Quote: "original evidence"}}
	messages[1].ProposedRevision = &Revision{RootCause: "revised cause", SuggestedFix: "proposed change"}
	messages[1].EvidenceWarnings = []string{"partial verification"}
	resolved := persistedResolvedAnalysis{Ref: ref, BuildPrefix: "logs/periodic-demo/123", AnalysisHash: "snapshot", Build: models.BuildInfo{BuildID: "123", WebURL: "https://example.test/123"}, TestCase: analyzedTest("TestCluster", "junit.xml", "2026-09-01T00:00:00Z")}
	state := persistedState{Version: 4, Sessions: map[string]*persistedSession{
		"original": {Resolved: resolved, Owner: "alice", ExpiresAt: start.Add(2 * time.Hour), View: SessionView{ID: "original", Analysis: ref, CreatedAt: start.Format(time.RFC3339), Messages: messages}, Requests: map[string]persistedRequest{"ask": {Actor: "alice", Status: requestSucceeded, CreatedAt: start.Format(time.RFC3339), UpdatedAt: start.Add(time.Minute).Format(time.RFC3339)}}},
		"prepared": {Owner: "alice", ExpiresAt: start.Add(2 * time.Hour), View: SessionView{ID: "prepared", CreatedAt: start.Format(time.RFC3339)}, Requests: map[string]persistedRequest{"seed": {Prepared: true, Status: requestSucceeded, CreatedAt: start.Format(time.RFC3339)}}},
	}}
	path := filepath.Join(stateDir, stateFileName)
	if err := writePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, t.Context(), dir, &fakeRunner{}, opts)
	got, err := service.Get("original", "bob")
	if err != nil || !reflect.DeepEqual(got.Messages, messages) || got.HistoryExpiresAt != start.Add(time.Minute+opts.HistoryRetention).Format(time.RFC3339) {
		t.Fatalf("migrated history: %+v %v", got, err)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions["original"]
		if !reflect.DeepEqual(current.Resolved, resolved) || current.Requests["ask"].Status != requestSucceeded || current.View.ID != "original" {
			t.Fatalf("migration changed immutable source: %+v", current)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".v4.bak")
	if err != nil {
		t.Fatal(err)
	}
	var before persistedState
	if err := json.Unmarshal(backup, &before); err != nil || before.Version != 4 || len(before.Sessions) != 2 {
		t.Fatalf("backup: %+v %v", before, err)
	}
	if info, err := os.Stat(path + ".v4.bak"); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("backup permissions: %v %v", info, err)
	}
	if _, err := service.Get("prepared", "alice"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("prepared retention changed: %v", err)
	}
	restarted := newTestService(t, t.Context(), dir, &fakeRunner{}, opts)
	if _, err := restarted.Get("original", "alice"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path + ".v4.bak")
	if string(after) != string(backup) {
		t.Fatal("backup was overwritten")
	}
}

func TestHistoryOversizedWritePreservesSavedSessions(t *testing.T) {
	dir, ref := historyFixture(t, ScopeTest)
	service := newTestService(t, t.Context(), dir, &fakeRunner{reply: Reply{Answer: "saved"}}, Options{})
	created, err := service.Create(ref, "alice", "create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), created.ID, "alice", "ask", "question"); err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(created.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	err = service.store.update(ctx, func(state *persistedState) (bool, error) {
		state.Sessions[created.ID].View.Messages = append(state.Sessions[created.ID].View.Messages, Message{Role: "assistant", Content: strings.Repeat("x", maxStateBytes)})
		return true, nil
	})
	if err == nil {
		t.Fatal("oversized history write accepted")
	}
	view, err := service.Get(created.ID, "bob")
	if err != nil || len(view.Messages) != 2 || view.Messages[1].Content != "saved" {
		t.Fatalf("history was damaged: messages=%d err=%v", len(view.Messages), err)
	}
}
