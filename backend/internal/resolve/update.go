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

// RemoveMatching removes only entries that still match the staged values.
func RemoveMatching(dir string, expected map[string]Entry) error {
	return Update(dir, func(state *State) bool {
		changed := false
		for id, want := range expected {
			if current, ok := state.Resolved[id]; ok && current == want {
				delete(state.Resolved, id)
				changed = true
			}
		}
		return changed
	})
}
