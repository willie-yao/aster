// Package corrections promotes reviewed analysis-chat revisions without mutating fetcher output.
package corrections

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
)

const (
	// FileName is the private correction proposal and audit ledger.
	FileName = "analysis_correction_state.json"
	// PublicFileName is the active/revoked correction overlay consumed by the frontend.
	PublicFileName = "analysis_corrections.json"

	stateVersion       = 1
	previewPending     = "pending"
	previewConfirmed   = "confirmed"
	previewExpired     = "expired"
	StatusActive       = "active"
	StatusRevoked      = "revoked"
	statusSuperseded   = "superseded"
	defaultPreviewTTL  = 15 * time.Minute
	maxPendingPreviews = 50
	maxCorrections     = 500
	maxPrivateBytes    = 16 << 20
	maxPublicBytes     = 4 << 20
)

var (
	ErrPreviewNotFound    = errors.New("analysis correction preview not found")
	ErrPreviewExpired     = errors.New("analysis correction preview expired")
	ErrCorrectionNotFound = errors.New("analysis correction not found")
	ErrCorrectionState    = errors.New("analysis correction is not active")
	ErrCorrectionLimit    = errors.New("analysis correction limit reached")
)

// Source supplies structured chat revisions and validates their source analysis.
type Source interface {
	CorrectionCandidate(sessionID, owner, requestID string) (analysischat.CorrectionCandidate, error)
	ValidateCorrectionCandidate(analysischat.CorrectionCandidate) error
}

// Preview is the owner-bound confirmation payload.
type Preview struct {
	Token     string                   `json:"token"`
	Analysis  analysischat.AnalysisRef `json:"analysis"`
	Original  analysischat.Revision    `json:"original"`
	Proposed  analysischat.Revision    `json:"proposed"`
	Citations []analysischat.Citation  `json:"citations"`
	ExpiresAt string                   `json:"expires_at"`
}

// PublicState is the correction overlay served to every dashboard viewer.
type PublicState struct {
	Corrections map[string]PublicCorrection `json:"corrections"`
}

// PublicCorrection is the current correction state for one analysis identity.
type PublicCorrection struct {
	ID          string                   `json:"id"`
	Status      string                   `json:"status"`
	Analysis    analysischat.AnalysisRef `json:"analysis"`
	Revision    analysischat.Revision    `json:"revision"`
	Citations   []analysischat.Citation  `json:"citations"`
	CorrectedBy string                   `json:"corrected_by"`
	CorrectedAt string                   `json:"corrected_at"`
	RevokedBy   string                   `json:"revoked_by,omitempty"`
	RevokedAt   string                   `json:"revoked_at,omitempty"`
}

type state struct {
	Version     int                    `json:"version"`
	Previews    map[string]*proposal   `json:"previews"`
	Corrections map[string]*correction `json:"corrections"`
	Current     map[string]string      `json:"current"`
}

type proposal struct {
	Token        string                           `json:"token"`
	Owner        string                           `json:"owner"`
	Candidate    analysischat.CorrectionCandidate `json:"candidate"`
	Status       string                           `json:"status"`
	CreatedAt    string                           `json:"created_at"`
	ExpiresAt    string                           `json:"expires_at"`
	CorrectionID string                           `json:"correction_id,omitempty"`
}

type correction struct {
	PublicCorrection
	Original   analysischat.Revision `json:"original"`
	SessionID  string                `json:"session_id"`
	RequestID  string                `json:"request_id"`
	ProposedBy string                `json:"proposed_by"`
	Audit      []auditEvent          `json:"audit"`
}

type auditEvent struct {
	Action string `json:"action"`
	Actor  string `json:"actor"`
	At     string `json:"at"`
}

// Options configures correction preview expiry and time.
type Options struct {
	PreviewTTL time.Duration
	Now        func() time.Time
}

