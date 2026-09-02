package analysischat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	stateVersion    = 4
	stateFileName   = "sessions.json"
	stateLockName   = "sessions.lock"
	maxStateBytes   = 64 << 20
	lockRetryPeriod = 10 * time.Millisecond
)

type persistedState struct {
	Version       int                          `json:"version"`
	Sessions      map[string]*persistedSession `json:"sessions"`
	OwnerRequests map[string][]time.Time       `json:"owner_requests,omitempty"`
}

type persistedSession struct {
	View              SessionView                       `json:"view"`
	Owner             string                            `json:"owner"`
	Resolved          persistedResolvedAnalysis         `json:"resolved"`
	Turns             int                               `json:"turns"`
	ExpiresAt         time.Time                         `json:"expires_at"`
	CreateRequestID   string                            `json:"create_request_id"`
	CreateRequestHash string                            `json:"create_request_hash"`
	Requests          map[string]persistedRequest       `json:"requests,omitempty"`
	FixSources        map[string]persistedTestFixSource `json:"fix_sources,omitempty"`
	Active            *persistedActiveTurn              `json:"active,omitempty"`
	Retired           bool                              `json:"retired,omitempty"`
	FixBaseExpiresAt  time.Time                         `json:"fix_base_expires_at,omitempty"`
}

type persistedResolvedAnalysis struct {
	Ref            AnalysisRef                    `json:"ref"`
	AnalysisHash   string                         `json:"analysis_hash,omitempty"`
	Source         sourceinvestigation.Repository `json:"source,omitempty"`
	JobID          string                         `json:"job_id"`
	BuildPrefix    string                         `json:"build_prefix"`
	Build          models.BuildInfo               `json:"build"`
	TestCase       models.TestCase                `json:"test_case"`
	Pattern        *models.PatternAnalysis        `json:"pattern,omitempty"`
	EvidenceBuilds []persistedArtifactBuild       `json:"evidence_builds,omitempty"`
	Comparison     *persistedCauseComparison      `json:"comparison,omitempty"`
	FixTarget      *persistedResolvedFixTarget    `json:"fix_target,omitempty"`
}

type persistedResolvedFixTarget struct {
	Ref          AnalysisRef                    `json:"ref"`
	AnalysisHash string                         `json:"analysis_hash"`
	Source       sourceinvestigation.Repository `json:"source"`
	Build        models.BuildInfo               `json:"build"`
	TestCase     models.TestCase                `json:"test_case"`
}

type persistedArtifactBuild struct {
	BuildPrefix string `json:"build_prefix"`
	BuildID     string `json:"build_id"`
	JobName     string `json:"job_name"`
}

type persistedCauseComparison struct {
	BuildPrefix string    `json:"build_prefix"`
	BuildID     string    `json:"build_id"`
	JobName     string    `json:"job_name"`
	Started     time.Time `json:"started,omitempty"`
	Passed      bool      `json:"passed"`
	Result      string    `json:"result,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Revision    string    `json:"revision,omitempty"`
	TestNames   []string  `json:"test_names,omitempty"`
}

type persistedRequest struct {
	Actor        string `json:"actor,omitempty"`
	QuestionHash string `json:"question_hash"`
	Question     string `json:"question,omitempty"`
	Status       string `json:"status"`
	FailureKind  string `json:"failure_kind,omitempty"`
	FailureGate  string `json:"failure_gate,omitempty"`
	Turn         int    `json:"turn,omitempty"`
	Prepared     bool   `json:"prepared,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type persistedTestFixSource struct {
	PendingReservations      map[string]bool   `json:"pending_reservations,omitempty"`
	References               map[string]bool   `json:"references,omitempty"`
	TargetRef                AnalysisRef       `json:"target_ref"`
	FailureRevision          string            `json:"failure_revision"`
	GenerationBaseRevision   string            `json:"generation_base_revision"`
	VerifiedSourceFileHashes map[string]string `json:"verified_source_file_hashes"`
}

