package recurrenceledger

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const lockFileName = ".recurrence-ledger.lock"

// Update serializes ledger mutations across the fetcher and the server. It holds
// a dedicated lock, so it is safe to call while the pattern publication lock is
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

// Store is a directory-bound ledger accessor. It satisfies the durable-memory
// seam the remediation investigation service consumes.
type Store struct {
	dir string
	now func() time.Time
}

// NewStore binds a ledger to the fetcher output directory.
func NewStore(dir string) *Store {
	return &Store{dir: dir, now: time.Now}
}

// ClaimReuse returns a prior answer for a cause that has already been
// investigated and charges it against that verdict's bounded reuse budget, so a
// repeat request does not re-spend model budget while no conclusion answers
// indefinitely. A failure to record the claim yields no reuse, so the caller
// investigates rather than reusing without accounting.
func (s *Store) ClaimReuse(signature string) (Verdict, bool, error) {
	if s == nil {
		return Verdict{}, false, nil
	}
	var verdict Verdict
	var ok bool
	err := Update(s.dir, func(ledger *Ledger) bool {
		verdict, ok = ledger.ClaimReuse(signature, s.now())
		return ok
	})
	if err != nil {
		return Verdict{}, false, err
	}
	return verdict, ok, nil
}

// RecordVerdict persists one terminal answer. Non-terminal states are ignored,
// and the ledger refuses an answer older than the one already on record.
func (s *Store) RecordVerdict(signature string, verdict Verdict) error {
	if s == nil || !TerminalVerdictState(verdict.State) {
		return nil
	}
	return Update(s.dir, func(ledger *Ledger) bool {
		return ledger.RecordVerdict(signature, verdict, s.now())
	})
}
