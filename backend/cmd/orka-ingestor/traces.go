package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

type orkaTraceIdentity struct {
	Namespace    string
	TaskName     string
	ContractHash string
	JobID        string
	BuildID      string
	TestName     string
}

func buildOrkaAnalysisTrace(identity orkaTraceIdentity, telemetry analysisTelemetry) ai.AnalysisTrace {
	events := append([]executionEvent(nil), telemetry.events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	origin := traceOrigin(events)
	traceEvents := make([]ai.TraceEvent, 0, len(events))
	taskStarts := 0
	for _, event := range events {
		traceEvent, ok := orkaTraceEvent(event, origin, &taskStarts)
		if ok {
			traceEvents = append(traceEvents, traceEvent)
		}
	}
	outcome := strings.TrimSpace(telemetry.TaskOutcome)
	if outcome == "" {
		outcome = "unknown"
	}
	trace := ai.AnalysisTrace{
		Backend:       "orka",
		TaskNamespace: identity.Namespace,
		TaskName:      identity.TaskName,
		ContractHash:  identity.ContractHash,
		JobID:         identity.JobID,
		BuildID:       identity.BuildID,
		TestName:      identity.TestName,
		APIMode:       telemetry.APIMode,
		ElapsedMs:     telemetry.ElapsedMs,
		Outcome:       outcome,
		Events:        traceEvents,
	}
	if !origin.IsZero() {
		trace.StartedAt = origin.UTC().Format(time.RFC3339Nano)
	}
	if latest := traceLatest(events); !latest.IsZero() {
		trace.RecordedAt = latest.UTC().Format(time.RFC3339Nano)
	}
	switch outcome {
	case "failed":
		trace.ErrorCode = "task_failed"
	case "cancelled":
		trace.ErrorCode = "task_cancelled"
	}
	return trace
}

func traceLatest(events []executionEvent) time.Time {
	var latest time.Time
	for _, event := range events {
		if !event.CreatedAt.IsZero() && event.CreatedAt.After(latest) {
			latest = event.CreatedAt
		}
	}
	return latest
}

func traceOrigin(events []executionEvent) time.Time {
	var earliest time.Time
	for _, event := range events {
		if event.CreatedAt.IsZero() {
			continue
		}
		if event.Type == "TaskStarted" {
			return event.CreatedAt
		}
		if earliest.IsZero() || event.CreatedAt.Before(earliest) {
			earliest = event.CreatedAt
		}
	}
	return earliest
}

func orkaTraceEvent(event executionEvent, origin time.Time, taskStarts *int) (ai.TraceEvent, bool) {
	trace := ai.TraceEvent{ElapsedMs: elapsedFrom(origin, event.CreatedAt)}
	switch event.Type {
	case "TaskStarted":
		trace.Kind = "task"
		trace.Outcome = "started"
		trace.Retry = *taskStarts
		*taskStarts++
	case "TaskSucceeded", "TaskFailed", "TaskCancelled":
		trace.Kind = "task"
		trace.Outcome = strings.ToLower(strings.TrimPrefix(event.Type, "Task"))
		if event.Type != "TaskSucceeded" {
			trace.ErrorCode = "task_" + trace.Outcome
		}
	case "ToolCallStarted", "ToolCallCompleted", "ToolCallFailed":
		trace.Kind = "tool_call"
		trace.Tool = traceToolName(event.ToolName)
		switch event.Type {
		case "ToolCallStarted":
			trace.Outcome = "started"
		case "ToolCallCompleted":
			trace.Outcome = "success"
			trace.Bytes = eventResultLength(event.Content)
		case "ToolCallFailed":
			trace.Outcome = "error"
			trace.ErrorCode = "tool_call_failed"
		}
	case "ContextTruncated":
		trace.Kind = "context_compaction"
		trace.Outcome = "applied"
	case "ModelRequestCompleted", "ModelRequestFailed":
		trace.Kind = "model_request"
		trace.InputTokens = event.InputTokens
		trace.OutputTokens = event.OutputTokens
		trace.FinishReason = event.StopReason
		if event.Type == "ModelRequestCompleted" {
			trace.Outcome = "success"
			trace.Status = "completed"
			trace.ResponseID = eventContentString(event.Content, "responseID", "response_id")
		} else {
			trace.Outcome = "error"
			trace.Status = "failed"
			trace.ErrorCode = "model_request_failed"
		}
	default:
		return ai.TraceEvent{}, false
	}
	return trace, true
}

func elapsedFrom(origin, eventTime time.Time) int {
	if origin.IsZero() || eventTime.IsZero() || eventTime.Before(origin) {
		return 0
	}
	return int(eventTime.Sub(origin) / time.Millisecond)
}

func traceToolName(name string) string {
	normalized := normalizeToolName(name)
	if base := qualityToolBase(normalized); base != "" {
		return base
	}
	return normalized
}

func saveOrkaAnalysisTrace(dataDir string, trace ai.AnalysisTrace) error {
	path := filepath.Join(dataDir, output.AITraceFilename)
	store, err := ai.LoadTraceStore(path)
	if err != nil {
		return fmt.Errorf("load trace store: %w", err)
	}
	store.Upsert(trace)
	return store.Save(path)
}
