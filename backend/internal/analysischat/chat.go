// Package analysischat manages bounded conversations about published failure analyses.
package analysischat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

var (
	// ErrAnalysisNotFound means the published data has no matching analysis.
	ErrAnalysisNotFound = errors.New("analysis not found")
	// ErrAnalysisChanged means the selected analysis was replaced after the client loaded it.
	ErrAnalysisChanged = errors.New("analysis changed")
	// ErrPatternNotFound means the selected recurring pattern is absent.
	ErrPatternNotFound = errors.New("recurring pattern not found")
	// ErrPatternChanged means the selected recurring pattern was replaced.
	ErrPatternChanged = errors.New("recurring pattern changed")
	// ErrSessionNotFound means the session is absent, expired, or owned by another user.
	ErrSessionNotFound = errors.New("analysis chat session not found")
	// ErrSessionBusy means another turn is already running for the session.
	ErrSessionBusy = errors.New("analysis chat session is busy")
	// ErrRequestPending means this idempotent request is still running.
	ErrRequestPending = errors.New("analysis chat request is pending")
	// ErrRequestNotFound means the session has no request with this ID.
	ErrRequestNotFound = errors.New("analysis chat request not found")
	// ErrSessionLimit means the deployment or owner has too many live sessions.
	ErrSessionLimit = errors.New("analysis chat session limit reached")
	// ErrActiveTurnLimit means an owner has too many concurrent turns.
	ErrActiveTurnLimit = errors.New("analysis chat active turn limit reached")
	// ErrRateLimit means an owner exceeded the admitted turn rate.
	ErrRateLimit           = errors.New("analysis chat rate limit reached") // ErrIdempotencyConflict means a request key was reused for different input.
	ErrIdempotencyConflict = errors.New("analysis chat idempotency key conflict")
	// ErrRequestOutcomeUnknown means a replica died before recording a turn result.
	ErrRequestOutcomeUnknown = errors.New("analysis chat request outcome unknown")
	// ErrRequestFailed means an earlier idempotent attempt failed before answering.
	ErrRequestFailed = errors.New("analysis chat request failed")
	// ErrProviderRequestFailed means the model provider request failed safely.
	ErrProviderRequestFailed = errors.New("analysis chat provider request failed")
	// ErrResponseValidationFailed means the model response did not match the contract.
	ErrResponseValidationFailed = errors.New("analysis chat model response could not be validated")
	// ErrTurnLimit means the session has used its allowed turns.
	ErrTurnLimit = errors.New("analysis chat turn limit reached")
	// ErrInvalidRequest means a request field is missing, ambiguous, or too large.
	ErrInvalidRequest = errors.New("invalid analysis chat request")
)

// Unverified reasons name the evidence gate an answer failed. They are a closed
// engine-owned set and never carry model or provider text.
const (
	// UnverifiedCitation means a quote, line range, or citation count failed verification.
	UnverifiedCitation = "citation"
	// UnverifiedReference means a cited path was unsafe or was never read in this conversation.
	UnverifiedReference = "reference"
	// UnverifiedMissing means an evidence-claiming answer carried no citations.
	UnverifiedMissing = "missing"
	// UnverifiedFormat means the answer did not follow the response contract, so
	// no evidence could be verified. The answer text is still shown.
	UnverifiedFormat = "format"
)

// ValidationGate names the response gate a hard validation failure tripped. It
// is a closed engine-owned set safe to return to owners and operators.
const (
	GateCandidate = "candidate_selection"
	GateJSON      = "json_validation"
	GateContract  = "response_contract"
)

// ValidationError reports which response gate rejected a turn. It always
// unwraps to ErrResponseValidationFailed.
type ValidationError struct {
	Gate string
}

func (e *ValidationError) Error() string { return GateMessage(e.Gate) }

// GateMessage returns the owner-safe message for a response validation gate.
func GateMessage(gate string) string {
	switch gate {
	case GateCandidate:
		return "analysis chat model response did not contain a usable answer"
	case GateJSON:
		return "analysis chat model response was not valid JSON"
	default:
		return ErrResponseValidationFailed.Error()
	}
}

func (e *ValidationError) Unwrap() error { return ErrResponseValidationFailed }

// ValidationGateOf returns the gate a validation failure tripped, if any.
func ValidationGateOf(err error) (string, bool) {
	var validationErr *ValidationError
	if errors.As(err, &validationErr) && validationErr.Gate != "" {
		return validationErr.Gate, true
	}
	return "", false
}

const (
	ScopeTest    = "test"
	ScopePattern = "pattern"

	maxJobDetailBytes                  = 64 << 20
	maxJobIDBytes                      = 1024
	maxBuildIDBytes                    = 256
	maxTestNameBytes                   = 4096
	maxSuiteNameBytes                  = 4096
	maxClassNameBytes                  = 4096
	maxJUnitFileBytes                  = 1024
	maxTimestampBytes                  = 128
	maxRequestIDBytes                  = 128
	maxPatternIDBytes                  = 512
	maxPatternHashBytes                = 128
	maxPatternEvidenceBuilds           = 3
	maxPatternChatCausalGroups         = 10
	maxPatternChatBuildsPerGroup       = 10
	maxPatternChatUnclassifiedBuilds   = 10
	maxPatternChatRemediationSummaries = 10
)

// AnalysisRef addresses one published test or recurring-pattern analysis.
type AnalysisRef struct {
	Scope               string `json:"scope,omitempty"`
	JobID               string `json:"job_id"`
	BuildID             string `json:"build_id"`
	TestName            string `json:"test_name"`
	Source              string `json:"source,omitempty"`
	SuiteName           string `json:"suite_name,omitempty"`
	ClassName           string `json:"class_name,omitempty"`
	JUnitFile           string `json:"junit_file,omitempty"`
	AnalysisGeneratedAt string `json:"analysis_generated_at,omitempty"`
	PatternID           string `json:"pattern_id,omitempty"`
	PatternHash         string `json:"pattern_hash,omitempty"`
}

