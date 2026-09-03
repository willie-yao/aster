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
	"log"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// DefaultTurnTimeout is the default budget for one analysis chat turn.
const DefaultTurnTimeout = 10 * time.Minute

var (
	// ErrAnalysisNotFound means the published data has no matching analysis.
	ErrAnalysisNotFound = errors.New("analysis not found")
	// ErrAnalysisChanged means the selected analysis was replaced after the client loaded it.
	ErrAnalysisChanged = errors.New("analysis changed")
	// ErrPatternNotFound means the selected recurring pattern is absent.
	ErrPatternNotFound = errors.New("recurring pattern not found")
	// ErrPatternChanged means the selected recurring pattern was replaced.
	ErrPatternChanged = errors.New("recurring pattern changed")
	// ErrCauseNotFound means the selected causal group is absent.
	ErrCauseNotFound = errors.New("causal group not found")
	// ErrCauseChanged means the selected causal group was replaced.
	ErrCauseChanged = errors.New("causal group changed")
	// ErrPreparedFindingNotFound means no engine-prepared cause answer is available.
	ErrPreparedFindingNotFound = errors.New("prepared cause finding not found")
	// ErrSessionNotFound means the session is absent, expired, or owned by another user.
	ErrSessionNotFound = errors.New("analysis chat session not found")
	// ErrSessionBusy means another turn is already running for the session.
	ErrSessionBusy = errors.New("analysis chat session is busy")
	// ErrSessionReferenced means a Fix request still depends on the session.
	ErrSessionReferenced = errors.New("analysis chat session supports a fix request")
	// ErrRequestPending means this idempotent request is still running.
	ErrRequestPending = errors.New("analysis chat request is pending")
	// ErrRequestNotFound means the session has no request with this ID.
	ErrRequestNotFound = errors.New("analysis chat request not found")
	// ErrSessionLimit means the deployment or creator has too many live sessions.
	ErrSessionLimit = errors.New("analysis chat session limit reached")
	// ErrActiveTurnLimit means an operator has too many concurrent turns.
	ErrActiveTurnLimit = errors.New("analysis chat active turn limit reached")
	// ErrRateLimit means an operator exceeded the admitted turn rate.
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

// GateMessage returns the operator-safe message for a response validation gate.
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
	ScopeCause   = "cause"

	maxJobDetailBytes                = 64 << 20
	maxJobIDBytes                    = 1024
	maxBuildIDBytes                  = 256
	maxTestNameBytes                 = 4096
	maxSuiteNameBytes                = 4096
	maxClassNameBytes                = 4096
	maxJUnitFileBytes                = 1024
	maxTimestampBytes                = 128
	maxRequestIDBytes                = 128
	maxPatternIDBytes                = 512
	maxPatternHashBytes              = 128
	maxCausalGroupIDBytes            = 512
	maxCausalGroupHashBytes          = 128
	maxPatternEvidenceBuilds         = 3
	maxPatternChatCausalGroups       = 10
	maxPatternChatBuildsPerGroup     = 10
	maxPatternChatUnclassifiedBuilds = 10
)

// AnalysisRef addresses one published test, recurring pattern, or causal group analysis.
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
	CausalGroupID       string `json:"causal_group_id,omitempty"`
	CausalGroupHash     string `json:"causal_group_hash,omitempty"`
}

