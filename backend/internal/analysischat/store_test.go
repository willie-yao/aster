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

func TestPersistResolvedBoundsPatternEvidenceBuilds(t *testing.T) {
	resolved := resolvedAnalysis{
		ref: AnalysisRef{Scope: ScopePattern, JobID: "job", PatternID: "pattern", PatternHash: "hash"},
		evidenceBuilds: []ArtifactBuild{{
			BuildPrefix: "logs/job/1/",
			Build: models.BuildInfo{
				BuildID: "1", JobName: "job", JUnitURLs: []string{"private-junit-url"},
				RepoRefs: map[string]string{"example/repo": "main:deadbeef"},
			},
		}},
	}
	persisted := persistResolved(resolved, "")
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
