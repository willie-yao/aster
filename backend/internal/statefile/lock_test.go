package statefile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWithLockSerializesConcurrentReadModifyWrite proves the lock orders the
// sequence the server and the worker both run against issue_state.json. Without
// it each writer saves the copy it loaded and the later save silently drops the
// other's entry.
func TestWithLockSerializesConcurrentReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := (&State[string]{Repo: "owner/repo", Tracked: map[string]string{}}).Save(path); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, key := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithLock(path, func() error {
				state := Load[string](path, "owner/repo", "test")
				state.Tracked[key] = key
				return state.Save(path)
			})
			if err != nil {
				t.Errorf("WithLock(%s): %v", key, err)
			}
		}()
	}
	wg.Wait()

	final := Load[string](path, "owner/repo", "test")
	for _, key := range []string{"a", "b", "c", "d"} {
		if final.Tracked[key] != key {
			t.Errorf("key %q was lost: %v", key, final.Tracked)
		}
	}
}

func TestWithLockRunsUnlockedForEmptyPath(t *testing.T) {
	called := false
	if err := WithLock("  ", func() error { called = true; return nil }); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !called {
		t.Fatal("fn was not called for an empty path")
	}
}

func TestWithLockReleasesOnPanicFreeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WithLock(path, func() error { return os.ErrPermission }); err != os.ErrPermission {
		t.Fatalf("WithLock error = %v, want the callback error", err)
	}
	// A second acquisition proves the first released rather than deadlocking.
	if err := WithLock(path, func() error { return nil }); err != nil {
		t.Fatalf("second WithLock: %v", err)
	}
}
