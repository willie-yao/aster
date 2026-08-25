package resolve

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const lockFileName = ".resolved.lock"

// Update serializes resolved-state mutations across the fetcher and server.
func Update(dir string, mutate func(*State) bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open resolved-state lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock resolved state: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	state := Load(dir)
	if mutate == nil || !mutate(state) {
		return nil
	}
	return state.Save(dir)
}

// RemoveMatching removes only the pattern and cause entries that still match the
// staged values, so a resolution rewritten since staging is left alone.
func RemoveMatching(dir string, expected *State) error {
	if expected == nil {
		return nil
	}
	return Update(dir, func(state *State) bool {
		changed := false
		for id, want := range expected.Resolved {
			if current, ok := state.Resolved[id]; ok && current == want {
				delete(state.Resolved, id)
				changed = true
			}
		}
		for signature, want := range expected.Causes {
			if current, ok := state.Causes[signature]; ok && current == want {
				delete(state.Causes, signature)
				changed = true
			}
		}
		return changed
	})
}
