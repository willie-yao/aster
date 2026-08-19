package remediationinvestigation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
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

// RecurrenceVerdict is one terminal answer for a recurring cause.
type RecurrenceVerdict struct {
	State models.PatternRemediationInvestigationState
	// Reason is the safe published explanation recorded with the verdict.
	Reason string
	// RecordedAt is when the answer was actually reached, RFC 3339 UTC. It is
	// surfaced as the completion time so a reused answer is not presented as a
	// fresh investigation, and it lets the ledger keep the later-completed of two
	// conclusions.
	RecordedAt string
}

// RecurrenceLedger is durable memory of prior terminal verdicts, keyed by a
// causal group's signature. Causal groups are recomputed from the current build
// window every pass, so without it a cause that returns after aging out is
// re-investigated at full model cost to reach an answer already on record.
type RecurrenceLedger interface {
	// ClaimReuse returns a prior answer and charges it against that verdict's
	// bounded reuse budget, so no conclusion answers indefinitely.
	ClaimReuse(signature string) (RecurrenceVerdict, bool, error)
	RecordVerdict(signature string, verdict RecurrenceVerdict) error
}

// OperationOptions configure asynchronous causal remediation investigations.
type OperationOptions struct {
	Timeout       time.Duration
	MaxOperations int
	Now           func() time.Time
	UsageRecorder *aiusage.Recorder
	// Ledger supplies recurrence memory. When nil, every request is investigated.
	Ledger RecurrenceLedger
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
	signature string
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
	now := s.opts.Now().UTC()
	signature := strings.TrimSpace(resolved.Input.Group.Signature)

	// Fast path: a request that only replays an idempotent result or joins an
	// in-flight run needs no cache, ledger, or validation work, and must not be
	// refused because one of those is unavailable.
	if view, done, err := s.joinExisting(idempotencyKey, identity, cacheKey, refresh); done {
		return view, err
	}

	// Resolved outside the mutex so neither the cache nor the ledger can stall
	// Start and Get behind file I/O. A cause that recurred after aging out of the
	// window gets a fresh cache key even though the question was already
	// answered, so recurrence memory answers it. An exact frozen-input cache
	// entry is a precise match for this input and always wins over a
	// signature-level one, so memory is only consulted on a true cache miss.
	var reused models.PatternRemediationInvestigationSummary
	reusable := false
	if !refresh && s.opts.Ledger != nil && signature != "" {
		if _, cached, lookupErr := s.cache.Lookup(cacheKey); lookupErr != nil || !cached {
			// A reused answer is published immediately, so it needs the same
			// current-subject check a completing run performs before publishing.
			// Running it before the claim avoids charging a reuse that is then
			// discarded.
			if err := s.validateBeforePublish(ctx, ref, resolved.Input); err != nil {
				if errors.Is(err, ErrOperationStale) || errors.Is(err, ErrOperationInactive) {
					return operationStaleView(ref, s.opts.Now()), nil
				}
				// A run would need this same validation to publish, so spending
				// model budget now would only reach the same failure.
				return models.PatternRemediationInvestigationSummary{}, fmt.Errorf("%w: publication state unavailable", ErrOperationUnavailable)
			}
			reused, reusable = s.claimReuse(ref, signature, now)
		}
	}
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
	if reusable {
		record := &operationRecord{
			identity: identity, cacheKey: cacheKey, signature: signature, ref: ref,
			view: cloneOperationView(reused), updatedAt: now,
		}
		s.byIdentity[identity] = record
		s.byCacheKey[cacheKey] = record
		s.idempotency[idempotencyKey] = idempotencyRecord{identity: identity, refresh: refresh}
		s.pruneLocked()
		s.mu.Unlock()
		return reused, nil
	}
	record := &operationRecord{
		identity: identity, cacheKey: cacheKey, signature: signature, ref: ref, running: true, updatedAt: now,
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
	s.storeRecovered(identity, cacheKey, strings.TrimSpace(resolved.Input.Group.Signature), ref, view)
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
		s.finish(record, operationFailureView(record.ref, ctx.Err(), s.opts.Now()), false)
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
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()), false)
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
					s.finish(record, operationStaleView(record.ref, s.opts.Now()), false)
					return
				}
				s.finish(record, *previousView, false)
				return
			}
		}
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()), false)
		return
	}

	s.updatePhase(record, models.PatternRemediationVerifying)
	verified, err := verifyOperationResult(ctx, resolved, run.Entry)
	if err != nil {
		s.finish(record, operationFailureView(record.ref, err, s.opts.Now()), false)
		return
	}
	if err := s.validateBeforePublish(ctx, record.ref, resolved.Input); err != nil {
		s.finish(record, operationStaleView(record.ref, s.opts.Now()), false)
		return
	}
	s.finish(record, safeOperationView(record.ref, verified, run.Entry.Provenance.CompletedAt), !run.CacheHit)
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

// finish publishes the terminal view, and records it in durable memory only when
// it came from a freshly computed, verified investigation. Recording a cache hit
// or a recovered previous result would reset the bounded reuse budget without any
// new work having happened, which would make the bound meaningless.
//
// The ledger write happens outside the mutex so a slow file write cannot stall
// Start and Get.
func (s *OperationService) finish(record *operationRecord, view models.PatternRemediationInvestigationSummary, investigated bool) {
	s.mu.Lock()
	published := s.byCacheKey[record.cacheKey] == record
	if published {
		record.running = false
		record.view = cloneOperationView(view)
		record.updatedAt = s.opts.Now().UTC()
	}
	s.mu.Unlock()
	if !published || !investigated || s.opts.Ledger == nil || record.signature == "" {
		return
	}
	// Non-terminal states (failed, stale, in-flight) are ignored by the ledger.
	verdict := RecurrenceVerdict{State: view.State, Reason: view.Reason, RecordedAt: view.CompletedAt}
	if err := s.opts.Ledger.RecordVerdict(record.signature, verdict); err != nil {
		log.Printf("Warning: failed to record remediation verdict in recurrence history: %v", err)
	}
}

