package benchmarks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/modules/universal"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/aster/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// This file is an opt-in quality benchmark: it runs the real agentic analysis
// against real GCS artifacts of a known historical failure and scores whether
// the model reaches the true root cause. It costs real model tokens / GPU, so
// it never runs under `go test ./...` unless RUN_AI_BENCHMARK is set and an
// endpoint is configured. It doubles as a regression gate for prompt, tool, and
// harness changes: a must-signal miss fails the test.
//
// Run it with, e.g.:
//
//	RUN_AI_BENCHMARK=1 \
//	AI_API=chat_completions \
//	AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
//	AI_MODEL=moonshotai/Kimi-K2.7-Code AI_TOKEN=x \
//	go -C backend/benchmarks test . -run TestAIBenchmark -v -timeout 60m
//
// Point BENCH_PROJECT_DIR at a consumer repo to load its real project.yaml AI
// tuning and prompts/system.md so the run matches that live deploy exactly;
// otherwise a compact built-in prompt and the live CAPZ-Dynamo tuning are used.
// BENCH_MANIFEST selects a strict external case manifest. BENCH_REPETITIONS
// repeats every selected case with a fresh cache. BENCH_CACHE_MODE accepts only
// "cold". BENCH_CACHE_DIR retains the private cache for reload verification, and
// BENCH_VERIFY_CACHE_REUSE performs a zero-request policy lookup after saving it.
// BENCH_RESULTS_JSONL writes private scoring inputs and requires a stable
// anonymous BENCH_MODEL_LABEL.

// benchSignal is one scored expectation against the model's root cause text.
type benchSignal struct {
	name    string
	re      *regexp.Regexp
	negated *regexp.Regexp
	// must marks a signal whose absence fails the benchmark. Non-must signals
	// are informational, tracking how deep the analysis got.
	must bool
}

// benchCase pins one historical failure and the signals a correct root cause
// should contain.
type benchmarkSourceRef struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type benchCase struct {
	name         string
	stableID     string
	bucket       string
	evidenceMode string
	// fixtureAsset is the .tar.gz on the benchmark-fixtures release holding a
	// full snapshot of this build's bucket-relative artifact tree. The default
	// run extracts it and reads through the local storage provider, so the
	// benchmark survives Prow garbage-collecting the original GCS artifacts. Set
	// BENCH_USE_GCS=1 to read live GCS instead (only works before GC).
	fixtureAsset    string
	fixtureSHA256   string
	jobType         string
	repo            string // org/repo, required for presubmits
	jobName         string
	buildID         string
	pullNumber      string
	webURL          string
	commit          string
	repoVersion     string
	repoRefs        map[string]string
	sourceRefs      []benchmarkSourceRef
	primarySourceID string
	sourceRepo      [2]string // primary owner and name for analyzer configuration
	testName        string
	testSource      string
	junitFile       string
	failureMsg      string
	// consecutiveFailures is how many consecutive builds this test had failed at
	// the time of the snapshot. The live engine derives this from the flakiness
	// report; the benchmark feeds it so the analysis (and the critique gate's
	// transient-vs-persistent check) see the real persistence signal.
	consecutiveFailures int
	oppositeDiagnosis   string
	oppositeTransient   bool
	referenceDiagnosis  string
	referenceTransient  bool
	allowUnavailable    bool
	expectedTransient   *bool
	forbidden           []benchSignal
	consumerCommit      string
	projectSHA256       string
	promptSHA256        string
	signals             []benchSignal
	sourceRanges        []benchmarkSourceRange
	sourceSignals       []benchSignal
	// causeRepository is the "owner/repo" a correct analysis must hold
	// responsible, and causeExternal whether that repository is a dependency
	// rather than the project under test. Set both to score how reliably the
	// model distinguishes an own-repo cause from an upstream one. causeFiles are
	// paths the reported location must contain.
	causeRepository string
	causeExternal   bool
	causeFiles      []string

	evidenceGroups       []benchmarkEvidenceGroup
	oracleEvidenceSHA256 string
}

const (
	benchmarkEvidenceModeArtifactOnly      = "artifact_only"
	benchmarkEvidenceModeArtifactAndSource = "artifact_and_source"
)

func validBenchmarkEvidenceMode(value string) bool {
	return value == benchmarkEvidenceModeArtifactOnly || value == benchmarkEvidenceModeArtifactAndSource
}

func benchmarkSourceExpectationSHA256(bc benchCase) string {
	if len(bc.sourceRefs) == 0 && len(bc.sourceRanges) == 0 && len(bc.sourceSignals) == 0 {
		return strings.Repeat("0", 64)
	}
	var input strings.Builder
	fmt.Fprintf(&input, "primary\x00%s\n", bc.primarySourceID)
	sourceRefs := append([]benchmarkSourceRef(nil), bc.sourceRefs...)
	sort.Slice(sourceRefs, func(i, j int) bool { return sourceRefs[i].ID < sourceRefs[j].ID })
	for _, value := range sourceRefs {
		fmt.Fprintf(&input, "source\x00%s\x00%s\x00%s\n", value.ID, value.Repository, value.Revision)
	}
	for _, value := range bc.sourceRanges {
		fmt.Fprintf(&input, "range\x00%s\x00%s\x00%s\x00%d\x00%d\n", value.Repository, value.Revision, value.Path, value.LineStart, value.LineEnd)
	}
	for _, signal := range bc.sourceSignals {
		fmt.Fprintf(&input, "signal\x00%s\x00%s\x00%s\n", signal.name, signal.re.String(), func() string {
			if signal.negated == nil {
				return ""
			}
			return signal.negated.String()
		}())
	}
	sum := sha256.Sum256([]byte(input.String()))
	return fmt.Sprintf("%x", sum[:])
}

type benchmarkOutcome string

const (
	benchmarkOutcomeUsable                    benchmarkOutcome = "usable"
	benchmarkOutcomeCitationPolicyUnavailable benchmarkOutcome = "citation_policy_unavailable"
	benchmarkOutcomeUnknown                   benchmarkOutcome = "unknown"
)

// fixtureReleaseBase is the download root for benchmark-fixtures release assets.
const fixtureReleaseBase = "https://github.com/willie-yao/prow-ai-dashboard/releases/download/benchmark-fixtures/"

func mustRE(s string) *regexp.Regexp { return regexp.MustCompile(s) }

const flatcarNodePresencePattern = `(?:worker\s+)?node(?:\s+object)?\s+(?:exist(?:ed|s)?|registered|created|ready|became\s+ready|(?:is|was)\s+(?:created|registered|ready)|(?:has|had)(?:\s+been)?\s+(?:created|registered|ready)|(?:has|had)\s+existed|(?:did|does)\s+(?:exist|register))|(?:vm|machine|kubelet|it)\s+(?:did\s+)?register(?:ed)?\s+(?:as\s+)?(?:(?:a|the)\s+)?(?:worker\s+)?node(?:\s+object)?`

var (
	benchmarkClauseBoundaryRE = mustRE(`(?i)[.!?:;\n]+|(?:,\s*)?\b(?:but|however|yet|nevertheless|instead)\b\s*,?|\bnot\s+only\b`)
	flatcarNodePresenceRE     = mustRE(`(?is)` + flatcarNodePresencePattern)
	flatcarNodeNegationRE     = mustRE(`(?is)(?:\b(?:no|neither|nor|without)\b.*?(?:` + flatcarNodePresencePattern + `)|\b(?:not|never)\b[^,]*?(?:` + flatcarNodePresencePattern + `)|(?:` + flatcarNodePresencePattern + `)\s+(?:nowhere|not\s+anywhere|neither\b|in\s+(?:no|neither)\b))`)
)

func (s benchSignal) matches(text string) bool {
	if s.negated == nil {
		return s.re.MatchString(text)
	}
	for _, clause := range benchmarkSignalClauses(text) {
		negated := s.negated.FindAllStringIndex(clause, -1)
		for _, match := range s.re.FindAllStringIndex(clause, -1) {
			rejected := false
			for _, negative := range negated {
				if match[0] < negative[1] && negative[0] < match[1] {
					rejected = true
					break
				}
			}
			if !rejected {
				return true
			}
		}
	}
	return false
}

func benchmarkSignalClauses(text string) []string {
	boundaries := benchmarkClauseBoundaryRE.FindAllStringIndex(text, -1)
	clauses := make([]string, 0, len(boundaries)+1)
	start := 0
	for _, boundary := range boundaries {
		if start < boundary[0] {
			clauses = append(clauses, text[start:boundary[0]])
		}
		start = boundary[1]
	}
	if start < len(text) {
		clauses = append(clauses, text[start:])
	}
	return clauses
}

