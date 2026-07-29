// Package fetchprogress persists safe aggregate fetch progress for operators.
package fetchprogress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	// SchemaVersion is the current private fetch status schema.
	SchemaVersion = 2
	// StatusDirectory is hidden from the public /data file server.
	StatusDirectory = ".fetch-status"
	// StatusFilename is the current fetch status snapshot.
	StatusFilename = "status.json"

	defaultWriteInterval     = time.Second
	defaultHeartbeatInterval = 45 * time.Second
)

// PassType identifies the kind of fetch pass.
type PassType string

const (
	PassOneShot          PassType = "one-shot"
	PassInitialWatch     PassType = "initial-watch"
	PassLightweightWatch PassType = "lightweight-watch"
	PassReconcile        PassType = "reconcile"
)

// Phase identifies the current fetch phase.
type Phase string

const (
	PhaseSetup            Phase = "setup"
	PhaseDiscovery        Phase = "discovery"
	PhaseArtifacts        Phase = "artifacts"
	PhaseAggregation      Phase = "aggregation"
	PhaseAnalysisPlanning Phase = "analysis-planning"
	PhaseAnalysis         Phase = "analysis"
	PhasePatterns         Phase = "patterns"
	PhasePublication      Phase = "publication"
	PhaseSideEffects      Phase = "side-effects"
	PhaseIdle             Phase = "idle"
	PhaseComplete         Phase = "complete"
	PhaseFailed           Phase = "failed"
	PhaseCancelled        Phase = "cancelled"
	PhaseInterrupted      Phase = "interrupted"
)

// Outcome is the current or terminal pass outcome.
type Outcome string

const (
	OutcomeRunning     Outcome = "running"
	OutcomeSucceeded   Outcome = "succeeded"
	OutcomeFailed      Outcome = "failed"
	OutcomeCancelled   Outcome = "cancelled"
	OutcomeInterrupted Outcome = "interrupted"
)

// FailureCategory is a safe error classification without raw error text.
type FailureCategory string

const (
	FailureNone        FailureCategory = ""
	FailureSetup       FailureCategory = "setup"
	FailureDiscovery   FailureCategory = "discovery"
	FailureArtifacts   FailureCategory = "artifacts"
	FailureAggregation FailureCategory = "aggregation"
	FailureAnalysis    FailureCategory = "analysis"
	FailurePatterns    FailureCategory = "patterns"
	FailurePublication FailureCategory = "publication"
	FailureSideEffects FailureCategory = "side-effects"
	FailureCancelled   FailureCategory = "cancelled"
	FailureInterrupted FailureCategory = "interrupted"
	FailureUnknown     FailureCategory = "unknown"
)

// StageState is the safe state of a compound phase.
type StageState string

const (
	StagePending   StageState = "pending"
	StageRunning   StageState = "running"
	StageCompleted StageState = "completed"
	StageSkipped   StageState = "skipped"
	StageFailed    StageState = "failed"
	StageCancelled StageState = "cancelled"
)

// JobProgress tracks aggregate job completion.
type JobProgress struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
}

// BuildProgress tracks reused and newly fetched builds.
type BuildProgress struct {
	Cached  int `json:"cached"`
	Fetched int `json:"fetched"`
}

// AnalysisProgress separates logical analyses from Task attempts.
type AnalysisProgress struct {
	LogicalTotal           int `json:"logical_total"`
	AcceptedCacheHits      int `json:"accepted_cache_hits"`
	NewWork                int `json:"new_work"`
	StaleWork              int `json:"stale_work"`
	Queued                 int `json:"queued"`
	Running                int `json:"running"`
	Completed              int `json:"completed"`
	Failed                 int `json:"failed"`
	Cancelled              int `json:"cancelled"`
	TaskAttempts           int `json:"task_attempts"`
	Retries                int `json:"retries"`
	ExistingTasksAdopted   int `json:"existing_tasks_adopted"`
	ResultsRetrieved       int `json:"results_retrieved"`
	ResultRetrievalRetries int `json:"result_retrieval_retries"`
}

