// Command orka-producer turns a fetcher-produced dashboard skeleton into Orka
// analysis Tasks: one content-addressed `type: ai` Task per failing test. It
// reuses the engine's discovery output (jobs/*.json) and prompt composition, so
// the only thing that moves off the engine is the analysis step itself.
//
// Build isolation (model-independent): for each distinct build, the producer
// clones the base Tool CRDs with a static `X-Build-Prefix` header and a
// build-suffixed name, and each Task references its build's tool set. The shim
// routes by that header, so tools always read the Task's own build regardless of
// what the model does. (A model-passed `build` param proved unreliable.)
//
// Run-once: each Task name is content-addressed (az-<build>-<failureHash>-<ver>),
// so re-running and re-applying is idempotent. Bump -version to force re-analysis
// after a prompt/tool change.
//
// By default the producer is pure: it writes Task + Tool YAMLs to -tasks-out /
// -tools-out for a separate apply step. With -apply it server-side applies them
// directly (in-cluster config, or -context for local runs), Tools before Tasks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// engineToolGroups maps the engine's tool GROUP names (as selected in a
// consumer's project.yaml ai.tools) to the per-group Orka Tool CRD names.
var engineToolGroups = map[string][]string{
	"filesystem": {"list-artifacts", "find-artifacts", "grep-artifact", "read-artifact", "tail-artifact"},
	"k8s":        {"discover-clusters", "find-my-cluster", "list-cluster-machines", "list-machine-logs", "discover-controllers", "resolve-controller-log"},
}

// qualityTools are the deterministic shim tools added to every analysis. They
// degrade gracefully on non-CAPZ projects (return "no match" when their patterns
// do not apply), so they are safe to always include.
var qualityTools = []string{"validate-analysis", "verify-timeline", "check-transient-signatures", "recurrence", "required-evidence"}

