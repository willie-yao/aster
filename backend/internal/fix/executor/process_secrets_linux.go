//go:build linux

package executor

import "golang.org/x/sys/unix"

func lockProcessSecrets() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
