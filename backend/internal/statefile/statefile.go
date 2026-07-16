// Package statefile provides atomic JSON file writes and a repo-scoped tracking
// state shared by the output writer and the issue and fix-PR managers.
// Centralizing the writer keeps the atomic-rename and filesystem-compatibility
// behavior in one place.
package statefile

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// WriteJSON marshals v as indented JSON and writes it to path atomically: it
// writes a temp file in the same directory and renames it into place, so a
// concurrent reader (the server in Kubernetes-native mode) never observes a
// half-written file. Parent directories are created as needed.
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Best-effort: some filesystems (SMB/azurefile RWX) don't support chmod and
	// return EPERM, where the mount's file_mode governs readability instead.
	_ = os.Chmod(tmpName, 0o644)
	return os.Rename(tmpName, path)
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