// benchCases is the growing catalog of hard failures to benchmark against.
var benchCases = []benchCase{
	{
		// cloud-provider-azure dual-stack e2e failed 100% because CAPZ does not
		// default a route table onto the control-plane subnet. On dual-stack
		// Calico runs encapsulation:None, so the control plane has no route to
		// worker pod CIDRs, the Calico APIService goes unreachable, and every
		// namespace hangs Terminating. Fixed in CAPZ PR #6358. Every one of the
		// 64 failed tests reports only "timed out waiting for the condition", so
		// the agent must read the AzureCluster resource dump to find the empty
		// control-plane routeTable. The fix lives in a different repo than the
		// job, so a correct answer also recognizes it is a CAPZ change.
		name:          "ccm-dualstack-control-plane-routetable",
		bucket:        "kubernetes-ci-logs",
		fixtureAsset:  "ccm-dualstack-capz-6358.tar.gz",
		fixtureSHA256: "179dcf40be61d6c8f4e1369793ec2b0c8c73eda0a0eb0fa5d832e488418c832f",
		jobType:       models.JobTypePresubmit,
		repo:          "kubernetes-sigs/cloud-provider-azure",
		jobName:       "pull-cloud-provider-azure-e2e-ccm-dualstack-capz-1-30",
		buildID:       "2062345846720040960",
		pullNumber:    "10388",
		webURL:        "https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_cloud-provider-azure/10388/pull-cloud-provider-azure-e2e-ccm-dualstack-capz-1-30/2062345846720040960/",
		sourceRepo:    [2]string{"kubernetes-sigs", "cloud-provider-azure"},
		testName:      "[It] Azure node resources should set node provider id correctly [Node]",
		junitFile:     "junit_01.xml",
		failureMsg:    `Unexpected error: <wait.errInterrupted>: timed out waiting for the condition { cause: <*errors.errorString>{ s: "timed out waiting for the condition", }, } occurred`,
		// This dual-stack job failed ~9 consecutive builds before PR #6358; a
		// genuine flake would not, so a transient verdict is contradicted.
		consecutiveFailures: 9,
		// This is the hard/aspirational case. The MUST bar is the achievable
		// correct high-level diagnosis (systemic, not a flake; control-plane /
		// networking on CAPZ). The exact control-plane route-table root cause
		// requires reading one field in a resource dump and is a stretch "nice"
		// signal that even strong models miss today.
		signals: []benchSignal{
			{name: "not a transient flake", re: mustRE(`(?i)systemic|persistent|not\s+(?:a\s+)?(?:transient|flake)|real\s+(?:bug|issue|regression)|deterministic`), must: true},
			{name: "control-plane / networking / subnet involvement", re: mustRE(`(?i)control[\s_-]?plane|api[\s_-]?server|subnet|network|routing?|connectivity`), must: true},
			{name: "identifies CAPZ / AzureCluster as the fix site", re: mustRE(`(?i)cluster-api-provider-azure|capz|azurecluster`)},
			{name: "traces the calico/apiservice/namespace cascade", re: mustRE(`(?i)calico|apiservice|namespace|terminating|discovery`)},
			{name: "STRETCH: pinpoints the control-plane route table", re: mustRE(`(?i)route[\s_-]?table`)},
			{name: "STRETCH: notes dual-stack / encapsulation none", re: mustRE(`(?i)dual[\s_-]?stack|ipv6|encapsulation`)},
		},
	},
	{
		// The Flatcar worker VM and Node both came up, but the Node remained
		// cloud-provider uninitialized and had no providerID. cloud-node-manager
		// crash-looped because it could not reach the API Service ClusterIP. The
		// preceding kube-proxy log shows the initiating failure: it never synced
		// because the API endpoint lookup used [::1]:53, where DNS was refusing
		// connections. The next run passed with the same Kubernetes, Flatcar, and
		// containerd versions, so this is a concrete transient bootstrap failure.
		// Unlike the API-version case, the cause is not in build-log.txt; unlike
		// the dual-stack case, following it needs only generic Kubernetes control
		// plane, Service, and external cloud-provider reasoning.
		name:                "flatcar-worker-dns-providerid",
		bucket:              "kubernetes-ci-logs",
		fixtureAsset:        "flatcar-sysext-dns-providerid.tar.gz",
		fixtureSHA256:       "8ed886395742d145c014be4b6a2dc38b3ddf3db0ad6e7a5740da10eea80a1945",
		jobType:             models.JobTypePeriodic,
		jobName:             "periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-24",
		buildID:             "2073261474372915200",
		webURL:              "https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-24/2073261474372915200/",
		sourceRepo:          [2]string{"kubernetes-sigs", "cluster-api-provider-azure"},
		testName:            "[It] Workload cluster creation Creating a Flatcar sysext cluster [OPTIONAL] With Flatcar control-plane and worker nodes",
		junitFile:           "junit.e2e_suite.1.xml",
		failureMsg:          `Timed out after 1500.000s. Timed out waiting for 1 nodes to be created for MachineDeployment capz-e2e-asfxe1/capz-e2e-asfxe1-flatcar-sysext-md-0. Expected 0 to equal 1`,
		consecutiveFailures: 1,
		oppositeDiagnosis:   "The worker Node did not exist. Its providerID was set. cloud-node-manager reached the API Service.",
		signals: []benchSignal{
			{name: "recognizes the worker Node existed or registered", re: flatcarNodePresenceRE, negated: flatcarNodeNegationRE, must: true},
			{name: "identifies missing providerID or cloud-provider initialization", re: mustRE(`(?is)(?:missing|empty|unset|absent|lacked?|without|no)\s+(?:the\s+)?provider.?id|provider.?id.{0,40}(?:missing|empty|unset|absent|not\s+(?:set|populated|assigned))|cloud.?provider.{0,80}uninitialized|uninitialized.{0,80}cloud.?provider`), must: true},
			{name: "identifies cloud-node-manager API reachability as the blocking failure", re: mustRE(`(?is)cloud-node-manager.{0,200}(?:could\s+not|couldn't|cannot|can't|failed|unable|unreachable|refus|timed?\s*out|timeout|crash).{0,120}(?:10\.96\.0\.1|api(?:server)?|cluster.?ip|kubernetes\s+service)|cloud-node-manager.{0,200}(?:10\.96\.0\.1|api(?:server)?|cluster.?ip|kubernetes\s+service).{0,120}(?:could\s+not|couldn't|cannot|can't|failed|unable|unreachable|refus|timed?\s*out|timeout|crash)|(?:10\.96\.0\.1|cluster.?ip).{0,120}(?:refus|timeout|unreachable|failed).{0,120}cloud-node-manager`), must: true},
			{name: "STRETCH: traces kube-proxy failing to synchronize", re: mustRE(`(?is)kube-proxy.*(?:sync|watch|list|api|dns|lookup|resolve|service)`)},
			{name: "STRETCH: pinpoints DNS refusal on the loopback resolver", re: mustRE(`(?is)(?:\[?::1\]?|loopback).*(?:53|dns|resolv|refus)|(?:dns|resolv|nameserver).*(?:\[?::1\]?|connection refused)`)},
		},
	},
	{
		// apiversion-upgrade periodic fails on clusterctl upgrade: during the
		// management-cluster provider upgrade, clusterctl scales the Azure
		// Service Operator (ASO) controller-manager down, so ASO's CRD
		// conversion webhook becomes unreachable (connection refused). When
		// clusterctl's object-graph discovery then lists ASO resource CRDs
		// (network.azure.com VirtualNetworksSubnet, containerservice.azure.com
		// ManagedClustersAgentPool), the storage-version conversion call to the
		// downed webhook fails and retries until the client-side rate limiter
		// hits its context deadline ("action failed after 9 attempts"). Unlike
		// the route-table case, the proximate cause is stated verbatim in
		// build-log.txt and the clusterctl-upgrade.log dumps, so a competent
		// agent finds it by reading the logs. Persistent (7+ consecutive
		// builds); the real fix is partly upstream in sigs.k8s.io/cluster-api's
		// clusterctl upgrade sequencing.
		name:                "apiversion-upgrade-clusterctl-aso-ratelimit",
		bucket:              "kubernetes-ci-logs",
		fixtureAsset:        "apiversion-upgrade-aso-clusterctl.tar.gz",
		fixtureSHA256:       "74e87df63463559f917e22723e86757b6ea1027fe6b27cab4b07fa5a4647dca2",
		jobType:             models.JobTypePeriodic,
		jobName:             "periodic-cluster-api-provider-azure-apiversion-upgrade-main",
		buildID:             "2074603331648491520",
		webURL:              "https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/periodic-cluster-api-provider-azure-apiversion-upgrade-main/2074603331648491520/",
		sourceRepo:          [2]string{"kubernetes-sigs", "cluster-api-provider-azure"},
		testName:            "[It] Running the Cluster API E2E tests API Version Upgrade upgrade from the latest version of v1beta1 to current, and scale workload clusters created in the old version Should create a management cluster and then upgrade all the providers",
		junitFile:           "junit.e2e_suite.1.xml",
		failureMsg:          `failed to run clusterctl upgrade Unexpected error: failed to list objects for the "network.azure.com/v1api20201101, Kind=VirtualNetworksSubnet" GroupVersionKind: action failed after 9 attempts: client rate limiter Wait returned an error: context deadline exceeded`,
		consecutiveFailures: 7,
		signals: []benchSignal{
			{name: "identifies clusterctl upgrade as the failing step", re: mustRE(`(?i)clusterctl\s+upgrade|management[\s_-]?cluster.*upgrade|provider.*upgrade`), must: true},
			{name: "identifies ASO / the azure.com CRD listing as what failed", re: mustRE(`(?i)service\s?operator|\baso\b|azure\.com|virtualnetworkssubnet|managedclustersagentpool|crd`), must: true},
			{name: "names the rate-limiter / deadline mechanism", re: mustRE(`(?i)rate[\s_-]?limit|context deadline|timed?\s?out|9 attempts`)},
			{name: "recognizes it as systemic, not a flake", re: mustRE(`(?i)systemic|persistent|not\s+(?:a\s+)?(?:transient|flake)|recurring|scal`)},
			{name: "STRETCH: pinpoints the conversion-webhook / ASO scale-down mechanism", re: mustRE(`(?i)conversion\s?webhook|scal(?:e|ed|ing)\s?down|connection refused|webhook.*(?:unreachable|refused|down)`)},
		},
	},
}

func TestFlatcarBenchmarkSkillRequiresProviderIDChain(t *testing.T) {
	set, selection, err := skills.LoadForTools(t.TempDir(), []string{"filesystem", "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Kubernetes {
		t.Fatal("Kubernetes profile was not selected")
	}
	var flatcar *benchCase
	for i := range benchCases {
		if benchCases[i].name == "flatcar-worker-dns-providerid" {
			flatcar = &benchCases[i]
			break
		}
	}
	if flatcar == nil {
		t.Fatal("Flatcar benchmark case is missing")
	}
	matched := set.Match(flatcar.failureMsg)
	var machineSkill *skills.Skill
	for i := range matched {
		if matched[i].ID == "engine.kubernetes.machine-node-providerid" {
			machineSkill = &matched[i]
			break
		}
	}
	if machineSkill == nil {
		t.Fatalf("matched skills = %+v", matched)
	}
	benchmarkDraft := "The worker Node existed and is Ready but has no providerID; cloud-node-manager cannot reach the API Service ClusterIP"
	groups := map[string]bool{}
	for _, group := range machineSkill.RequiredEvidence {
		groups[group.ID] = true
		if !group.Applies(benchmarkDraft) {
			t.Errorf("benchmark evidence group %q did not apply to the providerID/API chain", group.ID)
		}
	}
	for _, want := range []string{"machine-state", "node-state", "cloud-provider-controller", "kube-proxy"} {
		if !groups[want] {
			t.Errorf("missing evidence group %q", want)
		}
	}
}

func TestFlatcarNodeExistenceSignal(t *testing.T) {
	var signal *benchSignal
	for i := range benchCases {
		if benchCases[i].name != "flatcar-worker-dns-providerid" {
			continue
		}
		for j := range benchCases[i].signals {
			if benchCases[i].signals[j].name == "recognizes the worker Node existed or registered" {
				signal = &benchCases[i].signals[j]
				break
			}
		}
	}
	if signal == nil {
		t.Fatal("Flatcar Node-existence signal is missing")
	}

	tests := []struct {
		text string
		want bool
	}{
		{text: "Node is registered and Ready.", want: true},
		{text: "The VM did register a Node object.", want: true},
		{text: "The worker Node exists.", want: true},
		{text: "The worker Node did exist.", want: true},
		{text: "The worker Node does exist.", want: true},
		{text: "The worker Node has existed.", want: true},
		{text: "The worker Node never registered.", want: false},
		{text: "No Node is registered.", want: false},
		{text: "No worker Node exists.", want: false},
		{text: "The VM did not register a Node object.", want: false},
		{text: "Neither the worker Node existed nor did the VM register a Node object.", want: false},
		{text: "No worker Node existed. The Node is registered and Ready.", want: true},
		{text: "No worker Node existed. Node is registered and Ready.", want: true},
		{text: "No VM, machine, or kubelet registered as a Node object.", want: false},
		{text: "No VM existed, but the Node registered.", want: true},
		{text: "Although the VM was not ready, the worker Node existed.", want: true},
		{text: "The worker Node existed nowhere in the cluster.", want: false},
		{text: "There is not a Node registered.", want: false},
		{text: "Never did the worker Node exist.", want: false},
		{text: "Not only did the VM register a Node object, the Node became Ready.", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := signal.matches(tt.text); got != tt.want {
				t.Errorf("MatchString(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestFlatcarBenchmarkSignalsMatchReferenceDiagnosis(t *testing.T) {
	var flatcar *benchCase
	for i := range benchCases {
		if benchCases[i].name == "flatcar-worker-dns-providerid" {
			flatcar = &benchCases[i]
			break
		}
	}
	if flatcar == nil {
		t.Fatal("Flatcar benchmark case is missing")
	}
	reference := `The worker Node existed and registered Ready, but it retained the cloud-provider uninitialized taint and had no providerID. cloud-node-manager crash-looped because it could not reach the API Service ClusterIP 10.96.0.1. kube-proxy never synchronized because the API hostname lookup used [::1]:53 and DNS returned connection refused.`
	for _, signal := range flatcar.signals {
		if !signal.matches(reference) {
			t.Errorf("reference diagnosis missed %q", signal.name)
		}
	}
}

func newBenchmarkToolRegistry() *tools.Registry {
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	repotree.Register(registry)
	return registry
}

func TestBenchmarkToolRegistryIncludesSourceTools(t *testing.T) {
	registry := newBenchmarkToolRegistry()
	for _, name := range []string{"grep_repo", "list_repo_tree", "read_repo_file"} {
		schemas := registry.Schemas([]string{name})
		if len(schemas) != 1 || schemas[0].Function.Name != name {
			t.Fatalf("benchmark registry missing %s", name)
		}
	}
}

func validateScoredBenchmarkTraceEnvironment(resultsPath string, getenv func(string) string) error {
	if strings.TrimSpace(resultsPath) != "" {
		if strings.TrimSpace(getenv("AGENTIC_TRACE_TOOLS")) != "" {
			return fmt.Errorf("AGENTIC_TRACE_TOOLS must be unset for scored benchmark execution")
		}
		if strings.TrimSpace(getenv("BENCH_USE_GCS")) != "" {
			return fmt.Errorf("BENCH_USE_GCS is unscored and cannot be used with BENCH_RESULTS_JSONL")
		}
	}
	return nil
}

func TestValidateScoredBenchmarkTraceEnvironment(t *testing.T) {
	if err := validateScoredBenchmarkTraceEnvironment("results.jsonl", func(string) string { return "1" }); err == nil {
		t.Fatal("scored benchmark accepted raw tool tracing")
	}
	if err := validateScoredBenchmarkTraceEnvironment("results.jsonl", func(key string) string {
		if key == "BENCH_USE_GCS" {
			return "1"
		}
		return ""
	}); err == nil {
		t.Fatal("scored benchmark accepted live GCS")
	}
	if err := validateScoredBenchmarkTraceEnvironment("", func(string) string { return "1" }); err != nil {
		t.Fatal(err)
	}
}

func TestAIBenchmark(t *testing.T) {
	if os.Getenv("RUN_AI_BENCHMARK") == "" {
		t.Skip("set RUN_AI_BENCHMARK=1 (plus AI_ENDPOINT/AI_MODEL) to run the AI quality benchmark")
	}
	endpoint, model := os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")
	if endpoint == "" || model == "" {
		t.Fatal("RUN_AI_BENCHMARK set but AI_ENDPOINT/AI_MODEL are not")
	}
	apiMode, err := benchmarkAPIMode()
	if err != nil {
		t.Fatal(err)
	}
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		token = "benchmark" // Dynamo needs no key; keep the client happy.
	}

	cases := benchCases
	if path := strings.TrimSpace(os.Getenv("BENCH_MANIFEST")); path != "" {
		cases, err = loadBenchmarkManifest(path)
		if err != nil {
			t.Fatalf("BENCH_MANIFEST=%s: %v", path, err)
		}
	}
	if selected := strings.TrimSpace(os.Getenv("BENCH_CASE")); selected != "" {
		cases, err = selectBenchmarkCases(cases, selected)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputs := loadBenchmarkInputs(t, cases, apiMode, endpoint, model)

	repetitions := 1
	if raw := strings.TrimSpace(os.Getenv("BENCH_REPETITIONS")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 10 {
			t.Fatalf("BENCH_REPETITIONS must be 1..10")
		}
		repetitions = value
	}
	cacheMode := strings.TrimSpace(os.Getenv("BENCH_CACHE_MODE"))
	if cacheMode != "" && cacheMode != "cold" {
		t.Fatalf("BENCH_CACHE_MODE only supports cold")
	}
	repetitionStart := benchmarkRepetitionStart(t)
	resultsPath := strings.TrimSpace(os.Getenv("BENCH_RESULTS_JSONL"))
	if err := validateScoredBenchmarkTraceEnvironment(resultsPath, os.Getenv); err != nil {
		t.Fatal(err)
	}
	for _, bc := range cases {
		bc := bc
		for index := 0; index < repetitions; index++ {
			repetition := repetitionStart + index
			t.Run(fmt.Sprintf("%s/rep-%02d", bc.name, repetition), func(t *testing.T) {
				runBenchCase(t, bc, repetition, resultsPath, apiMode, endpoint, model, token, inputs.systemPrompt, inputs.agentic, inputs.projectSkills, inputs.cacheGeneration, inputs.identity)
			})
		}
	}
}

func selectBenchmarkCases(cases []benchCase, selected string) ([]benchCase, error) {
	for _, bc := range cases {
		if bc.name == selected {
			return []benchCase{bc}, nil
		}
	}
	return nil, fmt.Errorf("BENCH_CASE %q does not match any benchmark case", selected)
}

func TestSelectBenchmarkCases(t *testing.T) {
	cases := []benchCase{{name: "a"}, {name: "b"}}
	selected, err := selectBenchmarkCases(cases, "b")
	if err != nil || len(selected) != 1 || selected[0].name != "b" {
		t.Fatalf("selected = %+v, error = %v", selected, err)
	}
	if _, err := selectBenchmarkCases(cases, "missing"); err == nil {
		t.Fatal("missing BENCH_CASE was accepted")
	}
}

func validateBenchmarkProjectDir(dir string, bc benchCase) error {
	for path, want := range map[string]string{
		filepath.Join(dir, "project.yaml"):         bc.projectSHA256,
		filepath.Join(dir, "prompts", "system.md"): bc.promptSHA256,
	} {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return fmt.Errorf("read pinned benchmark consumer file %s: %w", path, err)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(data))
		if got != want {
			return fmt.Errorf("pinned benchmark consumer file %s SHA-256 = %s, want %s", path, got, want)
		}
	}
	command := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("resolve pinned benchmark consumer commit: %w", err)
	}
	if got := strings.TrimSpace(string(output)); got != bc.consumerCommit {
		return fmt.Errorf("pinned benchmark consumer commit = %s, want %s", got, bc.consumerCommit)
	}
	status, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return fmt.Errorf("inspect pinned benchmark consumer worktree: %w", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("pinned benchmark consumer worktree is not clean")
	}
	return nil
}

func TestValidateBenchmarkProjectDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectData := []byte("id: test\n")
	promptData := []byte("test prompt\n")
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), projectData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), promptData, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "project.yaml", "prompts/system.md"},
		{"-c", "commit.gpgsign=false", "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-qm", "fixture"},
	} {
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	bc := benchCase{
		consumerCommit: strings.TrimSpace(string(head)),
		projectSHA256:  fmt.Sprintf("%x", sha256.Sum256(projectData)),
		promptSHA256:   fmt.Sprintf("%x", sha256.Sum256(promptData)),
	}
	if err := validateBenchmarkProjectDir(dir, bc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBenchmarkProjectDir(dir, bc); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed prompt error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), promptData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "variant.yaml"), []byte("id: variant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBenchmarkProjectDir(dir, bc); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("untracked skill error = %v", err)
	}
}

