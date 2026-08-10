//go:build linux || darwin

package analysisexecutor

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readSingleResultFile(root, name string, limit int64) (string, error) {
	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open result directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirFD), root)
	if dir == nil {
		_ = unix.Close(dirFD)
		return "", fmt.Errorf("open result directory")
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return "", fmt.Errorf("read result directory: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		return "", fmt.Errorf("OpenCode must write exactly result/%s", name)
	}
	fileFD, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open analysis result: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), name)
	if file == nil {
		_ = unix.Close(fileFD)
		return "", fmt.Errorf("open analysis result")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("analysis result is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("analysis result exceeds %d bytes", limit)
	}
	return string(data), nil
}
