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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	stateVersion    = 2
	createVersion   = 2
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
	View                 SessionView                       `json:"view"`
	Owner                string                            `json:"owner"`
	Resolved             persistedResolvedAnalysis         `json:"resolved"`
	Turns                int                               `json:"turns"`
	ExpiresAt            time.Time                         `json:"expires_at"`
	CreateRequestID      string                            `json:"create_request_id"`
	CreateRequestHash    string                            `json:"create_request_hash"`
	CreateRequestVersion int                               `json:"create_request_version,omitempty"`
	Requests             map[string]persistedRequest       `json:"requests,omitempty"`
	Active               *persistedActiveTurn              `json:"active,omitempty"`
	Investigations       map[string]persistedInvestigation `json:"investigations,omitempty"`
}

type persistedResolvedAnalysis struct {
	Ref            AnalysisRef              `json:"ref"`
	JobID          string                   `json:"job_id"`
	BuildPrefix    string                   `json:"build_prefix"`
	Build          models.BuildInfo         `json:"build"`
	TestCase       models.TestCase          `json:"test_case"`
	Pattern        *models.PatternAnalysis  `json:"pattern,omitempty"`
	EvidenceBuilds []persistedArtifactBuild `json:"evidence_builds,omitempty"`
}

type persistedArtifactBuild struct {
	BuildPrefix string `json:"build_prefix"`
	BuildID     string `json:"build_id"`
	JobName     string `json:"job_name"`
}

type persistedRequest struct {
	QuestionHash string `json:"question_hash"`
	Question     string `json:"question,omitempty"`
	Status       string `json:"status"`
	FailureKind  string `json:"failure_kind,omitempty"`
	Turn         int    `json:"turn,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type persistedInvestigation struct {
	View          sourceinvestigation.View    `json:"view"`
	InputHash     string                      `json:"input_hash"`
	Subject       sourceinvestigation.Subject `json:"subject"`
	Revision      string                      `json:"revision,omitempty"`
	FailureKind   string                      `json:"failure_kind,omitempty"`
	LeaseID       string                      `json:"lease_id,omitempty"`
	LeaseExpires  time.Time                   `json:"lease_expires,omitempty"`
	CancelRequest bool                        `json:"cancel_requested,omitempty"`
}

type persistedActiveTurn struct {
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
	failureCitation   = "citation"
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
	migrated := false
	if state.Version == 1 {
		migrateStateV1(&state)
		migrated = true
	}
	if state.Version == 3 {
		migrateStateV3(&state)
		migrated = true
	}
	if state.Version != stateVersion {
		return nil, false, fmt.Errorf("unsupported analysis chat state version %d", state.Version)
	}
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

func migrateStateV1(state *persistedState) {
	state.Version = stateVersion
	for _, session := range state.Sessions {
		if session == nil {
			continue
		}
		if session.View.Analysis.Scope == "" {
			session.View.Analysis.Scope = ScopeTest
		}
		if session.Resolved.Ref.Scope == "" {
			session.Resolved.Ref.Scope = ScopeTest
		}
		if session.CreateRequestVersion == 0 {
			session.CreateRequestVersion = 1
		}
	}
}

func migrateStateV3(state *persistedState) {
	state.Version = stateVersion
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
			if request.Turn == 0 {
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
		if request.Turn == 0 && session.Turns > 0 {
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

func persistResolved(resolved resolvedAnalysis, requiredRepo string) persistedResolvedAnalysis {
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
		}
	}
	return persistedResolvedAnalysis{
		Ref: resolved.ref, JobID: resolved.jobID, BuildPrefix: resolved.buildPrefix,
		Build: build, TestCase: testCase, Pattern: boundedPersistedPattern(resolved.pattern),
		EvidenceBuilds: persistArtifactBuilds(resolved.evidenceBuilds),
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
		Systemic: pattern.Systemic, Confidence: clampPersistedText(pattern.Confidence, 32),
		SharedRootCause: clampPersistedText(pattern.SharedRootCause, 32<<10),
		SharedBuilds:    boundedPersistedBuildIDs(pattern.SharedBuilds),
		SuggestedFix:    clampPersistedText(pattern.SuggestedFix, 16<<10),
		RelevantFiles:   boundedPersistedFiles(pattern.RelevantFiles),
		Summary:         clampPersistedText(pattern.Summary, 16<<10),
	}
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
	requiredFound := false
	requiredValid := true
	requiredRevision := ""
	keys := make([]string, 0, len(refs))
	for repo, value := range refs {
		if wanted != "" && strings.ToLower(strings.TrimSpace(repo)) == wanted {
			candidate, ok := exactRepoRevision(value)
			if !ok || requiredRevision != "" && requiredRevision != candidate {
				requiredValid = false
			}
			if ok && requiredRevision == "" {
				requiredRevision = candidate
			}
			requiredFound = true
			continue
		}
		keys = append(keys, repo)
	}
	slices.Sort(keys)
	limit := 20
	if requiredFound {
		limit--
	}
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make(map[string]string, len(keys)+1)
	for _, key := range keys {
		repo := strings.TrimSpace(key)
		revision := strings.TrimSpace(refs[key])
		if repo == "" || revision == "" || len(repo) > 512 || len(revision) > 256 {
			continue
		}
		out[repo] = revision
	}
	if requiredFound {
		if !requiredValid || requiredRevision == "" {
			requiredRevision = "ambiguous"
		}
		out[wanted] = requiredRevision
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

func clonePattern(pattern *models.PatternAnalysis) *models.PatternAnalysis {
	if pattern == nil {
		return nil
	}
	copy := clonePatternAnalyses([]models.PatternAnalysis{*pattern})[0]
	return &copy
}
