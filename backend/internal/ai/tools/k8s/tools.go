package k8s

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// Register adds every k8s tool to the registry. Tools are keyed in the
// registry by their bare name. The group alias is "k8s" so a consumer can
// enable the whole tier with one entry.
func Register(r *tools.Registry) {
	r.Register(&discoverClustersTool{})
	r.Register(&findMyClusterTool{})
	r.Register(&listMachinesTool{})
	r.Register(&listMachineLogsTool{})
	r.Register(&discoverControllersTool{})
	r.Register(&resolveControllerLogTool{})
}

// joinWeb appends a relative path to the env's WebURLBase if set, returning
// an empty string otherwise so the model never sees a malformed URL.
func joinWeb(env *tools.Env, rel string) string {
	if env.WebURLBase == "" {
		return ""
	}
	return env.WebURLBase + rel
}

// cachedDiscoverClusters memoizes the cluster listing per build via the
// Env.Cache. Repeated calls across failures of the same build return the
// same slice without re-listing GCS.
func cachedDiscoverClusters(ctx context.Context, env *tools.Env) ([]Cluster, error) {
	const key = "k8s.discover_clusters"
	if env.Cache != nil {
		if raw, ok := env.Cache.Get(key); ok {
			var out []Cluster
			if err := json.Unmarshal([]byte(raw), &out); err == nil {
				return out, nil
			}
		}
	}
	clusters, err := DiscoverClusters(ctx, env.Browser)
	if err != nil {
		return nil, err
	}
	if env.Cache != nil {
		if b, err := json.Marshal(clusters); err == nil {
			env.Cache.Set(key, string(b))
		}
	}
	return clusters, nil
}

type discoverClustersTool struct{}

func (*discoverClustersTool) Name() string  { return "discover_clusters" }
func (*discoverClustersTool) Group() string { return Group }
func (*discoverClustersTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "discover_clusters",
			Description: "List workload Kubernetes clusters whose debug artifacts were captured under artifacts/clusters/ for this build. Excludes the management ('bootstrap') cluster. Returns an empty list if the build did not capture per-cluster artifacts.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   []string{},
			},
		},
	}
}

func (*discoverClustersTool) Dispatch(ctx context.Context, env *tools.Env, _ json.RawMessage) tools.Result {
	clusters, err := cachedDiscoverClusters(ctx, env)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	items := make([]map[string]interface{}, 0, len(clusters))
	for _, c := range clusters {
		items = append(items, map[string]interface{}{
			"name":    c.Name,
			"path":    c.Path,
			"web_url": joinWeb(env, c.Path),
		})
	}
	return tools.Result{Payload: map[string]interface{}{
		"clusters": items,
		"count":    len(items),
	}}
}

type findMyClusterTool struct{}

func (*findMyClusterTool) Name() string  { return "find_my_cluster" }
func (*findMyClusterTool) Group() string { return Group }
func (*findMyClusterTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "find_my_cluster",
			Description: "Resolve which workload cluster a failed test most likely ran against. Uses provider-agnostic flavor-substring matching (cluster dir name minus random ID appears in normalized test name) with a CAPZ keyword-rules fallback. Returns the chosen cluster plus reason and the full candidate list so you can override.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"test_name": map[string]interface{}{
						"type":        "string",
						"description": "Full test name (typically TestCase.Name) including any Ginkgo brackets.",
					},
				},
				"required": []string{"test_name"},
			},
		},
	}
}

func (*findMyClusterTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		TestName string `json:"test_name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	clusters, err := cachedDiscoverClusters(ctx, env)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	candidates := make([]map[string]interface{}, 0, len(clusters))
	for _, c := range clusters {
		candidates = append(candidates, map[string]interface{}{
			"name":    c.Name,
			"path":    c.Path,
			"web_url": joinWeb(env, c.Path),
		})
	}

	if len(clusters) == 0 {
		return tools.Result{Payload: map[string]interface{}{
			"match":      nil,
			"reason":     "no clusters discovered for this build",
			"candidates": candidates,
		}}
	}
	matched := MapTestToCluster(args.TestName, clusters)
	if matched == nil {
		return tools.Result{Payload: map[string]interface{}{
			"match":      nil,
			"reason":     "no flavor-substring or rule match",
			"candidates": candidates,
		}}
	}
	reason := "single cluster"
	if len(clusters) > 1 {
		reason = "flavor or keyword rule"
	}
	return tools.Result{Payload: map[string]interface{}{
		"match": map[string]interface{}{
			"name":    matched.Name,
			"path":    matched.Path,
			"web_url": joinWeb(env, matched.Path),
		},
		"reason":     reason,
		"candidates": candidates,
	}}
}

type listMachinesTool struct{}

func (*listMachinesTool) Name() string  { return "list_cluster_machines" }
func (*listMachinesTool) Group() string { return Group }
func (*listMachinesTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "list_cluster_machines",
			Description: "List the per-machine (per-VM/node) debug directories under a discovered cluster. Returns machine names and their dir paths; use list_machine_logs to see which log files each machine has.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cluster": map[string]interface{}{
						"type":        "string",
						"description": "Cluster name as returned by discover_clusters (e.g. \"capz-e2e-abc123-windows\").",
					},
				},
				"required": []string{"cluster"},
			},
		},
	}
}

func (*listMachinesTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	machines, err := ListClusterMachines(ctx, env.Browser, args.Cluster)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	items := make([]map[string]interface{}, 0, len(machines))
	for _, m := range machines {
		items = append(items, map[string]interface{}{
			"name":    m.Name,
			"path":    m.Path,
			"web_url": joinWeb(env, m.Path),
		})
	}
	return tools.Result{Payload: map[string]interface{}{
		"cluster":  args.Cluster,
		"machines": items,
		"count":    len(items),
	}}
}

type listMachineLogsTool struct{}

func (*listMachineLogsTool) Name() string  { return "list_machine_logs" }
func (*listMachineLogsTool) Group() string { return Group }
func (*listMachineLogsTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "list_machine_logs",
			Description: "List the known log files actually present in a machine's debug directory (boot.log, kubelet.log, journal.log, etc.). Use this resolver before tail_artifact/grep_artifact so you don't fetch missing files. Returns files in priority order.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cluster": map[string]interface{}{"type": "string", "description": "Cluster name."},
					"machine": map[string]interface{}{"type": "string", "description": "Machine (VM/node) name as returned by list_cluster_machines."},
				},
				"required": []string{"cluster", "machine"},
			},
		},
	}
}

func (*listMachineLogsTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Cluster string `json:"cluster"`
		Machine string `json:"machine"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	logs, err := ListMachineLogs(ctx, env.Browser, args.Cluster, args.Machine)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	items := make([]map[string]interface{}, 0, len(logs))
	for _, l := range logs {
		items = append(items, map[string]interface{}{
			"name":    l.Name,
			"path":    l.Path,
			"size":    l.Size,
			"web_url": joinWeb(env, l.Path),
		})
	}
	return tools.Result{Payload: map[string]interface{}{
		"cluster": args.Cluster,
		"machine": args.Machine,
		"logs":    items,
		"count":   len(items),
	}}
}

