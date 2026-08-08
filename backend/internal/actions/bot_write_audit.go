package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"golang.org/x/sys/unix"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	botWriteAuditDirName   = ".action-write-audit"
	botWriteAuditStateName = "state.json"
	botWriteAuditVersion   = 1
	maxBotWriteAuditBytes  = 16 << 20
	botWriteConfirmed      = "confirmed"
	botWriteReconciled     = "reconciled"
)

type botWriteAuditRecord struct {
	ID          string `json:"id"`
	InitiatedBy string `json:"initiated_by"`
	ConfirmedBy string `json:"confirmed_by"`
	Kind        string `json:"kind"`
	FailureID   string `json:"failure_id,omitempty"`
	TargetRepo  string `json:"target_repo"`
	ResultURL   string `json:"result_url"`
	Outcome     string `json:"outcome"`
	InitiatedAt string `json:"initiated_at"`
	ConfirmedAt string `json:"confirmed_at"`
}

type botWriteAuditState struct {
	Version int                            `json:"version"`
	Records map[string]botWriteAuditRecord `json:"records"`
}

type botWriteAuditStore struct {
	path     string
	lockPath string
	maxBytes int
}

func newBotWriteAuditStore(dataDir string) *botWriteAuditStore {
	dir := filepath.Join(dataDir, botWriteAuditDirName)
	return &botWriteAuditStore{
		path:     filepath.Join(dir, botWriteAuditStateName),
		lockPath: filepath.Join(dir, ".lock"),
	}
}

func (s *Service) recordBotWrite(token, confirmedBy string, entry *previewEntry, resultURL, outcome string) error {
	if entry == nil {
		return ErrPreviewNotFound
	}
	write := s.writeAudit
	if write == nil {
		write = newBotWriteAuditStore(s.dataDir).record
	}
	return write(botWriteAuditRecord{
		ID: tokenHash(token), InitiatedBy: entry.initiatedBy, ConfirmedBy: confirmedBy,
		Kind: entry.kind, FailureID: entry.failureID, TargetRepo: entry.targetRepo,
		ResultURL: resultURL, Outcome: outcome, InitiatedAt: entry.initiatedAt,
		ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *botWriteAuditStore) record(record botWriteAuditRecord) error {
	record.InitiatedBy = normalizeActionOwner(record.InitiatedBy)
	record.ConfirmedBy = normalizeActionOwner(record.ConfirmedBy)
	if record.ID == "" || record.InitiatedBy == "" || record.ConfirmedBy == "" || record.Kind == "" ||
		record.TargetRepo == "" || record.ResultURL == "" || record.InitiatedAt == "" || record.ConfirmedAt == "" {
		return fmt.Errorf("bot write audit record is incomplete")
	}
	if record.Outcome != botWriteConfirmed && record.Outcome != botWriteReconciled {
		return fmt.Errorf("unsupported bot write audit outcome %q", record.Outcome)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.InitiatedAt); err != nil {
		return fmt.Errorf("invalid bot write initiation time: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.ConfirmedAt); err != nil {
		return fmt.Errorf("invalid bot write confirmation time: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating bot write audit directory: %w", err)
	}
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening bot write audit lock: %w", err)
	}
	defer lock.Close()
	_ = os.Chmod(s.lockPath, 0o600)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking bot write audit: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	state, err := s.load()
	if err != nil {
		return err
	}
	if existing, ok := state.Records[record.ID]; ok {
		if sameBotWrite(existing, record) {
			return nil
		}
		return fmt.Errorf("bot write audit record %s conflicts with persisted state", record.ID)
	}
	state.Records[record.ID] = record
	if err := statefile.WritePrivateJSONDurable(s.path, state); err != nil {
		return fmt.Errorf("writing bot write audit: %w", err)
	}
	return nil
}

func (s *botWriteAuditStore) load() (*botWriteAuditState, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return freshBotWriteAuditState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening bot write audit: %w", err)
	}
	defer file.Close()
	maxBytes := s.maxBytes
	if maxBytes <= 0 {
		maxBytes = maxBotWriteAuditBytes
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading bot write audit: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("bot write audit exceeds %d bytes", maxBytes)
	}
	state := freshBotWriteAuditState()
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("decoding bot write audit: %w", err)
	}
	if state.Version != botWriteAuditVersion || state.Records == nil {
		return nil, fmt.Errorf("unsupported bot write audit version %d", state.Version)
	}
	return state, nil
}

func freshBotWriteAuditState() *botWriteAuditState {
	return &botWriteAuditState{Version: botWriteAuditVersion, Records: map[string]botWriteAuditRecord{}}
}

func sameBotWrite(left, right botWriteAuditRecord) bool {
	// ConfirmedAt and Outcome may differ on a retry that reconciles an already
	// recorded write. The stable attribution and external result must not.
	left.ConfirmedAt, right.ConfirmedAt = "", ""
	left.Outcome, right.Outcome = "", ""
	return reflect.DeepEqual(left, right)
}
