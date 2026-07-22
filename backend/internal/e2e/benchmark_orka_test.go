package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type orkaBenchConfig struct {
	backendRoot   string
	producer      string
	ingestor      string
	projectDir    string
	toolManifests string
	namespace     string
	provider      string
	model         string
	apiMode       string
	version       string
	api           string
	tokenFile     string
	kubeContext   string
	taskTimeout   time.Duration
	wait          time.Duration
	taskExecution string
	toolAuthName  string
	toolAuthKey   string
}

func TestOrkaAIBenchmark(t *testing.T) {
	if os.Getenv("RUN_ORKA_BENCHMARK") == "" {
		t.Skip("set RUN_ORKA_BENCHMARK=1 and BENCH_ORKA_PROVIDER/MODEL/API/TOKEN to run the live Orka quality benchmark")
	}

	cfg := loadOrkaBenchConfig(t)
	t.Logf("live Orka benchmark will apply uniquely identified Tasks and Tools to %s; it does not delete them", cfg.namespace)
	for _, bc := range benchCases {
		t.Run(bc.name, func(t *testing.T) {
			runOrkaBenchCase(t, bc, cfg)
		})
	}
}

func loadOrkaBenchConfig(t *testing.T) orkaBenchConfig {
	t.Helper()
	provider := strings.TrimSpace(os.Getenv("BENCH_ORKA_PROVIDER"))
	model := strings.TrimSpace(os.Getenv("BENCH_ORKA_MODEL"))
	api := strings.TrimSpace(os.Getenv("BENCH_ORKA_API"))
	if provider == "" || model == "" || api == "" {
		t.Fatal("BENCH_ORKA_PROVIDER, BENCH_ORKA_MODEL, and BENCH_ORKA_API are required")
	}

	backendRoot, repoRoot := benchRepoRoots(t)
	binDir := t.TempDir()
	cfg := orkaBenchConfig{
		backendRoot:   backendRoot,
		producer:      buildOrkaBenchBinary(t, backendRoot, binDir, "orka-producer", "./cmd/orka-producer"),
		ingestor:      buildOrkaBenchBinary(t, backendRoot, binDir, "orka-ingestor", "./cmd/orka-ingestor"),
		projectDir:    strings.TrimSpace(os.Getenv("BENCH_PROJECT_DIR")),
		toolManifests: strings.TrimSpace(os.Getenv("BENCH_ORKA_TOOL_MANIFESTS")),
		namespace:     strings.TrimSpace(os.Getenv("BENCH_ORKA_NAMESPACE")),
		provider:      provider,
		model:         model,
		apiMode:       strings.TrimSpace(os.Getenv("BENCH_ORKA_API_MODE")),
		version:       strings.TrimSpace(os.Getenv("BENCH_ORKA_VERSION")),
		api:           api,
		kubeContext:   strings.TrimSpace(os.Getenv("BENCH_ORKA_CONTEXT")),
		taskTimeout:   benchEnvDuration("BENCH_ORKA_TASK_TIMEOUT", 15*time.Minute),
		wait:          benchEnvDuration("BENCH_ORKA_WAIT", 25*time.Minute),
		taskExecution: strings.TrimSpace(os.Getenv("BENCH_ORKA_TASK_EXECUTION")),
		toolAuthName:  strings.TrimSpace(os.Getenv("BENCH_ORKA_TOOL_AUTH_SECRET")),
		toolAuthKey:   strings.TrimSpace(os.Getenv("BENCH_ORKA_TOOL_AUTH_KEY")),
	}
	if cfg.apiMode == "" {
		cfg.apiMode = orka.APIModeAuto
	}
	if _, err := orka.NormalizeAPIMode(cfg.apiMode); err != nil {
		t.Fatal(err)
	}
	if cfg.toolManifests == "" {
		cfg.toolManifests = filepath.Join(repoRoot, "experimental", "orka", "manifests")
	}
	if cfg.namespace == "" {
		cfg.namespace = "orka-system"
	}
	if cfg.version == "" {
		cfg.version = "bench-" + time.Now().UTC().Format("20060102t150405.000000000")
	}
	if cfg.toolAuthName == "" {
		cfg.toolAuthName = "artifact-tool-auth"
	}
	if cfg.toolAuthKey == "" {
		cfg.toolAuthKey = "token"
	}
	cfg.tokenFile = orkaBenchTokenFile(t)
	return cfg
}