func benchmarkRepetitionStart(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("BENCH_REPETITION_START"))
	if raw == "" {
		return 1
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 1000 {
		t.Fatalf("BENCH_REPETITION_START must be 1..1000")
	}
	return value
}

func TestBenchmarkRepetitionStart(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("BENCH_REPETITION_START", "")
		if got := benchmarkRepetitionStart(t); got != 1 {
			t.Fatalf("start = %d, want 1", got)
		}
	})
	t.Run("configured", func(t *testing.T) {
		t.Setenv("BENCH_REPETITION_START", "2")
		if got := benchmarkRepetitionStart(t); got != 2 {
			t.Fatalf("start = %d, want 2", got)
		}
	})
}

func benchmarkAPIMode() (string, error) {
	apiMode := strings.ToLower(strings.TrimSpace(os.Getenv("AI_API")))
	if apiMode == "" {
		return ai.APIChatCompletions, nil
	}
	switch apiMode {
	case ai.APIChatCompletions, ai.APIResponses:
		return apiMode, nil
	default:
		return "", fmt.Errorf("AI_API must be %q or %q", ai.APIChatCompletions, ai.APIResponses)
	}
}

func TestBenchmarkAPIMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "default", want: ai.APIChatCompletions, ok: true},
		{name: "chat", value: " Chat_Completions ", want: ai.APIChatCompletions, ok: true},
		{name: "responses", value: " Responses ", want: ai.APIResponses, ok: true},
		{name: "invalid", value: "completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AI_API", tc.value)
			got, err := benchmarkAPIMode()
			if (err == nil) != tc.ok {
				t.Fatalf("benchmarkAPIMode error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("benchmarkAPIMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func runBenchCase(t *testing.T, bc benchCase, repetition int, resultsPath, apiMode, endpoint, model, token, systemPrompt string, agentic project.Agentic, projectSkills *skills.Set, cacheGeneration string, identity benchmarkRunIdentity) {
	identity.FixtureSHA256 = bc.fixtureSHA256
	backend, bucketLabel := benchStorage(t, bc)
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo},
		JobName:     bc.jobName,
		BuildID:     bc.buildID,
		PullNumber:  bc.pullNumber,
	}
	evidenceRecorder := newBenchmarkEvidenceRecorder(bc.evidenceGroups)
	baseFactory := artifacts.NewBackendFactory(backend, bucketLabel)
	preparation, err := prepareBenchmarkEvidence(context.Background(), baseFactory.ForBuild(loc.BuildPath(), bc.jobName), bc, identity.EvidenceCondition, evidenceRecorder)
	if err != nil {
		t.Fatalf("prepare benchmark evidence: %v", err)
	}
	effectivePrompt := systemPrompt + preparation.prompt
	identity.FrozenEvidenceSHA256 = preparation.frozenSHA256
	identity.EvidenceStageSHA256 = benchmarkEvidenceStageSHA256(bc.evidenceGroups)
	identity.EffectivePromptSHA256 = sha256Hex([]byte(effectivePrompt))
	identity.EffectiveInputSHA256 = benchmarkEffectiveInputSHA256(identity, agentic, cacheGeneration)
	identity.ComparisonInputSHA256 = benchmarkComparisonInputSHA256(bc, identity)
	if err := validateBenchmarkRunIdentity(identity); err != nil {
		t.Fatal(err)
	}
	cacheDir := benchmarkCacheDir(t, bc, repetition, identity)
	clientOptions := ai.Options{
		Token: token, API: apiMode, Endpoint: endpoint, Model: model, ReasoningEffort: identity.ReasoningEffort, CacheDir: cacheDir,
		MaxOutputTokens: identity.ModelOutputTokens,
	}
	client := ai.NewClientWithOptions(clientOptions)

	var factory artifacts.Factory = baseFactory
	if len(bc.evidenceGroups) > 0 {
		factory = benchmarkEvidenceFactory{inner: factory, recorder: evidenceRecorder}
	}

	registry := newBenchmarkToolRegistry()
	toolNames := agentic.Tools
	if len(toolNames) == 0 {
		toolNames = []string{"filesystem", "k8s"}
	}
	enabled, err := registry.Enable(toolNames)
	if err != nil {
		t.Fatalf("enable tools: %v", err)
	}

	// Feed the persistence signal the live engine would have from the flakiness
	// report. The service keys it as jobID + "::" + testName (consecutiveKey).
	jobID := models.JobIDFor(bc.jobType, bc.repo, bc.jobName)
	var consecutiveMap map[string]int
	if bc.consecutiveFailures > 0 {
		consecutiveMap = map[string]int{jobID + "::" + bc.testName: bc.consecutiveFailures}
	}

	serviceConfig := ai.ServiceConfig{
		Client: client, Module: universal.New(), SystemPrompt: effectivePrompt,
		ConsecutiveFailures: consecutiveMap, CacheGeneration: cacheGeneration,
		Skills: projectSkills, SourceRepoOwner: bc.sourceRepo[0], SourceRepoName: bc.sourceRepo[1],
	}
	if len(bc.sourceRefs) > 0 {
		sources := make([]tools.RepoSource, 0, len(bc.sourceRefs))
		for _, ref := range bc.sourceRefs {
			owner, name, ok := strings.Cut(ref.Repository, "/")
			if !ok {
				t.Fatalf("invalid source repository %q", ref.Repository)
			}
			sources = append(sources, tools.RepoSource{ID: ref.ID, Owner: owner, Name: name, Revision: ref.Revision, Reader: benchmarkRepoReader(t, ref)})
		}
		catalog, err := tools.NewSourceCatalog(bc.primarySourceID, sources)
		if err != nil {
			t.Fatal(err)
		}
		serviceConfig.AnalysisSourceCatalog = catalog
	}
	traceStore := ai.NewTraceStore()
	serviceConfig.TraceStore = traceStore
	var draftObservations []benchmarkDraftObservation
	selectedAttempt := 0
	serviceConfig.DraftObserver = func(observation ai.DraftObservation) {
		draftObservations = append(draftObservations, benchmarkDraftObservation{
			DraftObservation: observation,
			observedAt:       time.Now(),
		})
	}
	serviceConfig.DraftSelectionObserver = func(attempt int) { selectedAttempt = attempt }
	var sourceObservations []ai.SourceEvidenceObservation
	serviceConfig.SourceEvidenceObserver = func(observation ai.SourceEvidenceObservation) {
		sourceObservations = append(sourceObservations, observation)
	}

	// Scored comparisons use the exact frozen model window. Unscored local runs
	// retain endpoint detection and the bounded fallback.
	budgets := benchBudgets(t, client, identity.ModelContextTokens)
	serviceConfig.AgenticOptions = ai.AgenticOptions{
		MaxIters:            agentic.MaxIters,
		ModelByteBudget:     budgets.ModelByteBudget,
		GCSByteBudget:       benchGCSByteBudget,
		Timeout:             agentic.Timeout,
		ContextByteBudget:   budgets.ContextByteBudget,
		ContextWindowTokens: budgets.ContextWindowTokens,
		RequestTokenBudget:  budgets.RequestTokenBudget,
		MinToolCalls:        agentic.MinToolCalls,
		MinGCSBytes:         agentic.MinGCSBytes,
		CritiqueMaxRetries:  *agentic.Critique.MaxRetries,
		CritiqueCachePolicy: ai.CritiqueCachePolicy(agentic.Critique.EffectiveCachePolicy()),
		SingleToolCall:      agentic.SingleToolCall,
	}
	serviceConfig.BrowserFactory = factory
	serviceConfig.ToolRegistry = registry
	serviceConfig.EnabledTools = enabled
	service := ai.NewService(serviceConfig)

	run := &models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: bc.buildID, JobName: bc.jobName, PullNumber: bc.pullNumber, WebURL: bc.webURL,
		Commit: bc.commit, RepoVersion: bc.repoVersion, RepoRefs: maps.Clone(bc.repoRefs),
	}}
	tc := benchTestCase(bc)

	start := time.Now()
	result, analysisErr := service.AnalyzeFailure(context.Background(), &http.Client{Timeout: 60 * time.Second}, ai.FailureAnalysisRequest{
		JobID: jobID, BuildPrefix: loc.BuildPath(), Build: run.BuildInfo, TestCase: *tc,
		ConsecutiveFailures: bc.consecutiveFailures, CacheGeneration: cacheGeneration,
	})
	tc.AISummary, tc.AIAnalysis = result.Summary, result.Analysis
	outcome, outcomeErr := benchmarkOutcomeForAnalysisError(analysisErr)
	elapsed := time.Since(start).Round(time.Second)

	snapshot := traceStore.Snapshot()
	toolUsage := successfulBenchmarkToolUsage(snapshot)
	toolUsage.sourceObservations = append([]ai.SourceEvidenceObservation(nil), sourceObservations...)
	traceSummary := summarizeBenchmarkTrace(snapshot)
	requestCap := deriveBenchmarkRequestCap(agentic)
	t.Logf("provider request cap: configured_iterations=%d byte_floor_extensions=%d main_loop=%d forced_finalizations=%d critique_tool_turns=%d critique_finalizations=%d per_operation=%d",
		requestCap.ConfiguredIterations, requestCap.ByteFloorExtensions, requestCap.MainLoopRequests, requestCap.ForcedFinalizationRequests,
		requestCap.CritiqueToolRequests, requestCap.CritiqueFinalizationRequests, requestCap.PerOperation)
	if benchmarkPersistentCacheEnabled() {
		if err := client.Cache().Save(); err != nil {
			t.Fatalf("save benchmark cache: %v", err)
		}
	}
	cacheVerification := benchmarkCacheVerification{}
	if benchmarkCacheReuseEnabled() && tc.AIAnalysis != nil {
		cacheVerification = verifyBenchmarkCacheReuse(t, client, clientOptions, service, cacheGeneration, jobID, bc, run, tc.AIAnalysis)
	}
	critiquePolicy := ai.CritiqueCachePolicy(agentic.Critique.EffectiveCachePolicy())
	evidenceCoverage := evidenceRecorder.coverage()
	if len(bc.evidenceGroups) > 0 {
		t.Logf("evidence_groups selected=%v hit=%v missed=%v sources=%v", evidenceCoverage.selected, evidenceCoverage.hit, evidenceCoverage.missed, evidenceCoverage.sources)
	}
	if selectedAttempt == 0 {
		selectedAttempt = selectedBenchmarkDraftAttempt(draftObservations, tc)
	}
	trialStatus := benchmarkTrialStatus(outcome, analysisErr, tc, snapshot)
	if len(draftObservations) > 0 && selectedAttempt == 0 && (tc == nil || tc.AIAnalysis == nil) {
		trialStatus = "contract_violation"
	}
	stageReport := buildBenchmarkEvidenceStageReport(bc, preparation, evidenceCoverage, tc, draftObservations, selectedAttempt, traceSummary.modelRequests > 0, trialStatus)
	if err := validateBenchmarkEvidenceStageReport(bc, stageReport); err != nil {
		t.Fatalf("validate benchmark evidence stages: %v", err)
	}
	writeBenchmarkJSONL(t, resultsPath, bc, repetition, tc, outcome, elapsed, snapshot, draftObservations, selectedAttempt, toolUsage, traceSummary, requestCap.PerOperation, cacheGeneration, critiquePolicy, cacheVerification, identity, evidenceCoverage, stageReport)
	if trialStatus == "contract_violation" || trialStatus == "no_result" {
		t.Fatalf("benchmark trial status is %s", trialStatus)
	}
	if outcomeErr != nil {
		t.Fatalf("analysis failed before scoring: %v", outcomeErr)
	}
	if traceSummary.truncated {
		t.Fatalf("provider request cap cannot be verified from a truncated trace")
	}
	if traceSummary.modelRequests > requestCap.PerOperation || traceSummary.providerAttempts > requestCap.PerOperation {
		t.Fatalf("provider request cap exceeded: model_requests=%d provider_attempts=%d cap=%d", traceSummary.modelRequests, traceSummary.providerAttempts, requestCap.PerOperation)
	}
	scoreBenchCase(t, bc, tc, outcome, elapsed, "in-process", benchmarkMinGCSBytes(bc, agentic.MinGCSBytes), toolUsage, traceSummary, draftObservations, selectedAttempt)
}