// joinExisting replays an idempotent result or joins an in-flight run without
// touching the cache, the ledger, or publication validation. Callers re-check the
// same conditions under the mutex after that work, since it can race.
func (s *OperationService) joinExisting(idempotencyKey, identity, cacheKey string, refresh bool) (models.PatternRemediationInvestigationSummary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.idempotency[idempotencyKey]; ok {
		if previous.identity != identity || previous.refresh != refresh {
			return models.PatternRemediationInvestigationSummary{}, true, ErrOperationIdempotencyConflict
		}
		if record := s.byIdentity[identity]; record != nil {
			return cloneOperationView(record.view), true, nil
		}
	}
	if record := s.byCacheKey[cacheKey]; record != nil && (record.running || !refresh) {
		s.idempotency[idempotencyKey] = idempotencyRecord{identity: identity, refresh: refresh}
		return cloneOperationView(record.view), true, nil
	}
	return models.PatternRemediationInvestigationSummary{}, false, nil
}

// claimReuse turns a prior terminal verdict for the same durable cause into a
// completed result, so a recurrence does not re-spend model budget on a question
// already answered. The claim is charged against the verdict's bounded reuse
// budget; a failure to charge it yields no reuse, so an unaccounted answer is
// never served. The original completion time is carried through so an old answer
// is not presented as a fresh investigation.
func (s *OperationService) claimReuse(ref OperationRef, signature string, now time.Time) (models.PatternRemediationInvestigationSummary, bool) {
	if s.opts.Ledger == nil || signature == "" {
		return models.PatternRemediationInvestigationSummary{}, false
	}
	verdict, ok, err := s.opts.Ledger.ClaimReuse(signature)
	if err != nil {
		log.Printf("Warning: failed to claim recurrence memory: %v", err)
		return models.PatternRemediationInvestigationSummary{}, false
	}
	if !ok || !models.ValidPatternRemediationInvestigationState(verdict.State) {
		return models.PatternRemediationInvestigationSummary{}, false
	}
	completedAt := strings.TrimSpace(verdict.RecordedAt)
	if _, err := time.Parse(time.RFC3339, completedAt); err != nil {
		completedAt = now.UTC().Format(time.RFC3339)
	}
	return models.PatternRemediationInvestigationSummary{
		CausalGroupID: ref.CausalGroupID, CausalGroupHash: ref.CausalGroupHash,
		State: verdict.State, Reason: verdict.Reason, CompletedAt: completedAt,
	}, true
}

func (s *OperationService) storeRecovered(identity, cacheKey, signature string, ref OperationRef, view models.PatternRemediationInvestigationSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := &operationRecord{identity: identity, cacheKey: cacheKey, signature: signature, ref: ref, view: cloneOperationView(view), updatedAt: s.opts.Now().UTC()}
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

// ErrOperationNotActionable means the exact current investigation cannot enter fix preview.
var ErrOperationNotActionable = errors.New("remediation investigation is not exactly actionable")

// ActionableSubject is the private, reverified handoff for preview-only patch generation.
type ActionableSubject struct {
	Input                 FrozenInput
	ResultDigest          string
	Proposal              ActionableProposal
	Evidence              []EvidenceRecord
	EvidenceCatalogDigest string
	Source                sourceinvestigation.TreeReader
}

// MarshalJSON prevents private evidence and cache provenance from entering an API response.
func (ActionableSubject) MarshalJSON() ([]byte, error) {
	return nil, errors.New("actionable remediation subject is private")
}

// ResolveActionable revalidates the exact current operation and returns one verified proposal.
func (s *OperationService) ResolveActionable(ctx context.Context, ref OperationRef) (ActionableSubject, error) {
	ref, err := normalizeOperationRef(ref)
	if err != nil {
		return ActionableSubject{}, err
	}
	resolved, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return ActionableSubject{}, err
	}
	if err := s.validateBeforePublish(ctx, ref, resolved.Input); err != nil {
		return ActionableSubject{}, err
	}
	key, err := CacheKey(resolved.Input)
	if err != nil {
		return ActionableSubject{}, err
	}
	entry, ok, err := s.cache.Lookup(key)
	if err != nil || !ok {
		return ActionableSubject{}, ErrOperationNotActionable
	}
	verified, err := verifyOperationResult(ctx, resolved, entry)
	if err != nil {
		return ActionableSubject{}, err
	}
	if verified.Classification != ClassificationActionable || verified.Proposal == nil {
		return ActionableSubject{}, ErrOperationNotActionable
	}
	evidence, err := selectedEvidenceRecords(verified.Proposal.EvidenceIDs, entry.EvidenceCatalog)
	if err != nil {
		return ActionableSubject{}, err
	}
	return ActionableSubject{Input: resolved.Input, ResultDigest: entry.ResultDigest, Proposal: *verified.Proposal, Evidence: evidence, EvidenceCatalogDigest: entry.EvidenceCatalogDigest, Source: resolved.Source}, nil
}
