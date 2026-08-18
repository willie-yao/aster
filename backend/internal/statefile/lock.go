package statefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// WithLock runs fn while holding an exclusive advisory lock on a sidecar of
// path. Callers that load, mutate, and save one state file must hold it across
// that whole sequence: atomic replacement stops a reader from seeing a partial
// file, but it does not stop a second process from saving a copy it loaded
// before the first process wrote. The server files issues and the worker
// recovers them against the same issue_state.json, so an unlocked sequence can
// drop a freshly filed issue from tracking and leave it open forever.
//
// The lock is advisory and only orders processes that call WithLock on the same
// path. An empty path runs fn unlocked, matching the in-memory test managers
// that have no file to guard.
func WithLock(path string, fn func() error) error {
	if strings.TrimSpace(path) == "" {
		return fn()
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock state file: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	return fn()
}