func (o Options) normalized() Options {
	if o.PreviewTTL <= 0 {
		o.PreviewTTL = defaultPreviewTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Service owns private correction proposals and the public overlay projection.
type Service struct {
	dataDir string
	source  Source
	opts    Options
	local   chan struct{}
}

// NewService creates and reconciles a correction store.
func NewService(dataDir string, source Source, opts Options) (*Service, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("analysis correction data directory is required")
	}
	if source == nil {
		return nil, fmt.Errorf("analysis correction source is required")
	}
	s := &Service{dataDir: dataDir, source: source, opts: opts.normalized(), local: make(chan struct{}, 1)}
	if _, err := s.update(context.Background(), func(*state) (bool, error) { return false, nil }); err != nil {
		return nil, err
	}
	return s, nil
}

// Preview persists an evidence-backed agent revision for explicit confirmation.
func (s *Service) Preview(sessionID, requestID, owner string) (Preview, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return Preview{}, fmt.Errorf("authenticated owner is required")
	}
	candidate, err := s.source.CorrectionCandidate(sessionID, owner, requestID)
	if err != nil {
		return Preview{}, err
	}
	token, err := randomID()
	if err != nil {
		return Preview{}, fmt.Errorf("creating analysis correction preview: %w", err)
	}
	now := s.opts.Now().UTC()
	expires := now.Add(s.opts.PreviewTTL)
	var out Preview
	_, err = s.update(context.Background(), func(state *state) (bool, error) {
		for _, existing := range state.Previews {
			if existing.Owner == owner && existing.Candidate.SessionID == sessionID &&
				existing.Candidate.RequestID == requestID && existing.Status == previewPending {
				out = previewView(existing)
				return false, nil
			}
		}
		pending := 0
		for _, existing := range state.Previews {
			if existing.Status == previewPending {
				pending++
			}
		}
		if pending >= maxPendingPreviews {
			return false, ErrCorrectionLimit
		}
		created := &proposal{
			Token: token, Owner: owner, Candidate: cloneCandidate(candidate), Status: previewPending,
			CreatedAt: now.Format(time.RFC3339), ExpiresAt: expires.Format(time.RFC3339),
		}
		state.Previews[token] = created
		out = previewView(created)
		return true, nil
	})
	return out, err
}

// Confirm promotes one owner-bound preview into the public correction overlay.
func (s *Service) Confirm(token, owner string) (PublicCorrection, error) {
	token = strings.TrimSpace(token)
	owner = normalizeOwner(owner)
	if token == "" || owner == "" {
		return PublicCorrection{}, ErrPreviewNotFound
	}
	id, err := randomID()
	if err != nil {
		return PublicCorrection{}, fmt.Errorf("creating analysis correction: %w", err)
	}
	now := s.opts.Now().UTC()
	var out PublicCorrection
	_, err = s.update(context.Background(), func(state *state) (bool, error) {
		preview := state.Previews[token]
		if preview == nil || preview.Owner != owner {
			return false, ErrPreviewNotFound
		}
		if preview.Status == previewExpired {
			return false, ErrPreviewExpired
		}
		if preview.Status == previewConfirmed {
			current := state.Corrections[preview.CorrectionID]
			if current == nil {
				return false, ErrCorrectionNotFound
			}
			out = current.PublicCorrection
			return false, nil
		}
		if err := s.source.ValidateCorrectionCandidate(preview.Candidate); err != nil {
			return false, err
		}
		if len(state.Corrections) >= maxCorrections {
			return false, ErrCorrectionLimit
		}
		key := analysisKey(preview.Candidate.Analysis)
		if previousID := state.Current[key]; previousID != "" {
			if previous := state.Corrections[previousID]; previous != nil && previous.Status == StatusActive {
				previous.Status = statusSuperseded
				previous.Audit = append(previous.Audit, auditEvent{Action: statusSuperseded, Actor: owner, At: now.Format(time.RFC3339)})
			}
		}
		created := &correction{
			PublicCorrection: PublicCorrection{
				ID: id, Status: StatusActive, Analysis: preview.Candidate.Analysis,
				Revision: preview.Candidate.Proposed, Citations: cloneCitations(preview.Candidate.Citations),
				CorrectedBy: owner, CorrectedAt: now.Format(time.RFC3339),
			},
			Original: preview.Candidate.Original, SessionID: preview.Candidate.SessionID,
			RequestID: preview.Candidate.RequestID, ProposedBy: preview.Owner,
			Audit: []auditEvent{{Action: StatusActive, Actor: owner, At: now.Format(time.RFC3339)}},
		}
		state.Corrections[id] = created
		state.Current[key] = id
		preview.Status = previewConfirmed
		preview.CorrectionID = id
		out = created.PublicCorrection
		return true, nil
	})
	return out, err
}

