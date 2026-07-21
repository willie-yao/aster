package orka

import (
	"strings"
	"testing"
)

func TestBuildAITask(t *testing.T) {
	execution := map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}
	task := BuildAITask(AITaskSpec{
		Name: "analysis-1", Namespace: "orka-system", Provider: "models", Model: "test-model", APIMode: APIModeResponses,
		Timeout: "5m", MaxRetries: 2, Tools: []string{"read-artifact"},
		SystemPrompt: "system", Prompt: "user", Labels: map[string]string{"app": "test"},
		Execution: execution,
	})
	meta := task["metadata"].(map[string]any)
	if meta["name"] != "analysis-1" || meta["namespace"] != "orka-system" {
		t.Fatalf("metadata = %+v", meta)
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations[APIModeAnnotation] != APIModeResponses {
		t.Fatalf("annotations = %+v", annotations)
	}
	spec := task["spec"].(map[string]any)
	aiSpec := spec["ai"].(map[string]any)
	if aiSpec["model"] != "test-model" || len(aiSpec["tools"].([]string)) != 1 {
		t.Fatalf("ai spec = %+v", aiSpec)
	}
	if spec["retryPolicy"].(map[string]any)["maxRetries"] != 2 {
		t.Fatalf("retry policy = %+v", spec["retryPolicy"])
	}
	if spec["execution"].(map[string]any)["nodeSelector"].(map[string]any)["agentpool"] != "cpu" {
		t.Fatalf("execution = %+v", spec["execution"])
	}
}

func TestBuildAITaskOmitsEmptyExecution(t *testing.T) {
	task := BuildAITask(AITaskSpec{Name: "analysis-1", Namespace: "orka-system"})
	if _, ok := task["spec"].(map[string]any)["execution"]; ok {
		t.Fatal("empty execution was included")
	}
}

func TestParseTaskExecution(t *testing.T) {
	raw := `{
		"nodeSelector":{"agentpool":"cpu"},
		"tolerations":[{"key":"dedicated","operator":"Equal","value":"orka","effect":"NoSchedule"}],
		"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[]}}}
	}`
	got, err := ParseTaskExecution(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got["nodeSelector"].(map[string]any)["agentpool"] != "cpu" || len(got["tolerations"].([]any)) != 1 {
		t.Fatalf("execution = %+v", got)
	}
	if got["affinity"].(map[string]any)["nodeAffinity"] == nil {
		t.Fatalf("affinity = %+v", got["affinity"])
	}
}

func TestParseTaskExecutionDropsEmptyFields(t *testing.T) {
	got, err := ParseTaskExecution(`{"nodeSelector":{},"tolerations":[],"affinity":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("execution = %+v, want nil", got)
	}
}

func TestParseTaskExecutionRejectsUnsupportedShapes(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`{"runtimeClassName":"kata"}`,
		`{"nodeSelector":{"agentpool":1}}`,
		`{"tolerations":{}}`,
		`{"tolerations":["bad"]}`,
		`{"affinity":[]}`,
	} {
		t.Run(strings.ReplaceAll(raw, "/", "_"), func(t *testing.T) {
			if _, err := ParseTaskExecution(raw); err == nil {
				t.Fatalf("ParseTaskExecution(%s) succeeded", raw)
			}
		})
	}
}