// Citation identifies artifact evidence used in one answer.
type Citation struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Quote     string `json:"quote,omitempty"`
}

// Revision is a proposed replacement for the published conclusion.
type Revision struct {
	RootCause    string `json:"root_cause"`
	SuggestedFix string `json:"suggested_fix"`
}

// Reply is the structured answer returned by a conversation runner.
type Reply struct {
	Answer            string     `json:"answer"`
	Assessment        string     `json:"assessment"`
	Citations         []Citation `json:"citations,omitempty"`
	ProposedRevision  *Revision  `json:"proposed_revision,omitempty"`
	Unverified        bool       `json:"unverified,omitempty"`
	UnverifiedReason  string     `json:"unverified_reason,omitempty"`
	ToolCalls         int        `json:"tool_calls,omitempty"`
	GCSBytes          int        `json:"gcs_bytes,omitempty"`
	ElapsedMs         int        `json:"elapsed_ms,omitempty"`
	ProviderMs        int        `json:"provider_ms,omitempty"`
	ValidationRetries int        `json:"validation_retries,omitempty"`
}

// Message is one user or assistant entry in a session transcript.
type Message struct {
	Role              string     `json:"role"`
	RequestID         string     `json:"request_id,omitempty"`
	Content           string     `json:"content"`
	Assessment        string     `json:"assessment,omitempty"`
	Citations         []Citation `json:"citations,omitempty"`
	ProposedRevision  *Revision  `json:"proposed_revision,omitempty"`
	Unverified        bool       `json:"unverified,omitempty"`
	UnverifiedReason  string     `json:"unverified_reason,omitempty"`
	ToolCalls         int        `json:"tool_calls,omitempty"`
	GCSBytes          int        `json:"gcs_bytes,omitempty"`
	ElapsedMs         int        `json:"elapsed_ms,omitempty"`
	ProviderMs        int        `json:"provider_ms,omitempty"`
	ValidationRetries int        `json:"validation_retries,omitempty"`
	CreatedAt         string     `json:"created_at"`
}

