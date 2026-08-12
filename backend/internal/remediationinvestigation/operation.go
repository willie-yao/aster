package remediationinvestigation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

var (
	ErrOperationInvalid             = errors.New("invalid remediation investigation request")
	ErrOperationNotFound            = errors.New("remediation investigation subject not found")
	ErrOperationStale               = errors.New("remediation investigation subject is stale")
	ErrOperationInactive            = errors.New("remediation investigation subject is not active")
	ErrOperationRefreshRunning      = errors.New("dashboard refresh is in progress")
	ErrOperationUnavailable         = errors.New("remediation investigation is unavailable")
	ErrOperationIdempotencyConflict = errors.New("remediation investigation idempotency conflict")
)

// OperationRef binds an API operation to the exact causal group shown in the UI.
type OperationRef struct {
	JobID           string
	PatternID       string
	PatternHash     string
	CausalGroupID   string
	CausalGroupHash string
}

// ResolvedOperation contains the private frozen input and bounded readers for one run.
type ResolvedOperation struct {
	Input   FrozenInput
	Browser artifacts.Browser
	Source  sourceinvestigation.TreeReader
}

// OperationResolver validates current published identity and constructs frozen inputs.
type OperationResolver interface {
	Validate(context.Context, OperationRef) error
	Resolve(context.Context, OperationRef) (ResolvedOperation, error)
	RefreshActive() (bool, error)
}

// OperationOptions configure asynchronous causal remediation investigations.
type OperationOptions struct {
	Timeout       time.Duration
	MaxOperations int
	Now           func() time.Time
	UsageRecorder *aiusage.Recorder
}

func (o OperationOptions) normalized() OperationOptions {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxOperations <= 0 {
		o.MaxOperations = 256
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type operationRecord struct {
	identity  string
	cacheKey  string
	ref       OperationRef
	view      models.PatternRemediationInvestigationSummary
	running   bool
	updatedAt time.Time
}

type idempotencyRecord struct {
	identity string
	refresh  bool
}

// OperationService runs explicitly initiated investigations with one global active slot.
type OperationService struct {
	ctx      context.Context
	model    Model
	cache    *Cache
	resolver OperationResolver
	opts     OperationOptions
	active   chan struct{}

	mu          sync.Mutex
	byIdentity  map[string]*operationRecord
	byCacheKey  map[string]*operationRecord
	idempotency map[string]idempotencyRecord
	wg          sync.WaitGroup
}

func NewOperationService(ctx context.Context, model Model, cache *Cache, resolver OperationResolver, options OperationOptions) (*OperationService, error) {
	if ctx == nil || model == nil || cache == nil || resolver == nil {
		return nil, fmt.Errorf("remediation investigation operation dependencies are required")
	}
	options = options.normalized()
	return &OperationService{
		ctx: ctx, model: model, cache: cache, resolver: resolver, opts: options,
		active: make(chan struct{}, 1), byIdentity: map[string]*operationRecord{},
		byCacheKey: map[string]*operationRecord{}, idempotency: map[string]idempotencyRecord{},
	}, nil
}

// Start validates one exact current causal group and enqueues a bounded investigation.
func (s *OperationService) Start(ctx context.Context, ref OperationRef, owner, requestID string, refresh bool) (models.PatternRemediationInvestigationSummary, error) {
	ref, err := normalizeOperationRef(ref)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, err
	}
	owner, requestID = strings.TrimSpace(owner), strings.TrimSpace(requestID)
	if owner == "" || requestID == "" || len(owner) > 256 || len(requestID) > 256 {
		return models.PatternRemediationInvestigationSummary{}, ErrOperationInvalid
	}
	active, err := s.resolver.RefreshActive()
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, fmt.Errorf("%w: refresh state unavailable", ErrOperationUnavailable)
	}
	if active {
		return models.PatternRemediationInvestigationSummary{}, ErrOperationRefreshRunning
	}
	resolved, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, err
	}
	cacheKey, err := CacheKey(resolved.Input)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, fmt.Errorf("%w: frozen input rejected", ErrOperationUnavailable)
	}
	identity := operationIdentity(ref)
	idempotencyKey := owner + "\x00" + requestID

	s.mu.Lock()
	if previous, ok := s.idempotency[idempotencyKey]; ok {
		if previous.identity != identity || previous.refresh != refresh {
			s.mu.Unlock()
			return models.PatternRemediationInvestigationSummary{}, ErrOperationIdempotencyConflict
		}
		if record := s.byIdentity[identity]; record != nil {
			view := cloneOperationView(record.view)
			s.mu.Unlock()
			return view, nil
		}
	}
	if record := s.byCacheKey[cacheKey]; record != nil && (record.running || !refresh) {
		s.idempotency[idempotencyKey] = idempotencyRecord{identity: identity, refresh: refresh}
		view := cloneOperationView(record.view)
		s.mu.Unlock()
		return view, nil
	}
	now := s.opts.Now().UTC()
	record := &operationRecord{
		identity: identity, cacheKey: cacheKey, ref: ref, running: true, updatedAt: now,
		view: operationPhaseView(ref, models.PatternRemediationQueued),
	}
	s.byIdentity[identity] = record
	s.byCacheKey[cacheKey] = record
	s.idempotency[idempotencyKey] = idempotencyRecord{identity: identity, refresh: refresh}
	s.pruneLocked()
	view := cloneOperationView(record.view)
	s.wg.Add(1)
	s.mu.Unlock()

	go s.run(record, resolved, refresh)
	return view, nil
}

