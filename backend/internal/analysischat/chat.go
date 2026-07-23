// Package analysischat manages bounded conversations about published failure analyses.
package analysischat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// Options bounds in-memory session use.
type Options struct {
	SessionTTL          time.Duration
	MaxSessions         int
	MaxSessionsPerOwner int
	MaxTurns            int
	MaxQuestionBytes    int
	Now                 func() time.Time
}

func (o Options) normalized() Options {
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

type session struct {
	view     SessionView
	owner    string
	resolved resolvedAnalysis
	turns    int
	busy     bool
	expires  time.Time
}

// Service resolves published analyses and owns short-lived chat sessions.
type Service struct {
	dataDir string
	runner  Runner
	opts    Options

	mu       sync.Mutex
	sessions map[string]*session
}

// NewService creates an in-memory analysis chat service.
func NewService(dataDir string, runner Runner, opts Options) (*Service, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("analysis chat data directory is required")
	}
	if runner == nil {
		return nil, fmt.Errorf("analysis chat runner is required")
	}
	return &Service{
		dataDir:  dataDir,
		runner:   runner,
		opts:     opts.normalized(),
		sessions: map[string]*session{},
	}, nil
}

// Create resolves an analysis snapshot and starts an owner-bound session.
func (s *Service) Create(ref AnalysisRef, owner string) (SessionView, error) {
	owner = normalizeOwner(owner)
	if owner == "" {
		return SessionView{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	now := s.opts.Now().UTC()
	s.mu.Lock()
	s.evictExpiredLocked(now)
	limitReached := len(s.sessions) >= s.opts.MaxSessions || s.ownerSessionsLocked(owner) >= s.opts.MaxSessionsPerOwner
	s.mu.Unlock()
	if limitReached {
		return SessionView{}, ErrSessionLimit
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
	created := &session{
		owner:    owner,
		resolved: resolved,
		expires:  expires,
		view: SessionView{
			ID:        id,
			Analysis:  resolved.ref,
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: expires.Format(time.RFC3339),
			Messages:  []Message{},
		},
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(now)
	if len(s.sessions) >= s.opts.MaxSessions || s.ownerSessionsLocked(owner) >= s.opts.MaxSessionsPerOwner {
		return SessionView{}, ErrSessionLimit
	}
	s.sessions[id] = created
	return cloneSessionView(created.view), nil
}

// Get returns an owner-bound session.
func (s *Service) Get(id, owner string) (SessionView, error) {
	now := s.opts.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(now)
	current := s.sessions[id]
	if current == nil || current.owner != normalizeOwner(owner) {
		return SessionView{}, ErrSessionNotFound
	}
	return cloneSessionView(current.view), nil
}

// Send appends one question and runs a single serialized model turn.
func (s *Service) Send(ctx context.Context, id, owner, question string) (SessionView, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > s.opts.MaxQuestionBytes {
		return SessionView{}, fmt.Errorf("%w: question must be 1-%d bytes", ErrInvalidRequest, s.opts.MaxQuestionBytes)
	}
	owner = normalizeOwner(owner)
	now := s.opts.Now().UTC()

	s.mu.Lock()
	s.evictExpiredLocked(now)
	current := s.sessions[id]
	if current == nil || current.owner != owner {
		s.mu.Unlock()
		return SessionView{}, ErrSessionNotFound
	}
	if current.busy {
		s.mu.Unlock()
		return SessionView{}, ErrSessionBusy
	}
	if current.turns >= s.opts.MaxTurns {
		s.mu.Unlock()
		return SessionView{}, ErrTurnLimit
	}
	current.busy = true
	turn := Turn{
		SessionID:   id,
		JobID:       current.resolved.jobID,
		BuildPrefix: current.resolved.buildPrefix,
		Build:       cloneBuildInfo(current.resolved.build),
		TestCase:    cloneTestCase(current.resolved.testCase),
		History:     slices.Clone(current.view.Messages),
		Question:    question,
	}
	s.mu.Unlock()

	reply, runErr := s.runner.Reply(ctx, turn)

	s.mu.Lock()
	defer s.mu.Unlock()
	current = s.sessions[id]
	if current == nil || current.owner != owner {
		return SessionView{}, ErrSessionNotFound
	}
	current.busy = false
	if runErr != nil {
		return SessionView{}, runErr
	}
	stamp := s.opts.Now().UTC().Format(time.RFC3339)
	current.view.Messages = append(current.view.Messages,
		Message{Role: "user", Content: question, CreatedAt: stamp},
		Message{
			Role: "assistant", Content: reply.Answer, Assessment: reply.Assessment,
			Citations: slices.Clone(reply.Citations), ProposedRevision: cloneRevision(reply.ProposedRevision),
			ToolCalls: reply.ToolCalls, GCSBytes: reply.GCSBytes, ElapsedMs: reply.ElapsedMs,
			CreatedAt: stamp,
		},
	)
	current.turns++
	current.view.UpdatedAt = stamp
	return cloneSessionView(current.view), nil
}

func (s *Service) resolve(ref AnalysisRef) (resolvedAnalysis, error) {
	ref.JobID = strings.TrimSpace(ref.JobID)
	ref.BuildID = strings.TrimSpace(ref.BuildID)
	ref.TestName = strings.TrimSpace(ref.TestName)
	ref.SuiteName = strings.TrimSpace(ref.SuiteName)
	ref.ClassName = strings.TrimSpace(ref.ClassName)
	ref.JUnitFile = strings.TrimSpace(ref.JUnitFile)
	ref.AnalysisGeneratedAt = strings.TrimSpace(ref.AnalysisGeneratedAt)
	if ref.JobID == "" || ref.BuildID == "" || ref.TestName == "" {
		return resolvedAnalysis{}, fmt.Errorf("%w: job_id, build_id, and test_name are required", ErrInvalidRequest)
	}
	if len(ref.JobID) > maxJobIDBytes || len(ref.BuildID) > maxBuildIDBytes || len(ref.TestName) > maxTestNameBytes ||
		len(ref.SuiteName) > maxSuiteNameBytes || len(ref.ClassName) > maxClassNameBytes ||
		len(ref.JUnitFile) > maxJUnitFileBytes || len(ref.AnalysisGeneratedAt) > maxTimestampBytes {
		return resolvedAnalysis{}, fmt.Errorf("%w: analysis reference field exceeds its size limit", ErrInvalidRequest)
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

func (s *Service) ownerSessionsLocked(owner string) int {
	count := 0
	for _, current := range s.sessions {
		if current.owner == owner {
			count++
		}
	}
	return count
}

// evictExpiredLocked keeps an expired busy session until its in-flight turn returns.
// The next session access removes it and releases its capacity.
func (s *Service) evictExpiredLocked(now time.Time) {
	for id, current := range s.sessions {
		if !now.Before(current.expires) && !current.busy {
			delete(s.sessions, id)
		}
	}
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
