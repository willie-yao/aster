package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// verify_timeline orders the timestamped events in a log so the model reasons
// about causal ordering (was the initiating failure first, or is the cited cause
// a downstream/teardown symptom?) from structured evidence instead of ad-hoc
// greps. It handles two shapes: plain per-line logs, and cloud activity logs that
// are a stream of pretty-printed JSON records (where a record's timestamp and the
// resource name are on different lines), so a resource filter matches whole
// records. Universal across projects: no provider-specific behavior beyond
// best-effort field-name heuristics that degrade to generic timestamp scanning.

const (
	timelineReadChunk = 64 * 1024
	timelineReadCap   = 2 * 1024 * 1024
	timelineEventCap  = 200
	timelineLineCap   = 400
)

var (
	timelineTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	timelineOperationRe = regexp.MustCompile(`(?i)(PUT|DELETE|GET|POST|PATCH|write|delete|create|update|scale)`)
	timelineStatusRe    = regexp.MustCompile(`(?i)(Succeeded|Failed|Accepted|Started|provisioningState["':= ]+\w+|Updating|Created|Deleting)`)
)

type timelineTool struct{}

func (*timelineTool) Name() string  { return "verify_timeline" }
func (*timelineTool) Group() string { return Group }
func (*timelineTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "verify_timeline",
			Description: "Extract the timestamped events from a log file and return them ordered by time. Use to check causal ordering: whether the stated root cause is the earliest initiating failure or a later downstream/teardown symptom. Pass an optional resource substring to keep only records mentioning it (works even for cloud activity logs whose records are multi-line JSON, so the timestamp and the resource name are on different lines).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":     map[string]interface{}{"type": "string", "description": "File path relative to build root (e.g. a build log or a cloud activity log)."},
					"resource": map[string]interface{}{"type": "string", "description": "Optional substring; only events whose line/record contains it are returned (case-insensitive)."},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (*timelineTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Path     string `json:"path"`
		Resource string `json:"resource"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	if args.Path == "" {
		return tools.ErrPayload("path is required")
	}

	content, err := readTimelineFile(ctx, env, args.Path)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}

	events := timelineEvents(content, args.Resource)
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].parsedOK && events[j].parsedOK {
			return events[i].parsed.Before(events[j].parsed)
		}
		return events[i].Timestamp < events[j].Timestamp
	})

	truncated := len(events) > timelineEventCap
	if truncated {
		events = events[:timelineEventCap]
	}

	first, last := "", ""
	if len(events) > 0 {
		first = events[0].Timestamp
		last = events[len(events)-1].Timestamp
	}

	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]interface{}{
			"timestamp": e.Timestamp,
			"operation": e.Operation,
			"status":    e.Status,
			"line":      e.Line,
		})
	}

	return tools.Result{
		BytesFetched: len(content),
		Payload: map[string]interface{}{
			"path":      args.Path,
			"resource":  args.Resource,
			"count":     len(out),
			"first":     first,
			"last":      last,
			"events":    out,
			"truncated": truncated,
		},
	}
}

// readTimelineFile reads up to timelineReadCap bytes of path via the Browser.
func readTimelineFile(ctx context.Context, env *tools.Env, path string) ([]byte, error) {
	var content []byte
	for offset := 0; offset < timelineReadCap; {
		want := min(timelineReadChunk, timelineReadCap-offset)
		chunk, total, err := env.Browser.Read(ctx, path, offset, want)
		if err != nil {
			return nil, err
		}
		content = append(content, chunk...)
		offset += len(chunk)
		if len(chunk) < want || len(chunk) == 0 || (total >= 0 && int64(offset) >= total) {
			break
		}
	}
	return content, nil
}

type timelineEvent struct {
	Timestamp string
	Operation string
	Status    string
	Line      string

	parsed   time.Time
	parsedOK bool
}

// timelineEvents extracts events from a log. Cloud activity logs are a stream of
// pretty-printed JSON records; those are parsed as records so a resource filter
// matches the whole record. Everything else falls back to a per-line scan.
func timelineEvents(content []byte, resource string) []timelineEvent {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		if events, ok := timelineEventsFromJSON(trimmed, resource); ok {
			return events
		}
	}
	return timelineEventsFromLines(content, resource)
}

func timelineEventsFromJSON(content []byte, resource string) ([]timelineEvent, bool) {
	raws, ok := decodeJSONRecords(content)
	if !ok {
		return nil, false
	}
	resourceLower := strings.ToLower(resource)
	events := make([]timelineEvent, 0, len(raws))
	for _, raw := range raws {
		if resourceLower != "" && !strings.Contains(strings.ToLower(string(raw)), resourceLower) {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		ts := timelineTimestampRe.FindString(jsonFieldString(rec, "eventTimestamp", "EventTimestamp", "timestamp", "time"))
		if ts == "" {
			ts = timelineTimestampRe.FindString(string(raw))
		}
		if ts == "" {
			continue
		}
		parsed, parsedOK := parseTimelineTimestamp(ts)
		events = append(events, timelineEvent{
			Timestamp: ts,
			Operation: capTimelineLine(strings.TrimSpace(jsonFieldString(rec, "operationName", "OperationName", "authorization", "Authorization"))),
			Status:    strings.TrimSpace(jsonFieldString(rec, "status", "Status", "subStatus", "SubStatus")),
			Line:      capTimelineLine(strings.Join(strings.Fields(string(raw)), " ")),
			parsed:    parsed,
			parsedOK:  parsedOK,
		})
	}
	return events, true
}

// decodeJSONRecords reads a JSON value stream into a flat list of records. It
// handles a top-level array, a whitespace-concatenated stream of objects
// (NDJSON or pretty-printed), and a single object. ok=false when nothing decoded.
func decodeJSONRecords(content []byte) ([]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(content)))
	var out []json.RawMessage
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break // io.EOF or first malformed record; keep what we have
		}
		if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(raw, &arr) == nil {
				out = append(out, arr...)
				continue
			}
		}
		out = append(out, raw)
	}
	return out, len(out) > 0
}

// jsonFieldString returns a readable string for the first present key, digging
// one level into nested objects for the common {"action"|"value"|"localizedValue"}
// shapes.
func jsonFieldString(rec map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := rec[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			return t
		case map[string]any:
			for _, sub := range []string{"action", "value", "localizedValue"} {
				if s, ok := t[sub].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func timelineEventsFromLines(content []byte, resource string) []timelineEvent {
	resourceLower := strings.ToLower(resource)
	lines := strings.Split(string(content), "\n")
	events := make([]timelineEvent, 0)
	for _, line := range lines {
		if resourceLower != "" && !strings.Contains(strings.ToLower(line), resourceLower) {
			continue
		}
		ts := timelineTimestampRe.FindString(line)
		if ts == "" {
			continue
		}
		parsed, parsedOK := parseTimelineTimestamp(ts)
		events = append(events, timelineEvent{
			Timestamp: ts,
			Operation: timelineOperationRe.FindString(line),
			Status:    timelineStatusRe.FindString(line),
			Line:      capTimelineLine(strings.TrimSpace(line)),
			parsed:    parsed,
			parsedOK:  parsedOK,
		})
	}
	return events
}

func parseTimelineTimestamp(raw string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func capTimelineLine(line string) string {
	runes := []rune(line)
	if len(runes) <= timelineLineCap {
		return line
	}
	return string(runes[:timelineLineCap])
}
