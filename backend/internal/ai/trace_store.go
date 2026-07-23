package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const analysisTraceMaxFileBytes = 64 << 20

// LoadTraceStore loads an existing private trace snapshot. Missing files return
// an empty store so polling and webhook writers can share one upsert path.
func LoadTraceStore(path string) (*TraceStore, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewTraceStore(), nil
		}
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, analysisTraceMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > analysisTraceMaxFileBytes {
		return nil, fmt.Errorf("trace file exceeds %d bytes", analysisTraceMaxFileBytes)
	}
	var snapshot AnalysisTraceFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode trace file: %w", err)
	}
	if snapshot.Version > analysisTraceVersion {
		return nil, fmt.Errorf("trace version %d is newer than supported version %d", snapshot.Version, analysisTraceVersion)
	}
	store := NewTraceStore()
	store.dropped = snapshot.DroppedTraces
	for _, trace := range snapshot.Traces {
		store.Upsert(trace)
	}
	return store, nil
}

// BeforeRetention reports whether a completed analysis is older than the
// persisted rolling-window boundary and should not be restored.
func (s *TraceStore) BeforeRetention(generatedAt string) bool {
	if s == nil || strings.TrimSpace(generatedAt) == "" {
		return false
	}
	analysisTime, err := time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped == 0 || len(s.traces) == 0 {
		return false
	}
	boundary, ok := traceOrderTime(s.traces[oldestTraceIndex(s.traces)])
	return ok && !analysisTime.After(boundary)
}

// Upsert adds or replaces one completed trace using its analysis identity.
func (s *TraceStore) Upsert(trace AnalysisTrace) bool {
	if s == nil {
		return false
	}
	trace = normalizeAnalysisTrace(trace)
	key := analysisTraceIdentity(trace)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.traces {
		if analysisTraceIdentity(s.traces[i]) == key {
			if !traceAdvances(s.traces[i], trace) {
				return false
			}
			s.traces[i] = trace
			return true
		}
	}
	if len(s.traces) >= analysisTraceMaxTraces {
		oldest := oldestTraceIndex(s.traces)
		if !traceAfter(trace, s.traces[oldest]) {
			s.dropped++
			return false
		}
		s.traces = append(s.traces[:oldest], s.traces[oldest+1:]...)
		s.dropped++
	}
	s.traces = append(s.traces, trace)
	return true
}

