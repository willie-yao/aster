//go:build linux

package runtime

import "syscall"

type processTreeTracker struct {
	root   int
	errors chan error
}

func newProcessTreeTracker(root int) (*processTreeTracker, error) {
	return &processTreeTracker{root: root, errors: make(chan error)}, nil
}

func (t *processTreeTracker) Errors() <-chan error { return t.errors }

func (t *processTreeTracker) Kill() {
	_ = syscall.Kill(-t.root, syscall.SIGKILL)
	_ = syscall.Kill(t.root, syscall.SIGKILL)
}

func (t *processTreeTracker) Close() {}