// resolveTools maps a consumer's ai.tools group selection to the Orka Tool CRD
// names, always appending the quality tools. Group names expand; anything else
// passes through so an individual tool name still works. k8sEnabled reports
// whether the CAPZ-style cluster navigation tools are in the set, which gates the
// cluster-specific prompt guidance.
func resolveTools(aiTools []string) (names []string, k8sEnabled bool) {
	if len(aiTools) == 0 {
		aiTools = []string{"filesystem", "k8s"}
	}
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, t := range aiTools {
		if group, ok := engineToolGroups[t]; ok {
			if t == "k8s" {
				k8sEnabled = true
			}
			for _, n := range group {
				add(n)
			}
			continue
		}
		add(t)
	}
	for _, q := range qualityTools {
		add(q)
	}
	return names, k8sEnabled
}

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir (fetcher output, holds jobs/*.json)")
	projectDir := flag.String("project-dir", ".", "consumer dir with project.yaml + prompts/system.md")
	toolManifests := flag.String("tool-manifests", "experimental/orka/manifests", "dir with base Tool CRD YAMLs to clone per build")
	tasksOut := flag.String("tasks-out", "tasks", "output dir for generated Task YAMLs")
	toolsOut := flag.String("tools-out", "build-tools", "output dir for generated per-build Tool YAMLs")
	namespace := flag.String("namespace", "orka-system", "namespace for the Tasks and Tools")
	provider := flag.String("provider", "copilot", "Orka Provider name")
	model := flag.String("model", "claude-sonnet-4.5", "model id")
	version := flag.String("version", "v1", "content-address version suffix; bump to force re-analysis")
	timeout := flag.String("timeout", "10m", "per-Task timeout")
	toolsCSV := flag.String("tools", "", "override the comma-separated base Tool names (default: derive from the consumer's project.yaml ai.tools + quality tools)")
	bucketFlag := flag.String("bucket", "", "GCS bucket routed to the shim via the X-Bucket header (default: the consumer's storage.bucket)")
	retries := flag.Int("retries", 1, "Task retryPolicy maxRetries for transient model/tool errors")
	webhookURL := flag.String("webhook-url", "", "Task webhookURL for event-driven ingestion (must be a same-namespace ClusterIP service, e.g. http://orka-ingestor.orka-system.svc:8080/webhook)")
	apply := flag.Bool("apply", false, "apply Tools+Tasks to the cluster via client-go (in-cluster or -context) instead of only writing YAML")
	kubeContext := flag.String("context", "", "kubeconfig context to use when -apply runs outside the cluster")
	flag.Parse()

	cfg, addendum, err := project.LoadDir(*projectDir)
	if err != nil {
		log.Fatalf("load project %s: %v", *projectDir, err)
	}

	toolNames, k8sEnabled := resolveTools(cfg.AI.EffectiveAgentic().Tools)
	if *toolsCSV != "" {
		toolNames = splitCSV(*toolsCSV)
		k8sEnabled = hasK8sTool(toolNames)
	}
	systemPrompt := ai.ComposeSystemPrompt(addendum) + toolUsageAddendum(k8sEnabled)

	bucket := *bucketFlag
	if bucket == "" {
		bucket = cfg.Storage.Bucket
	}
	// Storage headers let the shim serve this consumer's provider (gcs, gcsweb
	// over an S3 gateway, ...) instead of assuming GCS. Only non-empty fields
	// are sent; the shim falls back to its own defaults for the rest.
	storageMeta := map[string]string{}
	if v := cfg.Storage.Provider; v != "" {
		storageMeta["X-Storage-Provider"] = v
	}
	if v := cfg.Storage.Base; v != "" {
		storageMeta["X-Storage-Base"] = v
	}
	if v := cfg.Storage.WebBase; v != "" {
		storageMeta["X-Web-Base"] = v
	}
	if v := cfg.Storage.ProwBase; v != "" {
		storageMeta["X-Prow-Base"] = v
	}
	projectLabel := cfg.DisplayShortName()

	baseTools, err := loadBaseTools(*toolManifests, toolNames)
	if err != nil {
		log.Fatalf("load base tools: %v", err)
	}

	mustMkdir(*tasksOut)
	mustMkdir(*toolsOut)

	jobFiles, _ := filepath.Glob(filepath.Join(*dataDir, "jobs", "*.json"))
	builds := map[string]string{} // buildID -> buildPrefix (distinct builds seen)
	var taskObjs []namedObj
	for _, jf := range jobFiles {
		var detail models.JobDetail
		if b, err := os.ReadFile(jf); err != nil || json.Unmarshal(b, &detail) != nil {
			continue
		}
		for _, run := range detail.Runs {
			buildPrefix := buildPrefixFor(bucket, detail.JobID, run)
			for _, tc := range run.TestCases {
				if tc.Status != "failed" {
					continue
				}
				builds[run.BuildID] = buildPrefix
				name := orka.TaskName(run.BuildID, orka.FailureHash(tc.Name, tc.FailureMessage), *version)
				task := orka.BuildAITask(orka.AITaskSpec{
					Name:         name,
					Namespace:    *namespace,
					Provider:     *provider,
					Model:        *model,
					Timeout:      *timeout,
					MaxRetries:   *retries,
					WebhookURL:   *webhookURL,
					Tools:        buildToolNames(toolNames, run.BuildID),
					SystemPrompt: systemPrompt,
					Prompt:       userPrompt(projectLabel, detail.JobID, buildPrefix, tc),
					Labels: map[string]string{
						orka.ManagedByLabel: orka.ManagedByValue,
						orka.BuildLabel:     run.BuildID,
					},
				})
				writeYAML(filepath.Join(*tasksOut, name+".yaml"), task)
				taskObjs = append(taskObjs, namedObj{name, task})
			}
		}
	}

	// Emit per-build Tool CRD clones (header-routed) for every distinct build.
	var toolObjs []namedObj
	for buildID, prefix := range builds {
		for _, base := range toolNames {
			doc := baseTools[base]
			clone := cloneToolForBuild(doc, base, buildID, prefix, bucket, *namespace, storageMeta)
			toolName := buildToolName(base, buildID)
			writeYAML(filepath.Join(*toolsOut, toolName+".yaml"), clone)
			toolObjs = append(toolObjs, namedObj{toolName, clone})
		}
	}

	log.Printf("wrote %d Tasks (%s) and %d per-build Tools across %d builds (%s) for %s [bucket=%s, k8s-tools=%v]",
		len(taskObjs), *tasksOut, len(toolObjs), len(builds), *toolsOut, projectLabel, bucket, k8sEnabled)

	if *apply {
		if err := applyAll(*namespace, *kubeContext, toolObjs, taskObjs); err != nil {
			log.Fatalf("apply: %v", err)
		}
	}
}

// namedObj pairs an object name with its unstructured content.
type namedObj struct {
	name string
	obj  map[string]any
}

// applyAll server-side applies the Tools before the Tasks that reference them.
func applyAll(namespace, kubeContext string, tools, tasks []namedObj) error {
	cfg, err := orka.RESTConfig(kubeContext)
	if err != nil {
		return fmt.Errorf("kube config: %w", err)
	}
	kc, err := orka.NewKubeClient(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, t := range tools {
		if err := kc.Apply(ctx, orka.ToolsGVR, namespace, t.obj); err != nil {
			return err
		}
	}
	for _, t := range tasks {
		if err := kc.Apply(ctx, orka.TasksGVR, namespace, t.obj); err != nil {
			return err
		}
	}
	log.Printf("applied %d Tools and %d Tasks to %s", len(tools), len(tasks), namespace)
	return nil
}

// toolUsageAddendum returns the engine-owned tool-usage guidance appended to the
// composed system prompt. The cluster-navigation guidance is included only when
// the CAPZ-style k8s tools are enabled, so a filesystem-only consumer (e.g. a
// project without a cluster-per-test model) is not told to call find_my_cluster.
func toolUsageAddendum(k8sEnabled bool) string {
	clusterGuidance := ""
	clusterBudgetStep := "read the logs around the EARLIEST failure"
	if k8sEnabled {
		clusterGuidance = "Resolve the right per-spec cluster (find_my_cluster) before reading\nper-cluster logs. "
		clusterBudgetStep = "find the failing test's cluster, read the logs around the EARLIEST\nfailure"
	}
	return `

## Tool usage for this analysis
The tools are scoped to THIS task's build automatically; just call them normally.
` + clusterGuidance + `For a transient-vs-bug decision, confirm any transient claim with
verify_timeline (did the expected operation actually register?) and
check_transient_signatures, and consult recurrence. Default to is_transient=false
unless a known transient class is proven from the evidence. Call validate_analysis
on every artifact path you cite.

## Tool budget: converge, do not exhaust it
You have a limited tool-call budget (aim for ~20 calls) and you WILL be forced to
answer when it runs out: near the budget the tools are removed and you must emit
the JSON from whatever you have gathered, so investigating past the budget only
wastes the evidence you could have synthesized. Investigate along ONE focused
path: ` + clusterBudgetStep + `, confirm the timeline once, then conclude.
Do not re-read a file you have already seen, re-run a search that already answered
your question, or gather redundant confirmation of a conclusion you can already
support. The moment your evidence is sufficient for a grounded root cause, run the
self-critique below and emit the JSON. A well-supported answer now is far better
than a marginally more certain one that never finishes.

## Self-critique before you finalize
Before emitting your JSON, re-check your own draft for these specific defects and
revise if any applies:
1. Causal ordering: is your root_cause the EARLIEST initiating failure, or a later
   downstream/teardown symptom (namespace/cluster deletion, cleanup timeout,
   credential expiry, a cascade of dependent timeouts)? Those happen AFTER the
   real failure.
2. Attribution: if you blamed an external/platform cause (throttling, a hung
   infrastructure operation, upstream flakiness) and set is_transient=true, did
   verify_timeline actually show the expected operation never registered? If you
   did not confirm it, treat the failure as a real bug (is_transient=false).
3. Grounding: is every claim tied to evidence you actually read (validate_analysis
   passed), not plausible-sounding speculation?
4. Fix validity: would suggested_fix actually resolve the stated root_cause?
Respond with ONLY the required JSON object.`
}

func userPrompt(projectLabel, jobID, buildPrefix string, tc models.TestCase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This %s CI test FAILED. Root-cause it and classify transient vs real bug.\n\n", projectLabel)
	fmt.Fprintf(&b, "Job: %s\n", jobID)
	fmt.Fprintf(&b, "Build: %s\n", buildPrefix)
	fmt.Fprintf(&b, "Failed test: %s\n", tc.Name)
	if tc.FailureLocation != "" {
		fmt.Fprintf(&b, "Failure location: %s\n", tc.FailureLocation)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&b, "JUnit file: %s\n", tc.JUnitFile)
	}
	msg := tc.FailureMessage
	if len(msg) > 1200 {
		msg = msg[:1200]
	}
	if msg != "" {
		fmt.Fprintf(&b, "Failure output:\n%s\n", msg)
	}
	fmt.Fprintf(&b, "\nInvestigate the build's artifacts with the tools and conclude with your JSON.")
	return b.String()
}

// loadBaseTools parses Tool CRDs from every YAML doc under dir and returns the
// ones named in want, keyed by name.
func loadBaseTools(dir string, want []string) (map[string]map[string]any, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	out := map[string]map[string]any{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc == nil || asString(doc["kind"]) != "Tool" {
				continue
			}
			meta, _ := doc["metadata"].(map[string]any)
			out[asString(meta["name"])] = doc
		}
	}
	for _, w := range want {
		if _, ok := out[w]; !ok {
			return nil, fmt.Errorf("base Tool %q not found under %s", w, dir)
		}
	}
	return out, nil
}

// cloneToolForBuild copies a base Tool CRD, renames it per build, and injects the
// X-Build-Prefix (and, when set, X-Bucket) headers so the shim serves this build
// from the right bucket.
// cloneToolForBuild copies a base Tool CRD, renames it per build, and injects the
// build/bucket/storage headers so the shim serves this build from the right
// bucket and provider.
func cloneToolForBuild(base map[string]any, baseName, buildID, prefix, bucket, namespace string, storageMeta map[string]string) map[string]any {
	doc := deepCopy(base).(map[string]any)
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta["name"] = buildToolName(baseName, buildID)
	meta["namespace"] = namespace
	meta["labels"] = map[string]any{
		orka.ManagedByLabel: orka.ManagedByValue,
		orka.BuildLabel:     buildID,
	}

	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		doc["spec"] = spec
	}
	httpCfg, _ := spec["http"].(map[string]any)
	if httpCfg == nil {
		httpCfg = map[string]any{}
		spec["http"] = httpCfg
	}
	headers, _ := httpCfg["headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{}
		httpCfg["headers"] = headers
	}
	headers["X-Build-Prefix"] = prefix
	if bucket != "" {
		headers["X-Bucket"] = bucket
	}
	for k, v := range storageMeta {
		headers[k] = v
	}
	return doc
}

// hasK8sTool reports whether an explicit tool list includes any CAPZ-style
// cluster navigation tool, so the cluster prompt guidance can be gated.
func hasK8sTool(names []string) bool {
	for _, n := range names {
		for _, k := range engineToolGroups["k8s"] {
			if n == k {
				return true
			}
		}
	}
	return false
}

// buildPrefixFor returns the bucket-relative build directory. It prefers deriving
// the prefix from the skeleton's stored artifact URL, which handles any Prow
// layout (periodic logs/<job>/<build>/ and presubmit
// pr-logs/pull/<org_repo>/<pr>/<job>/<build>/) and any bucket. It falls back to
// the periodic layout when no URL is present.
func buildPrefixFor(bucket, jobID string, run models.BuildResult) string {
	for _, u := range []string{run.WebURL, run.BuildLogURL} {
		if p := prefixFromURL(u, bucket); p != "" {
			return p
		}
	}
	return fmt.Sprintf("logs/%s/%s/", jobID, run.BuildID)
}

// prefixFromURL extracts the bucket-relative directory from an artifact URL by
// taking everything after "/<bucket>/", reducing a file URL to its directory.
func prefixFromURL(u, bucket string) string {
	if u == "" || bucket == "" {
		return ""
	}
	marker := "/" + bucket + "/"
	i := strings.Index(u, marker)
	if i < 0 {
		return ""
	}
	p := u[i+len(marker):]
	if k := strings.IndexAny(p, "?#"); k >= 0 {
		p = p[:k]
	}
	if !strings.HasSuffix(p, "/") {
		j := strings.LastIndex(p, "/")
		if j < 0 {
			return ""
		}
		p = p[:j+1]
	}
	return p
}

func buildToolNames(base []string, buildID string) []string {
	out := make([]string, len(base))
	for i, b := range base {
		out[i] = buildToolName(b, buildID)
	}
	return out
}

func buildToolName(base, buildID string) string { return orka.Sanitize(base + "-b" + buildID) }

// --- helpers ---

func writeYAML(path string, v any) {
	out, err := yaml.Marshal(v)
	if err != nil {
		log.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", dir, err)
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k := range t {
			m[k] = deepCopy(t[k])
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i := range t {
			s[i] = deepCopy(t[i])
		}
		return s
	default:
		return v
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
