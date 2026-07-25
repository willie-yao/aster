package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	previewStateVersion  = 1
	previewConfirmLease  = 10 * time.Minute
	maxPreviewStateBytes = 64 << 20
	maxPersistedPreviews = 128
	previewStatusReady   = "ready"
	previewStatusRunning = "confirming"
	previewStatusDone    = "confirmed"
)

type persistedPreview struct {
	Owner        string                      `json:"owner"`
	Kind         string                      `json:"kind"`
	CreatedAt    string                      `json:"created_at"`
	Status       string                      `json:"status"`
	ResultURL    string                      `json:"result_url,omitempty"`
	LeaseExpires string                      `json:"lease_expires,omitempty"`
	Issue        *issues.IssueSpec           `json:"issue,omitempty"`
	Fix          *fixpr.GeneratedFixSnapshot `json:"fix,omitempty"`
}

type previewState struct {
	Version  int                          `json:"version"`
	Previews map[string]*persistedPreview `json:"previews"`
}

type previewStore struct {
	path     string
	lockPath string
}

func newPreviewStore(dataDir string) *previewStore {
	return &previewStore{
		path:     filepath.Join(dataDir, "action_preview_state.json"),
		lockPath: filepath.Join(dataDir, ".action-preview.lock"),
	}
}

func (s *previewStore) stash(userToken string, entry *previewEntry) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("generating preview token: %w", err)
	}
	record, err := persistPreview(entry, tokenHash(userToken), time.Now().UTC())
	if err != nil {
		return "", err
	}
	err = s.update(func(state *previewState, now time.Time) (bool, error) {
		evictPersistedPreviews(state, now)
		state.Previews[tokenHash(token)] = record
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *previewStore) begin(userToken, token string) (*previewEntry, string, error) {
	var entry *previewEntry
	var resultURL string
	err := s.update(func(state *previewState, now time.Time) (bool, error) {
		changed := evictPersistedPreviews(state, now)
		record := state.Previews[tokenHash(token)]
		if record == nil || record.Owner != tokenHash(userToken) {
			return changed, ErrPreviewNotFound
		}
		if record.Status == previewStatusDone && record.ResultURL != "" {
			resultURL = record.ResultURL
			return changed, nil
		}
		if record.Status == previewStatusRunning {
			lease, leaseErr := time.Parse(time.RFC3339, record.LeaseExpires)
			if leaseErr == nil && now.Before(lease) {
				return changed, ErrPreviewPending
			}
		}
		var err error
		entry, err = restorePreview(record)
		if err != nil {
			return changed, err
		}
		record.Status = previewStatusRunning
		record.LeaseExpires = now.Add(previewConfirmLease).Format(time.RFC3339)
		record.CreatedAt = now.Format(time.RFC3339)
		return true, nil
	})
	return entry, resultURL, err
}

func (s *previewStore) finish(userToken, token, resultURL string, confirmErr error) error {
	return s.update(func(state *previewState, now time.Time) (bool, error) {
		record := state.Previews[tokenHash(token)]
		if record == nil || record.Owner != tokenHash(userToken) {
			return false, ErrPreviewNotFound
		}
		record.LeaseExpires = ""
		if confirmErr != nil {
			record.Status = previewStatusReady
			return true, nil
		}
		if resultURL == "" {
			record.Status = previewStatusReady
			return true, fmt.Errorf("confirmation returned an empty result URL")
		}
		record.Status = previewStatusDone
		record.ResultURL = resultURL
		record.CreatedAt = now.Format(time.RFC3339)
		return true, nil
	})
}

func (s *previewStore) take(userToken, token string) (*previewEntry, error) {
	var entry *previewEntry
	err := s.update(func(state *previewState, now time.Time) (bool, error) {
		changed := evictPersistedPreviews(state, now)
		key := tokenHash(token)
		record := state.Previews[key]
		if record == nil || record.Owner != tokenHash(userToken) {
			return changed, ErrPreviewNotFound
		}
		var err error
		entry, err = restorePreview(record)
		if err != nil {
			return changed, err
		}
		delete(state.Previews, key)
		return true, nil
	})
	return entry, err
}

func (s *previewStore) update(fn func(*previewState, time.Time) (bool, error)) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening preview state lock: %w", err)
	}
	defer lock.Close()
	_ = os.Chmod(s.lockPath, 0o600)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking preview state: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	state, err := s.load()
	if err != nil {
		return err
	}
	changed, opErr := fn(state, time.Now().UTC())
	if changed {
		if err := statefile.WriteJSON(s.path, state); err != nil {
			return fmt.Errorf("writing preview state: %w", err)
		}
		_ = os.Chmod(s.path, 0o600)
	}
	return opErr
}

func (s *previewStore) load() (*previewState, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return freshPreviewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening preview state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPreviewStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading preview state: %w", err)
	}
	if len(data) > maxPreviewStateBytes {
		return nil, fmt.Errorf("preview state exceeds %d bytes", maxPreviewStateBytes)
	}
	state := freshPreviewState()
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("decoding preview state: %w", err)
	}
	if state.Version != previewStateVersion || state.Previews == nil {
		return nil, fmt.Errorf("unsupported preview state version %d", state.Version)
	}
	return state, nil
}

func freshPreviewState() *previewState {
	return &previewState{Version: previewStateVersion, Previews: map[string]*persistedPreview{}}
}

func persistPreview(entry *previewEntry, owner string, now time.Time) (*persistedPreview, error) {
	if entry == nil {
		return nil, ErrPreviewNotFound
	}
	record := &persistedPreview{
		Owner: owner, Kind: entry.kind, CreatedAt: now.Format(time.RFC3339), Status: previewStatusReady,
	}
	switch entry.kind {
	case "issue":
		spec := entry.spec
		record.Issue = &spec
	case gfKind:
		if entry.fix == nil {
			return nil, fmt.Errorf("preview has no generated fix")
		}
		record.Fix = entry.fix.Snapshot()
	default:
		return nil, fmt.Errorf("unsupported preview kind %q", entry.kind)
	}
	return record, nil
}

func restorePreview(record *persistedPreview) (*previewEntry, error) {
	if record == nil {
		return nil, ErrPreviewNotFound
	}
	entry := &previewEntry{kind: record.Kind}
	switch record.Kind {
	case "issue":
		if record.Issue == nil {
			return nil, fmt.Errorf("persisted preview has no issue draft")
		}
		entry.spec = *record.Issue
	case gfKind:
		if record.Fix == nil {
			return nil, fmt.Errorf("persisted preview has no fix draft")
		}
		entry.fix = fixpr.RestoreGeneratedFix(record.Fix)
	default:
		return nil, fmt.Errorf("persisted preview has invalid kind %q", record.Kind)
	}
	return entry, nil
}

func evictPersistedPreviews(state *previewState, now time.Time) bool {
	changed := false
	cutoff := now.Add(-previewTTL)
	for key, record := range state.Previews {
		created, err := time.Parse(time.RFC3339, record.CreatedAt)
		if err != nil || created.Before(cutoff) {
			delete(state.Previews, key)
			changed = true
		}
	}
	if len(state.Previews) <= maxPersistedPreviews {
		return changed
	}
	type item struct{ key, created string }
	items := make([]item, 0, len(state.Previews))
	for key, record := range state.Previews {
		items = append(items, item{key: key, created: record.CreatedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].created < items[j].created })
	for len(state.Previews) > maxPersistedPreviews {
		delete(state.Previews, items[0].key)
		items = items[1:]
		changed = true
	}
	return changed
}
