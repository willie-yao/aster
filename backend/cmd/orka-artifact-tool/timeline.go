package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	timelineReadChunk = 64 * 1024
	timelineReadCap   = 4 * 1024 * 1024
	timelineEventCap  = 200
)

var (
	timelineTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	timelineOperationRe = regexp.MustCompile(`(?i)(PUT|DELETE|GET|POST|PATCH|write|delete|create|update|scale)`)
	timelineStatusRe    = regexp.MustCompile(`(?i)(Succeeded|Failed|Accepted|Started|provisioningState["':= ]+\w+|Updating|Created|Deleting)`)
)

func init() {
	registerQTool("/tool/verify_timeline", verifyTimeline)
}

func verifyTimeline(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Path     string `json:"path"`
		Resource string `json:"resource"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if args.Path == "" {
		writeJSON(w, map[string]any{"error": "path is required"})
		return
	}

	ctx, cancel := requestCtx(r)
	defer cancel()

	content, err := readTimelineFile(ctx, env, args.Path)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
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

	log.Printf("⏱ verify_timeline path=%s resource=%q events=%d", args.Path, args.Resource, len(events))
	writeJSON(w, map[string]any{
		"path":      args.Path,
		"resource":  args.Resource,
		"count":     len(events),
		"first":     first,
		"last":      last,
		"events":    events,
		"truncated": truncated,
	})
}

func readTimelineFile(ctx context.Context, env *toolEnv, path string) ([]byte, error) {
	var content []byte
	for offset := 0; offset < timelineReadCap; {
		want := min(timelineReadChunk, timelineReadCap-offset)
		chunk, total, err := env.browser.Read(ctx, path, offset, want)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
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
	Timestamp string `json:"timestamp"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	Line      string `json:"line"`

	parsed   time.Time
	parsedOK bool
}

// timelineEvents extracts ordered events from a log. Azure activity logs are
// pretty-printed JSON (one record spans many lines, so the timestamp and the
// resource name are on different lines); those are parsed as JSON records so a
// resource filter can match the whole record. Everything else falls back to a
// per-line scan (build logs, kubelet logs, ...).
func timelineEvents(content []byte, resource string) []timelineEvent {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		if events, ok := timelineEventsFromJSON(trimmed, resource); ok {
			return events
		}
	}
	return timelineEventsFromLines(content, resource)
}

// timelineEventsFromJSON parses a JSON array (or single object) of records and
// builds one event per record, associating each record's timestamp with the
// whole record for the resource filter. Returns ok=false if the content is not
// parseable as JSON records so the caller can fall back to the per-line scan.
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
			// No dedicated timestamp field; try any timestamp anywhere in the record.
			ts = timelineTimestampRe.FindString(string(raw))
		}
		if ts == "" {
			continue
		}
		parsed, parsedOK := parseTimelineTimestamp(ts)
		op := jsonFieldString(rec, "operationName", "OperationName", "authorization", "Authorization")
		status := jsonFieldString(rec, "status", "Status", "subStatus", "SubStatus")
		compact := strings.Join(strings.Fields(string(raw)), " ")
		events = append(events, timelineEvent{
			Timestamp: ts,
			Operation: capTimelineLine(strings.TrimSpace(op)),
			Status:    strings.TrimSpace(status),
			Line:      capTimelineLine(compact),
			parsed:    parsed,
			parsedOK:  parsedOK,
		})
	}
	return events, true
}

// decodeJSONRecords reads a JSON value stream into a flat list of record objects.
// It handles a top-level array ([{...},{...}]), a whitespace-concatenated stream
// of objects ({...}{...} or NDJSON), and a single object. Returns ok=false when
// nothing decoded, so the caller can fall back to the per-line scan.
func decodeJSONRecords(content []byte) ([]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(content)))
	var out []json.RawMessage
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			break // stop at the first malformed record; keep what we have
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

// jsonFieldString returns a readable string for the first present key. It digs
// one level into nested objects for the common Azure shapes
// ({"action": ...} / {"value": ...} / {"localizedValue": ...}).
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
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05.999999999Z0700",
		"2006-01-02 15:04:05Z0700",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
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
	if len(runes) <= 400 {
		return line
	}
	return string(runes[:400])
}
