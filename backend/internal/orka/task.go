package orka

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AITaskSpec is the shared shape for an Orka AI Task created by the dashboard
// analysis pipeline.
type AITaskSpec struct {
	Name         string
	Namespace    string
	Provider     string
	Model        string
	APIMode      string
	Timeout      string
	MaxRetries   int
	WebhookURL   string
	Tools        []string
	SystemPrompt string
	Prompt       string
	Labels       map[string]string
	Execution    map[string]any
}

// ParseTaskExecution parses the supported Task.spec.execution placement fields.
func ParseTaskExecution(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var execution map[string]any
	if err := json.Unmarshal([]byte(raw), &execution); err != nil {
		return nil, fmt.Errorf("parse Task execution JSON: %w", err)
	}
	if execution == nil {
		return nil, fmt.Errorf("task execution must be a JSON object")
	}
	for name, value := range execution {
		switch name {
		case "nodeSelector":
			selector, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("task execution nodeSelector must be an object")
			}
			for key, selected := range selector {
				if _, ok := selected.(string); !ok {
					return nil, fmt.Errorf("task execution nodeSelector %q must have a string value", key)
				}
			}
			if len(selector) == 0 {
				delete(execution, name)
			}
		case "tolerations":
			tolerations, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("task execution tolerations must be an array")
			}
			for i, toleration := range tolerations {
				if _, ok := toleration.(map[string]any); !ok {
					return nil, fmt.Errorf("task execution tolerations[%d] must be an object", i)
				}
			}
			if len(tolerations) == 0 {
				delete(execution, name)
			}
		case "affinity":
			affinity, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("task execution affinity must be an object")
			}
			if len(affinity) == 0 {
				delete(execution, name)
			}
		default:
			return nil, fmt.Errorf("unsupported task execution field %q", name)
		}
	}
	if len(execution) == 0 {
		return nil, nil
	}
	return execution, nil
}

// BuildAITask builds an unstructured core.orka.ai/v1alpha1 Task.
func BuildAITask(in AITaskSpec) map[string]any {
	aiSpec := map[string]any{
		"providerRef":  map[string]any{"name": in.Provider},
		"model":        in.Model,
		"systemPrompt": in.SystemPrompt,
		"prompt":       in.Prompt,
	}
	if len(in.Tools) > 0 {
		aiSpec["tools"] = in.Tools
	}
	spec := map[string]any{
		"type":    "ai",
		"timeout": in.Timeout,
		"ai":      aiSpec,
	}
	if in.MaxRetries > 0 {
		spec["retryPolicy"] = map[string]any{"maxRetries": in.MaxRetries}
	}
	if in.WebhookURL != "" {
		spec["webhookURL"] = in.WebhookURL
	}
	if len(in.Execution) > 0 {
		spec["execution"] = in.Execution
	}
	labels := make(map[string]any, len(in.Labels))
	for k, v := range in.Labels {
		labels[k] = v
	}
	metadata := map[string]any{"name": in.Name, "namespace": in.Namespace}
	if in.APIMode != "" {
		metadata["annotations"] = map[string]any{APIModeAnnotation: in.APIMode}
	}
	if len(labels) > 0 {
		metadata["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata":   metadata,
		"spec":       spec,
	}
}