// PatternFailureCategory is a privacy-safe final correlation failure class.
type PatternFailureCategory string

const (
	PatternFailureNone           PatternFailureCategory = ""
	PatternFailureJSON           PatternFailureCategory = "json"
	PatternFailureMissing        PatternFailureCategory = "missing"
	PatternFailureSchema         PatternFailureCategory = "schema"
	PatternFailureBuilds         PatternFailureCategory = "builds"
	PatternFailureAmbiguous      PatternFailureCategory = "ambiguous"
	PatternFailureRequestTimeout PatternFailureCategory = "request-timeout"
	PatternFailureRateLimited    PatternFailureCategory = "rate-limited"
	PatternFailureProvider5xx    PatternFailureCategory = "provider-5xx"
	PatternFailureProvider       PatternFailureCategory = "provider"
	PatternFailureCancelled      PatternFailureCategory = "cancelled"
	PatternFailureDeadline       PatternFailureCategory = "deadline"
	PatternFailureUnknown        PatternFailureCategory = "unknown"
	PatternFailureMultiple       PatternFailureCategory = "multiple"
)

// PatternProgress tracks bounded job-level correlation attempts.
type PatternProgress struct {
	Eligible        int                    `json:"eligible"`
	Completed       int                    `json:"completed"`
	Failed          int                    `json:"failed"`
	Attempts        int                    `json:"attempts"`
	Retries         int                    `json:"retries"`
	FailureCategory PatternFailureCategory `json:"failure_category,omitempty"`
}

// Status is the private, aggregate-only fetch progress snapshot.
type Status struct {
	SchemaVersion int      `json:"schema_version"`
	RunID         string   `json:"run_id"`
	PassID        string   `json:"pass_id"`
	PassType      PassType `json:"pass_type"`
	EngineVersion string   `json:"engine_version,omitempty"`
	Phase         Phase    `json:"phase"`

	RunStartedAt   time.Time `json:"run_started_at"`
	PassStartedAt  time.Time `json:"pass_started_at"`
	PhaseStartedAt time.Time `json:"phase_started_at"`
	LastProgressAt time.Time `json:"last_progress_at"`

	LastCheckedAt               *time.Time `json:"last_checked_at,omitempty"`
	LastSuccessfulPublicationAt *time.Time `json:"last_successful_publication_at,omitempty"`

	Outcome         Outcome         `json:"outcome"`
	FailureCategory FailureCategory `json:"failure_category,omitempty"`

	Jobs             JobProgress      `json:"jobs"`
	Builds           BuildProgress    `json:"builds"`
	Analyses         AnalysisProgress `json:"analyses"`
	Patterns         PatternProgress  `json:"patterns"`
	PhaseDurationsMS map[string]int64 `json:"phase_durations_ms,omitempty"`
	CurrentTasks     []TaskMapping    `json:"current_tasks,omitempty"`

	PatternPhase     StageState `json:"pattern_phase"`
	PublicationPhase StageState `json:"publication_phase"`
	SideEffectPhase  StageState `json:"side_effect_phase"`

	NextWatchAt     *time.Time `json:"next_watch_at,omitempty"`
	NextReconcileAt *time.Time `json:"next_reconcile_at,omitempty"`
}

// Path returns the private status path for a data directory.
func Path(dataDir string) string {
	return filepath.Join(dataDir, StatusDirectory, StatusFilename)
}

