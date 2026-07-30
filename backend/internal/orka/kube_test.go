package orka

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTaskStateFromObjectReadsAttempts(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "task", "resourceVersion": "rv"},
		"spec":     map[string]any{"execution": map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}},
		"status": map[string]any{
			"phase": "Running", "attempts": int64(2),
			"resultRef": map[string]any{"available": true}, "completionTime": "2026-07-30T18:00:00Z",
		},
	}}
	state, err := taskStateFromObject(object)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Phase != "Running" || state.Attempts != 2 || state.ResourceVersion != "rv" || !state.ResultAvailable || !state.CompletionTime.Equal(time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("Task state = %+v", state)
	}
}

func TestTaskStateFromObjectRejectsInvalidResultStatus(t *testing.T) {
	for _, status := range []map[string]any{
		{"resultRef": map[string]any{"available": "yes"}},
		{"completionTime": "not-a-time"},
	} {
		object := &unstructured.Unstructured{Object: map[string]any{"status": status}}
		if _, err := taskStateFromObject(object); err == nil {
			t.Fatalf("status accepted: %+v", status)
		}
	}
}

func TestTaskStateFromObjectRejectsInvalidAttempts(t *testing.T) {
	for _, attempts := range []any{int64(-1), "two"} {
		object := &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"phase": "Running", "attempts": attempts},
		}}
		if _, err := taskStateFromObject(object); err == nil {
			t.Fatalf("attempts %v accepted", attempts)
		}
	}
}
