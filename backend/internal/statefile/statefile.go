// Package statefile provides atomic JSON file writes and a repo-scoped tracking
// state shared by the output writer and the issue and fix-PR managers.
// Centralizing the writer keeps the atomic-rename and filesystem-compatibility
// behavior in one place.
package statefile

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// WriteJSON marshals v as indented JSON and writes it to path atomically: it
// writes a temp file in the same directory and renames it into place, so a
// concurrent reader (the server in Kubernetes-native mode) never observes a
// half-written file. Parent directories are created as needed.
func WriteJSON(path string, v any) error {
	return writeJSON(path, v, writeOptions{parentPerm: 0o755, filePerm: 0o644}, defaultWriteOps())
}

// WriteJSONDurable writes private JSON atomically and syncs the file and parent
// directory before returning.
func WriteJSONDurable(path string, v any) error {
	return writeJSON(path, v, writeOptions{parentPerm: 0o755, filePerm: 0o600, durable: true}, defaultWriteOps())
}

// WritePrivateJSONDurable writes private JSON with restrictive POSIX modes,
// atomic replacement, and file and parent-directory syncs. Filesystems that
// enforce permissions at mount level may reject only the mode changes.
func WritePrivateJSONDurable(path string, v any) error {
	return writeJSON(path, v, writeOptions{
		parentPerm:        0o700,
		filePerm:          0o600,
		enforceParentMode: true,
		enforceFinalMode:  true,
		durable:           true,
		trailingNewline:   true,
	}, defaultWriteOps())
}

type writeOptions struct {
	parentPerm        os.FileMode
	filePerm          os.FileMode
	enforceParentMode bool
	enforceFinalMode  bool
	durable           bool
	trailingNewline   bool
}

type writeOps struct {
	mkdirAll   func(string, os.FileMode) error
	chmodPath  func(string, os.FileMode) error
	createTemp func(string, string) (*os.File, error)
	chmodFile  func(*os.File, os.FileMode) error
	write      func(*os.File, []byte) (int, error)
	sync       func(*os.File) error
	close      func(*os.File) error
	rename     func(string, string) error
	open       func(string) (*os.File, error)
	remove     func(string) error
}

func defaultWriteOps() writeOps {
	return writeOps{
		mkdirAll:   os.MkdirAll,
		chmodPath:  os.Chmod,
		createTemp: os.CreateTemp,
		chmodFile:  func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) },
		write:      func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		sync:       func(file *os.File) error { return file.Sync() },
		close:      func(file *os.File) error { return file.Close() },
		rename:     os.Rename,
		open:       os.Open,
		remove:     os.Remove,
	}
}

func writeJSON(path string, v any, options writeOptions, ops writeOps) error {
	parent := filepath.Dir(path)
	if err := ops.mkdirAll(parent, options.parentPerm); err != nil {
		return err
	}
	if options.enforceParentMode {
		if err := tolerateUnsupportedMode(ops.chmodPath(parent, options.parentPerm)); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if options.trailingNewline {
		data = append(data, '\n')
	}
	tmp, err := ops.createTemp(parent, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = ops.remove(tmpName) }()
	if err := tolerateUnsupportedMode(ops.chmodFile(tmp, options.filePerm)); err != nil {
		_ = ops.close(tmp)
		return err
	}
	written, err := ops.write(tmp, data)
	if err != nil {
		_ = ops.close(tmp)
		return err
	}
	if written != len(data) {
		_ = ops.close(tmp)
		return io.ErrShortWrite
	}
	if options.durable {
		if err := ops.sync(tmp); err != nil {
			_ = ops.close(tmp)
			return err
		}
	}
	if err := ops.close(tmp); err != nil {
		return err
	}
	if err := ops.rename(tmpName, path); err != nil {
		return err
	}
	if options.enforceFinalMode {
		if err := tolerateUnsupportedMode(ops.chmodPath(path, options.filePerm)); err != nil {
			return err
		}
	}
	if !options.durable {
		return nil
	}
	dir, err := ops.open(parent)
	if err != nil {
		return err
	}
	if err := ops.sync(dir); err != nil {
		_ = ops.close(dir)
		return err
	}
	return ops.close(dir)
}

func tolerateUnsupportedMode(err error) error {
	if err == nil || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) {
		return nil
	}
	return err
}

// State is the on-disk tracking state for a repo-scoped manager: a set of
// tracked items keyed by a stable dedup key, scoped to the target repo so state
// from a different repo is never reused.
type State[T any] struct {
	// Repo is the "owner/name" the tracked items belong to. State for a
	// different repo is discarded on load, so retargeting never mis-skips a
	// finding or mutates an unrelated item.
	Repo    string       `json:"repo,omitempty"`
	Tracked map[string]T `json:"tracked"`
}

// Load reads the tracking state at path for repo. It returns a ready, non-nil
// State with a non-nil Tracked map in every case: a missing file, a parse
// error, or state scoped to a different repo all yield fresh empty state.
// label prefixes the log lines (e.g. "issues").
func Load[T any](path, repo, label string) *State[T] {
	fresh := &State[T]{Repo: repo, Tracked: map[string]T{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return fresh // no state yet
	}
	var s State[T]
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Warning: failed to parse %s state: %v", label, err)
		return fresh
	}
	if s.Repo != "" && s.Repo != repo {
		log.Printf("%s: target repo changed (%s -> %s); starting state fresh", label, s.Repo, repo)
		return fresh
	}
	if s.Tracked == nil {
		s.Tracked = map[string]T{}
	}
	s.Repo = repo
	return &s
}

// Save writes the state to path atomically.
func (s *State[T]) Save(path string) error {
	return WriteJSON(path, s)
}