// Read loads and validates a status snapshot.
func Read(path string) (Status, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	var status Status
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decode fetch status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Status{}, errors.New("fetch status has trailing data")
	}
	if status.SchemaVersion != 1 && status.SchemaVersion != SchemaVersion {
		return Status{}, fmt.Errorf("unsupported fetch status schema %d", status.SchemaVersion)
	}
	if err := status.validate(); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (s Status) validate() error {
	if !validPassType(s.PassType) || !validPhase(s.Phase) || !validOutcome(s.Outcome) ||
		!validFailureCategory(s.FailureCategory) || !validStage(s.PatternPhase) ||
		!validStage(s.PublicationPhase) || !validStage(s.SideEffectPhase) {
		return errors.New("fetch status has unknown state")
	}
	if s.RunID == "" || s.PassID == "" || s.RunStartedAt.IsZero() || s.PassStartedAt.IsZero() || s.PhaseStartedAt.IsZero() || s.LastProgressAt.IsZero() {
		return errors.New("fetch status is incomplete")
	}
	if s.Jobs.Total < 0 || s.Jobs.Completed < 0 || s.Jobs.Completed > s.Jobs.Total ||
		s.Builds.Cached < 0 || s.Builds.Fetched < 0 ||
		s.Analyses.LogicalTotal < 0 || s.Analyses.Queued < 0 || s.Analyses.Running < 0 ||
		s.Analyses.Completed < 0 || s.Analyses.Failed < 0 || s.Analyses.Cancelled < 0 ||
		s.Analyses.AcceptedCacheHits < 0 || s.Analyses.NewWork < 0 || s.Analyses.StaleWork < 0 ||
		s.Analyses.TaskAttempts < 0 || s.Analyses.Retries < 0 || s.Analyses.ExistingTasksAdopted < 0 ||
		s.Analyses.ResultsRetrieved < 0 || s.Analyses.ResultRetrievalRetries < 0 ||
		s.Patterns.Eligible < 0 || s.Patterns.Completed < 0 || s.Patterns.Failed < 0 ||
		s.Patterns.Attempts < 0 || s.Patterns.Retries < 0 ||
		s.Patterns.Completed+s.Patterns.Failed > s.Patterns.Eligible || s.Patterns.Retries > s.Patterns.Attempts ||
		!validPatternFailureCategory(s.Patterns.FailureCategory) {
		return errors.New("fetch status has invalid counters")
	}
	accounted := s.Analyses.Queued + s.Analyses.Running + s.Analyses.Completed + s.Analyses.Failed + s.Analyses.Cancelled
	if accounted > s.Analyses.LogicalTotal || (s.Outcome != OutcomeRunning && accounted != s.Analyses.LogicalTotal) {
		return errors.New("fetch status has inconsistent analysis counters")
	}
	if len(s.CurrentTasks) > currentTaskLimit {
		return errors.New("fetch status has too many Task mappings")
	}
	for _, task := range s.CurrentTasks {
		if task.WorkItem == "" || task.TaskName == "" || task.Attempts < 0 {
			return errors.New("fetch status has invalid Task mapping")
		}
	}
	for _, duration := range s.PhaseDurationsMS {
		if duration < 0 {
			return errors.New("fetch status has invalid phase duration")
		}
	}
	return nil
}

func validPatternFailureCategory(value PatternFailureCategory) bool {
	switch value {
	case PatternFailureNone, PatternFailureJSON, PatternFailureMissing, PatternFailureSchema,
		PatternFailureBuilds, PatternFailureAmbiguous, PatternFailureRequestTimeout,
		PatternFailureRateLimited, PatternFailureProvider5xx, PatternFailureProvider,
		PatternFailureCancelled, PatternFailureDeadline, PatternFailureUnknown, PatternFailureMultiple:
		return true
	default:
		return false
	}
}

func validPassType(value PassType) bool {
	switch value {
	case PassOneShot, PassInitialWatch, PassLightweightWatch, PassReconcile:
		return true
	default:
		return false
	}
}

func validPhase(value Phase) bool {
	switch value {
	case PhaseSetup, PhaseDiscovery, PhaseArtifacts, PhaseAggregation, PhaseAnalysisPlanning,
		PhaseAnalysis, PhasePatterns, PhasePublication, PhaseSideEffects, PhaseIdle,
		PhaseComplete, PhaseFailed, PhaseCancelled, PhaseInterrupted:
		return true
	default:
		return false
	}
}

