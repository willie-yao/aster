package orka

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func containerTaskRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID:       "periodic-job",
		BuildPrefix: "logs/periodic-job/1/",
		Build:       models.BuildInfo{JobName: "periodic-job", BuildID: "1"},
		TestCase:    models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "boom"},
	}
}

func containerTaskSpec() ContainerAnalysisTaskSpec {
	return ContainerAnalysisTaskSpec{
		Namespace:  "orka-system",
		NamePrefix: "flatcar-analyzer",
		Image:      "dashboard-analyzer:sha",
		Command:    []string{"/usr/local/bin/analyzer"},
		Args:       []string{"-project-dir=/project", "-data-dir=/tmp/analyzer"},
		Timeout:    "5m",
		MaxRetries: 1,
		Request:    containerTaskRequest(),
		SecretEnv: []SecretEnvVar{
			{Name: "AI_TOKEN", SecretName: "analyzer-model", SecretKey: "token"},
		},
	}
}

func TestBuildContainerAnalysisTask(t *testing.T) {
	task, err := BuildContainerAnalysisTask(containerTaskSpec())
	if err != nil {
		t.Fatal(err)
	}
	if task["apiVersion"] != "core.orka.ai/v1alpha1" || task["kind"] != "Task" {
		t.Fatalf("task type metadata = %+v", task)
	}
	metadata := task["metadata"].(map[string]any)
	if !strings.HasPrefix(metadata["name"].(string), "flatcar-analyzer-") || metadata["namespace"] != "orka-system" {
		t.Fatalf("metadata = %+v", metadata)
	}
	spec := task["spec"].(map[string]any)
	if spec["type"] != "container" || spec["image"] != "dashboard-analyzer:sha" {
		t.Fatalf("spec = %+v", spec)
	}
	for _, forbidden := range []string{"ai", "providerRef", "model", "tools", "agentRuntime"} {
		if _, ok := spec[forbidden]; ok {
			t.Fatalf("spec contains forbidden field %q: %+v", forbidden, spec)
		}
	}
	if !reflect.DeepEqual(spec["command"], []string{"/usr/local/bin/analyzer"}) || len(spec["args"].([]string)) != 2 {
		t.Fatalf("command/args = %+v %+v", spec["command"], spec["args"])
	}
	if spec["timeout"] != "5m" || spec["retryPolicy"].(map[string]any)["maxRetries"] != 1 {
		t.Fatalf("timeout/retry = %+v", spec)
	}
	selector := spec["execution"].(map[string]any)["nodeSelector"].(map[string]any)
	if selector["agentpool"] != "nodepool1" {
		t.Fatalf("nodeSelector = %+v", selector)
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"providerRef", `\"model\"`, `\"tools\"`, "orka-ai-worker", "compat"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("task contains forbidden %q: %s", forbidden, text)
		}
	}

	env := spec["env"].([]any)
	var requestValue, digestValue string
	secretFound := false
	for _, raw := range env {
		entry := raw.(map[string]any)
		switch entry["name"] {
		case analysisruntime.InlineRequestEnv:
			requestValue, _ = entry["value"].(string)
		case analysisruntime.InlineRequestDigestEnv:
			digestValue, _ = entry["value"].(string)
		case "AI_TOKEN":
			secret := entry["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			secretFound = secret["name"] == "analyzer-model" && secret["key"] == "token"
			if _, ok := entry["value"]; ok {
				t.Fatal("AI_TOKEN was inlined instead of using a Secret reference")
			}
		}
	}
	if requestValue == "" || digestValue == "" || !secretFound {
		t.Fatalf("env = %+v", env)
	}
	_, digest, err := analysisruntime.DecodeInlineRequest([]byte(requestValue))
	if err != nil || digest != digestValue {
		t.Fatalf("request digest = %q, want %q, error=%v", digest, digestValue, err)
	}
}

