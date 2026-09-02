package onboard

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
	"gopkg.in/yaml.v3"
)

var updateK8sValuesGolden = flag.Bool("update-k8s-values-golden", false, "update the generated Kubernetes values golden file")

var chartEqualGeneratedPaths = []string{
	"global.imageTag", "image.tag", "mode",
	"persistence.enabled", "persistence.existingClaim", "persistence.accessMode", "persistence.size", "persistence.retain",
	"ai.reasoningEffort", "ai.contextWindowTokens", "ai.tokenSecretKey", "ai.githubReadTokenSecretKey",
	"fetcher.buildsPerJob", "fetcher.workers", "fetcher.watchInterval", "fetcher.reconcileInterval",
	"server.chat.enabled", "server.actions.enabled", "server.service.type", "server.service.port",
	"ingress.enabled", "networkPolicy.enabled", "networkPolicy.ingress",
}

func TestK8sValuesGolden(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(true))
	goldenPath := filepath.Join("testdata", "k8s-values.golden.yaml")
	if *updateK8sValuesGolden {
		if err := os.WriteFile(goldenPath, []byte(values), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if values != string(want) {
		t.Fatalf("generated values differ from %s; review the change and run go test ./internal/onboard -run TestK8sValuesGolden -update-k8s-values-golden\n--- generated ---\n%s", goldenPath, values)
	}
}

func TestK8sValuesAreValidYAMLWithoutDuplicateKeys(t *testing.T) {
	parseYAMLMap(t, renderK8sValuesForTest(t, k8sValuesFixtureData(true)))
}

func TestK8sValuesActiveConfiguration(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(true))
	root := parseYAMLMap(t, values)

	for path, want := range map[string]any{
		"persistence.storageClass":     "fixture-rwx",
		"ai.enabled":                   true,
		"ai.api":                       project.AIAPIResponses,
		"ai.endpoint":                  "https://provider.example/v1/responses",
		"ai.model":                     "fixture-model",
		"ai.existingSecret":            "<existing-ai-secret>",
		"ai.githubReadTokenSecretName": "<existing-github-read-secret>",
		"fetcher.timeout":              "120m",
		"fetcher.suspend":              true,
	} {
		assertYAMLValue(t, root, want, strings.Split(path, ".")...)
	}

	for _, path := range chartEqualGeneratedPaths {
		assertYAMLAbsent(t, root, strings.Split(path, ".")...)
	}
}

func TestK8sValuesStorageSelection(t *testing.T) {
	t.Run("storage class", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLAbsent(t, root, "persistence", "existingClaim")
		assertYAMLValue(t, root, "fixture-rwx", "persistence", "storageClass")
	})

	t.Run("existing claim", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		data.K8sStorageClass = ""
		data.K8sExistingClaim = "shared-data"
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLValue(t, root, "shared-data", "persistence", "existingClaim")
		assertYAMLAbsent(t, root, "persistence", "storageClass")
	})

	t.Run("interactive placeholder", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		data.K8sStorageClass = ""
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLAbsent(t, root, "persistence", "existingClaim")
		assertYAMLValue(t, root, "<your-rwx-storage-class>", "persistence", "storageClass")
	})
}

func TestK8sValuesDocumentsChartFallbacks(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(true))
	for _, want := range []string{
		"All omitted settings use the",
		"defaults from the matching published chart version",
		"helm show values oci://ghcr.io/willie-yao/charts/aster",
		"Intentionally longer than the chart default",
		"Keep CronJob starts disabled while the chart-default watch worker is active",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("generated values missing %q\n---\n%s", want, values)
		}
	}
}

func TestK8sValuesDoNotGenerateInlineCredentials(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(true))
	for _, forbidden := range []string{
		"fixture-ai-token",
		"\n  token:",
		"clientSecret:",
		"sessionKey:",
		"botToken:",
		"\n    secret:",
	} {
		if strings.Contains(values, forbidden) {
			t.Errorf("generated values include forbidden credential field or value %q\n---\n%s", forbidden, values)
		}
	}
}

func TestK8sValuesDisabledAIStaysValid(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(false))
	root := parseYAMLMap(t, values)
	assertYAMLValue(t, root, false, "ai", "enabled")
	assertYAMLValue(t, root, "", "ai", "existingSecret")
	assertYAMLValue(t, root, "", "ai", "githubReadTokenSecretName")
	assertYAMLAbsent(t, root, "ai", "tokenSecretKey")
}

func TestK8sValuesDoNotDependOnMutableEngineRef(t *testing.T) {
	first := k8sValuesFixtureData(true)
	first.EngineRef = "main"
	second := first
	second.EngineRef = "feature/scaffold-values"
	firstValues := renderK8sValuesForTest(t, first)
	secondValues := renderK8sValuesForTest(t, second)
	if firstValues != secondValues {
		t.Fatalf("Kubernetes values changed with mutable engine ref")
	}
	if strings.Contains(firstValues, "/main/") || strings.Contains(firstValues, "feature/scaffold") {
		t.Fatalf("Kubernetes values contain a mutable engine-ref URL\n---\n%s", firstValues)
	}
}