func runOrkaBenchCase(t *testing.T, bc benchCase, cfg orkaBenchConfig) {
	t.Helper()
	start := time.Now()
	dataDir := writeOrkaBenchSkeleton(t, bc)
	projectDir := cfg.projectDir
	if projectDir == "" {
		projectDir = writeOrkaBenchProject(t, bc, defaultBenchAgentic())
	}

	deadline := cfg.taskTimeout + cfg.wait + 5*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	workDir := t.TempDir()

	producerArgs := []string{
		"-data=" + dataDir,
		"-project-dir=" + projectDir,
		"-tool-manifests=" + cfg.toolManifests,
		"-tasks-out=" + filepath.Join(workDir, "tasks"),
		"-tools-out=" + filepath.Join(workDir, "tools"),
		"-namespace=" + cfg.namespace,
		"-provider=" + cfg.provider,
		"-model=" + cfg.model,
		"-api-mode=" + cfg.apiMode,
		"-version=" + cfg.version,
		"-timeout=" + cfg.taskTimeout.String(),
		"-tool-auth-secret=" + cfg.toolAuthName,
		"-tool-auth-key=" + cfg.toolAuthKey,
		"-max-concurrent-tasks=1",
		"-wave-timeout=" + cfg.taskTimeout.String(),
		"-apply",
	}
	if cfg.kubeContext != "" {
		producerArgs = append(producerArgs, "-context="+cfg.kubeContext)
	}
	if cfg.taskExecution != "" {
		producerArgs = append(producerArgs, "-task-execution="+cfg.taskExecution)
	}
	runOrkaBenchCommand(t, ctx, cfg.backendRoot, "producer", cfg.producer, producerArgs...)
	waitForOrkaBenchTask(t, ctx, dataDir, bc, cfg)

	ingestorArgs := []string{
		"-data=" + dataDir,
		"-project-dir=" + projectDir,
		"-api=" + cfg.api,
		"-token-file=" + cfg.tokenFile,
		"-version=" + cfg.version,
		"-provider=" + cfg.provider,
		"-model=" + cfg.model,
		"-wait=" + cfg.wait.String(),
		"-poll=5s",
		"-pattern-wait=0",
		"-namespace=" + cfg.namespace,
		"-skip-side-effects",
	}
	if cfg.kubeContext != "" {
		ingestorArgs = append(ingestorArgs, "-context="+cfg.kubeContext)
	}

	runOrkaBenchCommand(t, ctx, cfg.backendRoot, "ingestor", cfg.ingestor, ingestorArgs...)
	elapsed := time.Since(start).Round(time.Second)
	tc := readOrkaBenchResult(t, dataDir, bc)
	scoreBenchCase(t, bc, tc, elapsed, "orka")
}

func waitForOrkaBenchTask(t *testing.T, ctx context.Context, dataDir string, bc benchCase, cfg orkaBenchConfig) {
	t.Helper()
	manifest, err := orka.LoadAnalysisManifest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	detail := readOrkaBenchDetail(t, dataDir, bc)
	ref, err := manifest.TaskRef(detail.JobID, detail.Runs[0], 0, detail.Runs[0].TestCases[0])
	if err != nil {
		t.Fatal(err)
	}
	restConfig, err := orka.RESTConfig(cfg.kubeContext)
	if err != nil {
		t.Fatal(err)
	}
	client, err := orka.NewKubeClient(restConfig)
	if err != nil {
		t.Fatal(err)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		phase, err := client.TaskPhase(ctx, cfg.namespace, ref.Name)
		if err != nil {
			t.Fatalf("read Orka benchmark Task %s: %v", ref.Name, err)
		}
		if orka.TerminalPhase(phase) {
			t.Logf("Orka benchmark Task %s reached %s", ref.Name, phase)
			if phase != "Succeeded" {
				t.Fatalf("Orka benchmark Task %s reached %s before producing an ingestible result", ref.Name, phase)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Orka benchmark Task %s: %v", ref.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeOrkaBenchSkeleton(t *testing.T, bc benchCase) string {
	t.Helper()
	dataDir := t.TempDir()
	jobID := models.JobIDFor(bc.jobType, bc.repo, bc.jobName)
	run := models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID:    bc.buildID,
			JobName:    bc.jobName,
			Passed:     false,
			Result:     "FAILURE",
			PullNumber: bc.pullNumber,
			ProwURL:    bc.webURL,
			WebURL:     bc.webURL,
		},
		TestCases:   []models.TestCase{*benchTestCase(bc)},
		TestsTotal:  1,
		TestsFailed: 1,
	}
	detail := models.JobDetail{
		Name: bc.jobName, JobID: jobID, JobType: bc.jobType, Repo: bc.repo,
		Runs: []models.BuildResult{run},
	}
	dashboard := models.Dashboard{Jobs: []models.JobSummary{{
		ProwJob:       models.ProwJob{Name: bc.jobName, JobType: bc.jobType, Repo: bc.repo, JobID: jobID},
		OverallStatus: "FAILING",
		RecentRuns:    []models.RunSummary{{BuildID: bc.buildID, Passed: false, Result: "FAILURE"}},
	}}}
	if err := output.WriteDashboard(dataDir, dashboard); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteJobDetail(dataDir, detail); err != nil {
		t.Fatal(err)
	}
	report := models.FlakinessReport{}
	if bc.consecutiveFailures > 1 {
		report.PersistentFailures = []models.TestFlakiness{{
			TestName: bc.testName, JobName: bc.jobName, JobID: jobID,
			ConsecutiveFailures: bc.consecutiveFailures, Classification: models.ClassificationPersistent,
		}}
	}
	if err := output.WriteFlakinessReport(dataDir, report); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func writeOrkaBenchProject(t *testing.T, bc benchCase, agentic project.Agentic) string {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"id":         "benchmark",
		"name":       "Prow AI benchmark",
		"short_name": "BENCH",
		"source": map[string]any{
			"include_presubmits": bc.jobType == models.JobTypePresubmit,
		},
		"testgrid": map[string]any{"dashboard": "benchmark"},
		"storage":  map[string]any{"provider": "gcs", "bucket": bc.bucket},
		"branding": map[string]any{
			"title":     "Prow AI benchmark",
			"base_path": "/benchmark",
			"site_url":  "https://example.invalid/benchmark",
			"source_repo": map[string]any{
				"owner": bc.sourceRepo[0],
				"name":  bc.sourceRepo[1],
			},
		},
		"ai": map[string]any{
			"tools":          []string{"filesystem", "k8s"},
			"min_tool_calls": agentic.MinToolCalls,
			"min_gcs_bytes":  agentic.MinGCSBytes,
		},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "system.md"), []byte(benchPromptAddendum+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readOrkaBenchResult(t *testing.T, dataDir string, bc benchCase) *models.TestCase {
	t.Helper()
	detail := readOrkaBenchDetail(t, dataDir, bc)
	return &detail.Runs[0].TestCases[0]
}

func readOrkaBenchDetail(t *testing.T, dataDir string, bc benchCase) *models.JobDetail {
	t.Helper()
	jobID := models.JobIDFor(bc.jobType, bc.repo, bc.jobName)
	path := filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var detail models.JobDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Runs) != 1 || len(detail.Runs[0].TestCases) != 1 {
		t.Fatalf("Orka benchmark output has %d runs and unexpected test count", len(detail.Runs))
	}
	return &detail
}

func benchRepoRoots(t *testing.T) (string, string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve benchmark source path")
	}
	backendRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return backendRoot, filepath.Dir(backendRoot)
}

func buildOrkaBenchBinary(t *testing.T, backendRoot, binDir, name, pkg string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runOrkaBenchCommand(t, ctx, backendRoot, "build "+name, "go", "build", "-o", path, pkg)
	return path
}

func runOrkaBenchCommand(t *testing.T, ctx context.Context, dir, label, command string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\n%s", label, err, output.String())
	}
	if text := strings.TrimSpace(output.String()); text != "" {
		t.Logf("%s:\n%s", label, text)
	}
}