// Citation identifies artifact or source evidence used in one answer.
type Citation struct {
	Repository string `json:"repository,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Path       string `json:"path"`
	LineStart  int    `json:"line_start,omitempty"`
	LineEnd    int    `json:"line_end,omitempty"`
	Quote      string `json:"quote,omitempty"`
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
	EvidenceWarnings  []string   `json:"evidence_warnings,omitempty"`
	ToolCalls         int        `json:"tool_calls,omitempty"`
	GCSBytes          int        `json:"gcs_bytes,omitempty"`
	ElapsedMs         int        `json:"elapsed_ms,omitempty"`
	ProviderMs        int        `json:"provider_ms,omitempty"`
	ValidationRetries int        `json:"validation_retries,omitempty"`
}

// Message is one user or assistant entry in a session transcript.
type Message struct {
	Role              string     `json:"role"`
	Actor             string     `json:"actor,omitempty"`
	RequestID         string     `json:"request_id,omitempty"`
	Content           string     `json:"content"`
	Assessment        string     `json:"assessment,omitempty"`
	Citations         []Citation `json:"citations,omitempty"`
	ProposedRevision  *Revision  `json:"proposed_revision,omitempty"`
	Unverified        bool       `json:"unverified,omitempty"`
	UnverifiedReason  string     `json:"unverified_reason,omitempty"`
	EvidenceWarnings  []string   `json:"evidence_warnings,omitempty"`
	Prepared          bool       `json:"prepared,omitempty"`
	ToolCalls         int        `json:"tool_calls,omitempty"`
	GCSBytes          int        `json:"gcs_bytes,omitempty"`
	ElapsedMs         int        `json:"elapsed_ms,omitempty"`
	ProviderMs        int        `json:"provider_ms,omitempty"`
	ValidationRetries int        `json:"validation_retries,omitempty"`
	CreatedAt         string     `json:"created_at"`
}

// Attempt is one operator-safe admitted model request.
type Attempt struct {
	RequestID   string `json:"request_id"`
	Actor       string `json:"actor,omitempty"`
	Question    string `json:"question,omitempty"`
	Outcome     string `json:"outcome"`
	FailureKind string `json:"failure_kind,omitempty"`
	Turn        int    `json:"turn,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// SessionView is the authenticated session representation returned by the API.
type SessionView struct {
	ID               string                          `json:"id"`
	CreatedBy        string                          `json:"created_by"`
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

// ActiveTurn is the authenticated state for one in-flight request.
type ActiveTurn struct {
	Actor                string `json:"actor,omitempty"`
	RequestID            string `json:"request_id"`
	Question             string `json:"question,omitempty"`
	Phase                string `json:"phase"`
	StartedAt            string `json:"started_at,omitempty"`
	UpdatedAt            string `json:"updated_at"`
	ValidationRetries    int    `json:"validation_retries,omitempty"`
	MaxValidationRetries int    `json:"max_validation_retries,omitempty"`
}

// Progress is a persisted operator-safe turn phase.
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
	SessionID            string
	Scope                string
	JobID                string
	BuildPrefix          string
	Build                models.BuildInfo
	TestCase             models.TestCase
	Pattern              *models.PatternAnalysis
	EvidenceBuilds       []ArtifactBuild
	Comparison           *CauseComparison
	History              []Message
	Question             string
	HistoricalSourceOnly bool
	Progress             func(string)
}

// ArtifactBuild identifies one build root available to a pattern conversation.
type ArtifactBuild struct {
	BuildPrefix string           `json:"build_prefix"`
	Build       models.BuildInfo `json:"build"`
}

// CauseComparison is the newest completed run after a cause's member failures.
type CauseComparison struct {
	ArtifactBuild ArtifactBuild
	TestNames     []string
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
	if o.TurnTimeout <= 0 {
		o.TurnTimeout = DefaultTurnTimeout
	}
	if o.TurnLeaseTTL <= o.TurnTimeout {
		o.TurnLeaseTTL = o.TurnTimeout + 30*time.Second
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

// Service resolves published analyses and owns durable chat sessions.
type Service struct {
	dataDir            string
	runner             Runner
	testFixPreflight   func(context.Context, sourceinvestigation.Repository, string, []string) (string, map[string]string, error)
	sourceRepo         sourceinvestigation.Repository
	opts               Options
	store              *sessionStore
	preparedGeneration string
	lifecycle          context.Context
	activeMu           sync.Mutex
	active             map[string]context.CancelFunc
	activeWG           sync.WaitGroup
	notifyMu           sync.Mutex
	notify             map[string]map[chan struct{}]struct{}
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

// ConfigurePreparedCauseFindings enables engine-prepared first answers.
func (s *Service) ConfigurePreparedCauseFindings(generation string) error {
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return fmt.Errorf("prepared cause finding generation is required")
	}
	s.preparedGeneration = generation
	return nil
}

// CreatePrepared starts a cause session only when a prepared finding is ready.
func (s *Service) CreatePrepared(ref AnalysisRef, owner, requestID string) (SessionView, error) {
	return s.create(ref, owner, requestID, true)
}

func (s *Service) preparedFinding(resolved resolvedAnalysis) (PreparedCauseFinding, string, bool) {
	if resolved.ref.Scope != ScopeCause || s.preparedGeneration == "" {
		return PreparedCauseFinding{}, "", false
	}
	prepared, err := LoadPreparedCauseFindings(preparedFindingPath(s.dataDir), s.preparedGeneration)
	if err != nil {
		return PreparedCauseFinding{}, "", false
	}
	return lookupPreparedFinding(prepared, resolved.ref, causeComparisonBuildID(resolved.comparison))
}

// PreparedAvailable reports which references have a usable prepared finding.
// The result is parallel to refs; the cache is read once for the whole batch.
func (s *Service) PreparedAvailable(refs []AnalysisRef) []bool {
	available := make([]bool, len(refs))
	if len(refs) == 0 || s.preparedGeneration == "" {
		return available
	}
	prepared, err := LoadPreparedCauseFindings(preparedFindingPath(s.dataDir), s.preparedGeneration)
	if err != nil {
		return available
	}
	details := map[string]struct {
		detail models.JobDetail
		err    error
	}{}
	for i, ref := range refs {
		normalized, err := normalizeAnalysisRef(ref)
		if err != nil || normalized.Scope != ScopeCause {
			continue
		}
		loaded, ok := details[normalized.JobID]
		if !ok {
			loaded.detail, loaded.err = s.loadJobDetail(normalized.JobID)
			details[normalized.JobID] = loaded
		}
		if loaded.err != nil {
			continue
		}
		resolved, err := resolveFromDetail(normalized, loaded.detail)
		if err != nil {
			continue
		}
		_, _, available[i] = lookupPreparedFinding(prepared, resolved.ref, causeComparisonBuildID(resolved.comparison))
	}
	return available
}

// lookupPreparedFinding resolves one normalized cause reference against an
// already-loaded cache. An unverified or uncited finding is not usable.
func lookupPreparedFinding(prepared PreparedCauseFindings, ref AnalysisRef, comparisonBuildID string) (PreparedCauseFinding, string, bool) {
	key, err := PreparedCauseKey(ref, comparisonBuildID)
	if err != nil {
		return PreparedCauseFinding{}, "", false
	}
	finding, ok := prepared.Findings[key]
	if !ok || finding.Ref != ref || finding.Reply.Unverified || len(finding.Reply.Citations) == 0 {
		return PreparedCauseFinding{}, "", false
	}
	return finding, key, true
}

// Create resolves an analysis snapshot and returns the shared session for it.
func (s *Service) Create(ref AnalysisRef, owner, requestID string) (SessionView, error) {
	return s.create(ref, owner, requestID, false)
}

func (s *Service) create(ref AnalysisRef, owner, requestID string, preparedOnly bool) (SessionView, error) {
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
	now := s.opts.Now().UTC()

	var existing SessionView
	ctx, cancel := s.store.context()
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current, err := findCreateRequest(state, owner, requestID, requestHash)
		if err != nil {
			return changed, err
		}
		if current != nil {
			existing = s.sessionView(current)
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
	seedMessages := []Message{}
	seedRequests := map[string]persistedRequest{}
	if finding, key, ok := s.preparedFinding(resolved); ok {
		message, request := preparedMessage(finding, key, now)
		seedMessages = append(seedMessages, message)
		seedRequests[message.RequestID] = request
	} else if preparedOnly {
		return SessionView{}, ErrPreparedFindingNotFound
	}
	id, err := newSessionID()
	if err != nil {
		return SessionView{}, fmt.Errorf("creating analysis chat session: %w", err)
	}
	expires := now.Add(s.opts.SessionTTL)
	created := &persistedSession{
		Owner:             owner,
		Resolved:          persistResolved(resolved, s.sourceRepo),
		ExpiresAt:         expires,
		CreateRequestID:   requestID,
		CreateRequestHash: requestHash,
		Requests:          seedRequests,
		View: SessionView{
			ID:        id,
			CreatedBy: owner,
			Analysis:  resolved.ref,
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: expires.Format(time.RFC3339),
			Messages:  seedMessages,
		},
	}

	ctx, cancel = s.store.context()
	defer cancel()
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current, err := findCreateRequest(state, owner, requestID, requestHash)
		if err != nil {
			return changed, err
		}
		if current == nil {
			current = s.latestSessionForAnalysis(state, resolved)
		}
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

// Get returns a shared session to an authenticated operator.
func (s *Service) Get(id, owner string) (SessionView, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return SessionView{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	now := s.opts.Now().UTC()
	var view SessionView
	ctx, cancel := s.store.context()
	defer cancel()
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		view = s.sessionView(current)
		return changed, nil
	})
	return view, err
}

// Delete removes an idle shared session.
func (s *Service) Delete(id, owner string) error {
	owner = normalizeOwner(owner)
	if owner == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	id = strings.TrimSpace(id)
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[id]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		if current.Active != nil {
			return changed, ErrSessionBusy
		}
		if hasFixDependency(current.FixSources) {
			return changed, ErrSessionReferenced
		}
		delete(state.Sessions, id)
		return true, nil
	})
}

// Find returns the latest shared session for the current analysis.
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
		current := s.latestSessionForAnalysis(state, resolved)
		if current == nil {
			return changed, ErrSessionNotFound
		}
		view = s.sessionView(current)
		return changed, nil
	})
	return view, err
}

