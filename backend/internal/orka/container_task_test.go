package orka

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func containerTaskProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	config := `id: analyzer-test
name: Analyzer Test
testgrid:
  dashboard: analyzer-test
storage:
  provider: local
  base: /fixtures
branding:
  title: Analyzer Test
  base_path: /analyzer-test
  site_url: https://example.invalid/analyzer-test
  source_repo:
    owner: example
    name: project
ai:
  endpoint: https://private-model.invalid/v1/chat/completions?token=secret
  model: private-model
  tools: [filesystem]
  min_tool_calls: 2
`
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func containerTaskSpec(t *testing.T) ContainerAnalysisTaskSpec {
	t.Helper()
	return ContainerAnalysisTaskSpec{
		Namespace:  "orka-system",
		NamePrefix: "flatcar-analyzer",
		Image:      "dashboard-analyzer:sha",
		Command:    []string{"/usr/local/bin/analyzer"},
		Args:       []string{"-data-dir=/tmp/analyzer"},
		Timeout:    "5m",
		MaxRetries: 1,
		ProjectDir: containerTaskProject(t),
		Request:    containerTaskRequest(),
		Environment: map[string]string{
			"AI_API":      "chat_completions",
			"AI_ENDPOINT": "https://model.invalid/v1/chat/completions",
			"AI_MODEL":    "script-model",
		},
		Labels: map[string]string{
			"prow-ai-dashboard/test":    "bundle",
			"prow-ai-dashboard/adapter": "wrong",
		},
		SecretEnv: []SecretEnvVar{
			{Name: "AI_TOKEN", SecretName: "analyzer-model", SecretKey: "token"},
			{Name: analysisruntime.ContainerStateKeyEnv, SecretName: "analyzer-model", SecretKey: "state-key"},
		},
	}
}