func benchmarkOutcomeForAnalysisError(err error) (benchmarkOutcome, error) {
	if err == nil {
		return benchmarkOutcomeUsable, nil
	}
	if errors.Is(err, ai.ErrMissingArtifactCitation) {
		return benchmarkOutcomeCitationPolicyUnavailable, nil
	}
	return benchmarkOutcomeUnknown, err
}

func TestBenchmarkOutcomeForAnalysisError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome benchmarkOutcome
		ok      bool
	}{
		{name: "usable", outcome: benchmarkOutcomeUsable, ok: true},
		{name: "citation policy", err: fmt.Errorf("wrapped: %w", ai.ErrMissingArtifactCitation), outcome: benchmarkOutcomeCitationPolicyUnavailable, ok: true},
		{name: "provider", err: errors.New("provider 503"), outcome: benchmarkOutcomeUnknown},
		{name: "timeout", err: context.DeadlineExceeded, outcome: benchmarkOutcomeUnknown},
		{name: "tools", err: ai.ErrToolsUnsupported, outcome: benchmarkOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := benchmarkOutcomeForAnalysisError(tc.err)
			if outcome != tc.outcome || (err == nil) != tc.ok {
				t.Fatalf("outcome = %q, error = %v", outcome, err)
			}
		})
	}
}

func benchmarkMinGCSBytes(bc benchCase, configured int) int {
	if bc.testSource == models.TestCaseSourceBuild {
		return 0
	}
	return configured
}

func benchmarkCacheGenerationFingerprint(configValue string) (string, error) {
	value, err := project.ResolveAICacheGeneration(configValue, os.Getenv(project.AICacheGenerationEnv))
	if err != nil {
		return "", err
	}
	return project.AICacheGenerationFingerprint(value), nil
}

func benchmarkCacheCaseKey(bc benchCase) string {
	if bc.stableID != "" {
		return bc.stableID
	}
	sum := sha256.Sum256([]byte(bc.name))
	return fmt.Sprintf("case-%x", sum[:10])
}

func benchmarkPersistentCacheEnabled() bool {
	return strings.TrimSpace(os.Getenv("BENCH_CACHE_DIR")) != ""
}