func (s *Service) latestSessionForAnalysis(state *persistedState, resolved resolvedAnalysis) *persistedSession {
	var latest *persistedSession
	latestID := ""
	for id, current := range state.Sessions {
		if current == nil || current.Retired || current.View.Analysis != resolved.ref {
			continue
		}
		if resolved.ref.Scope == ScopeCause && persistedCauseComparisonBuildID(current.Resolved.Comparison) != causeComparisonBuildID(resolved.comparison) {
			continue
		}
		if latest == nil || sharedSessionNewer(id, current, latestID, latest) {
			latest, latestID = current, id
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
			if previous.Actor == "" {
				previous.Actor = active.Actor
			}
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
	owned := 0
	for _, current := range state.Sessions {
		if current != nil && current.Owner == owner {
			owned++
		}
	}
	return owned >= s.opts.MaxSessionsPerOwner
}

func findCreateRequest(state *persistedState, owner, requestID, requestHash string) (*persistedSession, error) {
	for _, current := range state.Sessions {
		if current == nil || current.Retired || current.Owner != owner || current.CreateRequestID != requestID {
			continue
		}
		if current.CreateRequestHash != requestHash {
			return nil, ErrIdempotencyConflict
		}
		return current, nil
	}
	return nil, nil
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
	ref.CausalGroupID = strings.TrimSpace(ref.CausalGroupID)
	ref.CausalGroupHash = strings.TrimSpace(ref.CausalGroupHash)
	if ref.Scope == "" {
		if ref.CausalGroupID != "" || ref.CausalGroupHash != "" {
			ref.Scope = ScopeCause
		} else if ref.PatternID != "" || ref.PatternHash != "" {
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
		if ref.BuildID == "" || ref.TestName == "" || ref.PatternID != "" || ref.PatternHash != "" || ref.CausalGroupID != "" || ref.CausalGroupHash != "" {
			return AnalysisRef{}, fmt.Errorf("%w: test scope requires build_id and test_name only", ErrInvalidRequest)
		}
		if ref.Source != "" && ref.Source != models.TestCaseSourceBuild {
			return AnalysisRef{}, fmt.Errorf("%w: unsupported failure source %q", ErrInvalidRequest, ref.Source)
		}
		if ref.Source == models.TestCaseSourceBuild && ref.JUnitFile != "" {
			return AnalysisRef{}, fmt.Errorf("%w: build source must not include junit_file", ErrInvalidRequest)
		}
	case ScopePattern:
		if ref.PatternID == "" || ref.PatternHash == "" || ref.CausalGroupID != "" || ref.CausalGroupHash != "" || ref.BuildID != "" || ref.TestName != "" || ref.Source != "" || ref.SuiteName != "" || ref.ClassName != "" || ref.JUnitFile != "" || ref.AnalysisGeneratedAt != "" {
			return AnalysisRef{}, fmt.Errorf("%w: pattern scope requires pattern_id and pattern_hash only", ErrInvalidRequest)
		}
	case ScopeCause:
		if ref.PatternID == "" || ref.PatternHash == "" || ref.CausalGroupID == "" || ref.CausalGroupHash == "" || ref.BuildID != "" || ref.TestName != "" || ref.Source != "" || ref.SuiteName != "" || ref.ClassName != "" || ref.JUnitFile != "" || ref.AnalysisGeneratedAt != "" {
			return AnalysisRef{}, fmt.Errorf("%w: cause scope requires pattern_id, pattern_hash, causal_group_id, and causal_group_hash only", ErrInvalidRequest)
		}
	default:
		return AnalysisRef{}, fmt.Errorf("%w: unsupported analysis scope %q", ErrInvalidRequest, ref.Scope)
	}
	if len(ref.JobID) > maxJobIDBytes || len(ref.BuildID) > maxBuildIDBytes || len(ref.TestName) > maxTestNameBytes ||
		len(ref.SuiteName) > maxSuiteNameBytes || len(ref.ClassName) > maxClassNameBytes ||
		len(ref.JUnitFile) > maxJUnitFileBytes || len(ref.AnalysisGeneratedAt) > maxTimestampBytes ||
		len(ref.PatternID) > maxPatternIDBytes || len(ref.PatternHash) > maxPatternHashBytes ||
		len(ref.CausalGroupID) > maxCausalGroupIDBytes || len(ref.CausalGroupHash) > maxCausalGroupHashBytes {
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
		view.Messages[i].EvidenceWarnings = slices.Clone(view.Messages[i].EvidenceWarnings)
	}
	return view
}

func (s *Service) sessionView(current *persistedSession) SessionView {
	view := cloneSessionView(current.View)
	view.CreatedBy = current.Owner
	view.Attempts = attemptViews(current.Requests)
	view.TurnsUsed = current.Turns
	view.MaxTurns = s.opts.MaxTurns
	if view.Analysis.Scope == ScopeTest {
		if repo, ok := persistedBuildSourceRepository(current.Resolved, s.sourceRepo); ok {
			view.SourceRepository = &repo
		}
	}
	if current.Active != nil {
		view.Active = &ActiveTurn{
			Actor: current.Active.Actor, RequestID: current.Active.RequestID, Question: current.Active.Question, Phase: current.Active.Phase,
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
			RequestID: requestID, Actor: request.Actor, Question: request.Question, Outcome: outcome,
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
