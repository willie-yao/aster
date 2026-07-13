package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/universal"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
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
//	AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
//	AI_MODEL=moonshotai/Kimi-K2.7-Code AI_TOKEN=x \
//	go test ./internal/e2e -run TestAIBenchmark -v -timeout 60m
//
// Point BENCH_PROJECT_DIR at a consumer repo to load its real project.yaml AI
// tuning and prompts/system.md so the run matches that live deploy exactly;
// otherwise a compact built-in prompt and the live CAPZ-Dynamo tuning are used.

// benchSignal is one scored expectation against the model's root cause text.
type benchSignal struct {
	name string
	re   *regexp.Regexp
	// must marks a signal whose absence fails the benchmark. Non-must signals
	// are informational, tracking how deep the analysis got.
	must bool
}

// benchCase pins one historical failure and the signals a correct root cause
// should contain.
type benchCase struct {
	name   string
	bucket string
	// fixtureAsset is the .tar.gz on the benchmark-fixtures release holding a
	// full snapshot of this build's bucket-relative artifact tree. The default
	// run extracts it and reads through the local storage provider, so the
	// benchmark survives Prow garbage-collecting the original GCS artifacts. Set
	// BENCH_USE_GCS=1 to read live GCS instead (only works before GC).
	fixtureAsset string
	jobType      string
	repo         string // org/repo, required for presubmits
	jobName      string
	buildID      string
	pullNumber   string
	webURL       string
	sourceRepo   [2]string // owner, name for repo-relative file-link resolution
	testName     string
	junitFile    string
	failureMsg   string
	// consecutiveFailures is how many consecutive builds this test had failed at
	// the time of the snapshot. The live engine derives this from the flakiness
	// report; the benchmark feeds it so the analysis (and the critique gate's
	// transient-vs-persistent check) see the real persistence signal.
	consecutiveFailures int
	signals             []benchSignal
}

// fixtureReleaseBase is the download root for benchmark-fixtures release assets.
const fixtureReleaseBase = "https://github.com/willie-yao/prow-ai-dashboard/releases/download/benchmark-fixtures/"

func mustRE(s string) *regexp.Regexp { return regexp.MustCompile(s) }

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
		name:         "ccm-dualstack-control-plane-routetable",
		bucket:       "kubernetes-ci-logs",
		fixtureAsset: "ccm-dualstack-capz-6358.tar.gz",
		jobType:      models.JobTypePresubmit,
		repo:         "kubernetes-sigs/cloud-provider-azure",
		jobName:      "pull-cloud-provider-azure-e2e-ccm-dualstack-capz-1-30",
		buildID:      "2062345846720040960",
		pullNumber:   "10388",
		webURL:       "https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_cloud-provider-azure/10388/pull-cloud-provider-azure-e2e-ccm-dualstack-capz-1-30/2062345846720040960/",
		sourceRepo:   [2]string{"kubernetes-sigs", "cloud-provider-azure"},
		testName:     "[It] Azure node resources should set node provider id correctly [Node]",
		junitFile:    "junit_01.xml",
		failureMsg:   `Unexpected error: <wait.errInterrupted>: timed out waiting for the condition { cause: <*errors.errorString>{ s: "timed out waiting for the condition", }, } occurred`,
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

func TestAIBenchmark(t *testing.T) {
	if os.Getenv("RUN_AI_BENCHMARK") == "" {
		t.Skip("set RUN_AI_BENCHMARK=1 (plus AI_ENDPOINT/AI_MODEL) to run the AI quality benchmark")
	}
	endpoint, model := os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL")
	if endpoint == "" || model == "" {
		t.Fatal("RUN_AI_BENCHMARK set but AI_ENDPOINT/AI_MODEL are not")
	}
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		token = "benchmark" // Dynamo needs no key; keep the client happy.
	}

	// Optional: load a real consumer's AI tuning + system prompt so the run
	// matches that live deploy. Otherwise use the built-in prompt and defaults.
	systemPrompt := ComposeBenchPrompt()
	agentic := defaultBenchAgentic()
	if dir := os.Getenv("BENCH_PROJECT_DIR"); dir != "" {
		cfg, prompt, err := project.LoadDir(dir)
		if err != nil {
			t.Fatalf("BENCH_PROJECT_DIR=%s: %v", dir, err)
		}
		systemPrompt = ai.ComposeSystemPrompt(prompt)
		agentic = cfg.AI.EffectiveAgentic()
	}

	for _, bc := range benchCases {
		t.Run(bc.name, func(t *testing.T) {
			runBenchCase(t, bc, endpoint, model, token, systemPrompt, agentic)
		})
	}
}