func validOutcome(value Outcome) bool {
	switch value {
	case OutcomeRunning, OutcomeSucceeded, OutcomeFailed, OutcomeCancelled, OutcomeInterrupted:
		return true
	default:
		return false
	}
}

func validFailureCategory(value FailureCategory) bool {
	switch value {
	case FailureNone, FailureSetup, FailureDiscovery, FailureArtifacts, FailureAggregation,
		FailureAnalysis, FailurePatterns, FailurePublication, FailureSideEffects,
		FailureCancelled, FailureInterrupted, FailureUnknown:
		return true
	default:
		return false
	}
}

func validStage(value StageState) bool {
	switch value {
	case StagePending, StageRunning, StageCompleted, StageSkipped, StageFailed, StageCancelled:
		return true
	default:
		return false
	}
}

// Write atomically writes a private status snapshot.
func Write(path string, status Status) error {
	return writePrivateJSON(path, status)
}

func writePrivateJSON(path string, value any) error {
	return statefile.WritePrivateJSONDurable(path, value)
}

type trackerOptions struct {
	now               func() time.Time
	newID             func() string
	write             func(string, Status) error
	writeHistory      func(string, History) error
	logf              func(string, ...any)
	writeInterval     time.Duration
	heartbeatInterval time.Duration
}

// Tracker coordinates concurrent progress updates and bounded persistence.
type Tracker struct {
	mu sync.Mutex

	path              string
	engineVersion     string
	runID             string
	runStartedAt      time.Time
	status            Status
	history           History
	phaseCompleted    bool
	publishedThisPass bool
	plannedTasks      map[string]bool
	taskAttempts      map[string]int
	taskAdopted       map[string]bool
	taskResults       map[string]bool
	cacheDisposition  map[string]string

	now               func() time.Time
	newID             func() string
	write             func(string, Status) error
	writeHistory      func(string, History) error
	logf              func(string, ...any)
	writeInterval     time.Duration
	heartbeatInterval time.Duration
	lastWriteAttempt  time.Time
	lastWrite         time.Time
	lastHeartbeat     time.Time
}

// New creates a tracker and marks a previously running pass interrupted.
func New(dataDir, engineVersion string) *Tracker {
	return newTracker(dataDir, engineVersion, trackerOptions{})
}

func newTracker(dataDir, engineVersion string, opts trackerOptions) *Tracker {
	if opts.now == nil {
		opts.now = func() time.Time { return time.Now().UTC() }
	}
	if opts.newID == nil {
		opts.newID = generateID
	}
	if opts.write == nil {
		opts.write = Write
	}
	if opts.writeHistory == nil {
		opts.writeHistory = WriteHistory
	}
	if opts.logf == nil {
		opts.logf = log.Printf
	}
	if opts.writeInterval <= 0 {
		opts.writeInterval = defaultWriteInterval
	}
	if opts.heartbeatInterval <= 0 {
		opts.heartbeatInterval = defaultHeartbeatInterval
	}
	t := &Tracker{
		path: Path(dataDir), engineVersion: engineVersion,
		now: opts.now, newID: opts.newID, write: opts.write, writeHistory: opts.writeHistory, logf: opts.logf,
		writeInterval: opts.writeInterval, heartbeatInterval: opts.heartbeatInterval,
		history: History{SchemaVersion: HistorySchemaVersion, Passes: []PassSummary{}},
	}
	if history, err := ReadHistory(HistoryPath(dataDir)); err == nil {
		t.history = history
	}
	t.recoverInterrupted()
	return t
}