func benchmarkCacheDir(t *testing.T, bc benchCase, repetition int, identity benchmarkRunIdentity) string {
	t.Helper()
	root := strings.TrimSpace(os.Getenv("BENCH_CACHE_DIR"))
	if root == "" {
		return t.TempDir()
	}
	if !benchmarkCaseIDRE.MatchString(identity.Arm) || !benchmarkSHA256RE.MatchString(identity.EffectiveInputSHA256) {
		t.Fatal("benchmark cache identity requires a valid arm and effective input SHA-256")
	}
	dir := filepath.Join(root, benchmarkCacheCaseKey(bc), identity.Arm+"-"+identity.EffectiveInputSHA256[:12], fmt.Sprintf("rep-%02d", repetition))
	if _, err := os.Stat(filepath.Join(dir, ai.CacheFilename)); err == nil {
		t.Fatalf("BENCH_CACHE_DIR is not cold: %s", dir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect BENCH_CACHE_DIR: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create BENCH_CACHE_DIR: %v", err)
	}
	return dir
}

func benchmarkCacheReuseEnabled() bool {
	return strings.TrimSpace(os.Getenv("BENCH_VERIFY_CACHE_REUSE")) == "1"
}

func verifyBenchmarkCacheReuse(t *testing.T, client *ai.Client, clientOptions ai.Options, service *ai.Service, cacheGeneration, jobID string, bc benchCase, run *models.BuildResult, analysis *models.AIAnalysis) benchmarkCacheVerification {
	t.Helper()
	out := benchmarkCacheVerification{LookupAttempted: true, CacheGeneration: cacheGeneration}
	if analysis != nil {
		out.PersistenceAttempted = analysis.CachePersistenceAttempted
		out.PersistenceAccepted = analysis.CachePersistenceAccepted
		out.PolicyRejectionReason = ai.CacheRejectionReason(analysis.CachePolicyRejectionReason)
	}
	if err := client.Cache().Save(); err != nil {
		t.Fatalf("save benchmark cache: %v", err)
	}
	out.CacheSaveSucceeded = true
	reloadedClient := ai.NewClientWithOptions(clientOptions)
	fresh := benchTestCase(bc)
	policy := service.FailureCachePolicy(context.Background(), &http.Client{Timeout: 60 * time.Second}, run, fresh, bc.consecutiveFailures)
	key := ai.AgenticCacheKeyForGeneration(universal.New().Name(), cacheGeneration, jobID, bc.buildID, bc.testName, fresh.FailureMessage)
	result, reason := ai.LookupAgenticCache(reloadedClient.Cache(), key, policy)
	out.UnavailableCooldownHit = ai.LookupPolicyUnavailableCooldown(reloadedClient.Cache(), ai.PolicyUnavailableCacheKey(key), policy, time.Now())
	out.LookupRejectionReason = reason
	out.LookupAccepted = reason == ai.CacheAccepted
	if result.Analysis != nil {
		out.LookupHit = result.Analysis.CacheHit
		out.EvidencePlanCovered = result.Analysis.EvidencePlanCovered
		out.GCSFloorRetryExhausted = result.Analysis.GCSFloorRetryExhausted
		out.CacheGeneration = result.Analysis.CacheGeneration
	}
	return out
}

type benchmarkDraftObservation struct {
	ai.DraftObservation
	observedAt time.Time
}

type benchmarkDraftScore struct {
	observation benchmarkDraftObservation
	hit         int
	total       int
	requiredHit int
	required    int
}

type benchmarkToolUsage struct {
	names              []string
	counts             []string
	sourceObservations []ai.SourceEvidenceObservation
}

type benchmarkTraceSummary struct {
	floorNudges              int
	floorNudgeReasons        []string
	contextCompactionApplied int
	contextOverBudget        int
	critiqueRetries          int
	evidenceRetries          int
	unparseableRetries       int
	acceptedUncached         int
	truncated                bool
	modelRequests            int
	providerAttempts         int
	modelFailures            int
	toolFailures             int
	inputTokens              int
	cachedInputTokens        int
	outputTokens             int
	reasoningTokens          int
	reportedRequests         int
	providerAttemptsKnown    bool
}

const benchmarkHashPrefixLen = 12

var (
	benchmarkSafeHashRE     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	benchmarkSafeToolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

func successfulBenchmarkToolUsage(snapshot ai.AnalysisTraceFile) benchmarkToolUsage {
	counts := map[string]int{}
	for _, trace := range snapshot.Traces {
		for _, event := range trace.Events {
			if event.Kind == "tool_call" && event.Outcome == "success" && benchmarkSafeToolNameRE.MatchString(event.Tool) {
				counts[event.Tool]++
			}
		}
	}

	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	formattedCounts := make([]string, 0, len(names))
	for _, name := range names {
		formattedCounts = append(formattedCounts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return benchmarkToolUsage{names: names, counts: formattedCounts}
}

func summarizeBenchmarkTrace(snapshot ai.AnalysisTraceFile) benchmarkTraceSummary {
	summary := benchmarkTraceSummary{providerAttemptsKnown: true}
	for _, trace := range snapshot.Traces {
		if trace.Truncated {
			summary.truncated = true
		}
		for _, event := range trace.Events {
			switch event.Kind {
			case "floor_nudge":
				if event.Outcome == "" || event.Outcome == "retry" {
					summary.floorNudges++
					summary.floorNudgeReasons = append(summary.floorNudgeReasons, benchmarkFloorNudgeReason(event.Status))
				}
			case "context_compaction":
				switch event.Outcome {
				case "applied", "finalize":
					summary.contextCompactionApplied++
				case "over_budget":
					summary.contextOverBudget++
				}
			case "critique":
				switch event.Outcome {
				case "retry":
					summary.critiqueRetries++
				case "evidence_retry":
					summary.evidenceRetries++
				case "unparseable_retry":
					summary.unparseableRetries++
				case "accepted_uncached":
					summary.acceptedUncached++
				}
			case "model_request":
				summary.modelRequests++
				if event.Attempts > 0 {
					summary.providerAttempts += event.Attempts
				} else {
					summary.providerAttempts++
					summary.providerAttemptsKnown = false
				}
				if event.Outcome == "error" {
					summary.modelFailures++
				}
				if event.UsageReported {
					summary.reportedRequests++
				}
				summary.inputTokens += event.InputTokens
				summary.cachedInputTokens += event.CachedInputTokens
				summary.outputTokens += event.OutputTokens
				summary.reasoningTokens += event.ReasoningTokens
			case "tool_call":
				if event.Outcome == "error" {
					summary.toolFailures++
				}
			}
		}
	}
	return summary
}

func benchmarkFloorNudgeReason(status string) string {
	switch status {
	case "tool_calls", "gcs_bytes", "tool_calls+gcs_bytes":
		return status
	default:
		return "unknown"
	}
}

func benchmarkSkillHashPrefix(hash string) string {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return "none"
	}
	if !benchmarkSafeHashRE.MatchString(hash) {
		return "invalid"
	}
	hash = strings.ToLower(hash)
	if len(hash) > benchmarkHashPrefixLen {
		return hash[:benchmarkHashPrefixLen]
	}
	return hash
}

func benchmarkGCSFloorBypassed(analysis *models.AIAnalysis, minGCSBytes int) bool {
	return analysis != nil && analysis.GCSBytes < minGCSBytes && (analysis.EvidencePlanCovered || analysis.GCSFloorRetryExhausted)
}

func benchmarkTelemetryLines(elapsed time.Duration, analysis *models.AIAnalysis, minGCSBytes int, toolUsage benchmarkToolUsage, trace benchmarkTraceSummary) []string {
	lines := make([]string, 0, 3)
	if analysis == nil {
		lines = append(lines, fmt.Sprintf(
			"elapsed=%s tool_names=%v tool_counts=%v context_truncations=%d model_requests=%d provider_attempts=%d model_failures=%d tool_failures=%d input_tokens=%d output_tokens=%d",
			elapsed, toolUsage.names, toolUsage.counts, trace.contextCompactionApplied, trace.modelRequests, trace.providerAttempts, trace.modelFailures, trace.toolFailures, trace.inputTokens, trace.outputTokens))
	} else {
		lines = append(lines, fmt.Sprintf(
			"elapsed=%s tool_calls=%d tool_names=%v tool_counts=%v gcs_bytes=%d context_bytes=%d context_truncations=%d model_requests=%d provider_attempts=%d model_failures=%d tool_failures=%d input_tokens=%d output_tokens=%d",
			elapsed, analysis.ToolCalls, toolUsage.names, toolUsage.counts, analysis.GCSBytes, analysis.ContextBytes, trace.contextCompactionApplied,
			trace.modelRequests, trace.providerAttempts, trace.modelFailures, trace.toolFailures, trace.inputTokens, trace.outputTokens))
		lines = append(lines, fmt.Sprintf(
			"quality evidence_plan_covered=%v gcs_floor_retry_exhausted=%v gcs_floor_bypassed=%v critique_passed=%v critique_version=%d skill_set_hash=%s budget_exhausted=%v",
			analysis.EvidencePlanCovered, analysis.GCSFloorRetryExhausted, benchmarkGCSFloorBypassed(analysis, minGCSBytes), analysis.CritiquePassed, analysis.CritiqueVersion,
			benchmarkSkillHashPrefix(analysis.SkillSetHash), analysis.BudgetExhausted))
	}
	lines = append(lines, fmt.Sprintf(
		"trace floor_nudges=%d floor_nudge_reasons=%v context_compaction_applied=%d context_over_budget=%d critique_retries=%d evidence_retries=%d unparseable_retries=%d accepted_uncached=%d",
		trace.floorNudges, trace.floorNudgeReasons, trace.contextCompactionApplied, trace.contextOverBudget,
		trace.critiqueRetries, trace.evidenceRetries, trace.unparseableRetries, trace.acceptedUncached))
	return lines
}

func scoreBenchmarkDraft(bc benchCase, observation benchmarkDraftObservation) benchmarkDraftScore {
	text := strings.ToLower(strings.Join([]string{observation.Summary, observation.RootCause, observation.SuggestedFix}, "\n"))
	score := benchmarkDraftScore{observation: observation, total: len(bc.signals)}
	for _, signal := range bc.signals {
		matched := signal.matches(text)
		if matched {
			score.hit++
		}
		if signal.must {
			score.required++
			if matched {
				score.requiredHit++
			}
		}
	}
	return score
}

func benchmarkDraftTelemetryLines(bc benchCase, observations []benchmarkDraftObservation, tc *models.TestCase, selected int) []string {
	if len(observations) == 0 {
		return nil
	}
	scores := make([]benchmarkDraftScore, 0, len(observations))
	for _, observation := range observations {
		scores = append(scores, scoreBenchmarkDraft(bc, observation))
	}
	if selected == 0 {
		selected = selectedBenchmarkDraftAttempt(observations, tc)
	}
	lines := make([]string, 0, len(scores)*2)
	for _, score := range scores {
		lines = append(lines, fmt.Sprintf(
			"draft attempt=%d phase=%s score=%d/%d required_signals=%d/%d issue_vector=%s tool_calls=%d evidence_reads=%d selected=%v",
			score.observation.Attempt, score.observation.Phase, score.hit, score.total, score.requiredHit, score.required,
			benchmarkDraftIssueVector(score.observation.DraftObservation), score.observation.ToolCalls, score.observation.EvidenceReads,
			score.observation.Attempt == selected))
	}
	for i := 1; i < len(scores); i++ {
		revised := scores[i]
		if !benchmarkRepairPhase(revised.observation.Phase) {
			continue
		}
		initial := scores[i-1]
		duration := revised.observation.observedAt.Sub(initial.observation.observedAt)
		if duration < 0 {
			duration = 0
		}
		newEvidenceReads := revised.observation.EvidenceReads - initial.observation.EvidenceReads
		if newEvidenceReads < 0 {
			newEvidenceReads = 0
		}
		lines = append(lines, fmt.Sprintf(
			"draft_pair initial_attempt=%d revised_attempt=%d initial_score=%d/%d revised_score=%d/%d score_delta=%d initial_required_signals=%d/%d revised_required_signals=%d/%d initial_issue_vector=%s revised_issue_vector=%s root_cause_changed=%v new_evidence_reads=%d retry_duration_ms=%d selected_attempt=%d",
			initial.observation.Attempt, revised.observation.Attempt, initial.hit, initial.total, revised.hit, revised.total, revised.hit-initial.hit,
			initial.requiredHit, initial.required, revised.requiredHit, revised.required,
			benchmarkDraftIssueVector(initial.observation.DraftObservation), benchmarkDraftIssueVector(revised.observation.DraftObservation),
			normalizeBenchmarkRootCause(initial.observation.RootCause) != normalizeBenchmarkRootCause(revised.observation.RootCause),
			newEvidenceReads, duration.Milliseconds(), selected))
	}
	return lines
}

func benchmarkRepairPhase(phase string) bool {
	switch phase {
	case "critique_retry", "evidence_retry":
		return true
	default:
		return false
	}
}

func benchmarkDraftIssueVector(observation ai.DraftObservation) string {
	return fmt.Sprintf("punt=%d,unread=%d,citation=%d,missing=%d,transient=%v,rules=%v,published_hard=%d,published_punt=%d,published_missing=%d,published_rules=%v",
		observation.PuntCount, observation.UnreadCitationCount, observation.CitationIssueCount,
		observation.MissingGroupCount, observation.TransientConflict, observation.RuleIDs,
		observation.PublishedHardIssues, observation.PublishedPuntCount, observation.PublishedMissing, observation.PublishedRuleIDs)
}

func normalizeBenchmarkRootCause(rootCause string) string {
	return strings.ToLower(strings.Join(strings.Fields(rootCause), " "))
}

func selectedBenchmarkDraftAttempt(observations []benchmarkDraftObservation, tc *models.TestCase) int {
	if tc == nil || tc.AISummary == nil || tc.AIAnalysis == nil {
		return 0
	}
	for i := len(observations) - 1; i >= 0; i-- {
		observation := observations[i]
		if observation.Summary == tc.AISummary.Summary &&
			observation.RootCause == tc.AIAnalysis.RootCause &&
			observation.SuggestedFix == tc.AIAnalysis.SuggestedFix &&
			observation.Severity == tc.AIAnalysis.Severity &&
			slices.Equal(observation.RelevantFiles, tc.AIAnalysis.RelevantFiles) &&
			observation.IsTransient == tc.AISummary.IsTransient {
			return observation.Attempt
		}
	}
	return 0
}

func TestSuccessfulBenchmarkToolUsage(t *testing.T) {
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{
		{Events: []ai.TraceEvent{
			{Kind: "tool_call", Tool: "read_artifact", Outcome: "success"},
			{Kind: "tool_call", Tool: "grep_artifact", Outcome: "success"},
			{Kind: "tool_call", Tool: "read_artifact", Outcome: "success"},
			{Kind: "tool_call", Tool: "list_artifacts", Outcome: "error"},
			{Kind: "model_request", Tool: "ignored", Outcome: "success"},
		}},
		{Events: []ai.TraceEvent{
			{Kind: "tool_call", Tool: "discover_clusters", Outcome: "success"},
			{Kind: "tool_call", Outcome: "success"},
		}},
	}}

	got := successfulBenchmarkToolUsage(snapshot)
	if want := []string{"discover_clusters", "grep_artifact", "read_artifact"}; !slices.Equal(got.names, want) {
		t.Errorf("names = %v, want %v", got.names, want)
	}
	if want := []string{"discover_clusters=1", "grep_artifact=1", "read_artifact=2"}; !slices.Equal(got.counts, want) {
		t.Errorf("counts = %v, want %v", got.counts, want)
	}
}

func TestSummarizeBenchmarkTrace(t *testing.T) {
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Truncated: true, Events: []ai.TraceEvent{
		{Kind: "floor_nudge", Status: "tool_calls"},
		{Kind: "floor_nudge", Status: "tool_calls+gcs_bytes"},
		{Kind: "floor_nudge"},
		{Kind: "context_compaction", Outcome: "applied"},
		{Kind: "context_compaction", Outcome: "finalize"},
		{Kind: "context_compaction", Outcome: "over_budget"},
		{Kind: "critique", Outcome: "retry"},
		{Kind: "critique", Outcome: "evidence_retry"},
		{Kind: "critique", Outcome: "unparseable_retry"},
		{Kind: "critique", Outcome: "accepted_uncached"},
		{Kind: "model_request", Outcome: "success", Attempts: 2, InputTokens: 10, OutputTokens: 4},
		{Kind: "model_request", Outcome: "error", InputTokens: 3, OutputTokens: 1},
		{Kind: "tool_call", Outcome: "error"},
	}}}}

	got := summarizeBenchmarkTrace(snapshot)
	if got.floorNudges != 3 || !slices.Equal(got.floorNudgeReasons, []string{"tool_calls", "tool_calls+gcs_bytes", "unknown"}) {
		t.Fatalf("floor summary = count:%d reasons:%v", got.floorNudges, got.floorNudgeReasons)
	}
	if got.contextCompactionApplied != 2 || got.contextOverBudget != 1 {
		t.Fatalf("context summary = applied:%d over_budget:%d", got.contextCompactionApplied, got.contextOverBudget)
	}
	if got.critiqueRetries != 1 || got.evidenceRetries != 1 || got.unparseableRetries != 1 || got.acceptedUncached != 1 {
		t.Fatalf("critique summary = retries:%d evidence:%d unparseable:%d uncached:%d", got.critiqueRetries, got.evidenceRetries, got.unparseableRetries, got.acceptedUncached)
	}
	if !got.truncated || got.modelRequests != 2 || got.providerAttempts != 3 || got.modelFailures != 1 || got.toolFailures != 1 || got.inputTokens != 13 || got.outputTokens != 5 {
		t.Fatalf("usage summary = %+v", got)
	}
}

