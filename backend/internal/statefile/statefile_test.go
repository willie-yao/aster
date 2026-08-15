package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestWriteJSONDurableSyncsFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	fileSynced := false
	dirSynced := false
	ops := defaultWriteOps()
	ops.sync = func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			dirSynced = true
			if _, err := os.Stat(path); err != nil {
				return err
			}
		} else {
			fileSynced = true
		}
		return nil
	}
	err := writeJSON(path, map[string]int{"a": 1}, writeOptions{
		parentPerm: 0o755,
		filePerm:   0o600,
		durable:    true,
	}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !fileSynced || !dirSynced {
		t.Fatalf("sync calls = file:%t dir:%t", fileSynced, dirSynced)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWritePrivateJSONDurableToleratesUnsupportedModeOperations(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		err   error
	}{
		{name: "parent EPERM", phase: "parent", err: syscall.EPERM},
		{name: "temporary EPERM", phase: "temporary", err: syscall.EPERM},
		{name: "final EPERM", phase: "final", err: syscall.EPERM},
		{name: "temporary ENOTSUP", phase: "temporary", err: syscall.ENOTSUP},
		{name: "temporary ENOSYS", phase: "temporary", err: syscall.ENOSYS},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "private", "state.json")
			parent := filepath.Dir(path)
			ops := defaultWriteOps()
			if testCase.phase == "temporary" {
				ops.chmodFile = func(*os.File, os.FileMode) error { return testCase.err }
			} else {
				ops.chmodPath = func(name string, mode os.FileMode) error {
					if testCase.phase == "parent" && name == parent {
						return testCase.err
					}
					if testCase.phase == "final" && name == path {
						return testCase.err
					}
					return os.Chmod(name, mode)
				}
			}
			if err := writeJSON(path, map[string]string{"value": "new"}, privateWriteOptions(), ops); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"new"`) {
				t.Fatalf("written data = %s", data)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
				t.Fatalf("parent entries = %v", entries)
			}
		})
	}
}

func TestWritePrivateJSONDurableRejectsOtherModeErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.json")
	want := errors.New("chmod failed")
	ops := defaultWriteOps()
	ops.chmodFile = func(*os.File, os.FileMode) error { return want }
	if err := writeJSON(path, map[string]int{"a": 1}, privateWriteOptions(), ops); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestWritePrivateJSONDurableOperationalFailuresRemainFatal(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*writeOps, error)
	}{
		{
			name: "mkdir",
			configure: func(ops *writeOps, want error) {
				ops.mkdirAll = func(string, os.FileMode) error { return want }
			},
		},
		{
			name: "create",
			configure: func(ops *writeOps, want error) {
				ops.createTemp = func(string, string) (*os.File, error) { return nil, want }
			},
		},
		{
			name: "write",
			configure: func(ops *writeOps, want error) {
				ops.write = func(*os.File, []byte) (int, error) { return 0, want }
			},
		},
		{
			name: "file sync",
			configure: func(ops *writeOps, want error) {
				original := ops.sync
				ops.sync = func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if !info.IsDir() {
						return want
					}
					return original(file)
				}
			},
		},
		{
			name: "close",
			configure: func(ops *writeOps, want error) {
				original := ops.close
				ops.close = func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if !info.IsDir() {
						_ = original(file)
						return want
					}
					return original(file)
				}
			},
		},
		{
			name: "rename",
			configure: func(ops *writeOps, want error) {
				ops.rename = func(string, string) error { return want }
			},
		},
		{
			name: "open parent",
			configure: func(ops *writeOps, want error) {
				ops.open = func(string) (*os.File, error) { return nil, want }
			},
		},
		{
			name: "parent sync",
			configure: func(ops *writeOps, want error) {
				original := ops.sync
				ops.sync = func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if info.IsDir() {
						return want
					}
					return original(file)
				}
			},
		},
		{
			name: "parent close",
			configure: func(ops *writeOps, want error) {
				original := ops.close
				ops.close = func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if info.IsDir() {
						_ = original(file)
						return want
					}
					return original(file)
				}
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "state.json")
			want := errors.New(testCase.name + " failed")
			ops := defaultWriteOps()
			testCase.configure(&ops, want)
			if err := writeJSON(path, map[string]int{"a": 1}, privateWriteOptions(), ops); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestWriteJSONDurableSyncFailurePreservesPriorState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteJSON(path, map[string]string{"value": "old"}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("sync failed")
	ops := defaultWriteOps()
	ops.sync = func(*os.File) error { return want }
	err := writeJSON(path, map[string]string{"value": "new"}, writeOptions{
		parentPerm: 0o755,
		filePerm:   0o600,
		durable:    true,
	}, ops)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"old"`) {
		t.Fatalf("prior state was replaced: %s", data)
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
		t.Errorf("round-trip lost data: %+v", got.Tracked)
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

func privateWriteOptions() writeOptions {
	return writeOptions{
		parentPerm:        0o700,
		filePerm:          0o600,
		enforceParentMode: true,
		enforceFinalMode:  true,
		durable:           true,
		trailingNewline:   true,
	}
}