func (s *TraceStore) snapshotWithinLimit(limit int) (AnalysisTraceFile, error) {
	if limit <= 0 {
		return AnalysisTraceFile{}, fmt.Errorf("trace file limit must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := append([]AnalysisTrace(nil), s.traces...)
	sort.Slice(ordered, func(i, j int) bool { return traceBefore(ordered[i], ordered[j]) })
	generatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	candidate := func(drop int) (AnalysisTraceFile, []byte, error) {
		traces := append([]AnalysisTrace(nil), ordered[drop:]...)
		snapshot := AnalysisTraceFile{
			Version: analysisTraceVersion, GeneratedAt: generatedAt,
			DroppedTraces: s.dropped + drop, Traces: traces,
		}
		if snapshot.DroppedTraces > 0 && len(traces) > 0 {
			snapshot.RetainedSince = traces[0].RecordedAt
		}
		encoded, err := json.MarshalIndent(snapshot, "", "  ")
		return snapshot, encoded, err
	}

	snapshot, encoded, err := candidate(0)
	if err != nil {
		return AnalysisTraceFile{}, err
	}
	if len(encoded) <= limit {
		return snapshot, nil
	}

	low, high := 1, len(ordered)
	var best AnalysisTraceFile
	bestDrop := -1
	for low <= high {
		mid := low + (high-low)/2
		trial, data, err := candidate(mid)
		if err != nil {
			return AnalysisTraceFile{}, err
		}
		if len(data) <= limit {
			best, bestDrop = trial, mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	if bestDrop < 0 {
		return AnalysisTraceFile{}, fmt.Errorf("empty trace snapshot exceeds %d bytes", limit)
	}
	s.traces = append([]AnalysisTrace(nil), ordered[bestDrop:]...)
	s.dropped += bestDrop
	return best, nil
}

func oldestTraceIndex(traces []AnalysisTrace) int {
	oldest := 0
	for i := 1; i < len(traces); i++ {
		if traceBefore(traces[i], traces[oldest]) {
			oldest = i
		}
	}
	return oldest
}

func traceAdvances(current, next AnalysisTrace) bool {
	currentTerminal := terminalTraceOutcome(current.Outcome)
	nextTerminal := terminalTraceOutcome(next.Outcome)
	if currentTerminal && !nextTerminal {
		return false
	}
	if nextTerminal && !currentTerminal {
		return true
	}
	if len(next.Events) != len(current.Events) {
		return len(next.Events) > len(current.Events)
	}
	currentElapsed := traceLastElapsed(current)
	nextElapsed := traceLastElapsed(next)
	if nextElapsed != currentElapsed {
		return nextElapsed > currentElapsed
	}
	return traceInformation(next) >= traceInformation(current)
}

func terminalTraceOutcome(outcome string) bool {
	switch outcome {
	case "success", "succeeded", "failed", "cancelled", "rejected", "error", "unavailable", "ai_cache_hit", "build_cache_hit":
		return true
	}
	return false
}

func traceLastElapsed(trace AnalysisTrace) int {
	last := trace.ElapsedMs
	for _, event := range trace.Events {
		if event.ElapsedMs > last {
			last = event.ElapsedMs
		}
	}
	return last
}

func traceInformation(trace AnalysisTrace) int {
	score := 0
	if trace.ErrorCode != "" {
		score++
	}
	for _, event := range trace.Events {
		if event.ResponseID != "" {
			score += 4
		}
		if event.ErrorCode != "" {
			score += 2
		}
		if event.InputTokens > 0 || event.OutputTokens > 0 || event.Bytes > 0 || event.FinishReason != "" {
			score++
		}
	}
	return score
}

func traceBefore(a, b AnalysisTrace) bool {
	aTime, aOK := traceOrderTime(a)
	bTime, bOK := traceOrderTime(b)
	switch {
	case aOK && bOK && !aTime.Equal(bTime):
		return aTime.Before(bTime)
	case aOK != bOK:
		return !aOK
	}
	return analysisTraceIdentity(a) < analysisTraceIdentity(b)
}

func traceAfter(a, b AnalysisTrace) bool {
	aTime, aOK := traceOrderTime(a)
	bTime, bOK := traceOrderTime(b)
	switch {
	case aOK && bOK:
		return aTime.After(bTime)
	case aOK != bOK:
		return aOK
	default:
		return false
	}
}

func traceStartTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func traceOrderTime(trace AnalysisTrace) (time.Time, bool) {
	if parsed, ok := traceStartTime(trace.RecordedAt); ok {
		return parsed, true
	}
	return traceStartTime(trace.StartedAt)
}

func analysisTraceIdentity(trace AnalysisTrace) string {
	if trace.JobID == "" && trace.BuildID == "" && trace.TestName == "" {
		return ""
	}
	return trace.JobID + "\x00" + trace.BuildID + "\x00" + trace.TestName + "\x00" + trace.StartedAt
}

func normalizeAnalysisTrace(trace AnalysisTrace) AnalysisTrace {
	trace.JobID = traceText(trace.JobID)
	trace.BuildID = traceText(trace.BuildID)
	trace.TestName = traceText(trace.TestName)
	trace.APIMode = traceText(trace.APIMode)
	trace.StartedAt = traceText(trace.StartedAt)
	trace.RecordedAt = traceText(trace.RecordedAt)
	if trace.RecordedAt == "" {
		trace.RecordedAt = trace.StartedAt
	}
	trace.Outcome = traceCode(trace.Outcome)
	if trace.ErrorCode != "" {
		trace.ErrorCode = traceCode(trace.ErrorCode)
	}
	if trace.ElapsedMs < 0 {
		trace.ElapsedMs = 0
	}
	if len(trace.Events) > analysisTraceMaxEvents {
		trace.Events = trace.Events[:analysisTraceMaxEvents]
		trace.Truncated = true
	}
	for i := range trace.Events {
		event := &trace.Events[i]
		event.Sequence = i + 1
		if event.ElapsedMs < 0 {
			event.ElapsedMs = 0
		}
		event.Kind = traceText(event.Kind)
		event.Outcome = traceText(event.Outcome)
		event.ResponseID = traceResponseID(event.ResponseID)
		event.Status = traceText(event.Status)
		event.FinishReason = traceText(event.FinishReason)
		event.Tool = traceText(event.Tool)
		if event.ErrorCode != "" {
			event.ErrorCode = traceCode(event.ErrorCode)
		}
	}
	if trace.Events == nil {
		trace.Events = []TraceEvent{}
	}
	return trace
}