func orkaBenchTokenFile(t *testing.T) string {
	t.Helper()
	if file := strings.TrimSpace(os.Getenv("BENCH_ORKA_TOKEN_FILE")); file != "" {
		return file
	}
	token := strings.TrimSpace(os.Getenv("BENCH_ORKA_TOKEN"))
	if token == "" {
		t.Fatal("BENCH_ORKA_TOKEN or BENCH_ORKA_TOKEN_FILE is required")
	}
	path := filepath.Join(t.TempDir(), "orka-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteOrkaBenchProject(t *testing.T) {
	var bc benchCase
	for _, candidate := range benchCases {
		if candidate.name == "flatcar-worker-dns-providerid" {
			bc = candidate
			break
		}
	}
	if bc.name == "" {
		t.Fatal("Flatcar benchmark case is missing")
	}
	dir := writeOrkaBenchProject(t, bc, defaultBenchAgentic())
	cfg, prompt, err := project.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Bucket != bc.bucket || cfg.Branding.SourceRepo.Owner != bc.sourceRepo[0] {
		t.Fatalf("project config = %+v", cfg)
	}
	if prompt != benchPromptAddendum {
		t.Fatalf("prompt = %q", prompt)
	}
	set, selection, err := skills.LoadForTools(dir, cfg.AI.EffectiveAgentic().Tools)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Kubernetes || set.Hash() == "" {
		t.Fatalf("generated benchmark profiles were not loaded: selection=%+v hash=%q", selection, set.Hash())
	}
	matched := set.Match("MachineDeployment timed out while the Flatcar worker Node lacked providerID")
	found := false
	for _, skill := range matched {
		if skill.ID == "engine.kubernetes.machine-node-providerid" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("generated benchmark profiles did not include the Machine/Node recipe: %+v", matched)
	}
}

func TestWriteOrkaBenchSkeleton(t *testing.T) {
	bc := benchCases[0]
	dataDir := writeOrkaBenchSkeleton(t, bc)
	tc := readOrkaBenchResult(t, dataDir, bc)
	if tc.Name != bc.testName || tc.FailureMessage != bc.failureMsg || tc.Status != "failed" {
		t.Fatalf("test case = %+v", tc)
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "flakiness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report models.FlakinessReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.PersistentFailures) != 1 || report.PersistentFailures[0].ConsecutiveFailures != bc.consecutiveFailures {
		t.Fatalf("persistent failures = %+v", report.PersistentFailures)
	}
}