type persistedActiveTurn struct {
	Actor             string    `json:"actor,omitempty"`
	RequestID         string    `json:"request_id"`
	Question          string    `json:"question,omitempty"`
	LeaseID           string    `json:"lease_id"`
	ExpiresAt         time.Time `json:"expires_at"`
	Phase             string    `json:"phase"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
	ValidationRetries int       `json:"validation_retries,omitempty"`
	CancelRequested   bool      `json:"cancel_requested,omitempty"`
}

const (
	requestPending   = "pending"
	requestSucceeded = "succeeded"
	requestFailed    = "failed"
	requestUnknown   = "unknown"

	failureModel      = "model"
	failureProvider   = "provider"
	failureValidation = "validation"
	failureTimeout    = "timeout"
	failureCancelled  = "cancelled"
	failureSource     = "source"
)

type sessionStore struct {
	statePath   string
	lockPath    string
	lockTimeout time.Duration
	local       chan struct{}
}

func newSessionStore(dir string, lockTimeout time.Duration) (*sessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating analysis chat state directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	return &sessionStore{
		statePath:   filepath.Join(dir, stateFileName),
		lockPath:    filepath.Join(dir, stateLockName),
		lockTimeout: lockTimeout,
		local:       make(chan struct{}, 1),
	}, nil
}

func (s *sessionStore) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.lockTimeout)
}

func (s *sessionStore) validate() error {
	ctx, cancel := s.context()
	defer cancel()
	return s.update(ctx, func(*persistedState) (bool, error) { return false, nil })
}

// update serializes a short state transition across local goroutines and server
// replicas. The callback's changes are saved even when it returns an operation
// error, so cleanup and terminal request outcomes are not lost.
func (s *sessionStore) update(ctx context.Context, fn func(*persistedState) (bool, error)) error {
	select {
	case s.local <- struct{}{}:
		defer func() { <-s.local }()
	case <-ctx.Done():
		return fmt.Errorf("locking local analysis chat state: %w", ctx.Err())
	}

	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening analysis chat state lock: %w", err)
	}
	defer lock.Close()
	_ = os.Chmod(s.lockPath, 0o600)

	if err := lockFile(ctx, lock); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	state, migrated, err := s.load()
	if err != nil {
		return err
	}
	changed, opErr := fn(state)
	if changed || migrated {
		if err := writePrivateJSON(s.statePath, state); err != nil {
			return fmt.Errorf("writing analysis chat state: %w", err)
		}
	}
	return opErr
}

func lockFile(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("locking analysis chat state: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("locking analysis chat state: %w", err)
		}
		timer := time.NewTimer(lockRetryPeriod)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("locking analysis chat state: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *sessionStore) load() (*persistedState, bool, error) {
	file, err := os.Open(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return freshPersistedState(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening analysis chat state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("reading analysis chat state: %w", err)
	}
	if len(data) > maxStateBytes {
		return nil, false, fmt.Errorf("analysis chat state exceeds %d bytes", maxStateBytes)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, false, fmt.Errorf("decoding analysis chat state: %w", err)
	}
	if state.Version != stateVersion {
		return nil, false, fmt.Errorf("unsupported analysis chat state version %d", state.Version)
	}
	migrated := false
	if state.Sessions == nil {
		state.Sessions = map[string]*persistedSession{}
	}
	if state.OwnerRequests == nil {
		state.OwnerRequests = map[string][]time.Time{}
	}
	if migrateRequestSummaries(&state) {
		migrated = true
	}
	return &state, migrated, nil
}

func sharedSessionNewer(candidateID string, candidate *persistedSession, currentID string, current *persistedSession) bool {
	if current == nil {
		return true
	}
	candidateActivity := persistedSessionActivity(candidate)
	currentActivity := persistedSessionActivity(current)
	if !candidateActivity.Equal(currentActivity) {
		return candidateActivity.After(currentActivity)
	}
	if !candidate.ExpiresAt.Equal(current.ExpiresAt) {
		return candidate.ExpiresAt.After(current.ExpiresAt)
	}
	return candidateID > currentID
}

func persistedSessionActivity(session *persistedSession) time.Time {
	if session == nil {
		return time.Time{}
	}
	activity, err := time.Parse(time.RFC3339, session.View.UpdatedAt)
	if err != nil {
		activity, _ = time.Parse(time.RFC3339, session.View.CreatedAt)
	}
	if session.Active != nil && session.Active.UpdatedAt.After(activity) {
		activity = session.Active.UpdatedAt
	}
	return activity
}

func migrateRequestSummaries(state *persistedState) bool {
	changed := false
	for _, session := range state.Sessions {
		if session == nil || len(session.Requests) == 0 {
			continue
		}
		turns := map[string]int{}
		for _, message := range session.View.Messages {
			requestID := strings.TrimSpace(message.RequestID)
			if requestID == "" {
				continue
			}
			if turns[requestID] == 0 {
				turns[requestID] = len(turns) + 1
			}
			request, ok := session.Requests[requestID]
			if !ok {
				continue
			}
			if request.Turn == 0 && !request.Prepared {
				request.Turn = turns[requestID]
				changed = true
			}
			if request.Question == "" && message.Role == "user" && strings.TrimSpace(message.Content) != "" {
				request.Question = message.Content
				changed = true
			}
			if request.CreatedAt == "" && message.CreatedAt != "" {
				request.CreatedAt = message.CreatedAt
				changed = true
			}
			if request.UpdatedAt == "" && message.CreatedAt != "" {
				request.UpdatedAt = message.CreatedAt
				changed = true
			}
			session.Requests[requestID] = request
		}
		if session.Active == nil {
			continue
		}
		request, ok := session.Requests[session.Active.RequestID]
		if !ok {
			continue
		}
		if request.Question == "" && session.Active.Question != "" {
			request.Question = session.Active.Question
			changed = true
		}
		if request.Turn == 0 && !request.Prepared && session.Turns > 0 {
			request.Turn = session.Turns
			changed = true
		}
		stamp := session.Active.UpdatedAt.UTC().Format(time.RFC3339)
		if request.CreatedAt == "" && !session.Active.UpdatedAt.IsZero() {
			request.CreatedAt = stamp
			changed = true
		}
		if request.UpdatedAt == "" && !session.Active.UpdatedAt.IsZero() {
			request.UpdatedAt = stamp
			changed = true
		}
		session.Requests[session.Active.RequestID] = request
	}
	return changed
}

func freshPersistedState() *persistedState {
	return &persistedState{
		Version: stateVersion, Sessions: map[string]*persistedSession{},
		OwnerRequests: map[string][]time.Time{},
	}
}

func writePrivateJSON(path string, value any) error {
	return writePrivateJSONLimit(path, value, maxStateBytes)
}

func writePrivateJSONLimit(path string, value any, maxBytes int) error {
	sync := func(file *os.File) error { return file.Sync() }
	return writePrivateJSONLimitWithSync(path, value, maxBytes, sync, sync)
}

func writePrivateJSONLimitWithSync(
	path string,
	value any,
	maxBytes int,
	syncFile func(*os.File) error,
	syncDir func(*os.File) error,
) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("analysis chat state exceeds %d bytes", maxBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// Some RWX filesystems use mount-level modes and reject chmod.
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := syncFile(tmp); err != nil {
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
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing analysis chat state directory: %w", err)
	}
	return nil
}

func persistResolved(resolved resolvedAnalysis, sourceRepo sourceinvestigation.Repository) persistedResolvedAnalysis {
	requiredRepo := sourceRepositoryName(sourceRepo)
	build := models.BuildInfo{
		BuildID:     resolved.build.BuildID,
		JobName:     resolved.build.JobName,
		Started:     resolved.build.Started,
		Finished:    resolved.build.Finished,
		Passed:      resolved.build.Passed,
		Result:      resolved.build.Result,
		Commit:      resolved.build.Commit,
		Revision:    resolved.build.Revision,
		RepoVersion: resolved.build.RepoVersion,
		RepoRefs:    boundedRepoRefs(resolved.build.RepoRefs, requiredRepo),
		PullNumber:  resolved.build.PullNumber,
		WebURL:      resolved.build.WebURL,
	}
	testCase := models.TestCase{
		Name:           resolved.testCase.Name,
		Source:         resolved.testCase.Source,
		SuiteName:      resolved.testCase.SuiteName,
		ClassName:      resolved.testCase.ClassName,
		JUnitFile:      resolved.testCase.JUnitFile,
		FailureMessage: clampPersistedText(resolved.testCase.FailureMessage, 12<<10),
		FailureBody:    clampPersistedText(resolved.testCase.FailureBody, 8<<10),
	}
	if analysis := resolved.testCase.AIAnalysis; analysis != nil {
		testCase.AIAnalysis = &models.AIAnalysis{
			GeneratedAt:   analysis.GeneratedAt,
			RootCause:     clampPersistedText(analysis.RootCause, 32<<10),
			Severity:      analysis.Severity,
			SuggestedFix:  clampPersistedText(analysis.SuggestedFix, 16<<10),
			RelevantFiles: boundedPersistedFiles(analysis.RelevantFiles),
			Disposition:   analysis.Disposition,
			// The warnings qualify the disposition, so dropping them would make a
			// contested diagnosis look usable to the session snapshot.
			DispositionWarnings: slices.Clone(analysis.DispositionWarnings),
		}
	}
	if source, ok := resolveBuildSourceRepository(resolved.build, sourceRepo); ok {
		sourceRepo = source
	} else {
		sourceRepo.Revision = ""
	}
	return persistedResolvedAnalysis{
		Ref: resolved.ref, AnalysisHash: models.TestAnalysisContentHash(resolved.testCase), Source: sourceRepo,
		JobID: resolved.jobID, BuildPrefix: resolved.buildPrefix,
		Build: build, TestCase: testCase, Pattern: boundedPersistedPattern(resolved.pattern),
		EvidenceBuilds: persistArtifactBuilds(resolved.evidenceBuilds),
		Comparison:     persistCauseComparison(resolved.comparison),
		FixTarget:      persistResolvedFixTarget(resolved.fixTarget, sourceRepo),
	}
}

func persistResolvedFixTarget(target *resolvedFixTarget, sourceRepo sourceinvestigation.Repository) *persistedResolvedFixTarget {
	if target == nil {
		return nil
	}
	persisted := persistResolved(resolvedAnalysis{
		ref: target.ref, build: target.build, testCase: target.testCase,
	}, sourceRepo)
	if sourceinvestigation.ValidateRepository(persisted.Source) != nil || persisted.AnalysisHash == "" {
		return nil
	}
	return &persistedResolvedFixTarget{
		Ref: persisted.Ref, AnalysisHash: persisted.AnalysisHash, Source: persisted.Source,
		Build: persisted.Build, TestCase: persisted.TestCase,
	}
}

func boundedPersistedPattern(pattern *models.PatternAnalysis) *models.PatternAnalysis {
	if pattern == nil {
		return nil
	}
	return &models.PatternAnalysis{
		ID: pattern.ID, ContentHash: pattern.ContentHash,
		Subject: clampPersistedText(pattern.Subject, 4<<10), JobID: clampPersistedText(pattern.JobID, maxJobIDBytes),
		GeneratedAt: clampPersistedText(pattern.GeneratedAt, maxTimestampBytes), BuildsAnalyzed: pattern.BuildsAnalyzed,
		Recurrence: pattern.Recurrence, CausalGroups: boundedPersistedCausalGroups(pattern.CausalGroups),
		UnclassifiedBuilds: boundedPersistedPatternBuildIDs(pattern.UnclassifiedBuilds, maxPatternChatUnclassifiedBuilds),
		Systemic:           pattern.Systemic, Confidence: clampPersistedText(pattern.Confidence, 32),
		SharedRootCause: clampPersistedText(pattern.SharedRootCause, 32<<10),
		SharedBuilds:    boundedPersistedBuildIDs(pattern.SharedBuilds),
		SuggestedFix:    clampPersistedText(pattern.SuggestedFix, 16<<10),
		RelevantFiles:   boundedPersistedFiles(pattern.RelevantFiles),
		Lifecycle:       boundedPersistedPatternLifecycle(pattern.Lifecycle),
		Summary:         clampPersistedText(pattern.Summary, 16<<10),
	}
}

func boundedPersistedCausalGroups(groups []models.PatternCausalGroup) []models.PatternCausalGroup {
	if len(groups) > maxPatternChatCausalGroups {
		groups = groups[:maxPatternChatCausalGroups]
	}
	out := make([]models.PatternCausalGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, models.PatternCausalGroup{
			ID: clampPersistedText(group.ID, maxPatternIDBytes), ContentHash: clampPersistedText(group.ContentHash, maxPatternHashBytes),
			Builds:    boundedPersistedPatternBuildIDs(group.Builds, maxPatternChatBuildsPerGroup),
			RootCause: clampPersistedText(group.RootCause, 8<<10), Confidence: clampPersistedText(group.Confidence, 32),
		})
	}
	return out
}

func boundedPersistedPatternLifecycle(lifecycle *models.PatternLifecycle) *models.PatternLifecycle {
	if lifecycle == nil {
		return nil
	}
	return &models.PatternLifecycle{
		State: lifecycle.State, Reason: clampPersistedText(lifecycle.Reason, 4<<10),
		RecoveryStreak: lifecycle.RecoveryStreak,
		RecoveryBuilds: boundedPersistedBuildIDs(lifecycle.RecoveryBuilds),
	}
}

func boundedPersistedPatternBuildIDs(builds []string, limit int) []string {
	if len(builds) > limit {
		builds = builds[:limit]
	}
	out := make([]string, 0, len(builds))
	for _, build := range builds {
		build = strings.TrimSpace(build)
		if build == "" {
			continue
		}
		out = append(out, clampPersistedText(build, maxBuildIDBytes))
	}
	return out
}

func boundedPersistedBuildIDs(builds []string) []string {
	if len(builds) > 50 {
		builds = builds[:50]
	}
	out := make([]string, 0, len(builds))
	for _, build := range builds {
		build = strings.TrimSpace(build)
		if build == "" {
			continue
		}
		if len(build) > maxBuildIDBytes {
			build = build[:maxBuildIDBytes]
		}
		out = append(out, build)
	}
	return out
}

func boundedRepoRefs(refs map[string]string, requiredRepo string) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	wanted := strings.ToLower(strings.TrimSpace(requiredRepo))
	var required, others []string
	for repo := range refs {
		if wanted != "" && strings.ToLower(strings.TrimSpace(repo)) == wanted {
			required = append(required, repo)
		} else {
			others = append(others, repo)
		}
	}
	slices.Sort(required)
	slices.Sort(others)
	if len(required) > 20 {
		return map[string]string{wanted: "ambiguous"}
	}
	keys := append(required, others...)
	if len(keys) > 20 {
		keys = keys[:20]
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		repo := strings.TrimSpace(key)
		revision := strings.TrimSpace(refs[key])
		if repo == "" || revision == "" || len(repo) > 512 || len(revision) > 256 {
			continue
		}
		out[repo] = revision
	}
	return out
}

func boundedPersistedFiles(files []string) []string {
	if len(files) > 50 {
		files = files[:50]
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		if len(file) > 1024 {
			file = file[:1024]
		}
		out = append(out, file)
	}
	return out
}

func clampPersistedText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	const marker = "\n...[content elided]...\n"
	if maxBytes <= len(marker) {
		return strings.ToValidUTF8(value[:maxBytes], "")
	}
	available := maxBytes - len(marker)
	head := available * 3 / 4
	tail := available - head
	return strings.ToValidUTF8(value[:head], "") + marker + strings.ToValidUTF8(value[len(value)-tail:], "")
}

func restoreResolved(resolved persistedResolvedAnalysis) resolvedAnalysis {
	return resolvedAnalysis{
		ref:            resolved.Ref,
		jobID:          resolved.JobID,
		buildPrefix:    resolved.BuildPrefix,
		build:          cloneBuildInfo(resolved.Build),
		testCase:       cloneTestCase(resolved.TestCase),
		pattern:        clonePattern(resolved.Pattern),
		evidenceBuilds: restoreArtifactBuilds(resolved.EvidenceBuilds),
		comparison:     restoreCauseComparison(resolved.Comparison),
		fixTarget:      restoreResolvedFixTarget(resolved.FixTarget),
	}
}

func restoreResolvedFixTarget(target *persistedResolvedFixTarget) *resolvedFixTarget {
	if target == nil {
		return nil
	}
	return &resolvedFixTarget{
		ref: target.Ref, build: cloneBuildInfo(target.Build), testCase: cloneTestCase(target.TestCase),
	}
}

func persistArtifactBuilds(builds []ArtifactBuild) []persistedArtifactBuild {
	out := make([]persistedArtifactBuild, 0, len(builds))
	for _, build := range builds {
		out = append(out, persistedArtifactBuild{
			BuildPrefix: build.BuildPrefix, BuildID: build.Build.BuildID, JobName: build.Build.JobName,
		})
	}
	return out
}

func restoreArtifactBuilds(builds []persistedArtifactBuild) []ArtifactBuild {
	out := make([]ArtifactBuild, 0, len(builds))
	for _, build := range builds {
		out = append(out, ArtifactBuild{
			BuildPrefix: build.BuildPrefix,
			Build:       models.BuildInfo{BuildID: build.BuildID, JobName: build.JobName},
		})
	}
	return out
}

func persistCauseComparison(comparison *CauseComparison) *persistedCauseComparison {
	if comparison == nil {
		return nil
	}
	build := comparison.ArtifactBuild.Build
	return &persistedCauseComparison{
		BuildPrefix: comparison.ArtifactBuild.BuildPrefix,
		BuildID:     build.BuildID,
		JobName:     build.JobName,
		Started:     build.Started,
		Passed:      build.Passed,
		Result:      build.Result,
		Commit:      build.Commit,
		Revision:    build.Revision,
		TestNames:   boundedPersistedCauseTestNames(comparison.TestNames),
	}
}

func restoreCauseComparison(comparison *persistedCauseComparison) *CauseComparison {
	if comparison == nil {
		return nil
	}
	return &CauseComparison{
		ArtifactBuild: ArtifactBuild{
			BuildPrefix: comparison.BuildPrefix,
			Build: models.BuildInfo{
				BuildID: comparison.BuildID, JobName: comparison.JobName, Started: comparison.Started,
				Passed: comparison.Passed, Result: comparison.Result, Commit: comparison.Commit, Revision: comparison.Revision,
			},
		},
		TestNames: slices.Clone(comparison.TestNames),
	}
}

func persistedCauseComparisonBuildID(comparison *persistedCauseComparison) string {
	if comparison == nil {
		return ""
	}
	return strings.TrimSpace(comparison.BuildID)
}

func boundedPersistedCauseTestNames(names []string) []string {
	if len(names) > maxPatternChatBuildsPerGroup {
		names = names[:maxPatternChatBuildsPerGroup]
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out = append(out, clampPersistedText(name, maxTestNameBytes))
		}
	}
	return out
}

func clonePattern(pattern *models.PatternAnalysis) *models.PatternAnalysis {
	if pattern == nil {
		return nil
	}
	copy := clonePatternAnalyses([]models.PatternAnalysis{*pattern})[0]
	return &copy
}