// Attempt is one owner-safe admitted model request.
type Attempt struct {
	RequestID   string `json:"request_id"`
	Question    string `json:"question,omitempty"`
	Outcome     string `json:"outcome"`
	FailureKind string `json:"failure_kind,omitempty"`
	Turn        int    `json:"turn,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// SessionView is the owner-safe session representation returned by the API.
type SessionView struct {
	ID               string                          `json:"id"`
	Analysis         AnalysisRef                     `json:"analysis"`
	CreatedAt        string                          `json:"created_at"`
	UpdatedAt        string                          `json:"updated_at"`
	ExpiresAt        string                          `json:"expires_at"`
	Messages         []Message                       `json:"messages"`
	Attempts         []Attempt                       `json:"attempts"`
	Active           *ActiveTurn                     `json:"active,omitempty"`
	TurnsUsed        int                             `json:"turns_used"`
	MaxTurns         int                             `json:"max_turns"`
	SourceRepository *sourceinvestigation.Repository `json:"source_repository,omitempty"`
}

// ActiveTurn is the owner-safe state needed to resume an in-flight request.
type ActiveTurn struct {
	RequestID            string `json:"request_id"`
	Question             string `json:"question,omitempty"`
	Phase                string `json:"phase"`
	StartedAt            string `json:"started_at,omitempty"`
	UpdatedAt            string `json:"updated_at"`
	ValidationRetries    int    `json:"validation_retries,omitempty"`
	MaxValidationRetries int    `json:"max_validation_retries,omitempty"`
}

// Progress is a persisted, owner-safe turn phase.
type Progress struct {
	RequestID            string `json:"request_id"`
	Phase                string `json:"phase"`
	StartedAt            string `json:"started_at,omitempty"`
	UpdatedAt            string `json:"updated_at"`
	TurnsUsed            int    `json:"turns_used"`
	MaxTurns             int    `json:"max_turns"`
	ValidationRetries    int    `json:"validation_retries,omitempty"`
	MaxValidationRetries int    `json:"max_validation_retries,omitempty"`
}

const (
	PhaseQueued             = "queued"
	PhaseInvestigating      = "investigating"
	PhaseReadingEvidence    = "reading_evidence"
	PhaseEvaluating         = "evaluating"
	PhaseFinalizing         = "finalizing"
	PhaseValidationRetrying = "validation_retrying"
	PhaseCancelling         = "cancelling"
)

// Turn is the immutable analysis snapshot and transcript for one model call.
type Turn struct {
	SessionID      string
	JobID          string
	BuildPrefix    string
	Build          models.BuildInfo
	TestCase       models.TestCase
	Pattern        *models.PatternAnalysis
	EvidenceBuilds []ArtifactBuild
	History        []Message
	Question       string
	Progress       func(string)
}

// ArtifactBuild identifies one build root available to a pattern conversation.
type ArtifactBuild struct {
	BuildPrefix string           `json:"build_prefix"`
	Build       models.BuildInfo `json:"build"`
}

// ReportProgress records a non-sensitive phase when a turn observer is set.
func (t Turn) ReportProgress(phase string) {
	if t.Progress != nil {
		t.Progress(phase)
	}
}

// Runner answers one turn using the selected analysis and build artifacts.
type Runner interface {
	Reply(context.Context, Turn) (Reply, error)
}

// Options bounds persisted session use.
type Options struct {
	StateDir            string
	SessionTTL          time.Duration
	MaxSessions         int
	MaxSessionsPerOwner int
	// MaxTurns bounds admitted model attempts, including failed turns.
	MaxTurns         int
	MaxQuestionBytes int
	// TurnLeaseTTL bounds an in-flight turn owned by one replica.
	TurnLeaseTTL                 time.Duration
	StoreLockTimeout             time.Duration
	CleanupInterval              time.Duration
	TurnTimeout                  time.Duration
	PollInterval                 time.Duration
	MaxActiveTurnsPerOwner       int
	MaxRequestsPerOwnerPerMinute int
	UsageRecorder                *aiusage.Recorder
	Now                          func() time.Time
}

func (o Options) normalized(dataDir string) Options {
	if strings.TrimSpace(o.StateDir) == "" {
		o.StateDir = filepath.Join(dataDir, ".analysis-chat")
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = 2 * time.Hour
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = 128
	}
	if o.MaxSessionsPerOwner <= 0 {
		o.MaxSessionsPerOwner = 8
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = 10
	}
	if o.MaxQuestionBytes <= 0 {
		o.MaxQuestionBytes = 4096
	}
	if o.TurnLeaseTTL <= 0 {
		o.TurnLeaseTTL = 3 * time.Minute
	}
	if o.StoreLockTimeout <= 0 {
		o.StoreLockTimeout = 5 * time.Second
	}
	if o.CleanupInterval <= 0 {
		o.CleanupInterval = time.Minute
		if quarter := o.SessionTTL / 4; quarter < o.CleanupInterval {
			o.CleanupInterval = quarter
		}
		if o.CleanupInterval < time.Second {
			o.CleanupInterval = time.Second
		}
	}
	if o.TurnTimeout <= 0 {
		o.TurnTimeout = 2 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.MaxActiveTurnsPerOwner <= 0 {
		o.MaxActiveTurnsPerOwner = 2
	}
	if o.MaxRequestsPerOwnerPerMinute <= 0 {
		o.MaxRequestsPerOwnerPerMinute = 10
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type resolvedAnalysis struct {
	ref            AnalysisRef
	jobID          string
	buildPrefix    string
	build          models.BuildInfo
	testCase       models.TestCase
	patterns       []models.PatternAnalysis
	pattern        *models.PatternAnalysis
	patternFresh   bool
	evidenceBuilds []ArtifactBuild
}

// Service resolves published analyses and owns durable chat sessions.
type Service struct {
	dataDir          string
	runner           Runner
	testFixPreflight func(context.Context, sourceinvestigation.Repository, string, []string) (string, map[string]string, error)
	sourceRepo       sourceinvestigation.Repository
	opts             Options
	store            *sessionStore
	lifecycle        context.Context
	activeMu         sync.Mutex
	active           map[string]context.CancelFunc
	activeWG         sync.WaitGroup
	notifyMu         sync.Mutex
	notify           map[string]map[chan struct{}]struct{}
}

// NewService creates a durable analysis chat service.
func NewService(ctx context.Context, dataDir string, runner Runner, opts Options) (*Service, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("analysis chat data directory is required")
	}
	if runner == nil {
		return nil, fmt.Errorf("analysis chat runner is required")
	}
	opts = opts.normalized(dataDir)
	store, err := newSessionStore(opts.StateDir, opts.StoreLockTimeout)
	if err != nil {
		return nil, err
	}
	if err := validateStateDirPrivacy(dataDir, opts.StateDir); err != nil {
		return nil, err
	}
	if err := store.validate(); err != nil {
		return nil, fmt.Errorf("validating analysis chat state: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	service := &Service{
		dataDir: dataDir, runner: runner, opts: opts, store: store,
		lifecycle: ctx, active: map[string]context.CancelFunc{},
		notify: map[string]map[chan struct{}]struct{}{},
	}
	if err := service.cleanupPersisted(); err != nil {
		return nil, fmt.Errorf("cleaning analysis chat state: %w", err)
	}
	go service.cleanupLoop(ctx)
	return service, nil
}

func (s *Service) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(s.opts.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.cleanupPersisted(); err != nil {
				log.Printf("analysis chat cleanup: %v", err)
			}
		}
	}
}

func (s *Service) cleanupPersisted() error {
	ctx, cancel := s.store.context()
	defer cancel()
	now := s.opts.Now().UTC()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		return s.cleanup(state, now), nil
	})
}

// Create resolves an analysis snapshot and starts an owner-bound session.
func (s *Service) Create(ref AnalysisRef, owner, requestID string) (SessionView, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return SessionView{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return SessionView{}, err
	}
	ref, err = normalizeAnalysisRef(ref)
	if err != nil {
		return SessionView{}, err
	}
	requestHash, err := hashAnalysisRef(ref)
	if err != nil {
		return SessionView{}, err
	}
	legacyHash, err := hashLegacyTestAnalysisRef(ref)
	if err != nil {
		return SessionView{}, err
	}
	now := s.opts.Now().UTC()

	var existing SessionView
	ctx, cancel := s.store.context()
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current, migrated, err := findCreateRequest(state, owner, requestID, requestHash, legacyHash)
		if err != nil {
			return changed, err
		}
		changed = changed || migrated
		if current != nil {
			existing = s.sessionView(current)
			return changed, nil
		}
		if s.sessionLimitReached(state, owner) {
			return changed, ErrSessionLimit
		}
		return changed, nil
	})
	cancel()
	if err != nil || existing.ID != "" {
		return existing, err
	}

	resolved, err := s.resolve(ref)
	if err != nil {
		return SessionView{}, err
	}
	id, err := newSessionID()
	if err != nil {
		return SessionView{}, fmt.Errorf("creating analysis chat session: %w", err)
	}
	expires := now.Add(s.opts.SessionTTL)
	created := &persistedSession{
		Owner:                owner,
		Resolved:             persistResolved(resolved, s.sourceRepo),
		ExpiresAt:            expires,
		CreateRequestID:      requestID,
		CreateRequestHash:    requestHash,
		CreateRequestVersion: createVersion,
		Requests:             map[string]persistedRequest{},
		View: SessionView{
			ID:        id,
			Analysis:  resolved.ref,
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: expires.Format(time.RFC3339),
			Messages:  []Message{},
		},
	}

	ctx, cancel = s.store.context()
	defer cancel()
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current, migrated, err := findCreateRequest(state, owner, requestID, requestHash, legacyHash)
		if err != nil {
			return changed, err
		}
		changed = changed || migrated
		if current != nil {
			existing = s.sessionView(current)
			return changed, nil
		}
		if s.sessionLimitReached(state, owner) {
			return changed, ErrSessionLimit
		}
		state.Sessions[id] = created
		existing = s.sessionView(created)
		return true, nil
	})
	return existing, err
}

// Get returns an owner-bound session.
func (s *Service) Get(id, owner string) (SessionView, error) {
	owner = normalizeOwner(owner)
	now := s.opts.Now().UTC()
	var view SessionView
	ctx, cancel := s.store.context()
	defer cancel()
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		view = s.sessionView(current)
		return changed, nil
	})
	return view, err
}

// Find returns the latest owner-bound session for the current analysis.
func (s *Service) Find(ref AnalysisRef, owner string) (SessionView, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return SessionView{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	resolved, err := s.resolve(ref)
	if err != nil {
		return SessionView{}, err
	}
	now := s.opts.Now().UTC()
	var view SessionView
	ctx, cancel := s.store.context()
	defer cancel()
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := s.latestSessionForAnalysis(state, owner, resolved.ref)
		if current == nil {
			return changed, ErrSessionNotFound
		}
		view = s.sessionView(current)
		return changed, nil
	})
	return view, err
}

func (s *Service) latestSessionForAnalysis(state *persistedState, owner string, ref AnalysisRef) *persistedSession {
	var latest *persistedSession
	var latestActivity time.Time
	for _, current := range state.Sessions {
		if current == nil || current.Owner != owner || current.View.Analysis != ref {
			continue
		}
		activity, err := time.Parse(time.RFC3339, current.View.UpdatedAt)
		if err != nil {
			activity, _ = time.Parse(time.RFC3339, current.View.CreatedAt)
		}
		if current.Active != nil && current.Active.UpdatedAt.After(activity) {
			activity = current.Active.UpdatedAt
		}
		if latest == nil || activity.After(latestActivity) ||
			activity.Equal(latestActivity) && current.ExpiresAt.After(latest.ExpiresAt) ||
			activity.Equal(latestActivity) && current.ExpiresAt.Equal(latest.ExpiresAt) && current.View.ID > latest.View.ID {
			latest = current
			latestActivity = activity
		}
	}
	return latest
}

func (s *Service) cleanup(state *persistedState, now time.Time) bool {
	changed := false
	if state.OwnerRequests == nil {
		state.OwnerRequests = map[string][]time.Time{}
		changed = true
	}
	for owner, requests := range state.OwnerRequests {
		pruned := pruneOwnerRequestTimes(requests, now)
		if len(pruned) == 0 {
			delete(state.OwnerRequests, owner)
			changed = true
		} else if len(pruned) != len(requests) {
			state.OwnerRequests[owner] = pruned
			changed = true
		}
	}
	for id, current := range state.Sessions {
		if current.Requests == nil {
			current.Requests = map[string]persistedRequest{}
			changed = true
		}
		retainedOutcome := false
		if current.Active != nil && !now.Before(current.Active.ExpiresAt) {
			active := current.Active
			previous := current.Requests[active.RequestID]
			if active.CancelRequested {
				previous.Status = requestFailed
				previous.FailureKind = failureCancelled
			} else {
				previous.Status = requestUnknown
				previous.FailureKind = ""
			}
			if previous.Question == "" {
				previous.Question = active.Question
			}
			if previous.Turn == 0 {
				previous.Turn = current.Turns
			}
			if previous.CreatedAt == "" && !active.UpdatedAt.IsZero() {
				previous.CreatedAt = active.UpdatedAt.UTC().Format(time.RFC3339)
			}
			stamp := now.Format(time.RFC3339)
			previous.UpdatedAt = stamp
			current.Requests[active.RequestID] = previous
			current.Active = nil
			current.View.UpdatedAt = stamp
			retainedOutcome = true
			changed = true
		}
		if retainedOutcome {
			retainedUntil := now.Add(s.opts.SessionTTL)
			if current.ExpiresAt.Before(retainedUntil) {
				extendSessionExpiry(current, retainedUntil)
			}
		}
		if !now.Before(current.ExpiresAt) && current.Active == nil {
			delete(state.Sessions, id)
			changed = true
		}
	}
	return changed
}

func (s *Service) sessionLimitReached(state *persistedState, owner string) bool {
	if len(state.Sessions) >= s.opts.MaxSessions {
		return true
	}
	count := 0
	for _, current := range state.Sessions {
		if current.Owner == owner {
			count++
		}
	}
	return count >= s.opts.MaxSessionsPerOwner
}

func findCreateRequest(state *persistedState, owner, requestID, requestHash, legacyHash string) (*persistedSession, bool, error) {
	for _, current := range state.Sessions {
		if current.Owner != owner || current.CreateRequestID != requestID {
			continue
		}
		if current.CreateRequestHash != requestHash {
			if current.CreateRequestVersion == 1 && current.CreateRequestHash == legacyHash {
				current.CreateRequestHash = requestHash
				current.CreateRequestVersion = createVersion
				return current, true, nil
			}
			return nil, false, ErrIdempotencyConflict
		}
		return current, false, nil
	}
	return nil, false, nil
}

func normalizeRequestID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRequestIDBytes {
		return "", fmt.Errorf("%w: idempotency key must be 1-%d bytes", ErrInvalidRequest, maxRequestIDBytes)
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r) {
			continue
		}
		return "", fmt.Errorf("%w: idempotency key contains unsupported characters", ErrInvalidRequest)
	}
	return value, nil
}

func hashAnalysisRef(ref AnalysisRef) (string, error) {
	data, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("encoding analysis chat idempotency input: %w", err)
	}
	return hashBytes(data), nil
}

func hashLegacyTestAnalysisRef(ref AnalysisRef) (string, error) {
	if ref.Scope != ScopeTest {
		return "", nil
	}
	ref.Scope = ""
	return hashAnalysisRef(ref)
}

func hashText(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func requestFailureKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return failureTimeout
	case errors.Is(err, context.Canceled):
		return failureCancelled
	case errors.Is(err, ErrProviderRequestFailed):
		return failureProvider
	case errors.Is(err, ErrResponseValidationFailed):
		return failureValidation
	case errors.Is(err, sourceinvestigation.ErrInvalidResult), errors.Is(err, sourceinvestigation.ErrUnavailable):
		return failureSource
	default:
		return failureModel
	}
}

func persistedRequestError(kind, gate string) error {
	switch kind {
	case failureTimeout:
		return context.DeadlineExceeded
	case failureCancelled:
		return context.Canceled
	case failureSource:
		return sourceinvestigation.ErrUnavailable
	case failureProvider:
		return ErrProviderRequestFailed
	case failureValidation:
		if gate != "" {
			return &ValidationError{Gate: gate}
		}
		return ErrResponseValidationFailed
	default:
		return ErrRequestFailed
	}
}

func (s *Service) resolve(ref AnalysisRef) (resolvedAnalysis, error) {
	var err error
	ref, err = normalizeAnalysisRef(ref)
	if err != nil {
		return resolvedAnalysis{}, err
	}

	file, err := os.Open(filepath.Join(s.dataDir, "jobs", models.JobDataFilename(ref.JobID)))
	if err != nil {
		if os.IsNotExist(err) {
			return resolvedAnalysis{}, ErrAnalysisNotFound
		}
		return resolvedAnalysis{}, fmt.Errorf("reading analysis job data: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJobDetailBytes+1))
	if err != nil {
		return resolvedAnalysis{}, fmt.Errorf("reading analysis job data: %w", err)
	}
	if len(data) > maxJobDetailBytes {
		return resolvedAnalysis{}, fmt.Errorf("analysis job data exceeds %d bytes", maxJobDetailBytes)
	}
	var detail models.JobDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return resolvedAnalysis{}, fmt.Errorf("decoding analysis job data: %w", err)
	}
	if detail.JobID != "" && detail.JobID != ref.JobID {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	if ref.Scope == ScopePattern {
		return resolvePatternAnalysis(ref, detail)
	}

	var run *models.BuildResult
	for i := range detail.Runs {
		if detail.Runs[i].BuildID == ref.BuildID {
			run = &detail.Runs[i]
			break
		}
	}
	if run == nil {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}

	var matches []models.TestCase
	for _, testCase := range run.TestCases {
		testName := strings.TrimSpace(testCase.Name)
		source := strings.TrimSpace(testCase.Source)
		suiteName := strings.TrimSpace(testCase.SuiteName)
		className := strings.TrimSpace(testCase.ClassName)
		if testName != ref.TestName ||
			ref.Source == models.TestCaseSourceBuild && source != models.TestCaseSourceBuild ||
			ref.Source == "" && source == models.TestCaseSourceBuild ||
			ref.SuiteName != "" && suiteName != ref.SuiteName ||
			ref.ClassName != "" && className != ref.ClassName ||
			ref.JUnitFile != "" && testCase.JUnitFile != ref.JUnitFile {
			continue
		}
		if testCase.AIAnalysis != nil {
			matches = append(matches, testCase)
		}
	}
	if len(matches) == 0 {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	if len(matches) > 1 {
		return resolvedAnalysis{}, fmt.Errorf("%w: suite_name, class_name, or junit_file is required to disambiguate the test", ErrInvalidRequest)
	}
	testCase := cloneTestCase(matches[0])
	if ref.AnalysisGeneratedAt != "" && ref.AnalysisGeneratedAt != testCase.AIAnalysis.GeneratedAt {
		return resolvedAnalysis{}, ErrAnalysisChanged
	}
	ref.TestName = strings.TrimSpace(testCase.Name)
	ref.Source = strings.TrimSpace(testCase.Source)
	ref.SuiteName = strings.TrimSpace(testCase.SuiteName)
	ref.ClassName = strings.TrimSpace(testCase.ClassName)
	ref.JUnitFile = testCase.JUnitFile
	ref.AnalysisGeneratedAt = testCase.AIAnalysis.GeneratedAt

	artifactBuild, err := artifactBuildFor(detail, *run)
	if err != nil {
		return resolvedAnalysis{}, err
	}
	return resolvedAnalysis{
		ref:         ref,
		jobID:       ref.JobID,
		buildPrefix: artifactBuild.BuildPrefix,
		build:       cloneBuildInfo(run.BuildInfo),
		testCase:    testCase,
		patterns:    clonePatternAnalyses(detail.PatternAnalyses),
	}, nil
}

func resolvePatternAnalysis(ref AnalysisRef, detail models.JobDetail) (resolvedAnalysis, error) {
	var selected *models.PatternAnalysis
	for i := range detail.PatternAnalyses {
		pattern := &detail.PatternAnalyses[i]
		if pattern.ID == ref.PatternID {
			selected = pattern
			break
		}
	}
	if selected == nil || !selected.Systemic {
		return resolvedAnalysis{}, ErrPatternNotFound
	}
	if models.PatternHash(*selected) != ref.PatternHash {
		return resolvedAnalysis{}, ErrPatternChanged
	}
	if err := validatePatternChatBounds(*selected); err != nil {
		return resolvedAnalysis{}, err
	}
	selectedRuns, availableCount, eligibleCount := selectPatternEvidenceRuns(*selected, detail.Runs)
	if detail.PatternRefresh != nil && detail.PatternRefresh.State != models.PatternRefreshCurrent && availableCount != eligibleCount {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	builds := make([]ArtifactBuild, 0, len(selectedRuns))
	for _, run := range selectedRuns {
		build, err := artifactBuildFor(detail, run)
		if err != nil {
			return resolvedAnalysis{}, err
		}
		builds = append(builds, build)
	}
	if len(builds) == 0 {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	pattern := clonePatternAnalyses([]models.PatternAnalysis{*selected})[0]
	ref.PatternHash = models.PatternHash(pattern)
	severity := "Unknown"
	switch strings.ToLower(strings.TrimSpace(pattern.Confidence)) {
	case "high":
		severity = "High"
	case "medium":
		severity = "Medium"
	case "low":
		severity = "Low"
	}
	testCase := models.TestCase{
		Name: pattern.Subject,
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: pattern.GeneratedAt, RootCause: pattern.SharedRootCause, Severity: severity,
			SuggestedFix: pattern.SuggestedFix, RelevantFiles: slices.Clone(pattern.RelevantFiles),
		},
	}
	return resolvedAnalysis{
		ref: ref, jobID: ref.JobID, buildPrefix: builds[0].BuildPrefix,
		build: cloneBuildInfo(builds[0].Build), testCase: testCase,
		patterns: clonePatternAnalyses(detail.PatternAnalyses), pattern: &pattern,
		patternFresh:   detail.PatternRefresh == nil || detail.PatternRefresh.State == models.PatternRefreshCurrent,
		evidenceBuilds: cloneArtifactBuilds(builds),
	}, nil
}

func validatePatternChatBounds(pattern models.PatternAnalysis) error {
	if len(pattern.CausalGroups) > maxPatternChatCausalGroups {
		return fmt.Errorf("%w: pattern has %d causal groups, maximum %d", ErrInvalidRequest, len(pattern.CausalGroups), maxPatternChatCausalGroups)
	}
	for _, group := range pattern.CausalGroups {
		if len(group.Builds) > maxPatternChatBuildsPerGroup {
			return fmt.Errorf("%w: causal group %q has %d builds, maximum %d", ErrInvalidRequest, group.ID, len(group.Builds), maxPatternChatBuildsPerGroup)
		}
	}
	if len(pattern.UnclassifiedBuilds) > maxPatternChatUnclassifiedBuilds {
		return fmt.Errorf("%w: pattern has %d unclassified builds, maximum %d", ErrInvalidRequest, len(pattern.UnclassifiedBuilds), maxPatternChatUnclassifiedBuilds)
	}
	if len(pattern.RemediationInvestigations) > maxPatternChatRemediationSummaries {
		return fmt.Errorf("%w: pattern has %d remediation summaries, maximum %d", ErrInvalidRequest, len(pattern.RemediationInvestigations), maxPatternChatRemediationSummaries)
	}
	return nil
}

func selectPatternEvidenceRuns(pattern models.PatternAnalysis, runs []models.BuildResult) ([]models.BuildResult, int, int) {
	repeatedGroups := make([]models.PatternCausalGroup, 0, len(pattern.CausalGroups))
	eligible := map[string]struct{}{}
	for _, group := range pattern.CausalGroups {
		if len(group.Builds) < 2 {
			continue
		}
		repeatedGroups = append(repeatedGroups, group)
		for _, buildID := range group.Builds {
			if buildID = strings.TrimSpace(buildID); buildID != "" {
				eligible[buildID] = struct{}{}
			}
		}
	}
	if len(repeatedGroups) == 0 {
		for _, buildID := range pattern.SharedBuilds {
			if buildID = strings.TrimSpace(buildID); buildID != "" {
				eligible[buildID] = struct{}{}
			}
		}
	}

	matchingRuns := make([]models.BuildResult, 0, len(eligible))
	seenRuns := map[string]struct{}{}
	for _, run := range runs {
		if _, ok := eligible[run.BuildID]; !ok {
			continue
		}
		if _, duplicate := seenRuns[run.BuildID]; duplicate {
			continue
		}
		seenRuns[run.BuildID] = struct{}{}
		matchingRuns = append(matchingRuns, run)
	}
	sortPatternEvidenceRuns(matchingRuns)

	fillEligible := eligible
	if len(repeatedGroups) > 0 && len(pattern.SharedBuilds) > 0 {
		fillEligible = make(map[string]struct{}, len(pattern.SharedBuilds))
		for _, buildID := range pattern.SharedBuilds {
			buildID = strings.TrimSpace(buildID)
			if _, ok := eligible[buildID]; ok {
				fillEligible[buildID] = struct{}{}
			}
		}
	}
	selected := make([]models.BuildResult, 0, maxPatternEvidenceBuilds)
	selectedIDs := map[string]struct{}{}
	for _, group := range repeatedGroups {
		groupBuilds := make(map[string]struct{}, len(group.Builds))
		for _, buildID := range group.Builds {
			groupBuilds[strings.TrimSpace(buildID)] = struct{}{}
		}
		for _, run := range matchingRuns {
			if _, ok := groupBuilds[run.BuildID]; !ok {
				continue
			}
			if _, exists := selectedIDs[run.BuildID]; exists {
				continue
			}
			selected = append(selected, run)
			selectedIDs[run.BuildID] = struct{}{}
			break
		}
		if len(selected) == maxPatternEvidenceBuilds {
			return selected, len(matchingRuns), len(eligible)
		}
	}
	for _, run := range matchingRuns {
		if _, exists := selectedIDs[run.BuildID]; exists {
			continue
		}
		if _, ok := fillEligible[run.BuildID]; !ok {
			continue
		}
		selected = append(selected, run)
		if len(selected) == maxPatternEvidenceBuilds {
			break
		}
	}
	return selected, len(matchingRuns), len(eligible)
}

func sortPatternEvidenceRuns(runs []models.BuildResult) {
	slices.SortStableFunc(runs, func(left, right models.BuildResult) int {
		if !left.Started.Equal(right.Started) {
			if left.Started.After(right.Started) {
				return -1
			}
			return 1
		}
		leftID, leftErr := strconv.ParseUint(left.BuildID, 10, 64)
		rightID, rightErr := strconv.ParseUint(right.BuildID, 10, 64)
		if leftErr == nil && rightErr == nil && leftID != rightID {
			if leftID > rightID {
				return -1
			}
			return 1
		}
		return strings.Compare(right.BuildID, left.BuildID)
	})
}

func artifactBuildFor(detail models.JobDetail, run models.BuildResult) (ArtifactBuild, error) {
	jobLocation := prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo}
	if detail.JobType != models.JobTypePeriodic && detail.JobType != models.JobTypePresubmit {
		return ArtifactBuild{}, fmt.Errorf("%w: unsupported job type %q", ErrInvalidRequest, detail.JobType)
	}
	if detail.JobType == models.JobTypePresubmit && (detail.Repo == "" || run.PullNumber == "") {
		return ArtifactBuild{}, fmt.Errorf("%w: presubmit build identity is incomplete", ErrInvalidRequest)
	}
	prefix := (prowbuild.BuildLocation{
		JobLocation: jobLocation, JobName: detail.Name, BuildID: run.BuildID, PullNumber: run.PullNumber,
	}).BuildPath()
	return ArtifactBuild{BuildPrefix: prefix, Build: cloneBuildInfo(run.BuildInfo)}, nil
}

func cloneArtifactBuilds(builds []ArtifactBuild) []ArtifactBuild {
	out := slices.Clone(builds)
	for i := range out {
		out[i].Build = cloneBuildInfo(out[i].Build)
	}
	return out
}

func clonePatternAnalyses(patterns []models.PatternAnalysis) []models.PatternAnalysis {
	out := slices.Clone(patterns)
	for i := range out {
		out[i].SharedBuilds = slices.Clone(patterns[i].SharedBuilds)
		out[i].CausalGroups = slices.Clone(patterns[i].CausalGroups)
		for groupIndex := range out[i].CausalGroups {
			out[i].CausalGroups[groupIndex].Builds = slices.Clone(patterns[i].CausalGroups[groupIndex].Builds)
			out[i].CausalGroups[groupIndex].CauseLocation = patterns[i].CausalGroups[groupIndex].CauseLocation.Clone()
		}
		out[i].UnclassifiedBuilds = slices.Clone(patterns[i].UnclassifiedBuilds)
		out[i].RemediationTargets = slices.Clone(patterns[i].RemediationTargets)
		out[i].RemediationInvestigations = slices.Clone(patterns[i].RemediationInvestigations)
		out[i].RelevantFiles = slices.Clone(patterns[i].RelevantFiles)
		out[i].FileLinks = maps.Clone(patterns[i].FileLinks)
		if patterns[i].Lifecycle != nil {
			lifecycle := *patterns[i].Lifecycle
			lifecycle.PassingBuilds = slices.Clone(patterns[i].Lifecycle.PassingBuilds)
			lifecycle.RecoveryBuilds = slices.Clone(patterns[i].Lifecycle.RecoveryBuilds)
			out[i].Lifecycle = &lifecycle
		}
	}
	return out
}

func validateStateDirPrivacy(dataDir, stateDir string) error {
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolving analysis chat data directory path: %w", err)
	}
	stateAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolving analysis chat state directory path: %w", err)
	}
	if err := validateStateDirRelation(dataAbs, stateAbs); err != nil {
		return err
	}

	dataRoot, err := filepath.EvalSymlinks(dataAbs)
	if err != nil {
		return fmt.Errorf("resolving analysis chat data directory: %w", err)
	}
	stateRoot, err := filepath.EvalSymlinks(stateAbs)
	if err != nil {
		return fmt.Errorf("resolving analysis chat state directory: %w", err)
	}
	return validateStateDirRelation(dataRoot, stateRoot)
}

func validateStateDirRelation(dataRoot, stateRoot string) error {
	rel, err := filepath.Rel(dataRoot, stateRoot)
	if err != nil {
		return fmt.Errorf("comparing analysis chat state directory: %w", err)
	}
	if rel == "." {
		return fmt.Errorf("analysis chat state directory must not equal the public data directory")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if !strings.HasPrefix(first, ".") {
		return fmt.Errorf("analysis chat state beneath the public data directory must use a dot-prefixed top-level directory")
	}
	return nil
}

func normalizeAnalysisRef(ref AnalysisRef) (AnalysisRef, error) {
	ref.Scope = strings.ToLower(strings.TrimSpace(ref.Scope))
	ref.JobID = strings.TrimSpace(ref.JobID)
	ref.BuildID = strings.TrimSpace(ref.BuildID)
	ref.TestName = strings.TrimSpace(ref.TestName)
	ref.Source = strings.TrimSpace(ref.Source)
	ref.SuiteName = strings.TrimSpace(ref.SuiteName)
	ref.ClassName = strings.TrimSpace(ref.ClassName)
	ref.JUnitFile = strings.TrimSpace(ref.JUnitFile)
	ref.AnalysisGeneratedAt = strings.TrimSpace(ref.AnalysisGeneratedAt)
	ref.PatternID = strings.TrimSpace(ref.PatternID)
	ref.PatternHash = strings.TrimSpace(ref.PatternHash)
	if ref.Scope == "" {
		if ref.PatternID != "" || ref.PatternHash != "" {
			ref.Scope = ScopePattern
		} else {
			ref.Scope = ScopeTest
		}
	}
	if ref.JobID == "" {
		return AnalysisRef{}, fmt.Errorf("%w: job_id is required", ErrInvalidRequest)
	}
	switch ref.Scope {
	case ScopeTest:
		if ref.BuildID == "" || ref.TestName == "" || ref.PatternID != "" || ref.PatternHash != "" {
			return AnalysisRef{}, fmt.Errorf("%w: test scope requires build_id and test_name only", ErrInvalidRequest)
		}
		if ref.Source != "" && ref.Source != models.TestCaseSourceBuild {
			return AnalysisRef{}, fmt.Errorf("%w: unsupported failure source %q", ErrInvalidRequest, ref.Source)
		}
		if ref.Source == models.TestCaseSourceBuild && ref.JUnitFile != "" {
			return AnalysisRef{}, fmt.Errorf("%w: build source must not include junit_file", ErrInvalidRequest)
		}
	case ScopePattern:
		if ref.PatternID == "" || ref.PatternHash == "" || ref.BuildID != "" || ref.TestName != "" || ref.Source != "" || ref.SuiteName != "" || ref.ClassName != "" || ref.JUnitFile != "" || ref.AnalysisGeneratedAt != "" {
			return AnalysisRef{}, fmt.Errorf("%w: pattern scope requires pattern_id and pattern_hash only", ErrInvalidRequest)
		}
	default:
		return AnalysisRef{}, fmt.Errorf("%w: unsupported analysis scope %q", ErrInvalidRequest, ref.Scope)
	}
	if len(ref.JobID) > maxJobIDBytes || len(ref.BuildID) > maxBuildIDBytes || len(ref.TestName) > maxTestNameBytes ||
		len(ref.SuiteName) > maxSuiteNameBytes || len(ref.ClassName) > maxClassNameBytes ||
		len(ref.JUnitFile) > maxJUnitFileBytes || len(ref.AnalysisGeneratedAt) > maxTimestampBytes ||
		len(ref.PatternID) > maxPatternIDBytes || len(ref.PatternHash) > maxPatternHashBytes {
		return AnalysisRef{}, fmt.Errorf("%w: analysis reference field exceeds its size limit", ErrInvalidRequest)
	}
	return ref, nil
}

func normalizeOwner(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func cloneSessionView(view SessionView) SessionView {
	view.Messages = slices.Clone(view.Messages)
	view.Attempts = slices.Clone(view.Attempts)
	if view.Active != nil {
		active := *view.Active
		view.Active = &active
	}
	for i := range view.Messages {
		view.Messages[i].Citations = slices.Clone(view.Messages[i].Citations)
		view.Messages[i].ProposedRevision = cloneRevision(view.Messages[i].ProposedRevision)
	}
	return view
}

func (s *Service) sessionView(current *persistedSession) SessionView {
	view := cloneSessionView(current.View)
	view.Attempts = attemptViews(current.Requests)
	view.TurnsUsed = current.Turns
	view.MaxTurns = s.opts.MaxTurns
	if view.Analysis.Scope != ScopePattern {
		if repo, ok := persistedBuildSourceRepository(current.Resolved, s.sourceRepo); ok {
			view.SourceRepository = &repo
		}
	}
	if current.Active != nil {
		view.Active = &ActiveTurn{
			RequestID: current.Active.RequestID, Question: current.Active.Question, Phase: current.Active.Phase,
			StartedAt: optionalTimestamp(current.Active.StartedAt), UpdatedAt: current.Active.UpdatedAt.Format(time.RFC3339),
			ValidationRetries: current.Active.ValidationRetries, MaxValidationRetries: 1,
		}
	}
	return view
}

func optionalTimestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func attemptViews(requests map[string]persistedRequest) []Attempt {
	attempts := make([]Attempt, 0, len(requests))
	for requestID, request := range requests {
		outcome, failureKind := safeAttemptOutcome(request.Status, request.FailureKind)
		attempts = append(attempts, Attempt{
			RequestID: requestID, Question: request.Question, Outcome: outcome,
			FailureKind: failureKind, Turn: request.Turn,
			CreatedAt: safeAttemptTimestamp(request.CreatedAt), UpdatedAt: safeAttemptTimestamp(request.UpdatedAt),
		})
	}
	sort.Slice(attempts, func(i, j int) bool {
		left, right := attempts[i], attempts[j]
		if left.Turn != right.Turn {
			if left.Turn == 0 {
				return false
			}
			if right.Turn == 0 {
				return true
			}
			return left.Turn < right.Turn
		}
		if left.CreatedAt != right.CreatedAt {
			return left.CreatedAt < right.CreatedAt
		}
		return left.RequestID < right.RequestID
	})
	return attempts
}

func safeAttemptOutcome(status, failureKind string) (string, string) {
	switch status {
	case requestPending:
		return requestPending, ""
	case requestSucceeded:
		return requestSucceeded, ""
	case requestUnknown:
		return requestUnknown, ""
	case requestFailed:
		switch failureKind {
		case failureCancelled:
			return failureCancelled, ""
		case failureTimeout:
			return "timed_out", ""
		case failureProvider, failureValidation, failureSource, failureModel:
			return requestFailed, failureKind
		default:
			return requestFailed, failureModel
		}
	default:
		return requestUnknown, ""
	}
}

func safeAttemptTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

func cloneRevision(revision *Revision) *Revision {
	if revision == nil {
		return nil
	}
	copy := *revision
	return &copy
}

func cloneBuildInfo(build models.BuildInfo) models.BuildInfo {
	build.JUnitURLs = slices.Clone(build.JUnitURLs)
	build.RepoRefs = maps.Clone(build.RepoRefs)
	return build
}

func cloneTestCase(testCase models.TestCase) models.TestCase {
	if testCase.AISummary != nil {
		summary := *testCase.AISummary
		testCase.AISummary = &summary
	}
	if testCase.AIAnalysis != nil {
		analysis := *testCase.AIAnalysis
		analysis.RelevantFiles = slices.Clone(analysis.RelevantFiles)
		analysis.SearchSuggestions = slices.Clone(analysis.SearchSuggestions)
		analysis.EvidenceCitations = slices.Clone(analysis.EvidenceCitations)
		analysis.DispositionWarnings = slices.Clone(analysis.DispositionWarnings)
		analysis.FileLinks = maps.Clone(analysis.FileLinks)
		analysis.CauseLocation = analysis.CauseLocation.Clone()
		testCase.AIAnalysis = &analysis
	}
	return testCase
}