type discoverControllersTool struct{}

func (*discoverControllersTool) Name() string  { return "discover_controllers" }
func (*discoverControllersTool) Group() string { return Group }
func (*discoverControllersTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "discover_controllers",
			Description: "List management-cluster controller deployments captured under artifacts/clusters/bootstrap/logs/. Returns one entry per (namespace, deployment) pair. Pass a namespace to scope; omit for all namespaces.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace": map[string]interface{}{
						"type":        "string",
						"description": "Optional namespace filter (e.g. \"capz-system\", \"capi-system\"). Omit to list all namespaces.",
					},
				},
				"required": []string{},
			},
		},
	}
}

func (*discoverControllersTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Namespace string `json:"namespace"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return tools.ErrPayload("invalid arguments: " + err.Error())
		}
	}
	cacheKey := "k8s.discover_controllers/" + args.Namespace
	if env.Cache != nil {
		if raw, ok := env.Cache.Get(cacheKey); ok {
			return tools.Result{Payload: mustUnmarshalPayload(raw)}
		}
	}
	controllers, err := DiscoverControllers(ctx, env.Browser, args.Namespace)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	items := make([]map[string]interface{}, 0, len(controllers))
	for _, c := range controllers {
		items = append(items, map[string]interface{}{
			"namespace":  c.Namespace,
			"deployment": c.Deployment,
			"path":       c.Path,
			"web_url":    joinWeb(env, c.Path),
		})
	}
	payload := map[string]interface{}{
		"namespace":   args.Namespace,
		"controllers": items,
		"count":       len(items),
	}
	if env.Cache != nil {
		if b, err := json.Marshal(payload); err == nil {
			env.Cache.Set(cacheKey, string(b))
		}
	}
	return tools.Result{Payload: payload}
}

type resolveControllerLogTool struct{}

func (*resolveControllerLogTool) Name() string  { return "resolve_controller_log" }
func (*resolveControllerLogTool) Group() string { return Group }
func (*resolveControllerLogTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "resolve_controller_log",
			Description: "Find the concrete pod-level container-log path for a controller deployment. Returns the first pod (filtered by optional pod_name_regex) whose container_log file is present. Use the returned path with tail_artifact or grep_artifact.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace":      map[string]interface{}{"type": "string", "description": "Namespace, e.g. \"capz-system\"."},
					"deployment":     map[string]interface{}{"type": "string", "description": "Deployment name, e.g. \"capz-controller-manager\"."},
					"pod_name_regex": map[string]interface{}{"type": "string", "description": "Optional regex to filter pod names. Default matches any."},
					"container_log":  map[string]interface{}{"type": "string", "description": "Container log file name (default \"manager.log\").", "default": "manager.log"},
				},
				"required": []string{"namespace", "deployment"},
			},
		},
	}
}

func (*resolveControllerLogTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Namespace    string `json:"namespace"`
		Deployment   string `json:"deployment"`
		PodNameRegex string `json:"pod_name_regex"`
		ContainerLog string `json:"container_log"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	var podRe *regexp.Regexp
	if args.PodNameRegex != "" {
		re, err := regexp.Compile(args.PodNameRegex)
		if err != nil {
			return tools.ErrPayload("invalid pod_name_regex: " + err.Error())
		}
		podRe = re
	}
	log, pod, err := ResolveControllerLog(ctx, env.Browser, args.Namespace, args.Deployment, podRe, args.ContainerLog)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	if log == nil {
		return tools.Result{Payload: map[string]interface{}{
			"namespace":  args.Namespace,
			"deployment": args.Deployment,
			"match":      nil,
		}}
	}
	return tools.Result{Payload: map[string]interface{}{
		"namespace":  args.Namespace,
		"deployment": args.Deployment,
		"pod":        pod,
		"log": map[string]interface{}{
			"name":    log.Name,
			"path":    log.Path,
			"size":    log.Size,
			"web_url": joinWeb(env, log.Path),
		},
	}}
}

// mustUnmarshalPayload decodes a cached JSON payload back into a map. On decode
// failure it returns an error envelope instead of panicking.
func mustUnmarshalPayload(s string) map[string]interface{} {
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]interface{}{"error": "cache decode failed: " + err.Error()}
	}
	return out
}