func runBenchCase(t *testing.T, bc benchCase, endpoint, model, token, systemPrompt string, agentic project.Agentic) {
	client := ai.NewClientWithOptions(ai.Options{
		Token:    token,
		Endpoint: endpoint,
		Model:    model,
		CacheDir: t.TempDir(), // isolated cache: always a cold analysis.
	})

	backend, bucketLabel := benchStorage(t, bc)
	factory := artifacts.NewBackendFactory(backend, bucketLabel)

	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	enabled, err := registry.Enable([]string{"filesystem", "k8s"})
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

	service := ai.NewService(client, universal.New(), systemPrompt, consecutiveMap)
	service.SetSourceRepo(bc.sourceRepo[0], bc.sourceRepo[1])

	// Size the model/context budgets from the endpoint's window, matching the
	// fetcher. Fall back to a static budget with compaction off when absent.
	modelByteBudget, contextByteBudget := benchByteBudgets(t, client)
	service.EnableAgentic(ai.AgenticOptions{
		MaxIters:           agentic.MaxIters,
		ModelByteBudget:    modelByteBudget,
		GCSByteBudget:      benchGCSByteBudget,
		Timeout:            agentic.Timeout,
		ContextByteBudget:  contextByteBudget,
		MinToolCalls:       agentic.MinToolCalls,
		MinGCSBytes:        agentic.MinGCSBytes,
		CritiqueMaxRetries: *agentic.Critique.MaxRetries,
		SingleToolCall:     agentic.SingleToolCall,
		SemanticJudge:      true,
	}, factory, registry, enabled)

	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: bc.jobType, Repo: bc.repo},
		JobName:     bc.jobName,
		BuildID:     bc.buildID,
		PullNumber:  bc.pullNumber,
	}
	run := &models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID:    bc.buildID,
		JobName:    bc.jobName,
		PullNumber: bc.pullNumber,
		WebURL:     bc.webURL,
	}}
	tc := &models.TestCase{
		Name:           bc.testName,
		Status:         "failed",
		FailureMessage: bc.failureMsg,
		JUnitFile:      bc.junitFile,
	}

	start := time.Now()
	service.Analyze(context.Background(), &http.Client{Timeout: 60 * time.Second}, jobID, loc.BuildPath(), run, tc)
	elapsed := time.Since(start).Round(time.Second)

	if tc.AIAnalysis == nil {
		summary := "<none>"
		if tc.AISummary != nil {
			summary = tc.AISummary.Summary
		}
		t.Fatalf("analysis produced no AIAnalysis after %s (summary: %s)", elapsed, summary)
	}

	a := tc.AIAnalysis
	scored := strings.ToLower(strings.Join([]string{tc.AISummary.Summary, a.RootCause, a.SuggestedFix}, "\n"))

	t.Logf("\n===== %s =====", bc.name)
	t.Logf("elapsed=%s tool_calls=%d gcs_bytes=%d model_bytes=%d critique_passed=%v budget_exhausted=%v",
		elapsed, a.ToolCalls, a.GCSBytes, a.ModelBytes, a.CritiquePassed, a.BudgetExhausted)
	t.Logf("severity=%s transient=%v", a.Severity, tc.AISummary.IsTransient)
	t.Logf("SUMMARY:\n%s", tc.AISummary.Summary)
	t.Logf("ROOT CAUSE:\n%s", a.RootCause)
	t.Logf("SUGGESTED FIX:\n%s", a.SuggestedFix)

	var missedMust []string
	hit, total := 0, len(bc.signals)
	for _, s := range bc.signals {
		ok := s.re.MatchString(scored)
		if ok {
			hit++
		}
		tier := "nice"
		if s.must {
			tier = "MUST"
		}
		mark := "MISS"
		if ok {
			mark = "hit"
		}
		t.Logf("  [%s] %-4s %s", tier, mark, s.name)
		if s.must && !ok {
			missedMust = append(missedMust, s.name)
		}
	}
	t.Logf("SCORE: %d/%d signals hit", hit, total)

	if len(missedMust) > 0 {
		t.Errorf("benchmark %s missed required root-cause signal(s): %s", bc.name, strings.Join(missedMust, ", "))
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
	if os.Getenv("BENCH_USE_GCS") != "" || bc.fixtureAsset == "" {
		backend, err := storage.New(storage.Config{Provider: storage.ProviderGCS, Bucket: bc.bucket}, &http.Client{Timeout: 60 * time.Second})
		if err != nil {
			t.Fatalf("gcs backend: %v", err)
		}
		t.Logf("reading artifacts from live GCS bucket %q", bc.bucket)
		return backend, bc.bucket
	}
	root := ensureFixture(t, bc.fixtureAsset)
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	t.Logf("reading artifacts from fixture %s (extracted at %s)", bc.fixtureAsset, root)
	return backend, bc.bucket
}

