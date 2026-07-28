package fetchprogress

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStatus(now time.Time) Status {
	return Status{
		SchemaVersion: SchemaVersion,
		RunID:         "run", PassID: "pass", PassType: PassInitialWatch, EngineVersion: "sha-test",
		Phase: PhaseAnalysis, RunStartedAt: now.Add(-time.Minute), PassStartedAt: now.Add(-time.Minute),
		PhaseStartedAt: now.Add(-30 * time.Second), LastProgressAt: now,
		Outcome: OutcomeRunning, Jobs: JobProgress{Total: 2, Completed: 1},
		Builds:       BuildProgress{Cached: 3, Fetched: 4},
		Analyses:     AnalysisProgress{LogicalTotal: 2, Queued: 1, Running: 1},
		PatternPhase: StagePending, PublicationPhase: StagePending, SideEffectPhase: StagePending,
	}
}

func TestWriteAtomicPermissionsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	status := testStatus(now)
	if err := Write(path, status); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != status.RunID || got.Builds != status.Builds || got.Analyses != status.Analyses {
		t.Fatalf("round trip = %+v", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != StatusFilename {
		t.Fatalf("status directory entries = %v", entries)
	}
}

func TestWriteReadersNeverObservePartialJSON(t *testing.T) {
	path := Path(t.TempDir())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if err := Write(path, testStatus(now)); err != nil {
		t.Fatal(err)
	}
	var bad atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, err := Read(path); err != nil {
					bad.Add(1)
				}
			}
		}()
	}
	for i := range 100 {
		status := testStatus(now.Add(time.Duration(i) * time.Second))
		status.Builds.Fetched = i
		if err := Write(path, status); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("readers observed %d partial snapshots", bad.Load())
	}
}

func TestReadRejectsUnknownSchemaCorruptAndUnknownState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	cases := []string{
		`{"schema_version":99}`,
		`{"schema_version":1`,
		`{"schema_version":1,"run_id":"r","pass_id":"p","pass_type":"mystery","phase":"setup","run_started_at":"2026-07-28T00:00:00Z","pass_started_at":"2026-07-28T00:00:00Z","phase_started_at":"2026-07-28T00:00:00Z","last_progress_at":"2026-07-28T00:00:00Z","outcome":"running","pattern_phase":"pending","publication_phase":"pending","side_effect_phase":"pending","jobs":{"total":0,"completed":0},"builds":{"cached":0,"fetched":0},"analyses":{"logical_total":0,"queued":0,"running":0,"completed":0,"failed":0,"cancelled":0,"task_attempts":0,"retries":0}}`,
	}
	for _, body := range cases {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); err == nil {
			t.Fatalf("Read accepted %s", body)
		}
	}
}