func (t *Tracker) recoverInterrupted() {
	previous, err := Read(t.path)
	if err != nil {
		return
	}
	t.status = previous
	if previous.Outcome != OutcomeRunning {
		t.phaseCompleted = true
		t.appendHistoryLocked(previous.PhaseStartedAt)
		return
	}
	now := t.now()
	priorPhase := previous.Phase
	previous.SchemaVersion = SchemaVersion
	if previous.PhaseDurationsMS == nil {
		previous.PhaseDurationsMS = map[string]int64{}
	}
	previous.PhaseDurationsMS[string(priorPhase)] += durationMilliseconds(now.Sub(previous.PhaseStartedAt))
	previous.Phase = PhaseInterrupted
	previous.Outcome = OutcomeInterrupted
	previous.FailureCategory = FailureInterrupted
	previous.PhaseStartedAt = now
	previous.LastProgressAt = now
	previous.Analyses.Cancelled += previous.Analyses.Queued + previous.Analyses.Running
	previous.Analyses.Queued = 0
	previous.Analyses.Running = 0
	if previous.PatternPhase == StageRunning {
		previous.PatternPhase = StageCancelled
	}
	if previous.PublicationPhase == StageRunning {
		previous.PublicationPhase = StageCancelled
	}
	if previous.SideEffectPhase == StageRunning {
		previous.SideEffectPhase = StageCancelled
	}
	t.status = previous
	if err := t.write(t.path, previous); err != nil {
		t.logPersistenceFailure("status", err)
		return
	}
	t.lastWrite = now
	t.phaseCompleted = true
	t.appendHistoryLocked(now)
	t.logf("fetch pass interrupted: pass=%s phase=%s", previous.PassType, previous.Phase)
}

// StartPass resets pass-local counters while retaining freshness timestamps.
func (t *Tracker) StartPass(passType PassType) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.runID == "" {
		t.runID = t.newID()
		t.runStartedAt = now
	}
	t.status = Status{
		SchemaVersion: SchemaVersion,
		RunID:         t.runID, PassID: t.newID(), PassType: passType, EngineVersion: t.engineVersion,
		Phase: PhaseSetup, RunStartedAt: t.runStartedAt, PassStartedAt: now,
		PhaseStartedAt: now, LastProgressAt: now, Outcome: OutcomeRunning,
		LastCheckedAt:               t.status.LastCheckedAt,
		LastSuccessfulPublicationAt: t.status.LastSuccessfulPublicationAt,
		PatternPhase:                StagePending, PublicationPhase: StagePending, SideEffectPhase: StagePending,
		PhaseDurationsMS: map[string]int64{}, CurrentTasks: []TaskMapping{},
	}
	t.phaseCompleted = false
	t.publishedThisPass = false
	t.plannedTasks = map[string]bool{}
	t.taskAttempts = map[string]int{}
	t.taskAdopted = map[string]bool{}
	t.taskResults = map[string]bool{}
	t.cacheDisposition = map[string]string{}
	t.lastHeartbeat = now
	t.persistLocked(true)
	t.logPhaseStartedLocked()
}

// StartPhase begins a new phase and forces a status write.
func (t *Tracker) StartPhase(phase Phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.recordPhaseDurationLocked(now)
	t.status.Phase = phase
	t.status.PhaseStartedAt = now
	t.status.LastProgressAt = now
	t.phaseCompleted = false
	switch phase {
	case PhasePatterns:
		t.status.PatternPhase = StageRunning
	case PhasePublication:
		t.status.PublicationPhase = StageRunning
	case PhaseSideEffects:
		t.status.SideEffectPhase = StageRunning
	}
	t.lastHeartbeat = now
	t.persistLocked(true)
	t.logPhaseStartedLocked()
}

// CompletePhase records a phase boundary and writes the latest counters.
func (t *Tracker) CompletePhase() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.recordPhaseDurationLocked(now)
	t.status.LastProgressAt = now
	switch t.status.Phase {
	case PhasePatterns:
		if t.status.PatternPhase == StageRunning {
			t.status.PatternPhase = StageCompleted
		}
	case PhasePublication:
		if t.status.PublicationPhase == StageRunning {
			t.status.PublicationPhase = StageCompleted
		}
	case PhaseSideEffects:
		if t.status.SideEffectPhase == StageRunning {
			t.status.SideEffectPhase = StageCompleted
		}
	}
	t.persistLocked(true)
	t.logPhaseCompletedLocked(now.Sub(t.status.PhaseStartedAt))
}

