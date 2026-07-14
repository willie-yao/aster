package filesystem

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

func dispatchTimeline(t *testing.T, env *tools.Env, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res := (&timelineTool{}).Dispatch(context.Background(), env, raw)
	return res.Payload
}

func timelineEventsOf(t *testing.T, payload map[string]interface{}) []map[string]interface{} {
	t.Helper()
	if e, ok := payload["error"]; ok {
		t.Fatalf("unexpected error payload: %v", e)
	}
	rawEvents, ok := payload["events"].([]map[string]interface{})
	if !ok {
		t.Fatalf("events missing or wrong type: %#v", payload["events"])
	}
	return rawEvents
}

// TestTimelineOrdersLineLogByTimestamp checks a plain per-line log is returned in
// chronological order regardless of input order.
func TestTimelineOrdersLineLogByTimestamp(t *testing.T) {
	log := "2026-07-04T02:41:44Z deleting rule\n" +
		"2026-07-04T02:41:20Z creating rule\n" +
		"no timestamp here\n" +
		"2026-07-04T02:41:35Z creating rule two\n"
	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte(log)}}}

	payload := dispatchTimeline(t, env, map[string]interface{}{"path": "build-log.txt"})
	events := timelineEventsOf(t, payload)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %#v", len(events), events)
	}
	want := []string{"2026-07-04T02:41:20Z", "2026-07-04T02:41:35Z", "2026-07-04T02:41:44Z"}
	for i, w := range want {
		if got := events[i]["timestamp"]; got != w {
			t.Errorf("event %d timestamp = %v, want %v", i, got, w)
		}
	}
	if payload["first"] != want[0] || payload["last"] != want[len(want)-1] {
		t.Errorf("first/last = %v/%v, want %v/%v", payload["first"], payload["last"], want[0], want[len(want)-1])
	}
}

// TestTimelineResourceFilterLineLog keeps only lines containing the resource.
func TestTimelineResourceFilterLineLog(t *testing.T) {
	log := "2026-07-04T02:41:20Z touch alpha\n2026-07-04T02:41:35Z touch beta\n"
	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{"log": []byte(log)}}}
	payload := dispatchTimeline(t, env, map[string]interface{}{"path": "log", "resource": "ALPHA"})
	events := timelineEventsOf(t, payload)
	if len(events) != 1 || events[0]["timestamp"] != "2026-07-04T02:41:20Z" {
		t.Fatalf("resource filter wrong: %#v", events)
	}
}

// TestTimelineParsesJSONActivityRecords is the key case: a cloud activity log is
// a stream of concatenated pretty-printed JSON records where the timestamp and
// the resource name are on different lines. A resource filter must match the
// whole record, and events must be ordered by eventTimestamp.
func TestTimelineParsesJSONActivityRecords(t *testing.T) {
	rec := func(ts, resource, action, status string) string {
		b, _ := json.MarshalIndent(map[string]any{
			"eventTimestamp": ts,
			"resourceId":     "/subscriptions/x/providers/Microsoft.Network/networkSecurityGroups/" + resource,
			"authorization":  map[string]any{"action": action},
			"status":         map[string]any{"value": status},
		}, "", "    ")
		return string(b)
	}
	// Concatenated (not a JSON array), deliberately out of chronological order,
	// and one unrelated record that the resource filter must drop.
	content := rec("2026-07-04T02:41:35.8Z", "test-security-rule", "Microsoft.Network/networkSecurityGroups/write", "Accepted") +
		"\n" + rec("2026-07-04T02:41:20.6Z", "test-security-rule", "Microsoft.Network/networkSecurityGroups/write", "Started") +
		"\n" + rec("2026-07-04T02:41:30.0Z", "other-thing", "Microsoft.Compute/disks/write", "Succeeded")

	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{"act.log": []byte(content)}}}
	payload := dispatchTimeline(t, env, map[string]interface{}{"path": "act.log", "resource": "test-security-rule"})
	events := timelineEventsOf(t, payload)
	if len(events) != 2 {
		t.Fatalf("want 2 filtered records, got %d: %#v", len(events), events)
	}
	if events[0]["timestamp"] != "2026-07-04T02:41:20.6Z" || events[1]["timestamp"] != "2026-07-04T02:41:35.8Z" {
		t.Errorf("records not ordered by eventTimestamp: %#v", events)
	}
	if events[0]["operation"] != "Microsoft.Network/networkSecurityGroups/write" {
		t.Errorf("operation not extracted from authorization.action: %v", events[0]["operation"])
	}
	if events[0]["status"] != "Started" {
		t.Errorf("status not extracted from status.value: %v", events[0]["status"])
	}
}

// TestTimelineJSONArrayShape handles a top-level JSON array of records.
func TestTimelineJSONArrayShape(t *testing.T) {
	content := `[{"eventTimestamp":"2026-07-04T02:00:02Z","resourceId":"nsg/rule-a"},{"eventTimestamp":"2026-07-04T02:00:01Z","resourceId":"nsg/rule-a"}]`
	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{"a.json": []byte(content)}}}
	payload := dispatchTimeline(t, env, map[string]interface{}{"path": "a.json", "resource": "rule-a"})
	events := timelineEventsOf(t, payload)
	if len(events) != 2 || events[0]["timestamp"] != "2026-07-04T02:00:01Z" {
		t.Fatalf("array shape not ordered/parsed: %#v", events)
	}
}

func TestTimelineNoMatchesReturnsEmpty(t *testing.T) {
	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{"log": []byte("2026-07-04T02:00:00Z hello\n")}}}
	payload := dispatchTimeline(t, env, map[string]interface{}{"path": "log", "resource": "absent"})
	if got := payload["count"]; got != 0 {
		t.Fatalf("want count 0, got %v", got)
	}
}

func TestTimelineMissingPathErrors(t *testing.T) {
	env := &tools.Env{Browser: &fakeBrowser{files: map[string][]byte{}}}
	if _, ok := dispatchTimeline(t, env, map[string]interface{}{"path": "nope"})["error"]; !ok {
		t.Fatal("want error payload for missing file")
	}
	if _, ok := dispatchTimeline(t, env, map[string]interface{}{})["error"]; !ok {
		t.Fatal("want error payload for empty path")
	}
}
