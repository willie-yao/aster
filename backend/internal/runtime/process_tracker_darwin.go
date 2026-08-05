//go:build darwin

package runtime

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	pid  int
	sec  int64
	usec int32
}

type processTreeTracker struct {
	root      int
	mu        sync.Mutex
	tracked   map[int]processIdentity
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
}

func newProcessTreeTracker(root int) (*processTreeTracker, error) {
	tracker := &processTreeTracker{
		root:    root,
		tracked: make(map[int]processIdentity),
		errors:  make(chan error, 1),
		done:    make(chan struct{}),
	}
	if err := tracker.update(); err != nil {
		return nil, err
	}
	go tracker.watch()
	return tracker, nil
}

func (t *processTreeTracker) watch() {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := t.update(); err != nil && !errors.Is(err, unix.EINTR) {
				t.report(fmt.Errorf("track sandbox process tree: %w", err))
				return
			}
		case <-t.done:
			return
		}
	}
}

func (t *processTreeTracker) update() error {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return err
	}
	current := make(map[int]processIdentity, len(processes))
	children := make(map[int][]int)
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		parent := int(process.Eproc.Ppid)
		if pid <= 0 {
			continue
		}
		current[pid] = processIdentity{
			pid:  pid,
			sec:  process.Proc.P_starttime.Sec,
			usec: process.Proc.P_starttime.Usec,
		}
		if parent > 0 {
			children[parent] = append(children[parent], pid)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, identity := range t.tracked {
		if current[pid] != identity {
			delete(t.tracked, pid)
		}
	}
	if root, ok := current[t.root]; ok {
		t.tracked[t.root] = root
	}
	queue := make([]int, 0, len(t.tracked))
	for pid := range t.tracked {
		queue = append(queue, pid)
	}
	seen := make(map[int]struct{}, len(queue))
	for _, pid := range queue {
		seen[pid] = struct{}{}
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			t.tracked[child] = current[child]
			queue = append(queue, child)
		}
	}
	return nil
}

func (t *processTreeTracker) report(err error) {
	select {
	case t.errors <- err:
	default:
	}
}

func (t *processTreeTracker) Errors() <-chan error { return t.errors }

func (t *processTreeTracker) Kill() {
	_ = syscall.Kill(-t.root, syscall.SIGSTOP)
	_ = t.update()
	t.mu.Lock()
	identities := make([]processIdentity, 0, len(t.tracked))
	for _, identity := range t.tracked {
		identities = append(identities, identity)
	}
	t.mu.Unlock()
	for _, identity := range identities {
		if processIdentityMatches(identity) {
			_ = syscall.Kill(identity.pid, syscall.SIGSTOP)
		}
	}
	for _, identity := range identities {
		if processIdentityMatches(identity) {
			_ = syscall.Kill(identity.pid, syscall.SIGKILL)
		}
	}
	_ = syscall.Kill(-t.root, syscall.SIGKILL)
	_ = syscall.Kill(t.root, syscall.SIGKILL)
	t.Close()
}

func (t *processTreeTracker) Close() {
	t.closeOnce.Do(func() { close(t.done) })
}

func processIdentityMatches(want processIdentity) bool {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", want.pid)
	if err != nil {
		return false
	}
	return int(process.Proc.P_pid) == want.pid &&
		process.Proc.P_starttime.Sec == want.sec &&
		process.Proc.P_starttime.Usec == want.usec
}
