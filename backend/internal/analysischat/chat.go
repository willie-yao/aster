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
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

var (
	// ErrAnalysisNotFound means the published data has no matching analysis.
	ErrAnalysisNotFound = errors.New("analysis not found")
	// ErrAnalysisChanged means the selected analysis was replaced after the client loaded it.
	ErrAnalysisChanged = errors.New("analysis changed")
	// ErrSessionNotFound means the session is absent, expired, or owned by another user.
	ErrSessionNotFound = errors.New("analysis chat session not found")
	// ErrSessionBusy means another turn is already running for the session.
	ErrSessionBusy = errors.New("analysis chat session is busy")
	// ErrSessionLimit means the deployment or owner has too many live sessions.
	ErrSessionLimit = errors.New("analysis chat session limit reached")
	// ErrIdempotencyConflict means a request key was reused for different input.
	ErrIdempotencyConflict = errors.New("analysis chat idempotency key conflict")
	// ErrRequestOutcomeUnknown means a replica died before recording a turn result.
	ErrRequestOutcomeUnknown = errors.New("analysis chat request outcome unknown")
	// ErrRequestFailed means an earlier idempotent attempt failed before answering.
	ErrRequestFailed = errors.New("analysis chat request failed")
	// ErrTurnLimit means the session has used its allowed turns.
	ErrTurnLimit = errors.New("analysis chat turn limit reached")
	// ErrInvalidRequest means a request field is missing, ambiguous, or too large.
	ErrInvalidRequest = errors.New("invalid analysis chat request")
)

const (
	maxJobDetailBytes = 64 << 20
	maxJobIDBytes     = 1024
	maxBuildIDBytes   = 256
	maxTestNameBytes  = 4096
	maxSuiteNameBytes = 4096
	maxClassNameBytes = 4096
	maxJUnitFileBytes = 1024
	maxTimestampBytes = 128
	maxRequestIDBytes = 128
)

// AnalysisRef addresses one published test analysis.
type AnalysisRef struct {
	JobID               string `json:"job_id"`
	BuildID             string `json:"build_id"`
	TestName            string `json:"test_name"`
	SuiteName           string `json:"suite_name,omitempty"`
	ClassName           string `json:"class_name,omitempty"`
	JUnitFile           string `json:"junit_file,omitempty"`
	AnalysisGeneratedAt string `json:"analysis_generated_at,omitempty"`
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
	Answer           string     `json:"answer"`
	Assessment       string     `json:"assessment"`
	Citations        []Citation `json:"citations,omitempty"`
	ProposedRevision *Revision  `json:"proposed_revision,omitempty"`
	ToolCalls        int        `json:"tool_calls,omitempty"`
	GCSBytes         int        `json:"gcs_bytes,omitempty"`
	ElapsedMs        int        `json:"elapsed_ms,omitempty"`
}

// Message is one user or assistant entry in a session transcript.
type Message struct {
	Role             string     `json:"role"`
	RequestID        string     `json:"request_id,omitempty"`
	Content          string     `json:"content"`
	Assessment       string     `json:"assessment,omitempty"`
	Citations        []Citation `json:"citations,omitempty"`
	ProposedRevision *Revision  `json:"proposed_revision,omitempty"`
	ToolCalls        int        `json:"tool_calls,omitempty"`
	GCSBytes         int        `json:"gcs_bytes,omitempty"`
	ElapsedMs        int        `json:"elapsed_ms,omitempty"`
	CreatedAt        string     `json:"created_at"`
}