// SetJobs initializes the artifact counters for a pass.
func (t *Tracker) SetJobs(total int) {
	t.update(false, func(status *Status) {
		status.Jobs = JobProgress{Total: total}
		status.Builds = BuildProgress{}
	})
}

// FinishJob records one processed job and its build source counts.
func (t *Tracker) FinishJob(cached, fetched int) {
	t.update(false, func(status *Status) {
		if status.Jobs.Completed < status.Jobs.Total {
			status.Jobs.Completed++
		}
		status.Builds.Cached += cached
		status.Builds.Fetched += fetched
	})
}

// MarkChecked records when source artifacts were last checked.
func (t *Tracker) MarkChecked() {
	t.update(true, func(status *Status) {
		now := t.now()
		status.LastCheckedAt = &now
	})
}

// PlanAnalyses initializes logical analysis progress.
func (t *Tracker) PlanAnalyses(total int) {
	t.update(true, func(status *Status) {
		status.Analyses = AnalysisProgress{LogicalTotal: total, Queued: total}
	})
}

// StartAnalysis records one logical item moving from queued to running.
func (t *Tracker) StartAnalysis() {
	t.update(false, func(status *Status) {
		if status.Analyses.Queued > 0 {
			status.Analyses.Queued--
		}
		status.Analyses.Running++
	})
}

// FinishAnalysis records one terminal logical result.
func (t *Tracker) FinishAnalysis(outcome Outcome) {
	t.update(false, func(status *Status) {
		if status.Analyses.Running > 0 {
			status.Analyses.Running--
		}
		switch outcome {
		case OutcomeSucceeded:
			status.Analyses.Completed++
		case OutcomeCancelled, OutcomeInterrupted:
			status.Analyses.Cancelled++
		default:
			status.Analyses.Failed++
		}
	})
}

// CancelQueuedAnalyses moves unscheduled work to cancelled.
func (t *Tracker) CancelQueuedAnalyses() {
	t.update(true, func(status *Status) {
		status.Analyses.Cancelled += status.Analyses.Queued
		status.Analyses.Queued = 0
	})
}

// SkipAnalysis marks all planned analyses unavailable without running them.
func (t *Tracker) SkipAnalysis() {
	t.update(true, func(status *Status) {
		status.Analyses.Failed += status.Analyses.Queued
		status.Analyses.Queued = 0
		status.PatternPhase = StageSkipped
	})
}

// PlanPatterns initializes the job-level correlation counters.
func (t *Tracker) PlanPatterns(total int) {
	t.update(true, func(status *Status) {
		status.Patterns = PatternProgress{Eligible: total}
	})
}

// RecordPatternAttempt records one bounded correlation attempt.
func (t *Tracker) RecordPatternAttempt(retry, succeeded, final bool, category PatternFailureCategory) {
	t.update(true, func(status *Status) {
		status.Patterns.Attempts++
		if retry {
			status.Patterns.Retries++
		}
		if succeeded {
			status.Patterns.Completed++
			return
		}
		if !final {
			return
		}
		status.Patterns.Failed++
		if !validPatternFailureCategory(category) || category == PatternFailureNone {
			category = PatternFailureUnknown
		}
		switch {
		case status.Patterns.FailureCategory == PatternFailureNone:
			status.Patterns.FailureCategory = category
		case status.Patterns.FailureCategory != category:
			status.Patterns.FailureCategory = PatternFailureMultiple
		}
	})
}

// SkipPatterns marks pattern analysis skipped.
func (t *Tracker) SkipPatterns() {
	t.update(true, func(status *Status) { status.PatternPhase = StageSkipped })
}

// SkipSideEffects marks side effects skipped.
func (t *Tracker) SkipSideEffects() {
	t.update(true, func(status *Status) { status.SideEffectPhase = StageSkipped })
}