func TestBenchmarkTelemetryQualityFields(t *testing.T) {
	analysis := &models.AIAnalysis{
		ToolCalls: 1, GCSBytes: 99, ContextBytes: 250, EvidencePlanCovered: true,
		CritiquePassed: true, CritiqueVersion: 7, SkillSetHash: strings.Repeat("a", 64),
		BudgetExhausted: true,
	}
	trace := benchmarkTraceSummary{contextCompactionApplied: 2, modelRequests: 4, providerAttempts: 5, modelFailures: 1, toolFailures: 2, inputTokens: 30, outputTokens: 12}
	got := strings.Join(benchmarkTelemetryLines(time.Second, analysis, 100, benchmarkToolUsage{}, trace), "\n")
	for _, want := range []string{
		"evidence_plan_covered=true", "gcs_floor_bypassed=true", "critique_passed=true", "critique_version=7",
		"skill_set_hash=aaaaaaaaaaaa", "budget_exhausted=true",
		"context_truncations=2", "model_requests=4", "provider_attempts=5", "model_failures=1", "tool_failures=2", "input_tokens=30", "output_tokens=12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("telemetry missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, analysis.SkillSetHash) {
		t.Fatalf("telemetry contains full skill hash: %s", got)
	}
	if strings.Contains(got, "tool_floor_bypassed=true") {
		t.Fatalf("telemetry claimed the Tool floor was bypassed: %s", got)
	}
}

func TestBenchmarkDraftScoringProducesPairedDeltas(t *testing.T) {
	bc := benchCase{signals: []benchSignal{
		{name: "required cause", re: mustRE(`correct cause`), must: true},
		{name: "supporting detail", re: mustRE(`providerid`)},
	}}
	start := time.Unix(0, 0)
	observations := []benchmarkDraftObservation{
		{
			DraftObservation: ai.DraftObservation{
				Attempt: 1, Phase: "initial", Summary: "correct cause", RootCause: "correct cause",
				SuggestedFix: "Check the controller.", PuntCount: 1, ToolCalls: 4, EvidenceReads: 2,
			},
			observedAt: start,
		},
		{
			DraftObservation: ai.DraftObservation{
				Attempt: 2, Phase: "critique_retry", Summary: "correct cause", RootCause: "correct cause from missing providerID",
				SuggestedFix: "Set spec.providerID from the Azure resource ID.", ToolCalls: 5, EvidenceReads: 3,
			},
			observedAt: start.Add(1500 * time.Millisecond),
		},
	}
	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: observations[1].Summary},
		AIAnalysis: &models.AIAnalysis{
			RootCause: observations[1].RootCause, SuggestedFix: observations[1].SuggestedFix,
			Severity: observations[1].Severity,
		},
	}
	got := strings.Join(benchmarkDraftTelemetryLines(bc, observations, tc, 2), "\n")
	for _, want := range []string{
		"draft attempt=1 phase=initial score=1/2",
		"draft attempt=2 phase=critique_retry score=2/2",
		"initial_score=1/2 revised_score=2/2 score_delta=1",
		"initial_required_signals=1/1 revised_required_signals=1/1",
		"initial_issue_vector=punt=1,unread=0,citation=0,missing=0,transient=false,rules=[],published_hard=0,published_punt=0,published_missing=0,published_rules=[]",
		"revised_issue_vector=punt=0,unread=0,citation=0,missing=0,transient=false,rules=[],published_hard=0,published_punt=0,published_missing=0,published_rules=[]",
		"root_cause_changed=true new_evidence_reads=1 retry_duration_ms=1500 selected_attempt=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("paired draft telemetry missing %q:\n%s", want, got)
		}
	}
}

func TestSelectedBenchmarkDraftAttemptUsesPublishedIdentity(t *testing.T) {
	observations := []benchmarkDraftObservation{
		{DraftObservation: ai.DraftObservation{
			Attempt: 1, Summary: "accepted summary", RootCause: "same cause", SuggestedFix: "same fix",
			Severity: "High", RelevantFiles: []string{"accepted.go"},
		}},
		{DraftObservation: ai.DraftObservation{
			Attempt: 2, Phase: "critique_retry", Summary: "rejected summary", RootCause: "same cause", SuggestedFix: "same fix",
			Severity: "High", RelevantFiles: []string{"rejected.go"},
		}},
	}
	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "accepted summary"},
		AIAnalysis: &models.AIAnalysis{
			RootCause: "same cause", SuggestedFix: "same fix", Severity: "High", RelevantFiles: []string{"accepted.go"},
		},
	}
	if got := selectedBenchmarkDraftAttempt(observations, tc); got != 1 {
		t.Fatalf("selected attempt = %d, want 1", got)
	}
}

func TestBenchmarkDraftTelemetryUsesRuntimeSelection(t *testing.T) {
	bc := benchCase{signals: []benchSignal{{name: "cause", re: mustRE(`cause`)}}}
	observations := []benchmarkDraftObservation{
		{DraftObservation: ai.DraftObservation{Attempt: 1, Phase: "initial", Summary: "cause", RootCause: "cause", SuggestedFix: "fix"}},
		{DraftObservation: ai.DraftObservation{Attempt: 2, Phase: "critique_retry", Summary: "cause", RootCause: "cause", SuggestedFix: "fix"}},
	}
	tc := &models.TestCase{
		AISummary:  &models.AISummary{Summary: "cause"},
		AIAnalysis: &models.AIAnalysis{RootCause: "cause", SuggestedFix: "fix"},
	}
	got := strings.Join(benchmarkDraftTelemetryLines(bc, observations, tc, 1), "\n")
	if !strings.Contains(got, "attempt=1 phase=initial score=1/1 required_signals=0/0 issue_vector=punt=0,unread=0,citation=0,missing=0,transient=false,rules=[],published_hard=0,published_punt=0,published_missing=0,published_rules=[] tool_calls=0 evidence_reads=0 selected=true") {
		t.Fatalf("runtime selection was not reported:\n%s", got)
	}
	if strings.Contains(got, "attempt=2 phase=critique_retry score=1/1 required_signals=0/0 issue_vector=punt=0,unread=0,citation=0,missing=0,transient=false,rules=[],published_hard=0,published_punt=0,published_missing=0,published_rules=[] tool_calls=0 evidence_reads=0 selected=true") {
		t.Fatalf("identity fallback overrode runtime selection:\n%s", got)
	}
}

