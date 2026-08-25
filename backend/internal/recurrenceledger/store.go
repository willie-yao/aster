package recurrenceledger

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const lockFileName = ".recurrence-ledger.lock"

// Update serializes ledger mutations across fetcher processes. It holds a
// dedicated lock, so it is safe to call while the pattern publication lock is
// already held.
func Update(dir string, mutate func(*Ledger) bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open recurrence ledger lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock recurrence ledger: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	ledger := Load(dir)
	if mutate == nil || !mutate(ledger) {
		return nil
	}
	return ledger.Save(dir)
}
