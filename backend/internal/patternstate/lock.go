// Package patternstate serializes pattern publication and write-side validation.
package patternstate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const lockFileName = ".pattern-publication.lock"

// WithLock runs fn while holding the shared pattern publication lock.
func WithLock(dataDir string, fn func() error) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open pattern publication lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock pattern publication: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	if fn == nil {
		return nil
	}
	return fn()
}
