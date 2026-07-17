package orka

import "testing"

func TestBuildAITask(t *testing.T) {
	task := BuildAITask(AITaskSpec{
		Name: "analysis-1", Namespace: "orka-system", Provider: "models", Model: "test-model",
		Timeout: "5m", MaxRetries: 2, Tools: []string{"read-artifact"},
		SystemPrompt: "system", Prompt: "user", Labels: map[string]string{"app": "test"},
	})
	meta := task["metadata"].(map[string]any)
	if meta["name"] != "analysis-1" || meta["namespace"] != "orka-system" {
		t.Fatalf("metadata = %+v", meta)
	}
	spec := task["spec"].(map[string]any)
	aiSpec := spec["ai"].(map[string]any)
	if aiSpec["model"] != "test-model" || len(aiSpec["tools"].([]string)) != 1 {
		t.Fatalf("ai spec = %+v", aiSpec)
	}
	if spec["retryPolicy"].(map[string]any)["maxRetries"] != 2 {
		t.Fatalf("retry policy = %+v", spec["retryPolicy"])
	}
}