// SessionView is the owner-safe session representation returned by the API.
type SessionView struct {
	ID        string      `json:"id"`
	Analysis  AnalysisRef `json:"analysis"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ExpiresAt string      `json:"expires_at"`
	Messages  []Message   `json:"messages"`
}

// Turn is the immutable analysis snapshot and transcript for one model call.
type Turn struct {
	SessionID   string
	JobID       string
	BuildPrefix string
	Build       models.BuildInfo
	TestCase    models.TestCase
	History     []Message
	Question    string
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
	TurnLeaseTTL     time.Duration
	StoreLockTimeout time.Duration
	CleanupInterval  time.Duration
	Now              func() time.Time
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
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type resolvedAnalysis struct {
	ref         AnalysisRef
	jobID       string
	buildPrefix string
	build       models.BuildInfo
	testCase    models.TestCase
}

// Service resolves published analyses and owns durable chat sessions.
type Service struct {
	dataDir string
	runner  Runner
	opts    Options
	store   *sessionStore
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
	service := &Service{dataDir: dataDir, runner: runner, opts: opts, store: store}
	if err := service.cleanupPersisted(); err != nil {
		return nil, fmt.Errorf("cleaning analysis chat state: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
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
			existing = cloneSessionView(current.View)
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
		Owner:             owner,
		Resolved:          persistResolved(resolved),
		ExpiresAt:         expires,
		CreateRequestID:   requestID,
		CreateRequestHash: requestHash,
		Requests:          map[string]persistedRequest{},
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
		current, err := findCreateRequest(state, owner, requestID, requestHash)
		if err != nil {
			return changed, err
		}
		if current != nil {
			existing = cloneSessionView(current.View)
			return changed, nil
		}
		if s.sessionLimitReached(state, owner) {
			return changed, ErrSessionLimit
		}
		state.Sessions[id] = created
		existing = cloneSessionView(created.View)
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
		view = cloneSessionView(current.View)
		return changed, nil
	})
	return view, err
}

// Send appends one question and runs a single serialized model turn.
func (s *Service) Send(ctx context.Context, id, owner, requestID, question string) (SessionView, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > s.opts.MaxQuestionBytes {
		return SessionView{}, fmt.Errorf("%w: question must be 1-%d bytes", ErrInvalidRequest, s.opts.MaxQuestionBytes)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return SessionView{}, err
	}
	owner = normalizeOwner(owner)
	questionHash := hashText(question)
	now := s.opts.Now().UTC()
	leaseID, err := newSessionID()
	if err != nil {
		return SessionView{}, fmt.Errorf("creating analysis chat turn lease: %w", err)
	}

	var turn Turn
	var immediate SessionView
	storeCtx, cancel := context.WithTimeout(ctx, s.opts.StoreLockTimeout)
	storeErr := s.store.update(storeCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Requests == nil {
			current.Requests = map[string]persistedRequest{}
			changed = true
		}
		if previous, ok := current.Requests[requestID]; ok {
			if previous.QuestionHash != questionHash {
				return changed, ErrIdempotencyConflict
			}
			switch previous.Status {
			case requestSucceeded:
				immediate = cloneSessionView(current.View)
				return changed, nil
			case requestFailed:
				return changed, persistedRequestError(previous.FailureKind)
			case requestUnknown:
				return changed, ErrRequestOutcomeUnknown
			default:
				return changed, ErrSessionBusy
			}
		}
		if current.Active != nil {
			return changed, ErrSessionBusy
		}
		if current.Turns >= s.opts.MaxTurns {
			return changed, ErrTurnLimit
		}
		current.Turns++
		current.Requests[requestID] = persistedRequest{QuestionHash: questionHash, Status: requestPending}
		current.Active = &persistedActiveTurn{
			RequestID: requestID,
			LeaseID:   leaseID,
			ExpiresAt: now.Add(s.opts.TurnLeaseTTL),
		}
		resolved := restoreResolved(current.Resolved)
		turn = Turn{
			SessionID:   current.View.ID,
			JobID:       resolved.jobID,
			BuildPrefix: resolved.buildPrefix,
			Build:       cloneBuildInfo(resolved.build),
			TestCase:    cloneTestCase(resolved.testCase),
			History:     cloneSessionView(current.View).Messages,
			Question:    question,
		}
		return true, nil
	})
	cancel()
	if storeErr != nil || immediate.ID != "" {
		return immediate, storeErr
	}

	reply, runErr := s.runner.Reply(ctx, turn)
	finishedAt := s.opts.Now().UTC()
	finalCtx, cancel := s.store.context()
	defer cancel()
	var view SessionView
	finalErr := s.store.update(finalCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, finishedAt)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Active == nil || current.Active.RequestID != requestID || current.Active.LeaseID != leaseID {
			return changed, ErrRequestOutcomeUnknown
		}
		previous := current.Requests[requestID]
		current.Active = nil
		if runErr != nil {
			previous.Status = requestFailed
			previous.FailureKind = requestFailureKind(runErr)
			current.Requests[requestID] = previous
			if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
				return true, runErr
			}
			return true, fmt.Errorf("%w: %v", ErrRequestFailed, runErr)
		}
		stamp := finishedAt.Format(time.RFC3339)
		current.View.Messages = append(current.View.Messages,
			Message{Role: "user", RequestID: requestID, Content: question, CreatedAt: stamp},
			Message{
				Role: "assistant", Content: reply.Answer, Assessment: reply.Assessment,
				Citations: slices.Clone(reply.Citations), ProposedRevision: cloneRevision(reply.ProposedRevision),
				ToolCalls: reply.ToolCalls, GCSBytes: reply.GCSBytes, ElapsedMs: reply.ElapsedMs,
				CreatedAt: stamp,
			},
		)
		current.View.UpdatedAt = stamp
		previous.Status = requestSucceeded
		current.Requests[requestID] = previous
		view = cloneSessionView(current.View)
		return true, nil
	})
	return view, finalErr
}

func (s *Service) cleanup(state *persistedState, now time.Time) bool {
	changed := false
	for id, current := range state.Sessions {
		if current.Requests == nil {
			current.Requests = map[string]persistedRequest{}
			changed = true
		}
		if current.Active != nil && !now.Before(current.Active.ExpiresAt) {
			previous := current.Requests[current.Active.RequestID]
			previous.Status = requestUnknown
			previous.FailureKind = ""
			current.Requests[current.Active.RequestID] = previous
			current.Active = nil
			changed = true
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

func findCreateRequest(state *persistedState, owner, requestID, requestHash string) (*persistedSession, error) {
	for _, current := range state.Sessions {
		if current.Owner != owner || current.CreateRequestID != requestID {
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
	default:
		return failureModel
	}
}

func persistedRequestError(kind string) error {
	switch kind {
	case failureTimeout:
		return context.DeadlineExceeded
	case failureCancelled:
		return context.Canceled
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
		suiteName := strings.TrimSpace(testCase.SuiteName)
		className := strings.TrimSpace(testCase.ClassName)
		if testName != ref.TestName ||
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
	ref.SuiteName = strings.TrimSpace(testCase.SuiteName)
	ref.ClassName = strings.TrimSpace(testCase.ClassName)
	ref.JUnitFile = testCase.JUnitFile
	ref.AnalysisGeneratedAt = testCase.AIAnalysis.GeneratedAt

	jobLocation := prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo}
	if detail.JobType != models.JobTypePeriodic && detail.JobType != models.JobTypePresubmit {
		return resolvedAnalysis{}, fmt.Errorf("%w: unsupported job type %q", ErrInvalidRequest, detail.JobType)
	}
	if detail.JobType == models.JobTypePresubmit && (detail.Repo == "" || run.PullNumber == "") {
		return resolvedAnalysis{}, fmt.Errorf("%w: presubmit build identity is incomplete", ErrInvalidRequest)
	}
	buildPrefix := (prowbuild.BuildLocation{
		JobLocation: jobLocation,
		JobName:     detail.Name,
		BuildID:     run.BuildID,
		PullNumber:  run.PullNumber,
	}).BuildPath()
	return resolvedAnalysis{
		ref:         ref,
		jobID:       ref.JobID,
		buildPrefix: buildPrefix,
		build:       cloneBuildInfo(run.BuildInfo),
		testCase:    testCase,
	}, nil
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
	ref.JobID = strings.TrimSpace(ref.JobID)
	ref.BuildID = strings.TrimSpace(ref.BuildID)
	ref.TestName = strings.TrimSpace(ref.TestName)
	ref.SuiteName = strings.TrimSpace(ref.SuiteName)
	ref.ClassName = strings.TrimSpace(ref.ClassName)
	ref.JUnitFile = strings.TrimSpace(ref.JUnitFile)
	ref.AnalysisGeneratedAt = strings.TrimSpace(ref.AnalysisGeneratedAt)
	if ref.JobID == "" || ref.BuildID == "" || ref.TestName == "" {
		return AnalysisRef{}, fmt.Errorf("%w: job_id, build_id, and test_name are required", ErrInvalidRequest)
	}
	if len(ref.JobID) > maxJobIDBytes || len(ref.BuildID) > maxBuildIDBytes || len(ref.TestName) > maxTestNameBytes ||
		len(ref.SuiteName) > maxSuiteNameBytes || len(ref.ClassName) > maxClassNameBytes ||
		len(ref.JUnitFile) > maxJUnitFileBytes || len(ref.AnalysisGeneratedAt) > maxTimestampBytes {
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
	for i := range view.Messages {
		view.Messages[i].Citations = slices.Clone(view.Messages[i].Citations)
		view.Messages[i].ProposedRevision = cloneRevision(view.Messages[i].ProposedRevision)
	}
	return view
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
		analysis.FileLinks = maps.Clone(analysis.FileLinks)
		testCase.AIAnalysis = &analysis
	}
	return testCase
}
