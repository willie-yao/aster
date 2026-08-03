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
	previewStateVersion     = 3
	maxPreviewStateBytes    = 64 << 20
	maxPersistedPreviews    = 128
	previewStatusReady      = "ready"
	previewStatusRunning    = "confirming"
	previewStatusDone       = "confirmed"
	previewStatusUnknown    = "unknown"
	previewAttemptReconcile = "reconcile"
)

type persistedPreview struct {
	Owner               string                      `json:"owner"`
	Kind                string                      `json:"kind"`
	FailureID           string                      `json:"failure_id,omitempty"`
	PatternHash         string                      `json:"pattern_hash,omitempty"`
	TargetRepo          string                      `json:"target_repo"`
	TargetConfig        string                      `json:"target_config,omitempty"`
	VerificationVersion int                         `json:"verification_version,omitempty"`
	CreatedAt           string                      `json:"created_at"`
	Status              string                      `json:"status"`
	ResultURL           string                      `json:"result_url,omitempty"`
	LeaseExpires        string                      `json:"lease_expires,omitempty"`
	AttemptID           string                      `json:"attempt_id,omitempty"`
	AttemptMode         string                      `json:"attempt_mode,omitempty"`
	Issue               *issues.IssueSpec           `json:"issue,omitempty"`
	Fix                 *fixpr.GeneratedFixSnapshot `json:"fix,omitempty"`
}

type previewState struct {
	Version  int                          `json:"version"`
	Previews map[string]*persistedPreview `json:"previews"`
}

