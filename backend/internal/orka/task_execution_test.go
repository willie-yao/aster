package orka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeTaskExecutionClient struct {
	state                  TaskState
	stateSequence          []TaskState
	deleted                bool
	remainAfterDel         bool
	conflictState          *TaskState
	replacementState       *TaskState
	deleteCalls            int
	deletedResourceVersion string
}

func (f *fakeTaskExecutionClient) TaskState(context.Context, string, string) (TaskState, error) {
	if len(f.stateSequence) > 0 {
		state := f.stateSequence[0]
		f.stateSequence = f.stateSequence[1:]
		return state, nil
	}
	if f.deleted {
		if f.replacementState != nil {
			return *f.replacementState, nil
		}
		if !f.remainAfterDel {
			return TaskState{}, nil
		}
	}
	return f.state, nil
}

func (f *fakeTaskExecutionClient) DeleteTask(_ context.Context, _, name, resourceVersion string) error {
	f.deleteCalls++
	f.deletedResourceVersion = resourceVersion
	if f.conflictState != nil {
		f.state = *f.conflictState
		f.conflictState = nil
		return apierrors.NewConflict(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, name, errors.New("resourceVersion changed"))
	}
	f.deleted = true
	return nil
}

func TestPrepareTaskExecution(t *testing.T) {
	cpu := map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}
	gpu := map[string]any{"nodeSelector": map[string]any{"agentpool": "gpu"}}
	for _, tc := range []struct {
		name       string
		state      TaskState
		desired    map[string]any
		wantSkip   bool
		wantDelete bool
	}{
		{name: "missing", state: TaskState{}, desired: cpu},
		{name: "same failed", state: TaskState{Exists: true, Phase: "Failed", Execution: cpu}, desired: cpu},
		{name: "changed succeeded", state: TaskState{Exists: true, Phase: "Succeeded", Execution: gpu}, desired: cpu, wantSkip: true},
		{name: "changed failed", state: TaskState{Exists: true, Phase: "Failed", Execution: gpu, ResourceVersion: "1", UID: "uid-1"}, desired: cpu, wantDelete: true},
		{name: "changed running", state: TaskState{Exists: true, Phase: "Running", Execution: gpu, ResourceVersion: "1", UID: "uid-1"}, desired: cpu, wantDelete: true},
		{name: "placement removed", state: TaskState{Exists: true, Phase: "Pending", Execution: gpu, ResourceVersion: "1", UID: "uid-1"}, wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeTaskExecutionClient{state: tc.state}
			skip, err := PrepareTaskExecution(context.Background(), client, "orka-system", "task", tc.desired, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if skip != tc.wantSkip || client.deleted != tc.wantDelete {
				t.Fatalf("skip=%v deleted=%v, want skip=%v deleted=%v", skip, client.deleted, tc.wantSkip, tc.wantDelete)
			}
			if tc.wantDelete && client.deletedResourceVersion != "1" {
				t.Fatalf("deleted resourceVersion = %q, want 1", client.deletedResourceVersion)
			}
		})
	}
}

func TestPrepareTaskExecutionPreservesConcurrentSuccess(t *testing.T) {
	oldExecution := map[string]any{"nodeSelector": map[string]any{"pool": "old"}}
	newExecution := map[string]any{"nodeSelector": map[string]any{"pool": "new"}}
	succeeded := TaskState{Exists: true, Phase: "Succeeded", Execution: oldExecution, ResourceVersion: "2", UID: "uid-1"}
	client := &fakeTaskExecutionClient{
		state:         TaskState{Exists: true, Phase: "Running", Execution: oldExecution, ResourceVersion: "1", UID: "uid-1"},
		conflictState: &succeeded,
	}
	skip, err := PrepareTaskExecution(context.Background(), client, "orka-system", "task", newExecution, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !skip || client.deleted || client.deleteCalls != 1 || client.deletedResourceVersion != "1" {
		t.Fatalf("skip=%v deleted=%v calls=%d resourceVersion=%q", skip, client.deleted, client.deleteCalls, client.deletedResourceVersion)
	}
}

func TestPrepareTaskExecutionRecognizesReplacementUID(t *testing.T) {
	oldExecution := map[string]any{"nodeSelector": map[string]any{"pool": "old"}}
	newExecution := map[string]any{"nodeSelector": map[string]any{"pool": "new"}}
	replacement := TaskState{Exists: true, Phase: "Pending", Execution: newExecution, ResourceVersion: "1", UID: "uid-2"}
	client := &fakeTaskExecutionClient{
		state:            TaskState{Exists: true, Phase: "Running", Execution: oldExecution, ResourceVersion: "1", UID: "uid-1"},
		replacementState: &replacement,
	}
	skip, err := PrepareTaskExecution(context.Background(), client, "orka-system", "task", newExecution, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if skip || !client.deleted || client.deleteCalls != 1 {
		t.Fatalf("skip=%v deleted=%v calls=%d", skip, client.deleted, client.deleteCalls)
	}
}

func TestPrepareTaskExecutionWaitsForTerminatingSuccess(t *testing.T) {
	oldExecution := map[string]any{"nodeSelector": map[string]any{"pool": "old"}}
	newExecution := map[string]any{"nodeSelector": map[string]any{"pool": "new"}}
	client := &fakeTaskExecutionClient{stateSequence: []TaskState{
		{Exists: true, Phase: "Succeeded", Execution: oldExecution, ResourceVersion: "2", UID: "uid-1", Deleting: true},
		{},
	}}
	skip, err := PrepareTaskExecution(context.Background(), client, "orka-system", "task", newExecution, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if skip || client.deleteCalls != 0 {
		t.Fatalf("skip=%v deleteCalls=%d, want replacement after terminating success", skip, client.deleteCalls)
	}
}

func TestPrepareTaskExecutionDeletionTimeout(t *testing.T) {
	client := &fakeTaskExecutionClient{
		state:          TaskState{Exists: true, Phase: "Running", Execution: map[string]any{"nodeSelector": map[string]any{"pool": "old"}}, ResourceVersion: "1", UID: "uid-1"},
		remainAfterDel: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := PrepareTaskExecution(ctx, client, "orka-system", "task", map[string]any{"nodeSelector": map[string]any{"pool": "new"}}, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error = %v, want deletion timeout", err)
	}
}

func TestTaskExecutionEqualNormalizesJSONNumbers(t *testing.T) {
	a := map[string]any{"tolerations": []any{map[string]any{"tolerationSeconds": int64(300)}}}
	b := map[string]any{"tolerations": []any{map[string]any{"tolerationSeconds": float64(300)}}}
	if !taskExecutionEqual(a, b) {
		t.Fatalf("executions should compare equal: %#v %#v", a, b)
	}
}

func TestTaskStateFromObject(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"resourceVersion": "7", "uid": "uid-7", "deletionTimestamp": "2026-07-20T22:00:00Z",
		},
		"spec":   map[string]any{"execution": map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}},
		"status": map[string]any{"phase": "Running"},
	}}
	state, err := taskStateFromObject(u)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Phase != "Running" || state.ResourceVersion != "7" || state.UID != "uid-7" || !state.Deleting || state.Execution["nodeSelector"].(map[string]any)["agentpool"] != "cpu" {
		t.Fatalf("state = %+v", state)
	}
}
