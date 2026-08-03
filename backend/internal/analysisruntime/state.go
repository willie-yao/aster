package analysisruntime

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

const (
	// ContainerStateKeyEnv carries a base64-encoded 256-bit AES key.
	ContainerStateKeyEnv = "PROW_AI_STATE_KEY"
	// ContainerStateMarker prefixes encrypted cache and trace state.
	ContainerStateMarker = "PROW_AI_STATE_B64:"
	// ContainerTaskNamespaceEnv identifies the owning Orka Task.
	ContainerTaskNamespaceEnv = "PROW_AI_TASK_NAMESPACE"
	// ContainerTaskNameEnv identifies the owning Orka Task.
	ContainerTaskNameEnv = "PROW_AI_TASK_NAME"
	// ContainerContractVersionEnv identifies the analyzer contract.
	ContainerContractVersionEnv = "PROW_AI_CONTRACT_VERSION"
	ContainerStateVersion       = 2
	containerStateAAD           = "prow-ai-container-state-v2"
	maxContainerStateBytes      = 512 << 10
	maxContainerCacheSeedBytes  = 48 << 10
)

// ContainerAnalysisState is the encrypted Task-to-dashboard state delta.
type ContainerAnalysisState struct {
	Version       int                      `json:"version"`
	TaskNamespace string                   `json:"task_namespace"`
	TaskName      string                   `json:"task_name"`
	CacheKey      string                   `json:"cache_key"`
	CacheEntries  map[string]ai.CacheEntry `json:"cache_entries,omitempty"`
	Traces        []ai.AnalysisTrace       `json:"traces,omitempty"`
}

// ContainerStateIdentity binds encrypted state to one Task and failure.
type ContainerStateIdentity struct {
	TaskNamespace string
	TaskName      string
	CacheKey      string
}

type containerStateDecryptionError struct{ err error }

func (e *containerStateDecryptionError) Error() string { return e.err.Error() }
func (e *containerStateDecryptionError) Unwrap() error { return e.err }

type containerStateIdentityError struct{ err error }

func (e *containerStateIdentityError) Error() string { return e.err.Error() }
func (e *containerStateIdentityError) Unwrap() error { return e.err }

// IsContainerStateDecryptionError reports authenticated-state decryption failure.
func IsContainerStateDecryptionError(err error) bool {
	var target *containerStateDecryptionError
	return errors.As(err, &target)
}

// IsContainerStateIdentityError reports a decrypted Task or cache-key mismatch.
func IsContainerStateIdentityError(err error) bool {
	var target *containerStateIdentityError
	return errors.As(err, &target)
}

// NewContainerStateIdentity builds the expected encrypted-state identity.
func NewContainerStateIdentity(namespace, taskName string, request ai.FailureAnalysisRequest) ContainerStateIdentity {
	return ContainerStateIdentity{TaskNamespace: strings.TrimSpace(namespace), TaskName: strings.TrimSpace(taskName), CacheKey: FailureCacheKey(request)}
}

// FailureCacheKey returns the canonical cache key for one request.
func FailureCacheKey(request ai.FailureAnalysisRequest) string {
	request = CanonicalFailureAnalysisRequest(request)
	return ai.AgenticCacheKeyForGeneration("universal", request.CacheGeneration, request.JobID, request.Build.BuildID, request.TestCase.Name, request.TestCase.FailureMessage)
}

// LoadContainerCacheSeed loads only the cache entry relevant to one request.
func LoadContainerCacheSeed(dataDir string, request ai.FailureAnalysisRequest) map[string]ai.CacheEntry {
	entries := ai.NewCache(dataDir).Entries(FailureCacheKey(request))
	data, err := json.Marshal(entries)
	if err != nil || len(data) > maxContainerCacheSeedBytes {
		return nil
	}
	return entries
}

// RestoreContainerCache writes a bounded Task cache seed before runtime setup.
func RestoreContainerCache(dataDir string, request ai.FailureAnalysisRequest, entries map[string]ai.CacheEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := validateContainerCacheEntries(FailureCacheKey(request), entries); err != nil {
		return err
	}
	cache := ai.NewCache(dataDir)
	cache.Merge(entries)
	return cache.Save()
}

// SnapshotContainerAnalysisState captures one Task's cache entry and traces.
func SnapshotContainerAnalysisState(cache *ai.Cache, traces *ai.TraceStore, request ai.FailureAnalysisRequest, identity ContainerStateIdentity) (ContainerAnalysisState, error) {
	key := FailureCacheKey(request)
	if identity.CacheKey != key {
		return ContainerAnalysisState{}, fmt.Errorf("container state identity cache key mismatch")
	}
	state := ContainerAnalysisState{Version: ContainerStateVersion, TaskNamespace: identity.TaskNamespace, TaskName: identity.TaskName, CacheKey: key, CacheEntries: map[string]ai.CacheEntry{}, Traces: []ai.AnalysisTrace{}}
	if cache != nil {
		state.CacheEntries = cache.Entries(key)
	}
	if traces != nil {
		state.Traces = traces.Snapshot().Traces
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return ContainerAnalysisState{}, err
	}
	return state, nil
}