// Revoke restores the original published analysis while retaining the audit record.
func (s *Service) Revoke(id, owner string) (PublicCorrection, error) {
	id = strings.TrimSpace(id)
	owner = normalizeOwner(owner)
	if id == "" || owner == "" {
		return PublicCorrection{}, ErrCorrectionNotFound
	}
	now := s.opts.Now().UTC()
	var out PublicCorrection
	_, err := s.update(context.Background(), func(state *state) (bool, error) {
		current := state.Corrections[id]
		if current == nil {
			return false, ErrCorrectionNotFound
		}
		if current.Status == StatusRevoked {
			out = current.PublicCorrection
			return false, nil
		}
		if current.Status != StatusActive {
			return false, ErrCorrectionState
		}
		current.Status = StatusRevoked
		current.RevokedBy = owner
		current.RevokedAt = now.Format(time.RFC3339)
		current.Audit = append(current.Audit, auditEvent{Action: StatusRevoked, Actor: owner, At: current.RevokedAt})
		out = current.PublicCorrection
		return true, nil
	})
	return out, err
}

func (s *Service) update(ctx context.Context, fn func(*state) (bool, error)) (bool, error) {
	select {
	case s.local <- struct{}{}:
		defer func() { <-s.local }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	lockPath := filepath.Join(s.dataDir, ".analysis-corrections.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return false, err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	state, changed, err := s.load()
	if err != nil {
		return false, err
	}
	if expirePreviews(state, s.opts.Now().UTC()) {
		changed = true
	}
	fnChanged, opErr := fn(state)
	changed = changed || fnChanged
	if changed {
		if err := writeJSONDurable(filepath.Join(s.dataDir, FileName), state, maxPrivateBytes, 0o600); err != nil {
			return false, err
		}
	}
	if err := writeJSONDurable(filepath.Join(s.dataDir, PublicFileName), publicState(state), maxPublicBytes, 0o644); err != nil {
		return false, err
	}
	return changed, opErr
}

func (s *Service) load() (*state, bool, error) {
	path := filepath.Join(s.dataDir, FileName)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return freshState(), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPrivateBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxPrivateBytes {
		return nil, false, fmt.Errorf("analysis correction state exceeds %d bytes", maxPrivateBytes)
	}
	var state state
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, false, err
	}
	if state.Version != stateVersion {
		return nil, false, fmt.Errorf("unsupported analysis correction state version %d", state.Version)
	}
	initializeState(&state)
	return &state, false, nil
}

func freshState() *state {
	state := &state{Version: stateVersion}
	initializeState(state)
	return state
}

func initializeState(state *state) {
	if state.Previews == nil {
		state.Previews = map[string]*proposal{}
	}
	if state.Corrections == nil {
		state.Corrections = map[string]*correction{}
	}
	if state.Current == nil {
		state.Current = map[string]string{}
	}
}

func expirePreviews(state *state, now time.Time) bool {
	changed := false
	for _, preview := range state.Previews {
		if preview.Status != previewPending {
			continue
		}
		expires, err := time.Parse(time.RFC3339, preview.ExpiresAt)
		if err != nil || !now.Before(expires) {
			preview.Status = previewExpired
			changed = true
		}
	}
	return changed
}

func publicState(state *state) PublicState {
	out := PublicState{Corrections: map[string]PublicCorrection{}}
	for key, id := range state.Current {
		if correction := state.Corrections[id]; correction != nil {
			public := correction.PublicCorrection
			public.Citations = cloneCitations(public.Citations)
			out.Corrections[key] = public
		}
	}
	return out
}

func previewView(preview *proposal) Preview {
	return Preview{
		Token: preview.Token, Analysis: preview.Candidate.Analysis,
		Original: preview.Candidate.Original, Proposed: preview.Candidate.Proposed,
		Citations: cloneCitations(preview.Candidate.Citations), ExpiresAt: preview.ExpiresAt,
	}
}

func analysisKey(ref analysischat.AnalysisRef) string {
	value := struct {
		JobID, BuildID, TestName, SuiteName, ClassName, JUnitFile string
	}{ref.JobID, ref.BuildID, ref.TestName, ref.SuiteName, ref.ClassName, ref.JUnitFile}
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneCandidate(candidate analysischat.CorrectionCandidate) analysischat.CorrectionCandidate {
	candidate.Citations = cloneCitations(candidate.Citations)
	return candidate
}

func cloneCitations(citations []analysischat.Citation) []analysischat.Citation {
	out := make([]analysischat.Citation, len(citations))
	copy(out, citations)
	return out
}

func normalizeOwner(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func writeJSONDurable(path string, value any, maxBytes int, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("analysis correction state exceeds %d bytes", maxBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(mode)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

var _ Source = (*analysischat.Service)(nil)
