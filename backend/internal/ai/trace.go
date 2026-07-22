package ai

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

const (
	analysisTraceVersion   = 1
	analysisTraceMaxEvents = 128
	analysisTraceMaxTraces = 500
	analysisTraceMaxText   = 256
)

// AnalysisTraceFile is the private, bounded trace snapshot for one fetch run.
type AnalysisTraceFile struct {
	Version       int             `json:"version"`
	GeneratedAt   string          `json:"generated_at"`
	DroppedTraces int             `json:"dropped_traces,omitempty"`
	Traces        []AnalysisTrace `json:"traces"`
}

// AnalysisTrace records sanitized control-flow metadata for one failure.
type AnalysisTrace struct {
	JobID     string       `json:"job_id"`
	BuildID   string       `json:"build_id"`
	TestName  string       `json:"test_name"`
	APIMode   string       `json:"api_mode"`
	StartedAt string       `json:"started_at"`
	ElapsedMs int          `json:"elapsed_ms"`
	Outcome   string       `json:"outcome"`
	ErrorCode string       `json:"error_code,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Events    []TraceEvent `json:"events"`
}

// TraceEvent is one bounded, content-free analysis event.
type TraceEvent struct {
	Sequence      int    `json:"sequence"`
	ElapsedMs     int    `json:"elapsed_ms"`
	Kind          string `json:"kind"`
	Outcome       string `json:"outcome,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	Status        string `json:"status,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	Tool          string `json:"tool,omitempty"`
	DurationMs    int    `json:"duration_ms,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	InputTokens   int    `json:"input_tokens,omitempty"`
	OutputTokens  int    `json:"output_tokens,omitempty"`
	MessageCount  int    `json:"message_count,omitempty"`
	ToolCallCount int    `json:"tool_call_count,omitempty"`
	Bytes         int    `json:"bytes,omitempty"`
	Elided        int    `json:"elided,omitempty"`
	Retry         int    `json:"retry,omitempty"`
	IssueCount    int    `json:"issue_count,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
}

// TraceMetadata identifies one analysis without model or endpoint details.
type TraceMetadata struct {
	JobID    string
	BuildID  string
	TestName string
	APIMode  string
}

// TraceStore collects completed traces for one fetch run.
type TraceStore struct {
	mu      sync.Mutex
	traces  []AnalysisTrace
	dropped int
}

// NewTraceStore creates an empty trace store.
func NewTraceStore() *TraceStore { return &TraceStore{} }

// Start begins a trace session for one failure.
func (s *TraceStore) Start(meta TraceMetadata) *TraceSession {
	if s == nil {
		return nil
	}
	now := time.Now().UTC()
	return &TraceSession{
		store: s,
		start: now,
		trace: AnalysisTrace{
			JobID:     traceText(meta.JobID),
			BuildID:   traceText(meta.BuildID),
			TestName:  traceText(meta.TestName),
			APIMode:   traceText(meta.APIMode),
			StartedAt: now.Format(time.RFC3339Nano),
			Events:    []TraceEvent{},
		},
	}
}

// Snapshot returns a deterministic copy of all completed traces.
func (s *TraceStore) Snapshot() AnalysisTraceFile {
	out := AnalysisTraceFile{Version: analysisTraceVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Traces: []AnalysisTrace{}}
	if s == nil {
		return out
	}
	s.mu.Lock()
	out.DroppedTraces = s.dropped
	out.Traces = append(out.Traces, s.traces...)
	s.mu.Unlock()
	sort.Slice(out.Traces, func(i, j int) bool {
		a, b := out.Traces[i], out.Traces[j]
		if a.StartedAt != b.StartedAt {
			return a.StartedAt < b.StartedAt
		}
		if a.JobID != b.JobID {
			return a.JobID < b.JobID
		}
		if a.BuildID != b.BuildID {
			return a.BuildID < b.BuildID
		}
		return a.TestName < b.TestName
	})
	return out
}

// Save writes the private trace snapshot atomically.
func (s *TraceStore) Save(path string) error {
	return statefile.WriteJSON(path, s.Snapshot())
}

// TraceSession records one analysis until Finish is called.
type TraceSession struct {
	mu       sync.Mutex
	store    *TraceStore
	start    time.Time
	trace    AnalysisTrace
	finished bool
}

// Record appends one sanitized event while the per-analysis cap has room.
func (s *TraceSession) Record(event TraceEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if len(s.trace.Events) >= analysisTraceMaxEvents {
		s.trace.Truncated = true
		return
	}
	event.Sequence = len(s.trace.Events) + 1
	event.ElapsedMs = int(time.Since(s.start) / time.Millisecond)
	event.Kind = traceText(event.Kind)
	event.Outcome = traceText(event.Outcome)
	event.ResponseID = traceText(event.ResponseID)
	event.Status = traceText(event.Status)
	event.FinishReason = traceText(event.FinishReason)
	event.Tool = traceText(event.Tool)
	event.ErrorCode = traceCode(event.ErrorCode)
	s.trace.Events = append(s.trace.Events, event)
}

// Finish completes the trace and transfers it to the store.
func (s *TraceSession) Finish(outcome string, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.trace.ElapsedMs = int(time.Since(s.start) / time.Millisecond)
	s.trace.Outcome = traceText(outcome)
	if err != nil {
		s.trace.ErrorCode = traceErrorCode(err)
	}
	completed := s.trace
	s.mu.Unlock()

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if len(s.store.traces) >= analysisTraceMaxTraces {
		s.store.dropped++
		return
	}
	s.store.traces = append(s.store.traces, completed)
}

type analysisTraceContextKey struct{}

func withAnalysisTrace(ctx context.Context, trace *TraceSession) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, analysisTraceContextKey{}, trace)
}

func recordTrace(ctx context.Context, event TraceEvent) {
	if ctx == nil {
		return
	}
	trace, _ := ctx.Value(analysisTraceContextKey{}).(*TraceSession)
	trace.Record(event)
}

func traceText(s string) string {
	s = strings.TrimSpace(redact.Credentials(redact.URLs(s)))
	return textutil.Truncate(s, analysisTraceMaxText)
}

var traceCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func traceCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if traceCodePattern.MatchString(code) {
		return code
	}
	return "analysis_error"
}

func traceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unsupported ai api"):
		return "unsupported_api"
	case strings.Contains(message, "function-calling support"), strings.Contains(message, "does not support function calling"):
		return "tools_unsupported"
	case strings.Contains(message, "marshal request"):
		return "request_marshal"
	case strings.Contains(message, "build request"):
		return "request_build"
	case strings.Contains(message, "post:"):
		return "request_transport"
	case strings.Contains(message, "decode response"):
		return "response_decode"
	case strings.Contains(message, "responses status"):
		return "provider_status"
	case strings.Contains(message, "chat returned"), strings.Contains(message, "responses returned"):
		return "http_error"
	case strings.Contains(message, "empty"):
		return "empty_response"
	default:
		return "analysis_error"
	}
}
