package analysischat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestWritePrivateJSONLimitPreservesReadableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writePrivateJSONLimit(path, map[string]string{"value": "old"}, 128); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSONLimit(path, map[string]string{"value": strings.Repeat("x", 256)}, 128); err == nil {
		t.Fatal("oversized state was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("oversized write replaced prior state: before=%q after=%q", before, after)
	}
}

func TestWritePrivateJSONSyncFailurePreservesReadableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writePrivateJSONLimit(path, map[string]string{"value": "old"}, 128); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSONLimitWithSync(
		path,
		map[string]string{"value": "new"},
		128,
		func(*os.File) error { return errors.New("sync failed") },
		func(*os.File) error { return nil },
	); err == nil {
		t.Fatal("sync failure was ignored")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("sync failure replaced prior state: before=%q after=%q", before, after)
	}
}

func TestWritePrivateJSONSyncsParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	dirSynced := false
	err := writePrivateJSONLimitWithSync(
		path,
		map[string]string{"value": "new"},
		128,
		func(file *os.File) error { return file.Sync() },
		func(*os.File) error {
			dirSynced = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !dirSynced {
		t.Fatal("parent directory was not synced")
	}
}

func TestWritePrivateJSONReportsDirectorySyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	err := writePrivateJSONLimitWithSync(
		path,
		map[string]string{"value": "new"},
		128,
		func(file *os.File) error { return file.Sync() },
		func(*os.File) error { return errors.New("directory sync failed") },
	)
	if err == nil || !strings.Contains(err.Error(), "syncing analysis chat state directory") {
		t.Fatalf("directory sync error = %v", err)
	}
}

func TestSessionStoreMigratesVersionOneTestSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &persistedState{
		Version: 1,
		Sessions: map[string]*persistedSession{
			"session": {
				View:     SessionView{ID: "session", Analysis: AnalysisRef{JobID: "job", BuildID: "1", TestName: "test"}},
				Resolved: persistedResolvedAnalysis{Ref: AnalysisRef{JobID: "job", BuildID: "1", TestName: "test"}},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	state, migrated, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	session := state.Sessions["session"]
	if !migrated || state.Version != stateVersion || session.View.Analysis.Scope != ScopeTest || session.Resolved.Ref.Scope != ScopeTest {
		t.Fatalf("migrated state = %+v", state)
	}
}

func TestSessionStoreVersionTwoActorsAreRollingSafe(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	legacy := &persistedState{
		Version: 2,
		Sessions: map[string]*persistedSession{
			"session": {
				Owner: "Bob", ExpiresAt: now.Add(time.Hour),
				View: SessionView{
					ID: "session", Analysis: AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"},
					Messages: []Message{{Role: "user", RequestID: "request", Content: "question", CreatedAt: now.Format(time.RFC3339)}},
				},
				Requests: map[string]persistedRequest{
					"request": {QuestionHash: hashText("question"), Question: "question", Status: requestPending},
				},
				Active: &persistedActiveTurn{
					RequestID: "request", Question: "question", LeaseID: "lease", ExpiresAt: now.Add(time.Minute),
					Phase: PhaseInvestigating, UpdatedAt: now,
				},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := store.context()
	defer cancel()
	if err := store.update(ctx, func(state *persistedState) (bool, error) {
		session := state.Sessions["session"]
		request := session.Requests["request"]
		if state.Version != stateVersion || session.Owner != "bob" || session.View.CreatedBy != "bob" ||
			session.View.Messages[0].Actor != "bob" || request.Actor != "bob" || session.Active.Actor != "bob" {
			t.Fatalf("migrated shared state = %+v", state)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != stateVersion || persisted.Version == 2 || persisted.Sessions["session"].Active.Actor != "bob" {
		t.Fatalf("persisted rolling state = %+v", persisted)
	}
}

func TestSessionStoreVersionTwoCanonicalizesDuplicateOwnerSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	ref := AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"}
	legacy := &persistedState{
		Version: 2,
		Sessions: map[string]*persistedSession{
			"alice-session": {
				Owner: "alice", ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "alice-session", Analysis: ref, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339)},
			},
			"bob-session": {
				Owner: "bob", ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "bob-session", Analysis: ref, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
				Requests: map[string]persistedRequest{
					"active": {QuestionHash: hashText("question"), Question: "question", Status: requestPending},
				},
				Active: &persistedActiveTurn{RequestID: "active", Question: "question", LeaseID: "lease", ExpiresAt: now.Add(time.Minute), Phase: PhaseInvestigating, UpdatedAt: now},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := store.context()
	defer cancel()
	if err := store.update(ctx, func(state *persistedState) (bool, error) {
		if len(state.Sessions) != 1 || state.Sessions["bob-session"] == nil || state.Sessions["alice-session"] != nil {
			t.Fatalf("canonical shared sessions = %+v", state.Sessions)
		}
		if state.Sessions["bob-session"].Active.Actor != "bob" {
			t.Fatalf("canonical active actor = %+v", state.Sessions["bob-session"].Active)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreVersionTwoRetainsFixBoundDuplicateAsRetired(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	ref := AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"}
	legacy := &persistedState{
		Version: 2,
		Sessions: map[string]*persistedSession{
			"fix-session": {
				Owner: "alice", ExpiresAt: now.Add(time.Hour),
				View:       SessionView{ID: "fix-session", Analysis: ref, UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339)},
				FixSources: map[string]persistedTestFixSource{"request": {FailureRevision: "deadbeef"}},
			},
			"current-session": {
				Owner: "bob", ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "current-session", Analysis: ref, UpdatedAt: now.Format(time.RFC3339)},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := store.context()
	defer cancel()
	if err := store.update(ctx, func(state *persistedState) (bool, error) {
		retired := state.Sessions["fix-session"]
		if len(state.Sessions) != 2 || retired == nil || !retired.Retired || state.Sessions["current-session"] == nil {
			t.Fatalf("retained Fix sessions = %+v", state.Sessions)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreBackfillsAttemptSummaries(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	legacy := &persistedState{
		Version: stateVersion,
		Sessions: map[string]*persistedSession{
			"session": {
				Owner: "alice", Turns: 2, ExpiresAt: now.Add(time.Hour),
				View: SessionView{
					ID: "session", Analysis: AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"},
					Messages: []Message{
						{Role: "user", RequestID: "success", Content: "legacy question", CreatedAt: now.Format(time.RFC3339)},
						{Role: "assistant", RequestID: "success", Content: "legacy answer", CreatedAt: now.Format(time.RFC3339)},
					},
				},
				Requests: map[string]persistedRequest{
					"success": {QuestionHash: hashText("legacy question"), Status: requestSucceeded},
					"pending": {QuestionHash: hashText("pending question"), Status: requestPending},
				},
				Active: &persistedActiveTurn{
					RequestID: "pending", Question: "pending question", LeaseID: "lease",
					ExpiresAt: now.Add(time.Minute), Phase: PhaseInvestigating, UpdatedAt: now.Add(time.Second),
				},
			},
		},
		OwnerRequests: map[string][]time.Time{},
	}
	if err := writePrivateJSON(store.statePath, legacy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := store.context()
	defer cancel()
	if err := store.update(ctx, func(state *persistedState) (bool, error) {
		success := state.Sessions["session"].Requests["success"]
		pending := state.Sessions["session"].Requests["pending"]
		if success.Question != "legacy question" || success.Turn != 1 || success.CreatedAt == "" || success.UpdatedAt == "" {
			t.Fatalf("migrated success = %+v", success)
		}
		if pending.Question != "pending question" || pending.Turn != 2 || pending.CreatedAt == "" || pending.UpdatedAt == "" {
			t.Fatalf("migrated pending = %+v", pending)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, migrated, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if migrated || loaded.Sessions["session"].Requests["success"].Question != "legacy question" {
		t.Fatalf("persisted migration = %+v", loaded.Sessions["session"].Requests)
	}
}

func TestSessionStoreBridgesVersionThreeToCurrent(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	versionThree := &persistedState{
		Version: 3,
		Sessions: map[string]*persistedSession{
			"session": {
				Owner: "alice", ExpiresAt: now.Add(time.Hour),
				View: SessionView{ID: "session", Analysis: AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"}},
				Active: &persistedActiveTurn{
					RequestID: "request", Question: "question", LeaseID: "lease",
					ExpiresAt: now.Add(time.Minute), Phase: PhaseInvestigating, UpdatedAt: now,
				},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, versionThree); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := store.context()
	defer cancel()
	if err := store.update(ctx, func(state *persistedState) (bool, error) {
		active := state.Sessions["session"].Active
		if state.Version != stateVersion || active == nil || active.Question != "question" || active.Actor != "alice" {
			t.Fatalf("bridged state = %+v", state)
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != stateVersion || persisted.Sessions["session"].Active.Question != "question" || persisted.Sessions["session"].Active.Actor != "alice" {
		t.Fatalf("persisted bridge state = %+v", persisted)
	}
}

func TestPersistResolvedBoundsPatternEvidenceBuilds(t *testing.T) {
	pattern := recurringPattern()
	pattern.Subject = strings.Repeat("s", 8<<10)
	pattern.SharedRootCause = strings.Repeat("r", 64<<10)
	pattern.SuggestedFix = strings.Repeat("f", 32<<10)
	pattern.Summary = strings.Repeat("m", 32<<10)
	pattern.SharedBuilds = make([]string, 100)
	pattern.RelevantFiles = make([]string, 100)
	for i := range 100 {
		pattern.SharedBuilds[i] = strings.Repeat("b", 300)
		pattern.RelevantFiles[i] = strings.Repeat("p", 1200)
	}
	resolved := resolvedAnalysis{
		ref:     AnalysisRef{Scope: ScopePattern, JobID: "job", PatternID: "pattern", PatternHash: "hash"},
		pattern: &pattern,
		evidenceBuilds: []ArtifactBuild{{
			BuildPrefix: "logs/job/1/",
			Build: models.BuildInfo{
				BuildID: "1", JobName: "job", JUnitURLs: []string{"private-junit-url"},
				RepoRefs: map[string]string{"example/repo": "main:deadbeef"},
			},
		}},
		comparison: &CauseComparison{
			ArtifactBuild: ArtifactBuild{
				BuildPrefix: "logs/job/2/",
				Build: models.BuildInfo{
					BuildID: "2", JobName: "job", Result: "SUCCESS", Passed: true,
					Started: time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC), Commit: "commit-two",
					JUnitURLs: []string{"private-comparison-junit"}, RepoRefs: map[string]string{"example/repo": "main:private"},
				},
			},
			TestNames: []string{"TestCluster"},
		},
	}
	pattern.Lifecycle = &models.PatternLifecycle{
		State: models.PatternLifecycleActive, Reason: "watching", RecoveryStreak: 1, RecoveryBuilds: []string{"2"},
	}
	resolved.pattern = &pattern
	persisted := persistResolved(resolved, sourceinvestigation.Repository{})
	if persisted.Pattern == nil || len(persisted.Pattern.Subject) > 4<<10 || len(persisted.Pattern.SharedRootCause) > 32<<10 ||
		len(persisted.Pattern.SuggestedFix) > 16<<10 || len(persisted.Pattern.Summary) > 16<<10 ||
		len(persisted.Pattern.SharedBuilds) != 50 || len(persisted.Pattern.RelevantFiles) != 50 {
		t.Fatalf("bounded pattern = %+v", persisted.Pattern)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-junit-url") || strings.Contains(string(encoded), "deadbeef") ||
		strings.Contains(string(encoded), "private-comparison-junit") || strings.Contains(string(encoded), "main:private") {
		t.Fatalf("oversized build metadata persisted: %s", encoded)
	}
	restored := restoreResolved(persisted)
	if len(restored.evidenceBuilds) != 1 || restored.evidenceBuilds[0].Build.BuildID != "1" || restored.evidenceBuilds[0].Build.JobName != "job" {
		t.Fatalf("restored evidence builds = %+v", restored.evidenceBuilds)
	}
	if restored.comparison == nil || restored.comparison.ArtifactBuild.Build.BuildID != "2" ||
		!restored.comparison.ArtifactBuild.Build.Passed || restored.comparison.ArtifactBuild.Build.Commit != "commit-two" ||
		!slices.Equal(restored.comparison.TestNames, []string{"TestCluster"}) {
		t.Fatalf("restored comparison = %+v", restored.comparison)
	}
	if restored.pattern.Lifecycle == nil || restored.pattern.Lifecycle.RecoveryStreak != 1 ||
		!slices.Equal(restored.pattern.Lifecycle.RecoveryBuilds, []string{"2"}) {
		t.Fatalf("restored lifecycle = %+v", restored.pattern.Lifecycle)
	}
}
