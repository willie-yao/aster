package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	eventPageSize = 500
	maxEventPages = 100
)

var errTaskEventsNotReadableYet = errors.New("task events are not readable yet")

type analysisTelemetry struct {
	EventCount          int
	ToolCalls           int
	ToolFailures        int
	ModelRequests       int
	ModelFailures       int
	ContextBytes        int
	ContextTruncations  int
	TaskRetries         int
	InputTokens         int
	OutputTokens        int
	ElapsedMs           int
	Provider            string
	Model               string
	APIMode             string
	ResponseID          string
	StopReason          string
	TaskOutcome         string
	TimelineVerified    bool
	ValidationPassed    bool
	BudgetExhausted     bool
	qualityToolOutcomes map[string]string
	events              []executionEvent
}

type eventListResponse struct {
	LatestSeq int64            `json:"latestSeq"`
	Events    []executionEvent `json:"events"`
}

type executionEvent struct {
	Seq          int64           `json:"seq"`
	Type         string          `json:"type"`
	ToolName     string          `json:"toolName,omitempty"`
	ToolCallID   string          `json:"toolCallID,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Model        string          `json:"model,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	InputTokens  int             `json:"inputTokens,omitempty"`
	OutputTokens int             `json:"outputTokens,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func (c *orkaClient) analysisTelemetry(ctx context.Context, namespace, taskName string) (analysisTelemetry, error) {
	var events []executionEvent
	var after int64
	for range maxEventPages {
		endpoint := c.base + "/api/v1/tasks/" + url.PathEscape(taskName) + "/events"
		query := url.Values{}
		query.Set("namespace", namespace)
		query.Set("after", strconv.FormatInt(after, 10))
		query.Set("limit", strconv.Itoa(eventPageSize))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return analysisTelemetry{}, fmt.Errorf("create Task events request: %w", err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return analysisTelemetry{}, fmt.Errorf("fetch Task events: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return analysisTelemetry{}, fmt.Errorf("read Task events: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			message := strings.TrimSpace(string(body))
			if len(message) > 1024 {
				message = message[:1024]
			}
			return analysisTelemetry{}, fmt.Errorf("fetch Task events: HTTP %d: %s", resp.StatusCode, message)
		}
		var page eventListResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return analysisTelemetry{}, fmt.Errorf("parse Task events: %w", err)
		}
		events = append(events, page.Events...)
		if len(page.Events) == 0 {
			if page.LatestSeq > after {
				return analysisTelemetry{}, fmt.Errorf("%w through sequence %d", errTaskEventsNotReadableYet, page.LatestSeq)
			}
			return summarizeEvents(events), nil
		}
		if after >= page.LatestSeq {
			return summarizeEvents(events), nil
		}
		next := page.Events[len(page.Events)-1].Seq
		if next <= after {
			return analysisTelemetry{}, fmt.Errorf("task events cursor did not advance past %d", after)
		}
		after = next
		if after >= page.LatestSeq {
			return summarizeEvents(events), nil
		}
	}
	return analysisTelemetry{}, fmt.Errorf("task events exceeded %d pages", maxEventPages)
}

func summarizeEvents(events []executionEvent) analysisTelemetry {
	out := analysisTelemetry{
		EventCount: len(events), qualityToolOutcomes: map[string]string{},
		events: append([]executionEvent(nil), events...),
	}
	toolCalls := map[string]bool{}
	apiModes := map[string]bool{}
	var earliest, latest, started, completed time.Time
	starts := 0
	for _, event := range events {
		if !event.CreatedAt.IsZero() {
			if earliest.IsZero() || event.CreatedAt.Before(earliest) {
				earliest = event.CreatedAt
			}
			if latest.IsZero() || event.CreatedAt.After(latest) {
				latest = event.CreatedAt
			}
		}
		switch event.Type {
		case "TaskStarted":
			starts++
			if started.IsZero() {
				started = event.CreatedAt
			}
		case "TaskSucceeded", "TaskFailed", "TaskCancelled":
			completed = event.CreatedAt
			out.TaskOutcome = strings.ToLower(strings.TrimPrefix(event.Type, "Task"))
		case "ToolCallStarted":
			key := event.ToolCallID
			if key == "" {
				key = fmt.Sprintf("seq:%d", event.Seq)
			}
			toolCalls[key] = true
		case "ToolCallCompleted":
			name := normalizeToolName(event.ToolName)
			out.recordQualityToolOutcome(name, "completed")
			out.ContextBytes += eventResultLength(event.Content)
		case "ToolCallFailed":
			out.ToolFailures++
			out.recordQualityToolOutcome(normalizeToolName(event.ToolName), "failed")
		case "ContextTruncated":
			out.ContextTruncations++
		case "ModelRequestCompleted", "ModelRequestFailed":
			out.ModelRequests++
			if event.Type == "ModelRequestFailed" {
				out.ModelFailures++
			} else {
				if apiMode := eventContentString(event.Content, "apiMode", "api_mode"); apiMode != "" {
					apiModes[apiMode] = true
				}
				if responseID := eventContentString(event.Content, "responseID", "response_id"); responseID != "" {
					out.ResponseID = responseID
				}
			}
			out.InputTokens += event.InputTokens
			out.OutputTokens += event.OutputTokens
			if event.Provider != "" {
				out.Provider = event.Provider
			}
			if event.Model != "" {
				out.Model = event.Model
			}
			if event.StopReason != "" {
				out.StopReason = event.StopReason
			}
			stop := strings.ToLower(strings.TrimSpace(event.StopReason))
			if strings.Contains(stop, "length") || strings.Contains(stop, "max_token") {
				out.BudgetExhausted = true
			}
		}
	}
	out.ToolCalls = len(toolCalls)
	for apiMode := range apiModes {
		if out.APIMode == "" {
			out.APIMode = apiMode
		} else if out.APIMode != apiMode {
			out.APIMode = "mixed"
		}
	}
	if starts > 1 {
		out.TaskRetries = starts - 1
	}
	if started.IsZero() {
		started = earliest
	}
	if completed.IsZero() {
		completed = latest
	}
	if !started.IsZero() && !completed.IsZero() && completed.After(started) {
		out.ElapsedMs = int(completed.Sub(started).Milliseconds())
	}
	return out
}

func (t *analysisTelemetry) recordQualityToolOutcome(name, outcome string) {
	base := qualityToolBase(name)
	if base == "" {
		return
	}
	t.qualityToolOutcomes[base] = outcome
	switch base {
	case "verify_timeline":
		t.TimelineVerified = outcome == "completed"
	case "validate_analysis", "submit_analysis":
		t.ValidationPassed = outcome == "completed"
	}
}

func qualityToolBase(name string) string {
	for _, base := range []string{
		"validate_analysis",
		"submit_analysis",
		"verify_timeline",
		"check_transient_signatures",
		"recurrence",
		"required_evidence",
		"diff_last_passing",
	} {
		if matchesScopedTool(name, base) {
			return base
		}
	}
	return ""
}

func eventContentString(content json.RawMessage, keys ...string) string {
	var payload map[string]any
	if json.Unmarshal(content, &payload) != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func eventResultLength(content json.RawMessage) int {
	var payload struct {
		ResultLength int `json:"resultLength"`
	}
	if json.Unmarshal(content, &payload) != nil || payload.ResultLength < 0 {
		return 0
	}
	return payload.ResultLength
}

func normalizeToolName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

func matchesScopedTool(name, base string) bool {
	return name == base || strings.HasPrefix(name, base+"_b") ||
		((base == "validate_analysis" || base == "submit_analysis") &&
			(strings.HasPrefix(name, base+"_az_analysis_") || strings.HasPrefix(name, base+"_cmp_")))
}
