package onboard

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
	"gopkg.in/yaml.v3"
)

var updateK8sValuesGolden = flag.Bool("update-k8s-values-golden", false, "update the generated Kubernetes values golden file")

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

	assertYAMLValue(t, root, "watch", "mode")
	assertYAMLValue(t, root, false, "server", "chat", "enabled")
	assertYAMLValue(t, root, false, "server", "actions", "enabled")
	assertYAMLValue(t, root, "ClusterIP", "server", "service", "type")
	assertYAMLValue(t, root, 80, "server", "service", "port")
	assertYAMLValue(t, root, false, "ingress", "enabled")
	assertYAMLValue(t, root, false, "networkPolicy", "enabled")

	for path, want := range map[string]any{
		"persistence.enabled":          true,
		"persistence.existingClaim":    "",
		"persistence.storageClass":     "fixture-rwx",
		"persistence.accessMode":       "ReadWriteMany",
		"persistence.size":             "1Gi",
		"persistence.retain":           true,
		"ai.enabled":                   true,
		"ai.api":                       project.AIAPIResponses,
		"ai.endpoint":                  "https://provider.example/v1/responses",
		"ai.model":                     "fixture-model",
		"ai.contextWindowTokens":       0,
		"ai.existingSecret":            "<existing-ai-secret>",
		"ai.tokenSecretKey":            "AI_TOKEN",
		"ai.githubReadTokenSecretName": "",
		"ai.githubReadTokenSecretKey":  "GITHUB_READ_TOKEN",
		"fetcher.buildsPerJob":         10,
		"fetcher.workers":              5,
		"fetcher.timeout":              "120m",
		"fetcher.watchInterval":        "5m",
		"fetcher.reconcileInterval":    "1h",
		"fetcher.suspend":              true,
	} {
		assertYAMLValue(t, root, want, strings.Split(path, ".")...)
	}

	fetcher := yamlMapAt(t, root, "fetcher")
	for _, cronOnly := range []string{"schedule", "concurrencyPolicy", "activeDeadlineSeconds", "backoffLimit", "restartPolicy"} {
		if _, ok := fetcher[cronOnly]; ok {
			t.Errorf("cron-only key fetcher.%s is active in the watch scaffold", cronOnly)
		}
	}
}

func TestK8sValuesStorageSelection(t *testing.T) {
	t.Run("storage class", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLValue(t, root, "", "persistence", "existingClaim")
		assertYAMLValue(t, root, "fixture-rwx", "persistence", "storageClass")
	})

	t.Run("existing claim", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		data.K8sStorageClass = ""
		data.K8sExistingClaim = "shared-data"
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLValue(t, root, "shared-data", "persistence", "existingClaim")
		assertYAMLValue(t, root, "", "persistence", "storageClass")
	})

	t.Run("interactive placeholder", func(t *testing.T) {
		data := k8sValuesFixtureData(true)
		data.K8sStorageClass = ""
		root := parseYAMLMap(t, renderK8sValuesForTest(t, data))
		assertYAMLValue(t, root, "", "persistence", "existingClaim")
		assertYAMLValue(t, root, "<your-rwx-storage-class>", "persistence", "storageClass")
	})
}

func TestK8sValuesDocumentsOptionalConfiguration(t *testing.T) {
	values := renderK8sValuesForTest(t, k8sValuesFixtureData(true))
	for _, want := range []string{
		`# schedule: "0 */6 * * *"`,
		"# oauth:",
		`#   # Include OAUTH_CLIENT_SECRET, SESSION_KEY, and BOT_TOKEN when actions are enabled.`,
		`#   existingSecret: "<oauth-secret>"`,
		"# proxy:",
		"# hosts:",
		"# ingress value above from [] to a list",
		"# resources: {}",
		"# nodeSelector: {}",
		"# tolerations: []",
		"# affinity: {}",
		"helm show values oci://ghcr.io/willie-yao/charts/aster",
		"Use values.schema.json from the same published chart version",
		"Published values reference:",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("generated values missing %q\n---\n%s", want, values)
		}
	}
	for _, duplicateExample := range []string{"# ingress:"} {
		if strings.Contains(values, duplicateExample) {
			t.Errorf("optional example repeats active key %q\n---\n%s", duplicateExample, values)
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
	assertYAMLValue(t, root, "AI_TOKEN", "ai", "tokenSecretKey")
	if !strings.Contains(values, "When enabling AI later") {
		t.Fatalf("AI-disabled values omit later-enablement guidance\n---\n%s", values)
	}
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

func TestK8sValuesActiveKeysExistInChartDefaults(t *testing.T) {
	values := parseYAMLMap(t, renderK8sValuesForTest(t, k8sValuesFixtureData(true)))
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	chartValuesPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "deploy", "helm", "aster", "values.yaml")
	chartBytes, err := os.ReadFile(chartValuesPath)
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	chartValues := parseYAMLMap(t, string(chartBytes))

	paths := yamlLeafPaths(values, nil)
	sort.Strings(paths)
	for _, path := range paths {
		if _, ok := lookupYAMLPath(chartValues, strings.Split(path, ".")...); !ok {
			t.Errorf("generated active key %s is absent from chart defaults", path)
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

func yamlMapAt(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	value, ok := lookupYAMLPath(root, path...)
	if !ok {
		t.Fatalf("missing active YAML object %s", strings.Join(path, "."))
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s has type %T, want object", strings.Join(path, "."), value)
	}
	return result
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