// MarkPublished records a successful public snapshot publication.
func (t *Tracker) MarkPublished() {
	t.update(true, func(status *Status) {
		now := t.now()
		status.LastSuccessfulPublicationAt = &now
		status.PublicationPhase = StageCompleted
		t.publishedThisPass = true
	})
}

// SetSchedule records the next natural watch and reconcile times.
func (t *Tracker) SetSchedule(nextWatch, nextReconcile time.Time) {
	t.update(true, func(status *Status) {
		watch := nextWatch.UTC()
		reconcile := nextReconcile.UTC()
		status.NextWatchAt = &watch
		status.NextReconcileAt = &reconcile
	})
}

// FinishSuccess marks the pass complete or idle between watch passes.
func (t *Tracker) FinishSuccess(watch bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.recordPhaseDurationLocked(now)
	if watch {
		t.status.Phase = PhaseIdle
	} else {
		t.status.Phase = PhaseComplete
	}
	t.status.PhaseStartedAt = now
	t.status.LastProgressAt = now
	t.status.Outcome = OutcomeSucceeded
	t.status.FailureCategory = FailureNone
	t.persistLocked(true)
	t.appendHistoryLocked(now)
	t.logf("fetch pass completed: pass=%s jobs=%d/%d analyses=%d/%d duration=%s",
		t.status.PassType, t.status.Jobs.Completed, t.status.Jobs.Total,
		t.status.Analyses.Completed+t.status.Analyses.Failed+t.status.Analyses.Cancelled,
		t.status.Analyses.LogicalTotal, formatDuration(now.Sub(t.status.PassStartedAt)))
}

// FinishFailure marks the pass failed with a safe category.
func (t *Tracker) FinishFailure(category FailureCategory) {
	t.finishTerminal(PhaseFailed, OutcomeFailed, category)
}

// FinishCancelled marks the active pass cancelled.
func (t *Tracker) FinishCancelled() {
	t.finishTerminal(PhaseCancelled, OutcomeCancelled, FailureCancelled)
}

// CancelIfRunning marks an active pass cancelled without overwriting a failure.
func (t *Tracker) CancelIfRunning() {
	t.mu.Lock()
	running := t.status.Outcome == OutcomeRunning
	t.mu.Unlock()
	if running {
		t.FinishCancelled()
	}
}

func (t *Tracker) finishTerminal(phase Phase, outcome Outcome, category FailureCategory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.recordPhaseDurationLocked(now)
	t.status.Phase = phase
	t.status.PhaseStartedAt = now
	t.status.LastProgressAt = now
	t.status.Outcome = outcome
	t.status.FailureCategory = category
	t.status.Analyses.Cancelled += t.status.Analyses.Queued + t.status.Analyses.Running
	t.status.Analyses.Queued = 0
	t.status.Analyses.Running = 0
	if t.status.PatternPhase == StageRunning {
		t.status.PatternPhase = terminalStage(outcome)
	}
	if t.status.PublicationPhase == StageRunning {
		t.status.PublicationPhase = terminalStage(outcome)
	}
	if t.status.SideEffectPhase == StageRunning {
		t.status.SideEffectPhase = terminalStage(outcome)
	}
	t.persistLocked(true)
	t.appendHistoryLocked(now)
	t.logf("fetch pass ended: pass=%s outcome=%s category=%s duration=%s",
		t.status.PassType, outcome, category, formatDuration(now.Sub(t.status.PassStartedAt)))
}

func terminalStage(outcome Outcome) StageState {
	if outcome == OutcomeCancelled || outcome == OutcomeInterrupted {
		return StageCancelled
	}
	return StageFailed
}

