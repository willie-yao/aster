package aiusage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

type operationContextKey struct{}

// Operation accumulates model usage for one dashboard feature operation.
type Operation struct {
	mu       sync.Mutex
	recorder *Recorder
	usage    OperationUsage
	finished bool
}

// Begin installs one execution in ctx. LogicalID groups retries, while a stable
// ExecutionID deduplicates only exact persistence replays.
func Begin(ctx context.Context, recorder *Recorder, metadata Metadata) (context.Context, *Operation) {
	if ctx == nil {
		ctx = context.Background()
	}
	if recorder == nil {
		return ctx, nil
	}
	started := metadata.StartedAt.UTC()
	if started.IsZero() {
		started = time.Now().UTC()
	}
	logicalID := safeOperationID(metadata.LogicalID)
	executionID := safeOperationID(metadata.ExecutionID)
	if executionID == "" {
		executionID = randomID()
	}
	if logicalID == "" {
		logicalID = executionID
	}
	feature := metadata.Feature
	if !validFeature(feature) {
		feature = FeatureUnknown
	}
	origin := metadata.Origin
	if !validOrigin(origin) {
		origin = OriginUnknown
	}
	op := &Operation{recorder: recorder, usage: OperationUsage{
		ID: executionID, LogicalID: logicalID, Origin: origin, Feature: feature,
		StartedAt:        started.Format(time.RFC3339Nano),
		ModelFingerprint: safeFingerprint(metadata.ModelFingerprint),
		Model:            safeModelID(metadata.Model),
		ReasoningEffort:  safeReasoningEffort(metadata.ReasoningEffort),
		Correlation:      metadata.Correlation,
	}}
	return context.WithValue(ctx, operationContextKey{}, op), op
}

// ObserveModelRequest adds one logical provider request to the active operation.
func ObserveModelRequest(ctx context.Context, usage TokenUsage) {
	ObserveModelRequestWithModel(ctx, usage, "", "")
}

// ObserveModelRequestWithModel adds one provider request and safe model provenance.
func ObserveModelRequestWithModel(ctx context.Context, usage TokenUsage, model, fingerprint string) {
	ObserveModelRequestWithModelAndReasoningEffort(ctx, usage, model, fingerprint, "")
}

// ObserveModelRequestWithModelAndReasoningEffort adds one provider request and
// safe model and requested-effort provenance.
func ObserveModelRequestWithModelAndReasoningEffort(ctx context.Context, usage TokenUsage, model, fingerprint, reasoningEffort string) {
	op, _ := ctx.Value(operationContextKey{}).(*Operation)
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.finished {
		return
	}
	op.usage.CoverageCountsKnown = true
	op.setModelLocked(model, fingerprint)
	op.setReasoningEffortLocked(reasoningEffort)
	op.setUsageSourceLocked(UsageSourceProviderResponse)
	accumulateModelRequest(&op.usage, usage)
}

// AccumulateModelRequest adds one provider request to a detached operation.
// It is used when encrypted analyzer traces are merged into the fetcher ledger.
func AccumulateModelRequest(operation *OperationUsage, usage TokenUsage) {
	if operation == nil {
		return
	}
	operation.CoverageCountsKnown = true
	if operation.UsageSource == "" {
		operation.UsageSource = UsageSourceProviderResponse
	} else if operation.UsageSource != UsageSourceProviderResponse {
		operation.UsageSource = UsageSourceMixed
	}
	accumulateModelRequest(operation, usage)
}

func accumulateModelRequest(operation *OperationUsage, usage TokenUsage) {
	if operation.ModelRequests == math.MaxInt {
		incrementInvalidCount(&operation.UnreportedRequests)
		incrementInvalidCount(&operation.InvalidUsageRequests)
		operation.UsageInvalid = true
		return
	}
	operation.ModelRequests++
	if !usage.Reported {
		incrementInvalidCount(&operation.UnreportedRequests)
		return
	}
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.CacheWriteInputTokens < 0 ||
		usage.CachedInputTokens > usage.InputTokens || usage.CacheWriteInputTokens > usage.InputTokens-usage.CachedInputTokens ||
		usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.ReasoningTokens > usage.OutputTokens {
		incrementInvalidCount(&operation.UnreportedRequests)
		incrementInvalidCount(&operation.InvalidUsageRequests)
		operation.UsageInvalid = true
		return
	}
	input, inputOK := checkedTokenAdd(operation.InputTokens, usage.InputTokens)
	cached, cachedOK := checkedTokenAdd(operation.CachedInputTokens, usage.CachedInputTokens)
	cacheWrite, cacheWriteOK := checkedTokenAdd(operation.CacheWriteInputTokens, usage.CacheWriteInputTokens)
	output, outputOK := checkedTokenAdd(operation.OutputTokens, usage.OutputTokens)
	reasoning, reasoningOK := checkedTokenAdd(operation.ReasoningTokens, usage.ReasoningTokens)
	if !inputOK || !cachedOK || !cacheWriteOK || !outputOK || !reasoningOK {
		incrementInvalidCount(&operation.UnreportedRequests)
		incrementInvalidCount(&operation.InvalidUsageRequests)
		operation.UsageInvalid = true
		return
	}
	if operation.ReportedRequests == math.MaxInt {
		incrementInvalidCount(&operation.UnreportedRequests)
		incrementInvalidCount(&operation.InvalidUsageRequests)
		operation.UsageInvalid = true
		return
	}
	operation.ReportedRequests++
	if usage.CacheWriteInputTokensReported {
		incrementInvalidCount(&operation.CacheWriteReportedRequests)
	} else {
		incrementInvalidCount(&operation.CacheWriteUnreportedRequests)
	}
	operation.InputTokens = input
	operation.CachedInputTokens = cached
	operation.CacheWriteInputTokens = cacheWrite
	operation.OutputTokens = output
	operation.ReasoningTokens = reasoning
}