func TestBuildContainerAnalysisResources(t *testing.T) {
	resources, err := BuildContainerAnalysisResources(containerTaskSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	task := resources.Task
	configMap := resources.BundleConfigMap
	if task["apiVersion"] != "core.orka.ai/v1alpha1" || task["kind"] != "Task" {
		t.Fatalf("task type metadata = %+v", task)
	}
	metadata := task["metadata"].(map[string]any)
	if !strings.HasPrefix(metadata["name"].(string), "flatcar-analyzer-") || metadata["namespace"] != "orka-system" {
		t.Fatalf("metadata = %+v", metadata)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations["prow-ai-dashboard/contract-version"] != ContainerAnalysisContractVersion {
		t.Fatalf("annotations = %+v", annotations)
	}
	configMetadata := configMap["metadata"].(map[string]any)
	if configMap["apiVersion"] != "v1" || configMap["kind"] != "ConfigMap" || configMap["immutable"] != true {
		t.Fatalf("bundle ConfigMap = %+v", configMap)
	}
	bundleName := configMetadata["name"].(string)
	if bundleName != metadata["name"].(string)+"-input" || len(bundleName) > 63 || configMetadata["namespace"] != "orka-system" {
		t.Fatalf("bundle ConfigMap metadata = %+v", configMetadata)
	}
	bundleLabels := configMetadata["labels"].(map[string]any)
	taskLabels := metadata["labels"].(map[string]any)
	for _, labels := range []map[string]any{bundleLabels, taskLabels} {
		if labels["prow-ai-dashboard/test"] != "bundle" || labels["prow-ai-dashboard/adapter"] != "container-analyzer" {
			t.Fatalf("resource labels = %+v", labels)
		}
	}
	if bundleLabels[containerAnalysisBundleLabel] != "true" {
		t.Fatalf("bundle labels = %+v", bundleLabels)
	}
	if _, ok := taskLabels[containerAnalysisBundleLabel]; ok {
		t.Fatalf("Task labels include bundle retention selector: %+v", taskLabels)
	}
	bundleJSON := configMap["data"].(map[string]any)[analysisruntime.ProjectBundleConfigMapKey].(string)
	bundle, err := analysisruntime.DecodeProjectBundle([]byte(bundleJSON))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Digest != annotations["prow-ai-dashboard/bundle-digest"] || bundle.ContractVersion != ContainerAnalysisContractVersion {
		t.Fatalf("bundle identity = %+v, annotations = %+v", bundle, annotations)
	}
	for _, forbidden := range []string{"private-model.invalid", "private-model", "token=secret", "AI_TOKEN"} {
		if strings.Contains(bundleJSON, forbidden) {
			t.Fatalf("bundle contains forbidden %q", forbidden)
		}
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
	if !reflect.DeepEqual(spec["command"], []string{"/usr/local/bin/analyzer"}) || len(spec["args"].([]string)) != 1 {
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
	var digestValue string
	bundleRefFound := false
	secretFound := false
	stateSecretFound := false
	derived := map[string]string{}
	for _, raw := range env {
		entry := raw.(map[string]any)
		switch entry["name"] {
		case analysisruntime.ProjectBundleEnv:
			ref := entry["valueFrom"].(map[string]any)["configMapKeyRef"].(map[string]any)
			bundleRefFound = ref["name"] == configMetadata["name"] && ref["key"] == analysisruntime.ProjectBundleConfigMapKey
			if _, ok := entry["value"]; ok {
				t.Fatal("project bundle was inlined into the Task")
			}
		case analysisruntime.ProjectBundleDigestEnv:
			digestValue, _ = entry["value"].(string)
		case "AI_TOKEN":
			secret := entry["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			secretFound = secret["name"] == "analyzer-model" && secret["key"] == "token"
			if _, ok := entry["value"]; ok {
				t.Fatal("AI_TOKEN was inlined instead of using a Secret reference")
			}
		case analysisruntime.ContainerStateKeyEnv:
			secret := entry["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			stateSecretFound = secret["name"] == "analyzer-model" && secret["key"] == "state-key"
		case analysisruntime.ContainerTaskNamespaceEnv, analysisruntime.ContainerTaskNameEnv, analysisruntime.ContainerContractVersionEnv:
			derived[entry["name"].(string)] = entry["value"].(string)
		}
	}
	if !bundleRefFound || digestValue != bundle.Digest || !secretFound || !stateSecretFound {
		t.Fatalf("env = %+v", env)
	}
	if derived[analysisruntime.ContainerTaskNamespaceEnv] != "orka-system" || derived[analysisruntime.ContainerTaskNameEnv] != metadata["name"] || derived[analysisruntime.ContainerContractVersionEnv] != ContainerAnalysisContractVersion {
		t.Fatalf("derived env = %+v", derived)
	}
}

func TestContainerAnalysisResourceIdentityChangesWithInputs(t *testing.T) {
	base, err := BuildContainerAnalysisResources(containerTaskSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	baseTask := base.Task["metadata"].(map[string]any)["name"]
	baseBundle := base.BundleConfigMap["metadata"].(map[string]any)["name"]
	mutations := []struct {
		name         string
		changeBundle bool
		mutate       func(*ContainerAnalysisTaskSpec)
	}{
		{name: "request", changeBundle: true, mutate: func(spec *ContainerAnalysisTaskSpec) { spec.Request.TestCase.FailureMessage = "changed" }},
		{name: "image", changeBundle: true, mutate: func(spec *ContainerAnalysisTaskSpec) { spec.Image = "dashboard-analyzer:other" }},
		{name: "cache seed", changeBundle: true, mutate: func(spec *ContainerAnalysisTaskSpec) {
			key := analysisruntime.FailureCacheKey(spec.Request)
			spec.CacheSeed = map[string]ai.CacheEntry{key: {Key: key, CreatedAt: time.Now().UTC(), Data: json.RawMessage(`{"cached":true}`)}}
		}},
		{name: "prompt", changeBundle: true, mutate: func(spec *ContainerAnalysisTaskSpec) {
			if err := os.WriteFile(filepath.Join(spec.ProjectDir, "prompts", "system.md"), []byte("Changed prompt.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			spec := containerTaskSpec(t)
			mutation.mutate(&spec)
			resources, err := BuildContainerAnalysisResources(spec)
			if err != nil {
				t.Fatal(err)
			}
			taskName := resources.Task["metadata"].(map[string]any)["name"]
			bundleName := resources.BundleConfigMap["metadata"].(map[string]any)["name"]
			if taskName == baseTask {
				t.Fatalf("%s mutation kept Task identity %q", mutation.name, taskName)
			}
			if got := bundleName != baseBundle; got != mutation.changeBundle {
				t.Fatalf("%s mutation bundle changed=%t, want %t", mutation.name, got, mutation.changeBundle)
			}
		})
	}
}

func TestBuildContainerAnalysisResourcesRequiresProviderAndToken(t *testing.T) {
	for name, mutate := range map[string]func(*ContainerAnalysisTaskSpec){
		"api":      func(spec *ContainerAnalysisTaskSpec) { delete(spec.Environment, "AI_API") },
		"endpoint": func(spec *ContainerAnalysisTaskSpec) { delete(spec.Environment, "AI_ENDPOINT") },
		"model":    func(spec *ContainerAnalysisTaskSpec) { delete(spec.Environment, "AI_MODEL") },
		"token": func(spec *ContainerAnalysisTaskSpec) {
			spec.SecretEnv = spec.SecretEnv[1:]
		},
		"state key": func(spec *ContainerAnalysisTaskSpec) {
			spec.SecretEnv = spec.SecretEnv[:1]
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := containerTaskSpec(t)
			mutate(&spec)
			if _, err := BuildContainerAnalysisResources(spec); err == nil || !strings.Contains(err.Error(), "requires") {
				t.Fatalf("BuildContainerAnalysisResources error = %v", err)
			}
		})
	}
	spec := containerTaskSpec(t)
	spec.Environment["AI_API"] = "invalid"
	if _, err := BuildContainerAnalysisResources(spec); err == nil || !strings.Contains(err.Error(), "AI API") {
		t.Fatalf("BuildContainerAnalysisResources invalid API error = %v", err)
	}
}

func TestBuildContainerAnalysisResourcesNormalizesProviderEnvironment(t *testing.T) {
	spec := containerTaskSpec(t)
	spec.Environment["AI_API"] = " Responses "
	spec.Environment["AI_ENDPOINT"] = "  https://model.invalid/v1/responses  "
	spec.Environment["AI_MODEL"] = "  model-name  "
	resources, err := BuildContainerAnalysisResources(spec)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Environment["AI_ENDPOINT"] != "  https://model.invalid/v1/responses  " {
		t.Fatal("BuildContainerAnalysisResources mutated the caller environment")
	}
	got := map[string]string{}
	for _, raw := range resources.Task["spec"].(map[string]any)["env"].([]any) {
		entry := raw.(map[string]any)
		if value, ok := entry["value"].(string); ok {
			got[entry["name"].(string)] = value
		}
	}
	want := map[string]string{
		"AI_API": "responses", "AI_ENDPOINT": "https://model.invalid/v1/responses", "AI_MODEL": "model-name",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q", name, got[name], value)
		}
	}
}

func TestBuildContainerAnalysisResourcesRejectsCredentialBearingEndpoints(t *testing.T) {
	for name, endpoint := range map[string]string{
		"relative":        "/v1/chat/completions",
		"scheme":          "ftp://model.invalid/v1/chat/completions",
		"userinfo":        "https://user:secret@model.invalid/v1/chat/completions",
		"token query":     "https://model.invalid/v1/chat/completions?token=secret",
		"other query":     "https://model.invalid/v1/chat/completions?region=west",
		"malformed query": "https://model.invalid/v1/chat/completions?token=secret;foo=bar",
		"fragment":        "https://model.invalid/v1/chat/completions#secret",
	} {
		t.Run(name, func(t *testing.T) {
			spec := containerTaskSpec(t)
			spec.Environment["AI_ENDPOINT"] = endpoint
			if _, err := BuildContainerAnalysisResources(spec); err == nil || !strings.Contains(err.Error(), "AI_ENDPOINT") {
				t.Fatalf("BuildContainerAnalysisResources endpoint error = %v", err)
			}
		})
	}
	spec := containerTaskSpec(t)
	spec.Environment["AI_ENDPOINT"] = "https://example.openai.azure.com/openai/v1/responses?api-version=2025-03-01-preview"
	if _, err := BuildContainerAnalysisResources(spec); err != nil {
		t.Fatalf("Azure API version endpoint rejected: %v", err)
	}
}

func TestBuildContainerAnalysisResourcesRejectsOversizedBundle(t *testing.T) {
	spec := containerTaskSpec(t)
	if err := os.WriteFile(filepath.Join(spec.ProjectDir, "prompts", "system.md"), []byte(strings.Repeat("x", analysisruntime.MaxProjectBundleBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildContainerAnalysisResources(spec); err == nil || !strings.Contains(err.Error(), "environment limit") {
		t.Fatalf("BuildContainerAnalysisResources error = %v", err)
	}
}

func TestParseAndApplyContainerAnalysisResult(t *testing.T) {
	want := ai.FailureAnalysisResult{
		Summary:  &models.AISummary{GeneratedAt: "2026-07-22T12:00:00Z", Summary: "summary"},
		Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic"},
	}
	var framed bytes.Buffer
	if err := analysisruntime.WriteFailureAnalysisResult(&framed, want); err != nil {
		t.Fatal(err)
	}
	got, err := ParseContainerAnalysisResult("runtime log before\n" + framed.String() + "runtime log after\n")
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
		`{"ai_summary":{"summary":"plain JSON is not framed"}}`,
		analysisruntime.FailureAnalysisResultMarker + "not-base64",
	} {
		if _, err := ParseContainerAnalysisResult(raw); err == nil {
			t.Fatalf("ParseContainerAnalysisResult(%q) succeeded", raw)
		}
	}
}

func TestBuildContainerAnalysisResourcesAllowsOnlyKnownSafeInlineEnvironment(t *testing.T) {
	spec := containerTaskSpec(t)
	spec.Environment = map[string]string{
		"AI_API": "chat_completions", "AI_ENDPOINT": "http://model.invalid/v1/chat/completions", "AI_MODEL": "model",
	}
	if _, err := BuildContainerAnalysisResources(spec); err != nil {
		t.Fatalf("safe inline environment rejected: %v", err)
	}
	for _, name := range []string{"AI_TOKEN", "GITHUB_PAT", "PRIVATE_KEY", "OPENAI_APIKEY"} {
		spec := containerTaskSpec(t)
		spec.Environment[name] = "inline-secret"
		if _, err := BuildContainerAnalysisResources(spec); err == nil || !strings.Contains(err.Error(), "Secret reference") {
			t.Fatalf("BuildContainerAnalysisResources(%s) error = %v", name, err)
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