// ParseContainerStateKey validates one base64-encoded AES-256 key.
func ParseContainerStateKey(raw string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(raw))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be standard base64 for exactly 32 bytes", ContainerStateKeyEnv)
	}
	return key, nil
}

// EncryptContainerAnalysisState encrypts one bounded state delta.
func EncryptContainerAnalysisState(state ContainerAnalysisState, key []byte, identity ContainerStateIdentity) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("container state key must be 32 bytes")
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return "", err
	}
	if err := validateContainerStateIdentity(state, identity); err != nil {
		return "", err
	}
	plaintext, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal container analysis state: %w", err)
	}
	if len(plaintext) > maxContainerStateBytes {
		return "", fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("create container state nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, containerStateAssociatedData(identity))
	payload := append(nonce, sealed...)
	return base64.StdEncoding.EncodeToString(payload), nil
}

// DecryptContainerAnalysisState decrypts and validates one state payload.
func DecryptContainerAnalysisState(encoded string, key []byte, identity ContainerStateIdentity) (ContainerAnalysisState, error) {
	var state ContainerAnalysisState
	if len(key) != 32 {
		return state, fmt.Errorf("container state key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return state, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return state, err
	}
	encoded = strings.TrimSpace(encoded)
	maxPayload := maxContainerStateBytes + gcm.NonceSize() + gcm.Overhead()
	if len(encoded) > base64.StdEncoding.EncodedLen(maxPayload) {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return state, fmt.Errorf("decode container analysis state: %w", err)
	}
	if len(payload) > maxPayload {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	if len(payload) < gcm.NonceSize() {
		return state, fmt.Errorf("container analysis state ciphertext is truncated")
	}
	plaintext, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], containerStateAssociatedData(identity))
	if err != nil {
		return state, &containerStateDecryptionError{err: fmt.Errorf("decrypt container analysis state: %w", err)}
	}
	if len(plaintext) > maxContainerStateBytes {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode container analysis state JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return state, fmt.Errorf("container analysis state contains multiple JSON values")
	} else if err != io.EOF {
		return state, fmt.Errorf("decode trailing container analysis state data: %w", err)
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return state, err
	}
	if err := validateContainerStateIdentity(state, identity); err != nil {
		return state, err
	}
	return state, nil
}

// WriteEncryptedContainerAnalysisState writes one encrypted state marker.
func WriteEncryptedContainerAnalysisState(w io.Writer, state ContainerAnalysisState, key []byte, identity ContainerStateIdentity) error {
	encoded, err := EncryptContainerAnalysisState(state, key, identity)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s%s\n", ContainerStateMarker, encoded); err != nil {
		return fmt.Errorf("write container analysis state: %w", err)
	}
	return nil
}

// ParseEncryptedContainerAnalysisState extracts the last valid encrypted marker.
func ParseEncryptedContainerAnalysisState(raw string, key []byte, identity ContainerStateIdentity) (ContainerAnalysisState, error) {
	var (
		state   ContainerAnalysisState
		found   bool
		lastErr error
	)
	for len(raw) > 0 {
		line, rest, cut := strings.Cut(raw, "\n")
		if cut {
			raw = rest
		} else {
			raw = ""
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ContainerStateMarker) {
			continue
		}
		parsed, err := DecryptContainerAnalysisState(strings.TrimSpace(strings.TrimPrefix(line, ContainerStateMarker)), key, identity)
		if err != nil {
			lastErr = err
			continue
		}
		state = parsed
		found = true
	}
	if found {
		return state, nil
	}
	if lastErr != nil {
		return state, fmt.Errorf("container analysis logs contain no valid state marker: %w", lastErr)
	}
	return state, fmt.Errorf("container analysis state marker is missing")
}

// ContainerStateStore merges Task deltas into shared cache and trace files.
type ContainerStateStore struct {
	mu      sync.Mutex
	dataDir string
	cache   *ai.Cache
	traces  *ai.TraceStore
	usage   *aiusage.Recorder
	staged  map[string]ai.CacheEntry
}

// NewContainerStateStore loads shared cache and trace state.
func NewContainerStateStore(dataDir string, usage ...*aiusage.Recorder) (*ContainerStateStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("container state data directory is required")
	}
	traces, err := ai.LoadTraceStore(filepath.Join(dataDir, output.AITraceFilename))
	if err != nil {
		return nil, err
	}
	store := &ContainerStateStore{dataDir: dataDir, cache: ai.NewCache(dataDir), traces: traces, staged: map[string]ai.CacheEntry{}}
	if len(usage) > 0 {
		store.usage = usage[0]
	}
	return store, nil
}