// Heartbeat writes and logs one rate-limited long-phase heartbeat.
func (t *Tracker) Heartbeat() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.status.Outcome != OutcomeRunning || t.status.Phase == PhaseIdle ||
		t.status.Phase == PhaseComplete || t.status.Phase == PhaseFailed ||
		t.status.Phase == PhaseCancelled || t.status.Phase == PhaseInterrupted ||
		now.Sub(t.lastHeartbeat) < t.heartbeatInterval {
		return
	}
	t.status.LastProgressAt = now
	t.lastHeartbeat = now
	t.persistLocked(true)
	if t.status.Phase == PhaseAnalysis {
		done := t.status.Analyses.Completed + t.status.Analyses.Failed + t.status.Analyses.Cancelled
		t.logf("analysis progress: completed=%d/%d running=%d queued=%d retries=%d elapsed=%s",
			done, t.status.Analyses.LogicalTotal, t.status.Analyses.Running,
			t.status.Analyses.Queued, t.status.Analyses.Retries,
			formatDuration(now.Sub(t.status.PhaseStartedAt)))
		return
	}
	t.logf("fetch progress: phase=%s pass=%s jobs=%d/%d cached_builds=%d fetched_builds=%d elapsed=%s",
		t.status.Phase, t.status.PassType, t.status.Jobs.Completed, t.status.Jobs.Total,
		t.status.Builds.Cached, t.status.Builds.Fetched,
		formatDuration(now.Sub(t.status.PhaseStartedAt)))
}

// RunHeartbeats emits periodic heartbeats until ctx is cancelled.
func (t *Tracker) RunHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(t.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Heartbeat()
		}
	}
}

// Snapshot returns a concurrency-safe copy of the current status.
func (t *Tracker) Snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	snapshot := t.status
	snapshot.PhaseDurationsMS = maps.Clone(t.status.PhaseDurationsMS)
	snapshot.CurrentTasks = append([]TaskMapping(nil), t.status.CurrentTasks...)
	return snapshot
}

func (t *Tracker) update(force bool, mutate func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mutate(&t.status)
	t.status.LastProgressAt = t.now()
	t.persistLocked(force)
}

func (t *Tracker) persistLocked(force bool) {
	now := t.now()
	if !force && !t.lastWriteAttempt.IsZero() && now.Sub(t.lastWriteAttempt) < t.writeInterval {
		return
	}
	t.lastWriteAttempt = now
	if err := t.write(t.path, t.status); err != nil {
		t.logPersistenceFailure("status", err)
		return
	}
	t.lastWrite = now
}

func (t *Tracker) logPersistenceFailure(kind string, err error) {
	category := "storage error"
	switch {
	case errors.Is(err, os.ErrPermission):
		category = "permission denied"
	case errors.Is(err, os.ErrNotExist):
		category = "path unavailable"
	}
	t.logf("fetch progress %s write failed: %s", kind, category)
}

func (t *Tracker) logPhaseStartedLocked() {
	t.logf("fetch phase started: phase=%s pass=%s jobs=%d analyses=%d",
		t.status.Phase, t.status.PassType, t.status.Jobs.Total, t.status.Analyses.LogicalTotal)
}

func (t *Tracker) logPhaseCompletedLocked(duration time.Duration) {
	done := t.status.Analyses.Completed + t.status.Analyses.Failed + t.status.Analyses.Cancelled
	t.logf("fetch phase completed: phase=%s jobs=%d/%d cached_builds=%d fetched_builds=%d analyses=%d/%d duration=%s",
		t.status.Phase, t.status.Jobs.Completed, t.status.Jobs.Total,
		t.status.Builds.Cached, t.status.Builds.Fetched, done,
		t.status.Analyses.LogicalTotal, formatDuration(duration))
}

func (t *Tracker) recordPhaseDurationLocked(now time.Time) {
	if t.phaseCompleted || t.status.PhaseStartedAt.IsZero() || t.status.Outcome != OutcomeRunning {
		return
	}
	if t.status.PhaseDurationsMS == nil {
		t.status.PhaseDurationsMS = map[string]int64{}
	}
	t.status.PhaseDurationsMS[string(t.status.Phase)] += durationMilliseconds(now.Sub(t.status.PhaseStartedAt))
	t.phaseCompleted = true
}

func durationMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

var fallbackIDCounter atomic.Uint64

func generateID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%x%08x", time.Now().UnixNano(), fallbackIDCounter.Add(1))
}