func TestSummarizeBenchmarkTraceCountsOnlyIssuedFloorNudges(t *testing.T) {
	summary := summarizeBenchmarkTrace(ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Events: []ai.TraceEvent{
		{Kind: "floor_nudge", Outcome: "retry", Status: "gcs_bytes"},
		{Kind: "floor_nudge", Outcome: "retry_exhausted", Status: "gcs_bytes"},
	}}}})
	if summary.floorNudges != 1 || !slices.Equal(summary.floorNudgeReasons, []string{"gcs_bytes"}) {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestBenchmarkGCSFloorBypassed(t *testing.T) {
	tests := []struct {
		name     string
		analysis *models.AIAnalysis
		minimum  int
		want     bool
	}{
		{name: "covered below floor", analysis: &models.AIAnalysis{EvidencePlanCovered: true, GCSBytes: 99}, minimum: 100, want: true},
		{name: "covered at floor", analysis: &models.AIAnalysis{EvidencePlanCovered: true, GCSBytes: 100}, minimum: 100},
		{name: "retry exhausted below floor", analysis: &models.AIAnalysis{GCSFloorRetryExhausted: true, GCSBytes: 99}, minimum: 100, want: true},
		{name: "uncovered below floor", analysis: &models.AIAnalysis{GCSBytes: 99}, minimum: 100},
		{name: "zero floor", analysis: &models.AIAnalysis{EvidencePlanCovered: true}, minimum: 0},
		{name: "no analysis", minimum: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkGCSFloorBypassed(tc.analysis, tc.minimum); got != tc.want {
				t.Fatalf("benchmarkGCSFloorBypassed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFailedBenchmarkTelemetryIncludesTraceAndTools(t *testing.T) {
	toolUsage := benchmarkToolUsage{names: []string{"grep_artifact", "read_artifact"}, counts: []string{"grep_artifact=1", "read_artifact=2"}}
	trace := benchmarkTraceSummary{
		floorNudges: 2, floorNudgeReasons: []string{"tool_calls", "gcs_bytes"}, contextCompactionApplied: 1,
		contextOverBudget: 1, modelRequests: 3, modelFailures: 1, toolFailures: 2,
	}
	got := strings.Join(benchmarkTelemetryLines(2*time.Second, nil, 100, toolUsage, trace), "\n")
	for _, want := range []string{
		"tool_names=[grep_artifact read_artifact]", "tool_counts=[grep_artifact=1 read_artifact=2]",
		"model_requests=3", "model_failures=1", "tool_failures=2", "floor_nudges=2",
		"floor_nudge_reasons=[tool_calls gcs_bytes]", "context_compaction_applied=1", "context_over_budget=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failed-analysis telemetry missing %q:\n%s", want, got)
		}
	}
}

func TestBenchmarkTelemetryOmitsSensitiveContent(t *testing.T) {
	const secret = "do not log this https://endpoint.example Bearer credential"
	snapshot := ai.AnalysisTraceFile{Traces: []ai.AnalysisTrace{{Events: []ai.TraceEvent{
		{Kind: "floor_nudge", Status: secret},
		{Kind: "model_request", Outcome: "error", ResponseID: secret, ErrorCode: "analysis_error"},
		{Kind: "tool_call", Outcome: "success", Tool: secret},
	}}}}
	analysis := &models.AIAnalysis{
		Model: secret, RootCause: secret, SuggestedFix: secret, SkillSetHash: secret,
	}
	got := strings.Join(benchmarkTelemetryLines(time.Second, analysis, 1, successfulBenchmarkToolUsage(snapshot), summarizeBenchmarkTrace(snapshot)), "\n")
	if strings.Contains(got, secret) {
		t.Fatalf("telemetry leaked sensitive content: %s", got)
	}
}

func TestBenchCasesRejectOppositeDiagnoses(t *testing.T) {
	for _, bc := range benchCases {
		if bc.oppositeDiagnosis == "" {
			continue
		}
		for _, signal := range bc.signals {
			if signal.must && signal.matches(bc.oppositeDiagnosis) {
				t.Errorf("benchmark %s required signal %q accepts opposite diagnosis %q", bc.name, signal.name, bc.oppositeDiagnosis)
			}
		}
	}
}

func benchTestCase(bc benchCase) *models.TestCase {
	return &models.TestCase{
		Name:           bc.testName,
		Source:         bc.testSource,
		Status:         "failed",
		FailureMessage: bc.failureMsg,
		JUnitFile:      bc.junitFile,
	}
}

func scoreBenchCase(t *testing.T, bc benchCase, tc *models.TestCase, outcome benchmarkOutcome, elapsed time.Duration, backend string, minGCSBytes int, toolUsage benchmarkToolUsage, traceSummary benchmarkTraceSummary, draftObservations []benchmarkDraftObservation, selectedAttempt int) {
	t.Helper()
	t.Logf("\n===== %s (%s) =====", bc.name, backend)
	for _, line := range benchmarkTelemetryLines(elapsed, tc.AIAnalysis, minGCSBytes, toolUsage, traceSummary) {
		t.Log(line)
	}
	for _, line := range benchmarkDraftTelemetryLines(bc, draftObservations, tc, selectedAttempt) {
		t.Log(line)
	}
	if tc.AIAnalysis == nil {
		if benchmarkAllowsUnavailable(bc, tc, outcome) {
			t.Logf("ALLOWED: %s produced a citation-policy unavailable result after %s", backend, elapsed)
			return
		}
		t.Fatalf("%s analysis produced no AIAnalysis after %s (ai_summary_present=%v)", backend, elapsed, tc.AISummary != nil)
	}
	if tc.AISummary == nil {
		t.Fatalf("%s analysis produced AIAnalysis without AISummary after %s", backend, elapsed)
	}

	assessment := assessBenchmarkCase(bc, tc)
	for _, result := range assessment.results {
		tier := "nice"
		if result.required {
			tier = "MUST"
		}
		mark := "MISS"
		if result.hit {
			mark = "hit"
		}
		t.Logf("  [%s] %-4s %s", tier, mark, result.name)
	}
	t.Logf("SCORE: %d/%d signals hit", assessment.hits, assessment.total)

	if len(assessment.missingMust) > 0 {
		t.Errorf("benchmark %s missed required root-cause signal(s): %s", bc.name, strings.Join(assessment.missingMust, ", "))
	}
}

type benchmarkSignalResult struct {
	name     string
	hit      bool
	required bool
}

// benchmarkOwnershipExpectation scores the structured repository a correct
// analysis must hold responsible. Ownership is a field rather than prose, so it
// is checked against the published location instead of the scored text.
type benchmarkOwnershipExpectation struct {
	name       string
	repository string
	external   bool
	file       string
}

func (e benchmarkOwnershipExpectation) satisfiedBy(location *models.AnalysisCauseLocation) bool {
	if location == nil || !strings.EqualFold(location.Repository, e.repository) || location.External != e.external {
		return false
	}
	if e.file == "" {
		return true
	}
	return slices.ContainsFunc(location.Files, func(candidate string) bool {
		return strings.EqualFold(candidate, e.file)
	})
}

func benchmarkOwnershipExpectations(bc benchCase) []benchmarkOwnershipExpectation {
	if bc.causeRepository == "" {
		return nil
	}
	kind := "own repository"
	if bc.causeExternal {
		kind = "dependency"
	}
	out := []benchmarkOwnershipExpectation{{
		name:       "cause owned by " + kind + " " + bc.causeRepository,
		repository: bc.causeRepository,
		external:   bc.causeExternal,
	}}
	for _, file := range bc.causeFiles {
		out = append(out, benchmarkOwnershipExpectation{
			name:       "cause location names " + file,
			repository: bc.causeRepository,
			external:   bc.causeExternal,
			file:       file,
		})
	}
	return out
}

type benchmarkAssessment struct {
	hits             int
	total            int
	diagnosisHits    int
	diagnosisTotal   int
	sourceHits       int
	sourceTotal      int
	transientCorrect *bool
	forbiddenPassed  int
	forbiddenTotal   int
	missingMust      []string
	results          []benchmarkSignalResult
}

func assessBenchmarkCase(bc benchCase, tc *models.TestCase) benchmarkAssessment {
	assessment := benchmarkAssessment{total: len(bc.signals) + len(bc.sourceSignals) + len(bc.forbidden), sourceTotal: len(bc.sourceSignals), forbiddenTotal: len(bc.forbidden)}
	for _, signal := range append(slices.Clone(bc.signals), bc.sourceSignals...) {
		if signal.must {
			assessment.diagnosisTotal++
		}
	}
	if bc.expectedTransient != nil {
		assessment.total++
	}
	ownershipExpectations := benchmarkOwnershipExpectations(bc)
	assessment.total += len(ownershipExpectations)
	assessment.diagnosisTotal += len(ownershipExpectations)
	if tc == nil || tc.AISummary == nil {
		return assessment
	}
	if bc.expectedTransient != nil {
		correct := tc.AISummary.IsTransient == *bc.expectedTransient
		assessment.transientCorrect = &correct
	}
	if tc.AIAnalysis == nil {
		return assessment
	}
	scored := strings.ToLower(strings.Join([]string{tc.AISummary.Summary, tc.AIAnalysis.RootCause, tc.AIAnalysis.SuggestedFix}, "\n"))
	for index, signal := range append(slices.Clone(bc.signals), bc.sourceSignals...) {
		hit := signal.matches(scored)
		assessment.results = append(assessment.results, benchmarkSignalResult{name: signal.name, hit: hit, required: signal.must})
		if hit {
			assessment.hits++
			if index >= len(bc.signals) {
				assessment.sourceHits++
			}
			if signal.must {
				assessment.diagnosisHits++
			}
		} else if signal.must {
			assessment.missingMust = append(assessment.missingMust, signal.name)
		}
	}
	if bc.expectedTransient != nil {
		hit := assessment.transientCorrect != nil && *assessment.transientCorrect
		assessment.results = append(assessment.results, benchmarkSignalResult{name: "transient classification", hit: hit, required: true})
		if hit {
			assessment.hits++
		} else {
			assessment.missingMust = append(assessment.missingMust, "transient classification")
		}
	}
	for _, forbidden := range bc.forbidden {
		hit := !forbidden.matches(scored)
		name := "forbidden: " + forbidden.name
		assessment.results = append(assessment.results, benchmarkSignalResult{name: name, hit: hit, required: true})
		if hit {
			assessment.hits++
			assessment.forbiddenPassed++
		} else {
			assessment.missingMust = append(assessment.missingMust, name)
		}
	}
	for _, expectation := range ownershipExpectations {
		hit := expectation.satisfiedBy(tc.AIAnalysis.CauseLocation)
		assessment.results = append(assessment.results, benchmarkSignalResult{name: expectation.name, hit: hit, required: true})
		if hit {
			assessment.hits++
			assessment.diagnosisHits++
		} else {
			assessment.missingMust = append(assessment.missingMust, expectation.name)
		}
	}
	return assessment
}

func benchmarkAllowsUnavailable(bc benchCase, tc *models.TestCase, outcome benchmarkOutcome) bool {
	return bc.allowUnavailable && outcome == benchmarkOutcomeCitationPolicyUnavailable && tc != nil && tc.AIAnalysis == nil && tc.AISummary != nil && !tc.AISummary.IsTransient
}

func TestBenchmarkAllowsUnavailable(t *testing.T) {
	valid := &models.TestCase{AISummary: &models.AISummary{Summary: "AI analysis unavailable: evidence remained inconclusive"}}
	if !benchmarkAllowsUnavailable(benchCase{allowUnavailable: true}, valid, benchmarkOutcomeCitationPolicyUnavailable) {
		t.Fatal("allowed unavailable result was rejected")
	}
	for _, tc := range []struct {
		name string
		bc   benchCase
		tc   *models.TestCase
		out  benchmarkOutcome
	}{
		{name: "case disabled", tc: valid, out: benchmarkOutcomeCitationPolicyUnavailable},
		{name: "wrong outcome", bc: benchCase{allowUnavailable: true}, tc: valid, out: benchmarkOutcomeUnknown},
		{name: "transient", bc: benchCase{allowUnavailable: true}, tc: &models.TestCase{AISummary: &models.AISummary{Summary: "AI analysis unavailable: x", IsTransient: true}}, out: benchmarkOutcomeCitationPolicyUnavailable},
		{name: "analysis attached", bc: benchCase{allowUnavailable: true}, tc: &models.TestCase{AISummary: valid.AISummary, AIAnalysis: &models.AIAnalysis{RootCause: "cause"}}, out: benchmarkOutcomeCitationPolicyUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if benchmarkAllowsUnavailable(tc.bc, tc.tc, tc.out) {
				t.Fatal("unexpected allowed unavailable result")
			}
		})
	}
}

// benchStorage returns the storage backend the analysis reads artifacts from.
// By default it downloads the case's committed fixture asset, extracts it to a
// local cache, and serves it via the local provider so the benchmark works
// after Prow has GC'd the original GCS objects. Set BENCH_USE_GCS=1 to read
// live GCS instead (only works before GC). The returned label is the bucket
// name used for artifact display, not for fetching.
func benchStorage(t *testing.T, bc benchCase) (storage.Backend, string) {
	t.Helper()
	if archive := strings.TrimSpace(os.Getenv("BENCH_FIXTURE_ARCHIVE")); archive != "" {
		root := ensureLocalFixture(t, archive, bc.fixtureSHA256)
		backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
		if err != nil {
			t.Fatalf("local backend: %v", err)
		}
		return backend, bc.bucket
	}
	if os.Getenv("BENCH_USE_GCS") != "" {
		backend, err := storage.New(storage.Config{Provider: storage.ProviderGCS, Bucket: bc.bucket}, &http.Client{Timeout: 60 * time.Second})
		if err != nil {
			t.Fatalf("gcs backend: %v", err)
		}
		return backend, bc.bucket
	}
	if bc.fixtureAsset == "" {
		t.Fatal("BENCH_FIXTURE_ARCHIVE is required when the case has no published fixture asset; BENCH_USE_GCS is unscored only")
	}
	root := ensureFixture(t, bc.fixtureAsset, bc.fixtureSHA256)
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	return backend, bc.bucket
}

// ensureLocalFixture verifies and extracts one locally retained fixture archive.
func ensureLocalFixture(t *testing.T, path, wantSHA256 string) string {
	t.Helper()
	archive, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read local fixture: %v", err)
	}
	if err := verifyFixtureDigest(archive, wantSHA256); err != nil {
		t.Fatal(err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	dir := filepath.Join(cacheRoot, "aster-benchmark", filepath.Base(path)+"-"+wantSHA256[:12])
	marker := filepath.Join(dir, ".sha256")
	if digest, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(digest)) == wantSHA256 {
		return dir
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGz(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(wantSHA256+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ensureFixture downloads and extracts a benchmark-fixtures release asset into a
// digest-scoped cache dir, returning the extract root. Cached fixtures are
// reused only when their verified digest marker matches.
func ensureFixture(t *testing.T, asset, wantSHA256 string) string {
	t.Helper()
	if len(wantSHA256) != sha256.Size*2 {
		t.Fatalf("fixture %s has invalid SHA-256 %q", asset, wantSHA256)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	cacheName := strings.TrimSuffix(asset, ".tar.gz") + "-" + wantSHA256[:12]
	dir := filepath.Join(cacheRoot, "prow-ai-dashboard-benchmark", cacheName)
	marker := filepath.Join(dir, ".sha256")
	if digest, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(digest)) == wantSHA256 {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 1 {
			return dir
		}
	}

	url := fixtureReleaseBase + asset
	t.Logf("downloading fixture %s", url)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("download fixture: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download fixture %s: HTTP %d", url, resp.StatusCode)
	}
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read fixture %s: %v", asset, err)
	}
	if err := verifyFixtureDigest(archive, wantSHA256); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("reset fixture cache dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture cache dir: %v", err)
	}
	if err := extractTarGz(bytes.NewReader(archive), dir); err != nil {
		t.Fatalf("extract fixture: %v", err)
	}
	if err := os.WriteFile(marker, []byte(wantSHA256+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture digest marker: %v", err)
	}
	return dir
}

func verifyFixtureDigest(archive []byte, wantSHA256 string) error {
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if got != wantSHA256 {
		return fmt.Errorf("fixture SHA-256 = %s, want %s", got, wantSHA256)
	}
	return nil
}

func TestVerifyFixtureDigest(t *testing.T) {
	archive := []byte("fixture archive")
	want := fmt.Sprintf("%x", sha256.Sum256(archive))
	if err := verifyFixtureDigest(archive, want); err != nil {
		t.Fatal(err)
	}
	if err := verifyFixtureDigest(archive, strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("verifyFixtureDigest accepted a mismatched digest")
	}
}

// extractTarGz unpacks a gzip'd tar stream under dest, rejecting entries whose
// path escapes dest.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		if rel, err := filepath.Rel(dest, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// Budget knobs mirrored from the fetcher so the benchmark uses the same
// bounded request headroom as dashboard analysis.
const benchGCSByteBudget = 1_000_000_000

func benchBudgets(t *testing.T, client *ai.Client, frozenTokens int) ai.ContextBudgets {
	t.Helper()
	overrideTokens, overridden, err := ai.ParseContextWindowTokens(os.Getenv("AI_CONTEXT_WINDOW_TOKENS"))
	if err != nil {
		t.Fatal(err)
	}
	if frozenTokens > 0 {
		if overridden && overrideTokens != frozenTokens {
			t.Fatalf("AI_CONTEXT_WINDOW_TOKENS=%d differs from frozen BENCH_MODEL_CONTEXT_TOKENS=%d", overrideTokens, frozenTokens)
		}
		budgets := ai.DeriveContextBudgets(frozenTokens)
		t.Logf("frozen benchmark context window: %d tokens; request_token_budget=%d reserved_tokens=%d", budgets.ContextWindowTokens, budgets.RequestTokenBudget, budgets.ContextWindowTokens-budgets.RequestTokenBudget)
		return budgets
	}
	tokens := overrideTokens
	detected := false
	if !overridden {
		tokens, detected = client.DetectContextWindowTokens(context.Background())
	}
	budgets := ai.DeriveContextBudgets(tokens)
	switch {
	case overridden:
		t.Logf("operator context window override: %d tokens; request_token_budget=%d reserved_tokens=%d", budgets.ContextWindowTokens, budgets.RequestTokenBudget, budgets.ContextWindowTokens-budgets.RequestTokenBudget)
	case detected:
		t.Logf("detected context window: %d tokens; request_token_budget=%d reserved_tokens=%d", budgets.ContextWindowTokens, budgets.RequestTokenBudget, budgets.ContextWindowTokens-budgets.RequestTokenBudget)
	default:
		t.Logf("context window unavailable; bounded fallback=%d tokens request_token_budget=%d", budgets.ContextWindowTokens, budgets.RequestTokenBudget)
	}
	return budgets
}

func TestBenchBudgetsUsesFrozenContextWithoutProviderDetection(t *testing.T) {
	t.Setenv("AI_CONTEXT_WINDOW_TOKENS", "200000")
	budgets := benchBudgets(t, nil, 200000)
	if budgets.ContextWindowTokens != 200000 || budgets.RequestTokenBudget <= 0 {
		t.Fatalf("budgets=%+v", budgets)
	}
}

// defaultBenchAgentic mirrors the live CAPZ-Dynamo tuning so a default run
// (no BENCH_PROJECT_DIR) is representative of that deploy.
// defaultBenchAgentic mirrors the live CAPZ-Dynamo tuning (a weak open-weights
// model) so a default run is representative of that deploy. Individual floors
// can be overridden via env (BENCH_MIN_TOOL_CALLS, BENCH_MIN_GCS_BYTES,
// BENCH_MAX_ITERS, BENCH_TIMEOUT) to benchmark stronger models fairly without a
// BENCH_PROJECT_DIR, since the weak-model floors distort a strong model that
// answers concisely.
func defaultBenchAgentic() project.Agentic {
	critiqueRetries := benchEnvInt("BENCH_CRITIQUE_RETRIES", 0)
	a := project.Agentic{
		MaxIters:     benchEnvInt("BENCH_MAX_ITERS", 15),
		Timeout:      benchEnvDuration("BENCH_TIMEOUT", 20*time.Minute),
		MinToolCalls: benchEnvInt("BENCH_MIN_TOOL_CALLS", 5),
		MinGCSBytes:  benchEnvInt("BENCH_MIN_GCS_BYTES", 500_000),
		Critique:     project.AgenticCritique{MaxRetries: &critiqueRetries},
	}
	return a
}

// benchEnvInt reads a non-negative integer env override, falling back to def.
func benchEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// benchEnvDuration reads a duration env override (e.g. "10m"), falling back to def.
func benchEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// ComposeBenchPrompt wraps a compact CAPZ/cloud-provider oriented addendum in
// the engine's standard prompt composition, so a default run still gets the
// engine BasePrompt + ResponseFormatFooter around it.
const benchPromptAddendum = `You are debugging Kubernetes CI failures for Cluster API Provider Azure (CAPZ) and cloud-provider-azure e2e jobs. Many failures surface only as a generic "timed out waiting for the condition"; the real cause is usually deeper in the cluster state. Use the k8s discovery tools to read the dumped cluster resources under artifacts/clusters/**/resources (AzureCluster, subnets, route tables, machines) and the controller logs before concluding. When a test times out with no direct error, check whether cluster networking (subnets, route tables, CNI) or a core add-on (Calico, cloud-provider) is the underlying cause. The fix may live in a different repository than the one running the job.`

func ComposeBenchPrompt() string {
	return ai.ComposeSystemPrompt(benchPromptAddendum)
}

func TestBenchmarkCacheGenerationFingerprint(t *testing.T) {
	t.Setenv(project.AICacheGenerationEnv, "operation-one")
	got, err := benchmarkCacheGenerationFingerprint("project-default")
	if err != nil {
		t.Fatal(err)
	}
	want := project.AICacheGenerationFingerprint("operation-one")
	if got != want || got == project.AICacheGenerationFingerprint("project-default") {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}

	t.Setenv(project.AICacheGenerationEnv, "invalid value")
	if _, err := benchmarkCacheGenerationFingerprint(""); err == nil {
		t.Fatal("invalid cache generation was accepted")
	}
}

func TestVerifyBenchmarkCacheReuseReloadsMarkerWithoutProviderRequest(t *testing.T) {
	cacheDir := t.TempDir()
	clientOptions := ai.Options{API: ai.APIChatCompletions, Endpoint: "https://example.invalid/v1/chat/completions", Model: "model", CacheDir: cacheDir}
	client := ai.NewClientWithOptions(clientOptions)
	const generation = "0123456789abcdef"
	const jobID = "periodic:example"
	bc := benchCase{
		name: "cache-case", stableID: "0123456789abcdef0123", jobType: "periodic", jobName: "example", buildID: "123", testName: "failed test",
		failureMsg: "failed", sourceRepo: [2]string{"example", "project"},
	}
	service := ai.NewService(ai.ServiceConfig{
		Client: client, Module: universal.New(), SystemPrompt: "sys", CacheGeneration: generation,
		AgenticOptions: ai.AgenticOptions{MinToolCalls: 1, MinGCSBytes: 50, CritiqueMaxRetries: 1},
	})
	now := time.Now().UTC()
	result := ai.FailureAnalysisResult{
		Summary: &models.AISummary{GeneratedAt: now.Format(time.RFC3339), Summary: "summary"},
		Analysis: &models.AIAnalysis{
			GeneratedAt: now.Format(time.RFC3339), Mode: ai.AgenticMode, Model: "model", RootCause: "root", Severity: "High", SuggestedFix: "fix",
			ToolCalls: 1, GCSBytes: 1, GCSFloorRetryExhausted: true, CritiquePassed: true, CritiqueVersion: ai.CurrentCritiqueVersion(),
			ModelHash: ai.ModelFingerprint(ai.APIChatCompletions, clientOptions.Endpoint, clientOptions.Model), PromptHash: ai.PromptFingerprint("sys"), CacheGeneration: generation,
		},
	}
	key := ai.AgenticCacheKeyForGeneration(universal.New().Name(), generation, jobID, bc.buildID, bc.testName, bc.failureMsg)
	entry, err := ai.NewAgenticCacheEntry(key, result, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Cache().StoreEntry(entry); err != nil {
		t.Fatal(err)
	}
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: bc.buildID, JobName: bc.jobName}}
	result.Analysis.CachePersistenceAttempted = true
	result.Analysis.CachePersistenceAccepted = true
	got := verifyBenchmarkCacheReuse(t, client, clientOptions, service, generation, jobID, bc, run, result.Analysis)
	if !got.PersistenceAttempted || !got.PersistenceAccepted || !got.CacheSaveSucceeded || !got.LookupAttempted || !got.LookupAccepted || !got.LookupHit ||
		got.ProviderRequests != 0 || !got.GCSFloorRetryExhausted || got.PolicyRejectionReason != ai.CacheAccepted || got.LookupRejectionReason != ai.CacheAccepted {
		t.Fatalf("verification = %+v", got)
	}
}

func TestVerifyBenchmarkCacheReusePreservesPolicyRejection(t *testing.T) {
	cacheDir := t.TempDir()
	clientOptions := ai.Options{API: ai.APIChatCompletions, Endpoint: "https://example.invalid/v1/chat/completions", Model: "model", CacheDir: cacheDir}
	client := ai.NewClientWithOptions(clientOptions)
	service := ai.NewService(ai.ServiceConfig{
		Client: client, Module: universal.New(), SystemPrompt: "sys", CacheGeneration: "generation",
		AgenticOptions: ai.AgenticOptions{CritiqueCachePolicy: ai.CritiqueCachePolicyStrict},
	})
	bc := benchCase{name: "case", stableID: "0123456789abcdef0123", jobName: "job", buildID: "1", testName: "test", failureMsg: "failed"}
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: bc.buildID, JobName: bc.jobName}}
	analysis := &models.AIAnalysis{CachePolicyRejectionReason: string(ai.CacheRejectedCritiqueStrictWarning)}
	got := verifyBenchmarkCacheReuse(t, client, clientOptions, service, "generation", "job", bc, run, analysis)
	if got.PersistenceAttempted || got.PersistenceAccepted || !got.CacheSaveSucceeded || !got.LookupAttempted || got.LookupAccepted || got.LookupHit ||
		got.PolicyRejectionReason != ai.CacheRejectedCritiqueStrictWarning || got.LookupRejectionReason != ai.CacheRejectedLookupMissing || got.ProviderRequests != 0 {
		t.Fatalf("verification = %+v", got)
	}
}

func TestBenchmarkCacheCaseKeyFallsBackForBuiltInCases(t *testing.T) {
	withStable := benchmarkCacheCaseKey(benchCase{name: "case", stableID: "0123456789abcdef0123"})
	if withStable != "0123456789abcdef0123" {
		t.Fatalf("stable key = %q", withStable)
	}
	first := benchmarkCacheCaseKey(benchCase{name: "first-case"})
	second := benchmarkCacheCaseKey(benchCase{name: "second-case"})
	if first == "" || first == second || first != benchmarkCacheCaseKey(benchCase{name: "first-case"}) {
		t.Fatalf("fallback keys = %q, %q", first, second)
	}
}

func TestBenchmarkPersistentCacheSavesWithoutReloadVerification(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BENCH_CACHE_DIR", root)
	t.Setenv("BENCH_VERIFY_CACHE_REUSE", "")
	if !benchmarkPersistentCacheEnabled() || benchmarkCacheReuseEnabled() {
		t.Fatal("cache option detection is inconsistent")
	}
	bc := benchCase{name: "built-in-case"}
	dir := benchmarkCacheDir(t, bc, 1, benchmarkRunIdentity{Arm: "baseline", EffectiveInputSHA256: strings.Repeat("a", 64)})
	client := ai.NewClientWithOptions(ai.Options{CacheDir: dir})
	entry := ai.CacheEntry{Key: "key", CreatedAt: time.Now(), Data: []byte(`{"summary":"cached"}`)}
	if err := client.Cache().StoreEntry(entry); err != nil {
		t.Fatal(err)
	}
	if err := client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ai.CacheFilename)); err != nil {
		t.Fatalf("persistent cache was not saved: %v", err)
	}
}