// CacheSeed returns the one cache entry relevant to a request.
func (s *ContainerStateStore) CacheSeed(request ai.FailureAnalysisRequest) map[string]ai.CacheEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.cache.Entries(FailureCacheKey(request))
	data, err := json.Marshal(entries)
	if err != nil || len(data) > maxContainerCacheSeedBytes {
		return nil
	}
	return entries
}

// AcceptCachedFailure evaluates one private cache entry under the current runtime contract.
func (s *ContainerStateStore) AcceptCachedFailure(ctx context.Context, httpClient *http.Client, request ai.FailureAnalysisRequest, planner *ai.Service) (ai.FailureAnalysisResult, ai.CacheRejectionReason, error) {
	if s == nil || s.cache == nil {
		return ai.FailureAnalysisResult{}, ai.CacheRejectedMissing, fmt.Errorf("container state store is required")
	}
	if planner == nil {
		return ai.FailureAnalysisResult{}, ai.CacheRejectedMissing, fmt.Errorf("analysis reuse planner is required")
	}
	request = CanonicalFailureAnalysisRequest(request)
	policy, err := FailureCachePolicy(ctx, httpClient, request, planner)
	if err != nil {
		return ai.FailureAnalysisResult{}, ai.CacheRejectedMissing, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, reason := ai.LookupAgenticCache(s.cache, FailureCacheKey(request), policy)
	return result, reason, nil
}

// FailureCachePolicy returns the current policy for one canonical request.
func FailureCachePolicy(ctx context.Context, httpClient *http.Client, request ai.FailureAnalysisRequest, planner *ai.Service) (ai.AgenticCachePolicy, error) {
	if planner == nil {
		return ai.AgenticCachePolicy{}, fmt.Errorf("analysis reuse planner is required")
	}
	request = CanonicalFailureAnalysisRequest(request)
	run := models.BuildResult{BuildInfo: request.Build}
	testCase := request.TestCase
	return planner.FailureCachePolicy(ctx, httpClient, &run, &testCase, max(1, request.ConsecutiveFailures)), nil
}

// StageCacheEntry adds one validated entry for the next private checkpoint.
func (s *ContainerStateStore) StageCacheEntry(entry ai.CacheEntry) error {
	if s == nil || s.cache == nil {
		return fmt.Errorf("container state store is required")
	}
	entries := map[string]ai.CacheEntry{entry.Key: entry}
	if err := validateContainerCacheEntries(entry.Key, entries); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cache.StoreEntry(entry); err != nil {
		return fmt.Errorf("stage container cache entry: %w", err)
	}
	staged, ok := s.cache.Lookup(entry.Key)
	if !ok || !staged.CreatedAt.Equal(entry.CreatedAt) || !bytes.Equal(staged.Data, entry.Data) {
		return fmt.Errorf("container cache entry was not staged exactly")
	}
	entry.Data = append(json.RawMessage(nil), entry.Data...)
	s.staged[entry.Key] = entry
	return nil
}

// TraceStore returns the shared private trace store.
func (s *ContainerStateStore) TraceStore() *ai.TraceStore {
	if s == nil {
		return nil
	}
	return s.traces
}

// Save persists the current shared trace and cache generation.
func (s *ContainerStateStore) Save() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.traces.Save(filepath.Join(s.dataDir, output.AITraceFilename)); err != nil {
		return err
	}
	if err := s.cache.Save(); err != nil {
		return err
	}
	clear(s.staged)
	return nil
}

