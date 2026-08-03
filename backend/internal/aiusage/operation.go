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
		Correlation:      metadata.Correlation,
	}}
	return context.WithValue(ctx, operationContextKey{}, op), op
}

// ObserveModelRequest adds one logical provider request to the active operation.
func ObserveModelRequest(ctx context.Context, usage TokenUsage) {
	op, _ := ctx.Value(operationContextKey{}).(*Operation)
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.finished {
		return
	}
	if op.usage.ModelRequests < math.MaxInt {
		op.usage.ModelRequests++
	}
	if !usage.Reported {
		op.usage.UnreportedRequests++
		return
	}
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens ||
		usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.ReasoningTokens > usage.OutputTokens {
		op.usage.UnreportedRequests++
		op.usage.UsageInvalid = true
		return
	}
	input, inputOK := checkedTokenAdd(op.usage.InputTokens, usage.InputTokens)
	cached, cachedOK := checkedTokenAdd(op.usage.CachedInputTokens, usage.CachedInputTokens)
	output, outputOK := checkedTokenAdd(op.usage.OutputTokens, usage.OutputTokens)
	reasoning, reasoningOK := checkedTokenAdd(op.usage.ReasoningTokens, usage.ReasoningTokens)
	if !inputOK || !cachedOK || !outputOK || !reasoningOK {
		op.usage.UnreportedRequests++
		op.usage.UsageInvalid = true
		return
	}
	op.usage.ReportedRequests++
	op.usage.InputTokens = input
	op.usage.CachedInputTokens = cached
	op.usage.OutputTokens = output
	op.usage.ReasoningTokens = reasoning
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
