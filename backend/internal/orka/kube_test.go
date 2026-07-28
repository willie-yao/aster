package orka

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTaskStateFromObjectReadsAttempts(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "task", "resourceVersion": "rv"},
		"spec":     map[string]any{"execution": map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}},
		"status":   map[string]any{"phase": "Running", "attempts": int64(2)},
	}}
	state, err := taskStateFromObject(object)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Phase != "Running" || state.Attempts != 2 || state.ResourceVersion != "rv" {
		t.Fatalf("Task state = %+v", state)
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