func TestBenchTestCasePreservesBuildSource(t *testing.T) {
	tc := benchTestCase(benchCase{testName: "Prow job execution", testSource: models.TestCaseSourceBuild, failureMsg: "failed"})
	if tc.Source != models.TestCaseSourceBuild || tc.JUnitFile != "" {
		t.Fatalf("test case = %+v", tc)
	}
}

func TestBenchmarkMinGCSBytesMatchesBuildPolicy(t *testing.T) {
	if got := benchmarkMinGCSBytes(benchCase{testSource: models.TestCaseSourceBuild}, 5_000_000); got != 0 {
		t.Fatalf("build floor = %d, want 0", got)
	}
	if got := benchmarkMinGCSBytes(benchCase{}, 5_000_000); got != 5_000_000 {
		t.Fatalf("JUnit floor = %d, want 5000000", got)
	}
}

func TestAssessBenchmarkCaseSeparatesDiagnosisAndPolicyChecks(t *testing.T) {
	expectedTransient := false
	bc := benchCase{
		signals: []benchSignal{
			{name: "cause-a", re: regexp.MustCompile(`cause a`), must: true},
			{name: "cause-b", re: regexp.MustCompile(`cause b`), must: true},
			{name: "detail", re: regexp.MustCompile(`detail`)},
		},
		expectedTransient: &expectedTransient,
		forbidden:         []benchSignal{{name: "wrong owner", re: regexp.MustCompile(`wrong owner`)}},
	}
	tc := &models.TestCase{
		AISummary:  &models.AISummary{Summary: "cause a detail", IsTransient: false},
		AIAnalysis: &models.AIAnalysis{RootCause: "cause a detail"},
	}
	got := assessBenchmarkCase(bc, tc)
	if got.hits != 4 || got.total != 5 || got.diagnosisHits != 1 || got.diagnosisTotal != 2 || got.transientCorrect == nil || !*got.transientCorrect || got.forbiddenPassed != 1 || got.forbiddenTotal != 1 || !slices.Equal(got.missingMust, []string{"cause-b"}) {
		t.Fatalf("assessment = %+v", got)
	}
}

type benchmarkLocalRepoReader struct{ root string }

func (r benchmarkLocalRepoReader) ListTree(ctx context.Context) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "-C", r.root, "ls-files", "-z")
	data, err := command.Output()
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, filepath.ToSlash(string(part)))
		}
	}
	return out, nil
}

func (r benchmarkLocalRepoReader) ReadFile(_ context.Context, path string) (string, bool, error) {
	clean, err := artifacts.SafePath(path)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(clean)))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(data) > 4<<20 {
		return "", false, fmt.Errorf("benchmark source file exceeds bound")
	}
	return string(data), true, nil
}

func benchmarkRepoReader(t *testing.T, ref benchmarkSourceRef) tools.RepoReader {
	t.Helper()
	root := strings.TrimSpace(os.Getenv("BENCH_SOURCE_ROOT"))
	if root == "" {
		owner, name, _ := strings.Cut(ref.Repository, "/")
		return ai.NewGitHubRepoReader(owner, name, ref.Revision, "")
	}
	root = filepath.Join(filepath.Clean(root), ref.ID)
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != ref.Revision {
		t.Fatalf("benchmark source %s revision mismatch", ref.ID)
	}
	status, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil || len(status) != 0 {
		t.Fatalf("benchmark source %s is not clean", ref.ID)
	}
	return benchmarkLocalRepoReader{root: root}
}