type previewStore struct {
	path     string
	lockPath string
	maxBytes int
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
	key := tokenHash(token)
	record, err := persistPreview(entry, tokenHash(userToken), time.Now().UTC())
	if err != nil {
		return "", err
	}
	err = s.updateProtected(key, func(state *previewState, now time.Time) (bool, error) {
		for _, existing := range state.Previews {
			if samePreviewAction(existing, record) &&
				(existing.Status == previewStatusRunning || existing.Status == previewStatusUnknown) {
				return false, ErrPreviewPending
			}
		}
		record.CreatedAt = now.Format(time.RFC3339Nano)
		state.Previews[key] = record
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *previewStore) begin(userToken, token string, lease time.Duration) (*previewEntry, string, string, bool, error) {
	if lease <= 0 {
		lease = defaultRequestTimeout
	}
	attemptID, err := newToken()
	if err != nil {
		return nil, "", "", false, fmt.Errorf("generating confirmation attempt id: %w", err)
	}
	var entry *previewEntry
	var resultURL string
	var reconcile bool
	err = s.update(func(state *previewState, now time.Time) (bool, error) {
		record := state.Previews[tokenHash(token)]
		if record == nil || record.Owner != tokenHash(userToken) {
			return false, ErrPreviewNotFound
		}
		if record.Status == previewStatusDone && record.ResultURL != "" {
			resultURL = record.ResultURL
			return false, nil
		}
		for otherKey, other := range state.Previews {
			if otherKey != tokenHash(token) && samePreviewAction(other, record) &&
				(other.Status == previewStatusRunning || other.Status == previewStatusUnknown) {
				return false, ErrPreviewPending
			}
		}
		if record.Status == previewStatusRunning {
			lease, leaseErr := time.Parse(time.RFC3339, record.LeaseExpires)
			if leaseErr == nil && now.Before(lease) {
				return false, ErrPreviewPending
			}
		}
		var err error
		entry, err = restorePreview(record)
		if err != nil {
			return false, err
		}
		reconcile = record.Status == previewStatusUnknown
		record.Status = previewStatusRunning
		record.LeaseExpires = now.Add(lease).Format(time.RFC3339Nano)
		record.AttemptID = attemptID
		if reconcile {
			record.AttemptMode = previewAttemptReconcile
		} else {
			record.AttemptMode = ""
			record.CreatedAt = now.Format(time.RFC3339Nano)
		}
		return true, nil
	})
	return entry, resultURL, attemptID, reconcile, err
}

func (s *previewStore) finish(userToken, token, attemptID, resultURL string, confirmErr error) error {
	return s.update(func(state *previewState, now time.Time) (bool, error) {
		record := state.Previews[tokenHash(token)]
		if record == nil || record.Owner != tokenHash(userToken) {
			return false, ErrPreviewNotFound
		}
		if record.Status != previewStatusRunning || record.AttemptID != attemptID {
			return false, ErrPreviewSuperseded
		}
		record.LeaseExpires = ""
		record.AttemptID = ""
		reconciling := record.AttemptMode == previewAttemptReconcile
		record.AttemptMode = ""
		if confirmErr != nil {
			record.Status = previewStatusUnknown
			if !reconciling {
				record.CreatedAt = now.Format(time.RFC3339Nano)
			}
			return true, nil
		}
		if resultURL == "" {
			record.Status = previewStatusUnknown
			if !reconciling {
				record.CreatedAt = now.Format(time.RFC3339Nano)
			}
			return true, fmt.Errorf("confirmation returned an empty result URL")
		}
		record.Status = previewStatusDone
		record.ResultURL = resultURL
		record.CreatedAt = now.Format(time.RFC3339Nano)
		return true, nil
	})
}

func (s *previewStore) take(userToken, token string) (*previewEntry, error) {
	var entry *previewEntry
	err := s.update(func(state *previewState, now time.Time) (bool, error) {
		key := tokenHash(token)
		record := state.Previews[key]
		if record == nil || record.Owner != tokenHash(userToken) {
			return false, ErrPreviewNotFound
		}
		var err error
		entry, err = restorePreview(record)
		if err != nil {
			return false, err
		}
		delete(state.Previews, key)
		return true, nil
	})
	return entry, err
}

func (s *previewStore) discard(userToken, token, attemptID string) error {
	return s.update(func(state *previewState, _ time.Time) (bool, error) {
		key := tokenHash(token)
		record := state.Previews[key]
		if record == nil || record.Owner != tokenHash(userToken) || record.AttemptID != attemptID {
			return false, ErrPreviewSuperseded
		}
		delete(state.Previews, key)
		return true, nil
	})
}

func (s *previewStore) update(fn func(*previewState, time.Time) (bool, error)) error {
	return s.updateProtected("", fn)
}

func (s *previewStore) updateProtected(protectedKey string, fn func(*previewState, time.Time) (bool, error)) error {
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
	now := time.Now().UTC()
	changed := evictPersistedPreviews(state, now)
	opChanged, opErr := fn(state, now)
	changed = changed || opChanged
	if changed {
		if err := fitPreviewState(state, s.maxBytes, protectedKey); err != nil {
			return err
		}
		if err := statefile.WriteJSONDurable(s.path, state); err != nil {
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
	maxBytes := s.maxBytes
	if maxBytes <= 0 {
		maxBytes = maxPreviewStateBytes
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading preview state: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("preview state exceeds %d bytes", maxBytes)
	}
	state := freshPreviewState()
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("decoding preview state: %w", err)
	}
	if state.Previews == nil || (state.Version != 1 && state.Version != 2 && state.Version != previewStateVersion) {
		return nil, fmt.Errorf("unsupported preview state version %d", state.Version)
	}
	if state.Version == 1 {
		for key, record := range state.Previews {
			if record != nil && (record.FailureID == "" || record.PatternHash == "") && (record.Status == previewStatusReady || record.Status == previewStatusRunning) {
				delete(state.Previews, key)
			}
		}
		state.Version = 2
	}
	if state.Version == 2 {
		for key, record := range state.Previews {
			if record != nil && record.VerificationVersion != sourceVerificationVersion &&
				(record.Status == previewStatusReady || record.Status == previewStatusRunning) {
				delete(state.Previews, key)
			}
		}
		state.Version = previewStateVersion
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
		Owner: owner, Kind: entry.kind, FailureID: entry.failureID, PatternHash: entry.patternHash, TargetRepo: entry.targetRepo, TargetConfig: entry.targetConfig, VerificationVersion: entry.verificationVersion,
		CreatedAt: now.Format(time.RFC3339Nano), Status: previewStatusReady,
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
	entry := &previewEntry{failureID: record.FailureID, patternHash: record.PatternHash, kind: record.Kind, targetRepo: record.TargetRepo, targetConfig: record.TargetConfig, verificationVersion: record.VerificationVersion}
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

func samePreviewAction(left, right *persistedPreview) bool {
	if left == nil || right == nil || left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case "issue":
		return left.Issue != nil && right.Issue != nil && left.Issue.Key != "" && left.Issue.Key == right.Issue.Key
	case gfKind:
		return left.Fix != nil && right.Fix != nil && left.Fix.Key != "" && left.Fix.Key == right.Fix.Key
	default:
		return false
	}
}

func evictPersistedPreviews(state *previewState, now time.Time) bool {
	changed := false
	cutoff := now.Add(-previewTTL)
	for key, record := range state.Previews {
		if record.Status == previewStatusRunning {
			lease, err := time.Parse(time.RFC3339, record.LeaseExpires)
			if err == nil && now.Before(lease) {
				continue
			}
			if record.AttemptMode == previewAttemptReconcile {
				record.Status = previewStatusUnknown
			} else {
				record.Status = previewStatusUnknown
				record.CreatedAt = now.Format(time.RFC3339Nano)
			}
			record.LeaseExpires = ""
			record.AttemptID = ""
			record.AttemptMode = ""
			changed = true
			continue
		}
		if record.Status == previewStatusUnknown {
			created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
			if err != nil {
				delete(state.Previews, key)
				changed = true
			} else if created.Before(cutoff) {
				record.Status = previewStatusReady
				record.CreatedAt = now.Format(time.RFC3339Nano)
				changed = true
			}
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		if err != nil || created.Before(cutoff) {
			delete(state.Previews, key)
			changed = true
		}
	}
	return changed
}

func fitPreviewState(state *previewState, maxBytes int, protectedKey string) error {
	if maxBytes <= 0 {
		maxBytes = maxPreviewStateBytes
	}
	type item struct {
		key     string
		created time.Time
	}
	items := make([]item, 0, len(state.Previews))
	for key, record := range state.Previews {
		if key == protectedKey || record.Status == previewStatusRunning || record.Status == previewStatusDone || record.Status == previewStatusUnknown {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		if err != nil {
			created = time.Time{}
		}
		items = append(items, item{key: key, created: created})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].created.Equal(items[j].created) {
			return items[i].key < items[j].key
		}
		return items[i].created.Before(items[j].created)
	})
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding preview state: %w", err)
	}
	for (len(state.Previews) > maxPersistedPreviews || len(encoded) > maxBytes) && len(items) > 0 {
		delete(state.Previews, items[0].key)
		items = items[1:]
		encoded, err = json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding preview state: %w", err)
		}
	}
	if len(state.Previews) > maxPersistedPreviews {
		return fmt.Errorf("preview state exceeds %d records", maxPersistedPreviews)
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("preview state exceeds %d bytes", maxBytes)
	}
	return nil
}
