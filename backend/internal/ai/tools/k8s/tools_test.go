package k8s

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// envWith returns an Env that points at a freshly populated capi-shaped
// browser, a fresh cache, and a known WebURLBase. Helps keep dispatch
// tests focused on the registry-facing contract.
func envWith(webBase string) *tools.Env {
	return &tools.Env{
		Browser:    capiShapedBrowser(),
		Cache:      tools.NewCache(),
		WebURLBase: webBase,
	}
}

func mustDispatch(t *testing.T, tool tools.Tool, env *tools.Env, args interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res := tool.Dispatch(context.Background(), env, raw)
	if res.Payload == nil {
		t.Fatalf("dispatch returned nil payload")
	}
	return res.Payload
}

// numAs reads a numeric payload field that may be int or cache-round-tripped float64.
func numAs(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func TestDiscoverClustersDispatchAttachesWebURLAndCount(t *testing.T) {
	env := envWith("https://gcsweb.k8s.io/gcs/bucket/logs/job/1/")
	payload := mustDispatch(t, &discoverClustersTool{}, env, struct{}{})

	if got := numAs(payload["count"]); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	clusters := payload["clusters"].([]map[string]interface{})
	for _, c := range clusters {
		path := c["path"].(string)
		want := "https://gcsweb.k8s.io/gcs/bucket/logs/job/1/" + path
		if c["web_url"].(string) != want {
			t.Errorf("cluster %q web_url = %q, want %q", c["name"], c["web_url"], want)
		}
	}
}

func TestDiscoverClustersDispatchOmitsWebURLWhenBaseEmpty(t *testing.T) {
	env := envWith("")
	payload := mustDispatch(t, &discoverClustersTool{}, env, struct{}{})

	clusters := payload["clusters"].([]map[string]interface{})
	if len(clusters) == 0 {
		t.Fatalf("expected clusters, got none")
	}
	for _, c := range clusters {
		if c["web_url"].(string) != "" {
			t.Errorf("expected empty web_url with no base, got %q", c["web_url"])
		}
		if c["path"].(string) == "" {
			t.Errorf("path must still be populated when web_url is empty")
		}
	}
}

func TestDiscoverClustersDispatchUsesCacheAcrossCalls(t *testing.T) {
	env := envWith("")
	tool := &discoverClustersTool{}
	_ = mustDispatch(t, tool, env, struct{}{})
	// Swap the browser to one that would fail; the cached payload should
	// keep returning the original two clusters.
	env.Browser = &fakeBrowser{}
	payload := mustDispatch(t, tool, env, struct{}{})
	if got := numAs(payload["count"]); got != 2 {
		t.Fatalf("expected cached count 2 after browser swap, got %d", got)
	}
}

func TestFindMyClusterDispatchSingleClusterShortCircuits(t *testing.T) {
	env := &tools.Env{
		Browser: &fakeBrowser{
			dirs: map[string][]string{
				"artifacts/clusters/": {"only-one/"},
			},
		},
		Cache: tools.NewCache(),
	}
	payload := mustDispatch(t, &findMyClusterTool{}, env, map[string]string{"test_name": "totally unrelated test"})
	match, ok := payload["match"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected match map, got %v", payload["match"])
	}
	if match["name"].(string) != "only-one" {
		t.Errorf("single-cluster fallback failed, got %v", match["name"])
	}
	if payload["reason"].(string) != "single cluster" {
		t.Errorf("expected reason 'single cluster', got %q", payload["reason"])
	}
}

func TestFindMyClusterDispatchKeywordRule(t *testing.T) {
	env := envWith("https://web/base/")
	payload := mustDispatch(t, &findMyClusterTool{}, env, map[string]string{
		"test_name": "[It] IPv6 networking works",
	})
	match := payload["match"].(map[string]interface{})
	if match["name"].(string) != "capz-e2e-abc123-ipv6" {
		t.Errorf("expected ipv6 cluster, got %v", match["name"])
	}
	if payload["reason"].(string) != "flavor or keyword rule" {
		t.Errorf("expected multi-cluster reason, got %q", payload["reason"])
	}
	cands := payload["candidates"].([]map[string]interface{})
	if len(cands) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(cands))
	}
}