func TestNewTrackerMarksRunningStatusInterrupted(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	status := testStatus(now.Add(-time.Minute))
	if err := Write(Path(dir), status); err != nil {
		t.Fatal(err)
	}
	logs := []string{}
	tracker := newTracker(dir, "sha-new", trackerOptions{
		now:  func() time.Time { return now },
		logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	got, err := Read(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != OutcomeInterrupted || got.Phase != PhaseInterrupted || got.FailureCategory != FailureInterrupted {
		t.Fatalf("recovered status = %+v", got)
	}
	if tracker.Snapshot().Outcome != OutcomeInterrupted || len(logs) != 1 {
		t.Fatalf("tracker snapshot=%+v logs=%v", tracker.Snapshot(), logs)
	}
}

func TestTrackerPhaseCountersAndTerminalOutcomes(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	current := base
	tracker := newTracker(t.TempDir(), "sha-test", trackerOptions{
		now:   func() time.Time { return current },
		newID: func() string { return fmt.Sprintf("id-%d", current.Unix()) },
		logf:  func(string, ...any) {},
	})
	tracker.StartPass(PassOneShot)
	current = current.Add(time.Second)
	tracker.CompletePhase()
	tracker.StartPhase(PhaseArtifacts)
	tracker.SetJobs(2)
	tracker.FinishJob(3, 1)
	tracker.FinishJob(2, 2)
	tracker.MarkChecked()
	tracker.CompletePhase()
	tracker.StartPhase(PhaseAnalysisPlanning)
	tracker.PlanAnalyses(3)
	tracker.CompletePhase()
	tracker.StartPhase(PhaseAnalysis)
	tracker.StartAnalysis()
	tracker.FinishAnalysis(OutcomeSucceeded)
	tracker.StartAnalysis()
	tracker.FinishAnalysis(OutcomeFailed)
	tracker.CancelQueuedAnalyses()
	tracker.CompletePhase()
	tracker.StartPhase(PhasePatterns)
	tracker.CompletePhase()
	tracker.StartPhase(PhasePublication)
	tracker.MarkPublished()
	tracker.CompletePhase()
	tracker.SkipSideEffects()
	tracker.FinishSuccess(false)
	status := tracker.Snapshot()
	if status.Phase != PhaseComplete || status.Outcome != OutcomeSucceeded {
		t.Fatalf("success status = %+v", status)
	}
	if status.Jobs != (JobProgress{Total: 2, Completed: 2}) || status.Builds != (BuildProgress{Cached: 5, Fetched: 3}) {
		t.Fatalf("fetch counters = jobs:%+v builds:%+v", status.Jobs, status.Builds)
	}
	if status.Analyses != (AnalysisProgress{LogicalTotal: 3, Completed: 1, Failed: 1, Cancelled: 1}) {
		t.Fatalf("analysis counters = %+v", status.Analyses)
	}
	if status.LastCheckedAt == nil || status.LastSuccessfulPublicationAt == nil || status.SideEffectPhase != StageSkipped {
		t.Fatalf("freshness/stages = %+v", status)
	}

	tracker.StartPass(PassOneShot)
	tracker.StartPhase(PhasePublication)
	tracker.FinishFailure(FailurePublication)
	if got := tracker.Snapshot(); got.Phase != PhaseFailed || got.Outcome != OutcomeFailed || got.PublicationPhase != StageFailed {
		t.Fatalf("failure status = %+v", got)
	}

	tracker.StartPass(PassOneShot)
	tracker.StartPhase(PhaseAnalysis)
	tracker.FinishCancelled()
	if got := tracker.Snapshot(); got.Phase != PhaseCancelled || got.Outcome != OutcomeCancelled {
		t.Fatalf("cancelled status = %+v", got)
	}
}

func TestTrackerBoundsWritesAndEmitsSafeHeartbeat(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	current := base
	var writes int
	var logs []string
	tracker := newTracker(t.TempDir(), "sha-test", trackerOptions{
		now:           func() time.Time { return current },
		newID:         func() string { return "safe-id" },
		write:         func(_ string, _ Status) error { writes++; return nil },
		logf:          func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		writeInterval: time.Minute, heartbeatInterval: 45 * time.Second,
	})
	tracker.StartPass(PassLightweightWatch)
	tracker.StartPhase(PhaseAnalysis)
	tracker.PlanAnalyses(2)
	initialWrites := writes
	tracker.StartAnalysis()
	tracker.FinishAnalysis(OutcomeSucceeded)
	if writes != initialWrites {
		t.Fatalf("progress writes = %d, want bounded at %d", writes, initialWrites)
	}
	current = current.Add(44 * time.Second)
	tracker.Heartbeat()
	if writes != initialWrites {
		t.Fatal("early heartbeat wrote status")
	}
	current = current.Add(time.Second)
	tracker.Heartbeat()
	if writes != initialWrites+1 {
		t.Fatalf("heartbeat writes = %d, want %d", writes, initialWrites+1)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"analysis progress:", "completed=1/2", "running=0", "queued=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q: %s", want, joined)
		}
	}
	for _, sensitive := range []string{"test-name", "/private/path", "token-value", "provider-body"} {
		if strings.Contains(joined, sensitive) {
			t.Fatalf("logs leaked %q: %s", sensitive, joined)
		}
	}
}

func TestTrackerWriteFailureDoesNotAbortOrExposePath(t *testing.T) {
	var logs []string
	tracker := newTracker("/private/sensitive/data", "sha-test", trackerOptions{
		now:   func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
		newID: func() string { return "safe-id" },
		write: func(path string, _ Status) error { return &os.PathError{Op: "open", Path: path, Err: os.ErrPermission} },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	tracker.StartPass(PassOneShot)
	tracker.SetJobs(1)
	tracker.FinishJob(0, 1)
	tracker.FinishSuccess(false)
	if got := tracker.Snapshot(); got.Outcome != OutcomeSucceeded || got.Jobs.Completed != 1 {
		t.Fatalf("status after write failure = %+v", got)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "permission denied") {
		t.Fatalf("write failure not logged safely: %s", joined)
	}
	if strings.Contains(joined, "/private/sensitive") {
		t.Fatalf("write failure exposed path: %s", joined)
	}
}

func TestReadMissingStatus(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestTrackerConcurrentUpdates(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tracker := newTracker(t.TempDir(), "sha-test", trackerOptions{
		now:           func() time.Time { return now },
		newID:         func() string { return "safe-id" },
		write:         func(string, Status) error { return nil },
		logf:          func(string, ...any) {},
		writeInterval: time.Hour,
	})
	tracker.StartPass(PassLightweightWatch)
	tracker.SetJobs(100)
	tracker.PlanAnalyses(100)
	tracker.StartPhase(PhaseAnalysis)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.FinishJob(1, 2)
			tracker.StartAnalysis()
			tracker.FinishAnalysis(OutcomeSucceeded)
		}()
	}
	wg.Wait()
	tracker.CompletePhase()
	status := tracker.Snapshot()
	if status.Jobs != (JobProgress{Total: 100, Completed: 100}) || status.Builds != (BuildProgress{Cached: 100, Fetched: 200}) {
		t.Fatalf("fetch counters = jobs:%+v builds:%+v", status.Jobs, status.Builds)
	}
	if status.Analyses != (AnalysisProgress{LogicalTotal: 100, Completed: 100}) {
		t.Fatalf("analysis counters = %+v", status.Analyses)
	}
}