func incrementInvalidCount(value *int) {
	if *value < math.MaxInt {
		*value++
	}
}

func checkedTokenAdd(current int64, value int) (int64, bool) {
	if value < 0 || current > math.MaxInt64-int64(value) {
		return current, false
	}
	return current + int64(value), true
}

// MarkExternalUnmetered records model work performed outside ai.Client.
func MarkExternalUnmetered(ctx context.Context) {
	op, _ := ctx.Value(operationContextKey{}).(*Operation)
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.finished {
		op.usage.ExternalUnmetered = true
		op.usage.CoverageCountsKnown = true
		op.setUsageSourceLocked(UsageSourceExternalRuntime)
	}
}

// MarkModelGatewayExcluded records external model work whose gateway does not
// return token counts through the runtime contract.
func MarkModelGatewayExcluded(ctx context.Context, model string) {
	op, _ := ctx.Value(operationContextKey{}).(*Operation)
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.finished {
		op.usage.ModelGatewayExcluded = true
		op.usage.CoverageCountsKnown = true
		op.setModelLocked(model, "")
		op.setUsageSourceLocked(UsageSourceModelGateway)
	}
}

// MarkCooldownRetry records that this operation retried after a persisted cooldown.
func MarkCooldownRetry(ctx context.Context) {
	op, _ := ctx.Value(operationContextKey{}).(*Operation)
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.finished {
		op.usage.CooldownRetry = true
	}
}

// Finish persists the completed operation once and returns its snapshot.
func (o *Operation) Finish(outcome Outcome) OperationUsage {
	if o == nil {
		return OperationUsage{}
	}
	o.mu.Lock()
	if o.finished {
		usage := o.usage
		o.mu.Unlock()
		return usage
	}
	if !validOutcome(outcome) {
		outcome = OutcomeError
	}
	o.finished = true
	o.usage.Outcome = outcome
	o.usage.CompletedAt = o.recorder.now().UTC().Format(time.RFC3339Nano)
	o.usage = o.recorder.Record(o.usage)
	usage := o.usage
	o.mu.Unlock()
	return usage
}

func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(raw[:])
}

func safeOperationID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	normalized := strings.ToLower(value)
	if len(normalized) >= 16 && len(normalized) <= 64 {
		valid := true
		for _, r := range normalized {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-' {
				valid = false
				break
			}
		}
		if valid {
			return normalized
		}
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func safeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 16 {
		return ""
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return value
}

func safeModelID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 || strings.Contains(value, "://") || strings.ContainsAny(value, "?#") {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._:/", r) {
			continue
		}
		return ""
	}
	return value
}

func (o *Operation) setModelLocked(model, fingerprint string) {
	model = safeModelID(model)
	fingerprint = safeFingerprint(fingerprint)
	if o.usage.MixedModels {
		return
	}
	if o.usage.Model == "" && o.usage.ModelFingerprint == "" {
		o.usage.Model = model
		o.usage.ModelFingerprint = fingerprint
		return
	}
	if model != "" && o.usage.Model != "" && model != o.usage.Model ||
		fingerprint != "" && o.usage.ModelFingerprint != "" && fingerprint != o.usage.ModelFingerprint {
		o.usage.Model = ""
		o.usage.ModelFingerprint = ""
		o.usage.MixedModels = true
		return
	}
	if o.usage.Model == "" {
		o.usage.Model = model
	}
	if o.usage.ModelFingerprint == "" {
		o.usage.ModelFingerprint = fingerprint
	}
}

func safeReasoningEffort(value string) string {
	effort, err := modelprovider.NormalizeReasoningEffort(value)
	if err != nil {
		return ""
	}
	return string(effort)
}

func (o *Operation) setReasoningEffortLocked(value string) {
	value = safeReasoningEffort(value)
	if o.usage.ReasoningEffort == "" {
		o.usage.ReasoningEffort = value
	}
}

func (o *Operation) setUsageSourceLocked(source UsageSource) {
	if source == "" || o.usage.UsageSource == UsageSourceMixed {
		return
	}
	if o.usage.UsageSource == "" {
		o.usage.UsageSource = source
		return
	}
	if o.usage.UsageSource != source {
		o.usage.UsageSource = UsageSourceMixed
	}
}