func TestFindMyClusterDispatchNoMatch(t *testing.T) {
	env := envWith("")
	payload := mustDispatch(t, &findMyClusterTool{}, env, map[string]string{
		"test_name": "some completely unrelated test name",
	})
	if payload["match"] != nil {
		t.Errorf("expected nil match, got %v", payload["match"])
	}
	if payload["reason"].(string) != "no flavor-substring or rule match" {
		t.Errorf("reason = %q", payload["reason"])
	}
}

func TestListClusterMachinesDispatchPathAndWebURL(t *testing.T) {
	env := envWith("https://web/base/")
	payload := mustDispatch(t, &listMachinesTool{}, env, map[string]string{"cluster": "capz-e2e-abc123-ha"})

	if got := numAs(payload["count"]); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	machines := payload["machines"].([]map[string]interface{})
	first := machines[0]
	if first["web_url"].(string) != "https://web/base/"+first["path"].(string) {
		t.Errorf("web_url = %q, path = %q", first["web_url"], first["path"])
	}
}

func TestListMachineLogsDispatchPriorityAndSize(t *testing.T) {
	env := envWith("")
	payload := mustDispatch(t, &listMachineLogsTool{}, env, map[string]string{
		"cluster": "capz-e2e-abc123-ha",
		"machine": "capz-e2e-abc123-ha-control-plane-jkl42",
	})
	logs := payload["logs"].([]map[string]interface{})
	if len(logs) == 0 {
		t.Fatal("expected logs")
	}
	if logs[0]["name"].(string) != "boot.log" {
		t.Errorf("first log = %q, want boot.log (priority order)", logs[0]["name"])
	}
}

func TestDiscoverControllersDispatchCachesByNamespace(t *testing.T) {
	env := envWith("")
	tool := &discoverControllersTool{}

	// Prime the empty-namespace cache, then swap the browser. Same namespace
	// should hit cache; a different namespace should hit the failing browser.
	_ = mustDispatch(t, tool, env, map[string]string{"namespace": ""})
	env.Browser = &fakeBrowser{}

	cached := mustDispatch(t, tool, env, map[string]string{"namespace": ""})
	if numAs(cached["count"]) != 3 {
		t.Errorf("cache miss: count = %v, want 3", cached["count"])
	}
	fresh := mustDispatch(t, tool, env, map[string]string{"namespace": "capz-system"})
	if numAs(fresh["count"]) != 0 {
		t.Errorf("expected fresh call to return empty (broken browser), got %v", fresh["count"])
	}
}

func TestResolveControllerLogDispatchDefaults(t *testing.T) {
	env := envWith("https://web/base/")
	payload := mustDispatch(t, &resolveControllerLogTool{}, env, map[string]string{
		"namespace":  "capz-system",
		"deployment": "capz-controller-manager",
	})
	log := payload["log"].(map[string]interface{})
	if log["name"].(string) != "manager.log" {
		t.Errorf("default container_log = %q, want manager.log", log["name"])
	}
	if log["web_url"].(string) != "https://web/base/"+log["path"].(string) {
		t.Errorf("web_url joining broken: %q vs %q", log["web_url"], log["path"])
	}
}

func TestResolveControllerLogDispatchInvalidRegex(t *testing.T) {
	env := envWith("")
	res := (&resolveControllerLogTool{}).Dispatch(context.Background(), env, json.RawMessage(`{"namespace":"capz-system","deployment":"capz-controller-manager","pod_name_regex":"["}`))
	errStr, _ := res.Payload["error"].(string)
	if errStr == "" {
		t.Errorf("expected error payload for invalid regex, got %v", res.Payload)
	}
}

func TestRegistryEnableK8sGroupSurfacesAllTools(t *testing.T) {
	r := tools.NewRegistry()
	Register(r)
	enabled, err := r.Enable([]string{"k8s"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	want := map[string]bool{
		"discover_clusters":      false,
		"find_my_cluster":        false,
		"list_cluster_machines":  false,
		"list_machine_logs":      false,
		"discover_controllers":   false,
		"resolve_controller_log": false,
	}
	for _, n := range enabled {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("group enable missed tool %q", n)
		}
	}
}