func TestK8sValuesChartDefaultClassification(t *testing.T) {
	chartBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "helm", "aster", "values.yaml"))
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	chart := parseYAMLMap(t, string(chartBytes))

	originalGenerated := map[string]any{
		"global.imageTag": "", "image.tag": "", "mode": "watch",
		"persistence.enabled": true, "persistence.existingClaim": "", "persistence.storageClass": "fixture-rwx",
		"persistence.accessMode": "ReadWriteMany", "persistence.size": "1Gi", "persistence.retain": true,
		"ai.enabled": true, "ai.api": project.AIAPIResponses, "ai.endpoint": "https://provider.example/v1/responses",
		"ai.model": "fixture-model", "ai.reasoningEffort": "", "ai.contextWindowTokens": 0,
		"ai.existingSecret": "<existing-ai-secret>", "ai.tokenSecretKey": "AI_TOKEN",
		"ai.githubReadTokenSecretName": "<existing-github-read-secret>", "ai.githubReadTokenSecretKey": "GITHUB_READ_TOKEN",
		"fetcher.buildsPerJob": 10, "fetcher.workers": 5, "fetcher.timeout": "120m",
		"fetcher.watchInterval": "5m", "fetcher.reconcileInterval": "1h", "fetcher.suspend": true,
		"server.chat.enabled": false, "server.actions.enabled": false, "server.service.type": "ClusterIP",
		"server.service.port": 80, "ingress.enabled": false, "networkPolicy.enabled": false,
		"networkPolicy.ingress": []any{},
	}
	type valuePair struct {
		generated any
		chart     any
	}
	fixtureDecisions := map[string]valuePair{
		"persistence.storageClass": {"fixture-rwx", ""},
		"ai.enabled":               {true, false},
		"ai.api":                   {project.AIAPIResponses, project.AIAPIChatCompletions},
		"ai.endpoint":              {"https://provider.example/v1/responses", ""},
		"ai.model":                 {"fixture-model", ""},
		"ai.existingSecret":        {"<existing-ai-secret>", ""},
	}
	intentionalDivergences := map[string]valuePair{
		"fetcher.timeout": {"120m", "30m"},
		"fetcher.suspend": {true, false},
		// Secret references are scaffold placeholders the operator replaces or deletes before install.
		"ai.githubReadTokenSecretName": {"<existing-github-read-secret>", ""},
	}

	counts := map[string]int{}
	retained := map[string]any{}
	for path, generatedValue := range originalGenerated {
		chartValue, ok := lookupYAMLPath(chart, strings.Split(path, ".")...)
		if !ok {
			t.Fatalf("generated active key %s is absent from chart defaults", path)
		}
		pair := valuePair{generatedValue, chartValue}
		switch {
		case reflect.DeepEqual(pair, fixtureDecisions[path]):
			counts["fixture"]++
			retained[path] = generatedValue
		case reflect.DeepEqual(pair, intentionalDivergences[path]):
			counts["divergent"]++
			retained[path] = generatedValue
		case reflect.DeepEqual(generatedValue, chartValue):
			counts["equal"]++
		default:
			t.Errorf("unclassified generated value %s: generated=%#v chart=%#v", path, generatedValue, chartValue)
		}
	}
	if len(originalGenerated) != 32 || counts["equal"] != 23 || counts["fixture"] != 6 || counts["divergent"] != 3 {
		t.Fatalf("classification paths=%d equal=%d fixture=%d divergent=%d", len(originalGenerated), counts["equal"], counts["fixture"], counts["divergent"])
	}

	generated := parseYAMLMap(t, renderK8sValuesForTest(t, k8sValuesFixtureData(true)))
	paths := yamlLeafPaths(generated, nil)
	if len(paths) != len(retained) {
		t.Fatalf("generated leaves=%d, want %d retained decisions", len(paths), len(retained))
	}
	for _, path := range paths {
		value, _ := lookupYAMLPath(generated, strings.Split(path, ".")...)
		if !reflect.DeepEqual(value, retained[path]) {
			t.Errorf("retained %s=%#v, want %#v", path, value, retained[path])
		}
	}
}

func renderK8sValuesForTest(t *testing.T, data scaffoldData) string {
	t.Helper()
	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatalf("render Kubernetes values: %v", err)
	}
	return values
}

func k8sValuesFixtureData(enabled bool) scaffoldData {
	return scaffoldData{
		EngineRef:       "fixture-engine-ref",
		AIEnabled:       enabled,
		AIAPI:           project.AIAPIResponses,
		AIEndpoint:      "https://provider.example/v1/responses",
		AIModel:         "fixture-model",
		K8sStorageClass: "fixture-rwx",
		Namespace:       "fixture-dashboard",
		DashboardName:   "fixture-dashboard",
	}
}

func parseYAMLMap(t *testing.T, text string) map[string]any {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		t.Fatalf("parse generated YAML: %v\n---\n%s", err, text)
	}
	return root
}

func assertYAMLValue(t *testing.T, root map[string]any, want any, path ...string) {
	t.Helper()
	got, ok := lookupYAMLPath(root, path...)
	if !ok {
		t.Fatalf("missing active YAML key %s", strings.Join(path, "."))
	}
	if got != want {
		t.Errorf("%s = %#v, want %#v", strings.Join(path, "."), got, want)
	}
}

func assertYAMLAbsent(t *testing.T, root map[string]any, path ...string) {
	t.Helper()
	if value, ok := lookupYAMLPath(root, path...); ok {
		t.Errorf("unexpected active YAML key %s=%#v", strings.Join(path, "."), value)
	}
}

func setYAMLPath(root map[string]any, value any, path ...string) {
	current := root
	for _, part := range path[:len(path)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
}

func lookupYAMLPath(root map[string]any, path ...string) (any, bool) {
	var current any = root
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func yamlLeafPaths(value any, prefix []string) []string {
	object, ok := value.(map[string]any)
	if !ok {
		return []string{strings.Join(prefix, ".")}
	}
	var paths []string
	for key, child := range object {
		paths = append(paths, yamlLeafPaths(child, append(prefix, key))...)
	}
	return paths
}
