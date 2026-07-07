package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

type tracked struct {
	N int `json:"n"`
}

func TestWriteJSON_AtomicAndParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.json")
	if err := WriteJSON(path, map[string]int{"a": 1}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Best-effort chmod runs on POSIX; the file must be readable at 0644.
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
	// No leftover temp files beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestLoad_MissingFileYieldsFreshState(t *testing.T) {
	s := Load[tracked](filepath.Join(t.TempDir(), "none.json"), "o/r", "test")
	if s == nil || s.Tracked == nil {
		t.Fatal("expected non-nil state with non-nil Tracked")
	}
	if s.Repo != "o/r" {
		t.Errorf("Repo = %q, want o/r", s.Repo)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State[tracked]{Repo: "o/r", Tracked: map[string]tracked{"k": {N: 7}}}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := Load[tracked](path, "o/r", "test")
	if got.Tracked["k"].N != 7 {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

func TestLoad_DiscardsDifferentRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := &State[tracked]{Repo: "old/repo", Tracked: map[string]tracked{"k": {N: 1}}}
	if err := old.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Loading for a different repo must discard the old tracked items so numbers
	// are never reused across repos.
	got := Load[tracked](path, "new/repo", "test")
	if len(got.Tracked) != 0 {
		t.Errorf("expected fresh state for new repo, got %+v", got.Tracked)
	}
	if got.Repo != "new/repo" {
		t.Errorf("Repo = %q, want new/repo", got.Repo)
	}
}
