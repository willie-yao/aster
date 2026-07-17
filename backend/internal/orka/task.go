package orka

// AITaskSpec is the shared shape for an Orka AI Task created by the dashboard
// analysis pipeline.
type AITaskSpec struct {
	Name         string
	Namespace    string
	Provider     string
	Model        string
	Timeout      string
	MaxRetries   int
	WebhookURL   string
	Tools        []string
	SystemPrompt string
	Prompt       string
	Labels       map[string]string
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
	labels := make(map[string]any, len(in.Labels))
	for k, v := range in.Labels {
		labels[k] = v
	}
	metadata := map[string]any{"name": in.Name, "namespace": in.Namespace}
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
