package orka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// TaskExecutionClient is the Task lifecycle surface used for placement recovery.
type TaskExecutionClient interface {
	TaskState(context.Context, string, string) (TaskState, error)
	DeleteTask(context.Context, string, string, string) error
}

// PrepareTaskExecution removes a non-successful Task when its worker placement
// changed. A successful Task remains reusable and returns skipApply=true.
func PrepareTaskExecution(ctx context.Context, client TaskExecutionClient, namespace, name string, desired map[string]any, poll time.Duration) (skipApply bool, err error) {
	if poll <= 0 {
		poll = time.Second
	}
	for {
		state, err := client.TaskState(ctx, namespace, name)
		if err != nil {
			return false, fmt.Errorf("read Task %s before apply: %w", name, err)
		}
		if !state.Exists {
			return false, nil
		}
		if state.Deleting {
			if state.UID == "" {
				return false, fmt.Errorf("terminating task %s has no UID", name)
			}
			if err := waitForTaskUIDChange(ctx, client, namespace, name, state.UID, poll); err != nil {
				return false, err
			}
			continue
		}
		if taskExecutionEqual(state.Execution, desired) {
			return false, nil
		}
		if state.Phase == "Succeeded" {
			return true, nil
		}
		if state.ResourceVersion == "" || state.UID == "" {
			return false, fmt.Errorf("task %s has incomplete object identity", name)
		}
		if err := client.DeleteTask(ctx, namespace, name, state.ResourceVersion); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return false, fmt.Errorf("delete Task %s after execution change: %w", name, err)
		}
		if err := waitForTaskUIDChange(ctx, client, namespace, name, state.UID, poll); err != nil {
			return false, err
		}
	}
}

func waitForTaskUIDChange(ctx context.Context, client TaskExecutionClient, namespace, name, uid string, poll time.Duration) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		current, err := client.TaskState(ctx, namespace, name)
		if err != nil {
			return fmt.Errorf("wait for Task %s deletion: %w", name, err)
		}
		if !current.Exists || current.UID != uid {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Task %s deletion: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func taskExecutionEqual(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}