// Get returns safe current state. Completed cache entries are reconstructed after restart.
func (s *OperationService) Get(ctx context.Context, ref OperationRef) (models.PatternRemediationInvestigationSummary, error) {
	ref, err := normalizeOperationRef(ref)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, err
	}
	if err := s.resolver.Validate(ctx, ref); err != nil {
		if errors.Is(err, ErrOperationStale) || errors.Is(err, ErrOperationInactive) {
			return operationStaleView(ref, s.opts.Now()), nil
		}
		return models.PatternRemediationInvestigationSummary{}, err
	}
	identity := operationIdentity(ref)
	s.mu.Lock()
	record := s.byIdentity[identity]
	if record != nil && record.running {
		view := cloneOperationView(record.view)
		s.mu.Unlock()
		return view, nil
	}
	s.mu.Unlock()

	resolved, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrOperationStale) || errors.Is(err, ErrOperationInactive) {
			return operationStaleView(ref, s.opts.Now()), nil
		}
		return models.PatternRemediationInvestigationSummary{}, err
	}
	cacheKey, err := CacheKey(resolved.Input)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, ErrOperationUnavailable
	}
	if record != nil {
		if record.cacheKey != cacheKey {
			return operationStaleView(ref, s.opts.Now()), nil
		}
		return cloneOperationView(record.view), nil
	}
	entry, ok, err := s.cache.Lookup(cacheKey)
	if err != nil {
		return models.PatternRemediationInvestigationSummary{}, ErrOperationUnavailable
	}
	if !ok {
		return operationPhaseView(ref, models.PatternRemediationNotInvestigated), nil
	}
	verified, err := verifyOperationResult(ctx, resolved, entry)
	if err != nil {
		return operationPhaseView(ref, models.PatternRemediationNotInvestigated), nil
	}
	if err := s.validateBeforePublish(ctx, ref, resolved.Input); err != nil {
		return operationStaleView(ref, s.opts.Now()), nil
	}
	view := safeOperationView(ref, verified, entry.Provenance.CompletedAt)
	s.storeRecovered(identity, cacheKey, ref, view)
	return view, nil
}

func (s *OperationService) run(record *operationRecord, resolved ResolvedOperation, refresh bool) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Timeout)
	defer cancel()
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	case <-ctx.Done():
		s.finish(record, operationFailureView(record.ref, ctx.Err(), s.opts.Now()))
		return
	}

	s.updatePhase(record, models.PatternRemediationInvestigating)
	previous, previousOK, _ := s.cache.Lookup(record.cacheKey)
	var previousView *models.PatternRemediationInvestigationSummary
	if previousOK {
		if verified, verifyErr := verifyOperationResult(ctx, resolved, previous); verifyErr == nil {
			view := safeOperationView(record.ref, verified, previous.Provenance.CompletedAt)
			previousView = &view
		}
	}
	service, err := NewService(s.model, resolved.Source, s.cache, ServiceOptions{Timeout: s.opts.Timeout, Now: s.opts.Now, UsageRecorder: s.opts.UsageRecorder})
	if err != nil {
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()))
		return
	}
	run, err := service.Investigate(ctx, resolved.Input, resolved.Browser, refresh)
	if err != nil {
		if previousOK {
			s.updatePhase(record, models.PatternRemediationVerifying)
			recoveryCtx, recoveryCancel := context.WithTimeout(s.ctx, recoveryTimeout(s.opts.Timeout))
			defer recoveryCancel()
			if previousView == nil {
				if verified, verifyErr := verifyOperationResult(recoveryCtx, resolved, previous); verifyErr == nil {
					view := safeOperationView(record.ref, verified, previous.Provenance.CompletedAt)
					previousView = &view
				}
			}
			if previousView != nil {
				validationErr := s.validateBeforePublish(recoveryCtx, record.ref, resolved.Input)
				if errors.Is(validationErr, ErrOperationStale) || errors.Is(validationErr, ErrOperationInactive) {
					s.finish(record, operationStaleView(record.ref, s.opts.Now()))
					return
				}
				s.finish(record, *previousView)
				return
			}
		}
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()))
		return
	}

	s.updatePhase(record, models.PatternRemediationVerifying)
	verified, err := verifyOperationResult(ctx, resolved, run.Entry)
	if err != nil {
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()))
		return
	}
	if err := s.validateBeforePublish(ctx, record.ref, resolved.Input); err != nil {
		s.finish(record, operationStaleView(record.ref, s.opts.Now()))
		return
	}
	s.finish(record, safeOperationView(record.ref, verified, run.Entry.Provenance.CompletedAt))
}