// MergeTraces persists authenticated traces without accepting cache entries.
func (s *ContainerStateStore) MergeTraces(state ContainerAnalysisState) error {
	if s == nil {
		return fmt.Errorf("container state store is required")
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	traces, err := ai.LoadTraceStore(filepath.Join(s.dataDir, output.AITraceFilename))
	if err != nil {
		return err
	}
	for _, trace := range state.Traces {
		traces.Upsert(trace)
	}
	if err := traces.Save(filepath.Join(s.dataDir, output.AITraceFilename)); err != nil {
		return err
	}
	for _, trace := range state.Traces {
		s.traces.Upsert(trace)
	}
	s.recordUsageLocked(state)
	return nil
}

// Merge persists one authenticated cache and trace delta.
func (s *ContainerStateStore) Merge(state ContainerAnalysisState) error {
	if s == nil {
		return fmt.Errorf("container state store is required")
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	traces, err := ai.LoadTraceStore(filepath.Join(s.dataDir, output.AITraceFilename))
	if err != nil {
		return err
	}
	cache := ai.NewCache(s.dataDir)
	cache.Merge(state.CacheEntries)
	for _, entry := range s.staged {
		if err := cache.StoreEntry(entry); err != nil {
			return fmt.Errorf("preserve staged container cache entry: %w", err)
		}
	}
	for _, trace := range state.Traces {
		traces.Upsert(trace)
	}
	if err := traces.Save(filepath.Join(s.dataDir, output.AITraceFilename)); err != nil {
		return err
	}
	if err := cache.Save(); err != nil {
		return err
	}
	s.cache = cache
	clear(s.staged)
	for _, trace := range state.Traces {
		s.traces.Upsert(trace)
	}
	s.recordUsageLocked(state)
	return nil
}

func (s *ContainerStateStore) recordUsageLocked(state ContainerAnalysisState) {
	if s.usage == nil {
		return
	}
	for _, trace := range state.Traces {
		completedAt := trace.RecordedAt
		if completedAt == "" {
			completedAt = trace.StartedAt
		}
		operation := aiusage.OperationUsage{
			ID:        state.TaskNamespace + "\x00" + state.TaskName + "\x00" + trace.StartedAt,
			LogicalID: state.CacheKey, Origin: aiusage.OriginAnalyzer, Feature: aiusage.FeatureFailureAnalysis,
			StartedAt: trace.StartedAt, CompletedAt: completedAt,
			Outcome:     aiusage.OutcomeSuccess,
			Correlation: aiusage.Correlation{JobID: trace.JobID, BuildID: trace.BuildID, TestName: trace.TestName},
		}
		switch trace.Outcome {
		case "ai_cache_hit", "build_cache_hit":
			operation.Outcome = aiusage.OutcomeCacheHit
		case "unavailable":
			operation.Outcome = aiusage.OutcomeUnavailable
		case "cancelled":
			operation.Outcome = aiusage.OutcomeCancelled
		case "error", "failed", "rejected":
			operation.Outcome = aiusage.OutcomeError
		}
		for _, event := range trace.Events {
			if event.Kind != "model_request" {
				continue
			}
			operation.ModelRequests++
			if event.UsageReported {
				operation.ReportedRequests++
				operation.InputTokens += int64(max(event.InputTokens, 0))
				operation.CachedInputTokens += int64(max(event.CachedInputTokens, 0))
				operation.OutputTokens += int64(max(event.OutputTokens, 0))
				operation.ReasoningTokens += int64(max(event.ReasoningTokens, 0))
			} else {
				operation.UnreportedRequests++
			}
		}
		s.usage.Record(operation)
	}
}

func containerStateAssociatedData(identity ContainerStateIdentity) []byte {
	return []byte(strings.Join([]string{containerStateAAD, identity.TaskNamespace, identity.TaskName, identity.CacheKey}, "\x00"))
}

func validateContainerStateIdentity(state ContainerAnalysisState, identity ContainerStateIdentity) error {
	if identity.TaskNamespace == "" || identity.TaskName == "" || identity.CacheKey == "" {
		return fmt.Errorf("container state identity is incomplete")
	}
	if state.TaskNamespace != identity.TaskNamespace || state.TaskName != identity.TaskName || state.CacheKey != identity.CacheKey {
		return &containerStateIdentityError{err: fmt.Errorf("container analysis state identity mismatch")}
	}
	return nil
}

func validateContainerCacheEntries(cacheKey string, entries map[string]ai.CacheEntry) error {
	if strings.TrimSpace(cacheKey) == "" {
		return fmt.Errorf("container analysis state cache key is required")
	}
	if len(entries) > 1 {
		return fmt.Errorf("container analysis state has too many cache entries")
	}
	for key, entry := range entries {
		if key != cacheKey || entry.Key != key || entry.CreatedAt.IsZero() || !json.Valid(entry.Data) {
			return fmt.Errorf("container analysis state has an invalid cache entry")
		}
	}
	return nil
}

func validateContainerAnalysisState(state ContainerAnalysisState) error {
	if state.Version != ContainerStateVersion {
		return fmt.Errorf("unsupported container analysis state version %d", state.Version)
	}
	if strings.TrimSpace(state.TaskNamespace) == "" || strings.TrimSpace(state.TaskName) == "" {
		return fmt.Errorf("container analysis state Task identity is required")
	}
	if err := validateContainerCacheEntries(state.CacheKey, state.CacheEntries); err != nil {
		return err
	}
	if len(state.Traces) > 4 {
		return fmt.Errorf("container analysis state has too many traces")
	}
	return nil
}

// RemoveContainerLocalState removes stale local files before seed restoration.
func RemoveContainerLocalState(dataDir string) error {
	var errs []error
	for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