// ensureFixture downloads and extracts a benchmark-fixtures release asset into a
// per-asset cache dir, returning the extract root (which contains the
// bucket-relative pr-logs/... tree). A present, non-empty cache is reused.
func ensureFixture(t *testing.T, asset string) string {
	t.Helper()
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	dir := filepath.Join(cacheRoot, "prow-ai-dashboard-benchmark", strings.TrimSuffix(asset, ".tar.gz"))
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return dir // already extracted
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture cache dir: %v", err)
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
	if err := extractTarGz(resp.Body, dir); err != nil {
		t.Fatalf("extract fixture: %v", err)
	}
	return dir
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

// Budget knobs mirrored from the fetcher so the benchmark sizes context the
// same way a live analysis does.
const (
	benchAvgBytesPerToken        = 4
	benchModelBudgetWindowPct    = 50
	benchContextBudgetWindowPct  = 75
	benchFallbackModelByteBudget = 300_000
	benchGCSByteBudget           = 1_000_000_000
)

func benchByteBudgets(t *testing.T, client *ai.Client) (modelByteBudget, contextByteBudget int) {
	modelByteBudget = benchFallbackModelByteBudget
	if tokens, ok := client.DetectContextWindowTokens(context.Background()); ok {
		windowBytes := tokens * benchAvgBytesPerToken
		modelByteBudget = windowBytes * benchModelBudgetWindowPct / 100
		contextByteBudget = windowBytes * benchContextBudgetWindowPct / 100
		t.Logf("detected context window: %d tokens; model_budget=%dKB context_budget=%dKB", tokens, modelByteBudget/1024, contextByteBudget/1024)
	}
	return modelByteBudget, contextByteBudget
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
	critiqueRetries := benchEnvInt("BENCH_CRITIQUE_RETRIES", 2)
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
func ComposeBenchPrompt() string {
	const addendum = `You are debugging Kubernetes CI failures for Cluster API Provider Azure (CAPZ) and cloud-provider-azure e2e jobs. Many failures surface only as a generic "timed out waiting for the condition"; the real cause is usually deeper in the cluster state. Use the k8s discovery tools to read the dumped cluster resources under artifacts/clusters/**/resources (AzureCluster, subnets, route tables, machines) and the controller logs before concluding. When a test times out with no direct error, check whether cluster networking (subnets, route tables, CNI) or a core add-on (Calico, cloud-provider) is the underlying cause. The fix may live in a different repository than the one running the job.`
	return ai.ComposeSystemPrompt(addendum)
}