func verifyOperationResult(ctx context.Context, resolved ResolvedOperation, entry CacheEntry) (VerifiedResult, error) {
	verifier, err := NewVerifier(resolved.Source)
	if err != nil {
		return VerifiedResult{}, err
	}
	return verifier.Verify(ctx, resolved.Input, entry, resolved.Browser)
}

func (s *OperationService) validateBeforePublish(parent context.Context, ref OperationRef, input FrozenInput) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := s.resolver.Validate(ctx, ref); err != nil {
		return err
	}
	resolved, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if FrozenInputDigest(resolved.Input) != FrozenInputDigest(input) {
		return ErrOperationStale
	}
	return nil
}

func (s *OperationService) updatePhase(record *operationRecord, state models.PatternRemediationInvestigationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byCacheKey[record.cacheKey] != record {
		return
	}
	record.view = operationPhaseView(record.ref, state)
	record.updatedAt = s.opts.Now().UTC()
}

func (s *OperationService) finish(record *operationRecord, view models.PatternRemediationInvestigationSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byCacheKey[record.cacheKey] != record {
		return
	}
	record.running = false
	record.view = cloneOperationView(view)
	record.updatedAt = s.opts.Now().UTC()
}

func (s *OperationService) storeRecovered(identity, cacheKey string, ref OperationRef, view models.PatternRemediationInvestigationSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := &operationRecord{identity: identity, cacheKey: cacheKey, ref: ref, view: cloneOperationView(view), updatedAt: s.opts.Now().UTC()}
	s.byIdentity[identity] = record
	s.byCacheKey[cacheKey] = record
	s.pruneLocked()
}

func (s *OperationService) pruneLocked() {
	for len(s.byIdentity) > s.opts.MaxOperations {
		var oldest *operationRecord
		for _, record := range s.byIdentity {
			if record.running {
				continue
			}
			if oldest == nil || record.updatedAt.Before(oldest.updatedAt) {
				oldest = record
			}
		}
		if oldest == nil {
			return
		}
		delete(s.byIdentity, oldest.identity)
		if s.byCacheKey[oldest.cacheKey] == oldest {
			delete(s.byCacheKey, oldest.cacheKey)
		}
		for key, value := range s.idempotency {
			if value.identity == oldest.identity {
				delete(s.idempotency, key)
			}
		}
	}
}

// Wait waits for queued or active investigations to stop.
func (s *OperationService) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func recoveryTimeout(operationTimeout time.Duration) time.Duration {
	timeout := operationTimeout / 5
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	return timeout
}

func normalizeOperationRef(ref OperationRef) (OperationRef, error) {
	ref.JobID = strings.TrimSpace(ref.JobID)
	ref.PatternID = strings.TrimSpace(ref.PatternID)
	ref.PatternHash = strings.ToLower(strings.TrimSpace(ref.PatternHash))
	ref.CausalGroupID = strings.TrimSpace(ref.CausalGroupID)
	ref.CausalGroupHash = strings.ToLower(strings.TrimSpace(ref.CausalGroupHash))
	for _, value := range []string{ref.JobID, ref.PatternID, ref.PatternHash, ref.CausalGroupID, ref.CausalGroupHash} {
		if value == "" || len(value) > 1024 {
			return OperationRef{}, ErrOperationInvalid
		}
	}
	return ref, nil
}

