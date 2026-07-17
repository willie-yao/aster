package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	eventPageSize         = 500
	maxEventPages         = 100
	orkaAcceptanceVersion = 1
)

type analysisTelemetry struct {
	EventCount       int
	ToolCalls        int
	InputTokens      int
	OutputTokens     int
	ElapsedMs        int
	Provider         string
	Model            string
	TimelineVerified bool
	ValidationPassed bool
	BudgetExhausted  bool
}

type eventListResponse struct {
	LatestSeq int64            `json:"latestSeq"`
	Events    []executionEvent `json:"events"`
}

type executionEvent struct {
	Seq          int64     `json:"seq"`
	Type         string    `json:"type"`
	ToolName     string    `json:"toolName,omitempty"`
	ToolCallID   string    `json:"toolCallID,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	StopReason   string    `json:"stopReason,omitempty"`
	InputTokens  int       `json:"inputTokens,omitempty"`
	OutputTokens int       `json:"outputTokens,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
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
		if len(page.Events) == 0 || after >= page.LatestSeq {
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
	out := analysisTelemetry{EventCount: len(events)}
	toolCalls := map[string]bool{}
	var earliest, latest, started, completed time.Time
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
			started = event.CreatedAt
		case "TaskSucceeded", "TaskFailed", "TaskCancelled":
			completed = event.CreatedAt
		case "ToolCallStarted":
			key := event.ToolCallID
			if key == "" {
				key = fmt.Sprintf("seq:%d", event.Seq)
			}
			toolCalls[key] = true
		case "ToolCallCompleted":
			name := normalizeToolName(event.ToolName)
			switch {
			case matchesScopedTool(name, "verify_timeline"):
				out.TimelineVerified = true
			case matchesScopedTool(name, "validate_analysis"):
				out.ValidationPassed = true
			}
		case "ModelRequestCompleted":
			out.InputTokens += event.InputTokens
			out.OutputTokens += event.OutputTokens
			if event.Provider != "" {
				out.Provider = event.Provider
			}
			if event.Model != "" {
				out.Model = event.Model
			}
			stop := strings.ToLower(strings.TrimSpace(event.StopReason))
			if strings.Contains(stop, "length") || strings.Contains(stop, "max_token") {
				out.BudgetExhausted = true
			}
		}
	}
	out.ToolCalls = len(toolCalls)
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

func normalizeToolName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

func matchesScopedTool(name, base string) bool {
	return name == base || strings.HasPrefix(name, base+"_b")
}