func TestContainerAnalysisTaskIdentityChangesWithContract(t *testing.T) {
	base, err := BuildContainerAnalysisTask(containerTaskSpec())
	if err != nil {
		t.Fatal(err)
	}
	baseName := base["metadata"].(map[string]any)["name"]
	mutations := []func(*ContainerAnalysisTaskSpec){
		func(spec *ContainerAnalysisTaskSpec) { spec.Request.TestCase.FailureMessage = "changed" },
		func(spec *ContainerAnalysisTaskSpec) { spec.Image = "dashboard-analyzer:other" },
		func(spec *ContainerAnalysisTaskSpec) { spec.ContractVersion = "v2" },
	}
	for i, mutate := range mutations {
		spec := containerTaskSpec()
		mutate(&spec)
		task, err := BuildContainerAnalysisTask(spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := task["metadata"].(map[string]any)["name"]; got == baseName {
			t.Fatalf("mutation %d kept Task identity %q", i, got)
		}
	}
}

func TestBuildContainerAnalysisTaskRejectsOversizedRequest(t *testing.T) {
	spec := containerTaskSpec()
	spec.Request.TestCase.FailureBody = strings.Repeat("x", analysisruntime.MaxInlineRequestBytes)
	if _, err := BuildContainerAnalysisTask(spec); err == nil || !strings.Contains(err.Error(), "inline limit") {
		t.Fatalf("BuildContainerAnalysisTask error = %v", err)
	}
}

func TestParseAndApplyContainerAnalysisResult(t *testing.T) {
	want := ai.FailureAnalysisResult{
		Summary:  &models.AISummary{GeneratedAt: "2026-07-22T12:00:00Z", Summary: "summary"},
		Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic"},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseContainerAnalysisResult("runtime log on stderr\n" + string(data) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	tc := models.TestCase{Name: "Test A", Status: "failed"}
	if err := ApplyContainerAnalysisResult(&tc, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tc.AISummary, want.Summary) || !reflect.DeepEqual(tc.AIAnalysis, want.Analysis) {
		t.Fatalf("test case = %+v", tc)
	}
}

func TestParseContainerAnalysisResultRejectsMalformedOrAbsentResult(t *testing.T) {
	for _, raw := range []string{
		"",
		"runtime log only\n",
		`{"ai_summary":null}`,
		`{"ai_summary":{}}`,
		`{"ai_summary":{"summary":"   "}}`,
		`{"ai_summary":{"summary":"ok"}} trailing`,
	} {
		if _, err := ParseContainerAnalysisResult(raw); err == nil {
			t.Fatalf("ParseContainerAnalysisResult(%q) succeeded", raw)
		}
	}
}

func TestBuildContainerAnalysisTaskAllowsOnlyKnownSafeInlineEnvironment(t *testing.T) {
	spec := containerTaskSpec()
	spec.Environment = map[string]string{
		"AI_API": "chat_completions", "AI_ENDPOINT": "http://model.invalid/v1/chat/completions", "AI_MODEL": "model",
	}
	if _, err := BuildContainerAnalysisTask(spec); err != nil {
		t.Fatalf("safe inline environment rejected: %v", err)
	}
	for _, name := range []string{"AI_TOKEN", "GITHUB_PAT", "PRIVATE_KEY", "OPENAI_APIKEY"} {
		spec := containerTaskSpec()
		spec.Environment = map[string]string{name: "inline-secret"}
		if _, err := BuildContainerAnalysisTask(spec); err == nil || !strings.Contains(err.Error(), "Secret reference") {
			t.Fatalf("BuildContainerAnalysisTask(%s) error = %v", name, err)
		}
	}
}

func TestApplyContainerAnalysisResultRejectsBlankSummary(t *testing.T) {
	tc := models.TestCase{Name: "Test A", Status: "failed"}
	err := ApplyContainerAnalysisResult(&tc, ai.FailureAnalysisResult{Summary: &models.AISummary{Summary: "  "}})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("ApplyContainerAnalysisResult error = %v", err)
	}
}