func operationIdentity(ref OperationRef) string {
	return strings.Join([]string{ref.JobID, ref.PatternID, ref.PatternHash, ref.CausalGroupID, ref.CausalGroupHash}, "\x00")
}

func operationPhaseView(ref OperationRef, state models.PatternRemediationInvestigationState) models.PatternRemediationInvestigationSummary {
	reason := ""
	switch state {
	case models.PatternRemediationNotInvestigated:
		reason = "No source-grounded implementation target has been verified for this recurring cause."
	case models.PatternRemediationQueued:
		reason = "The read-only remediation investigation is queued."
	case models.PatternRemediationInvestigating:
		reason = "The dashboard is inspecting frozen build evidence and pinned source."
	case models.PatternRemediationVerifying:
		reason = "The dashboard is independently verifying the proposed target."
	}
	return models.PatternRemediationInvestigationSummary{
		CausalGroupID: ref.CausalGroupID, CausalGroupHash: ref.CausalGroupHash, State: state, Reason: reason,
	}
}

func operationFailureView(ref OperationRef, err error, now time.Time) models.PatternRemediationInvestigationSummary {
	reason := "The read-only investigation failed. Published causal analysis is unchanged."
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "The read-only investigation timed out. Published causal analysis is unchanged."
	} else if errors.Is(err, context.Canceled) {
		reason = "The read-only investigation was cancelled. Published causal analysis is unchanged."
	}
	return models.PatternRemediationInvestigationSummary{
		CausalGroupID: ref.CausalGroupID, CausalGroupHash: ref.CausalGroupHash,
		State: models.PatternRemediationInvestigationFailed, Reason: reason,
		CompletedAt: now.UTC().Format(time.RFC3339),
	}
}

func operationStaleView(ref OperationRef, now time.Time) models.PatternRemediationInvestigationSummary {
	return models.PatternRemediationInvestigationSummary{
		CausalGroupID: ref.CausalGroupID, CausalGroupHash: ref.CausalGroupHash,
		State:       models.PatternRemediationStale,
		Reason:      "The displayed recurring cause is no longer the current active causal group. Refresh the dashboard before investigating again.",
		CompletedAt: now.UTC().Format(time.RFC3339),
	}
}

func safeOperationView(ref OperationRef, result VerifiedResult, completedAt string) models.PatternRemediationInvestigationSummary {
	state := models.PatternRemediationInsufficientEvidence
	reason := "The investigation could not verify one unambiguous implementation target."
	switch result.Classification {
	case ClassificationActionable:
		state = models.PatternRemediationActionable
		reason = "A source-grounded implementation target passed deterministic verification."
	case ClassificationAlreadyFixed:
		state = models.PatternRemediationAlreadyFixed
		reason = "Current source already contains the deterministically verified remediation."
	case ClassificationExternalDependency:
		state = models.PatternRemediationExternalDependency
		reason = "The recurring cause was verified outside the allowed destination repository."
	case ClassificationEnvironmentOrInfrastructure:
		state = models.PatternRemediationEnvironmentOrInfrastructure
		reason = "The recurring cause does not resolve to a verified repository change."
	case ClassificationMitigationOnly:
		state = models.PatternRemediationMitigationOnly
		reason = "The available response is an operational mitigation, not a durable implementation target."
	case ClassificationAmbiguous:
		reason = "Multiple distinct implementation targets passed deterministic verification, so no action is eligible."
	}
	view := models.PatternRemediationInvestigationSummary{
		CausalGroupID: ref.CausalGroupID, CausalGroupHash: ref.CausalGroupHash,
		State: state, Reason: reason, CompletedAt: completedAt,
	}
	if state == models.PatternRemediationActionable && result.Proposal != nil {
		target := result.Proposal.Target
		view.Target = &models.PatternRemediationTargetSummary{
			Kind:       string(result.Proposal.TargetKind),
			Repository: result.Proposal.Repository.Owner + "/" + result.Proposal.Repository.Name,
			Revision:   result.Proposal.Repository.Revision, Path: target.Path,
			Symbol: target.Symbol, RequiredCall: target.RequiredCall,
			Job: target.Job, Container: target.Container, Name: target.Name, Value: target.Value,
		}
	}
	return view
}

func cloneOperationView(view models.PatternRemediationInvestigationSummary) models.PatternRemediationInvestigationSummary {
	if view.Target != nil {
		target := *view.Target
		view.Target = &target
	}
	return view
}
