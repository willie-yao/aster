package analysischat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
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
	state, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	session := state.Sessions["session"]
	if state.Version != stateVersion || session.View.Analysis.Scope != ScopeTest || session.Resolved.Ref.Scope != ScopeTest {
		t.Fatalf("migrated state = %+v", state)
	}
}

func TestSessionStoreVersionTwoActiveQuestionIsRollingCompatible(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStore(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	current := &persistedState{
		Version: stateVersion,
		Sessions: map[string]*persistedSession{
			"session": {
				Owner: "alice", ExpiresAt: now.Add(time.Hour),
				View: SessionView{
					ID: "session", Analysis: AnalysisRef{Scope: ScopeTest, JobID: "job", BuildID: "1", TestName: "test"},
				},
				Active: &persistedActiveTurn{
					RequestID: "request", Question: "question", LeaseID: "lease", ExpiresAt: now.Add(time.Minute),
					Phase: PhaseInvestigating, UpdatedAt: now,
				},
			},
		},
	}
	if err := writePrivateJSON(store.statePath, current); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sessions["session"].Active.Question != "question" {
		t.Fatalf("active question = %q", loaded.Sessions["session"].Active.Question)
	}
	data, err := os.ReadFile(store.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var oldReader struct {
		Version  int `json:"version"`
		Sessions map[string]struct {
			Active *struct {
				RequestID string `json:"request_id"`
			} `json:"active,omitempty"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &oldReader); err != nil {
		t.Fatal(err)
	}
	if oldReader.Version != stateVersion || oldReader.Sessions["session"].Active.RequestID != "request" {
		t.Fatalf("old reader state = %+v", oldReader)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	sessions := raw["sessions"].(map[string]any)
	session := sessions["session"].(map[string]any)
	active := session["active"].(map[string]any)
	delete(active, "question")
	if err := writePrivateJSON(store.statePath, raw); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != stateVersion || loaded.Sessions["session"].Active.Question != "" {
		t.Fatalf("old writer state = %+v", loaded)
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
	}
	persisted := persistResolved(resolved, "")
	if persisted.Pattern == nil || len(persisted.Pattern.Subject) > 4<<10 || len(persisted.Pattern.SharedRootCause) > 32<<10 ||
		len(persisted.Pattern.SuggestedFix) > 16<<10 || len(persisted.Pattern.Summary) > 16<<10 ||
		len(persisted.Pattern.SharedBuilds) != 50 || len(persisted.Pattern.RelevantFiles) != 50 {
		t.Fatalf("bounded pattern = %+v", persisted.Pattern)
	}
	encoded, err := json.Marshal(persisted.EvidenceBuilds)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-junit-url") || strings.Contains(string(encoded), "deadbeef") {
		t.Fatalf("oversized build metadata persisted: %s", encoded)
	}
	restored := restoreResolved(persisted)
	if len(restored.evidenceBuilds) != 1 || restored.evidenceBuilds[0].Build.BuildID != "1" || restored.evidenceBuilds[0].Build.JobName != "job" {
		t.Fatalf("restored evidence builds = %+v", restored.evidenceBuilds)
	}
}
