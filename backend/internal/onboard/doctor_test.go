package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type doctorMapFS map[string]string

func (f doctorMapFS) ReadFile(path string) ([]byte, error) {
	value, ok := f[filepath.Clean(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(value), nil
}

type doctorFakeSweeper struct {
	jobs    []models.ProwJob
	err     error
	calls   int
	include bool
}

func (f *doctorFakeSweeper) Discover(_ context.Context, _ *project.Config, include bool) ([]models.ProwJob, error) {
	f.calls++
	f.include = include
	return append([]models.ProwJob(nil), f.jobs...), f.err
}

const doctorProjectYAML = `id: project
name: Project
testgrid:
  dashboard: dashboard
storage:
  provider: gcs
  bucket: bucket
branding:
  title: Project
  base_path: /dashboard
  site_url: https://example.test/dashboard
  source_repo:
    owner: example
    name: project
`

const doctorPagesWorkflow = `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: ${{ vars.AI_API }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-model: ${{ vars.AI_MODEL }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
`

func doctorFiles(extra map[string]string) doctorMapFS {
	files := doctorMapFS{
		"/consumer/project.yaml":      doctorProjectYAML,
		"/consumer/prompts/system.md": "# Prompt\n",
	}
	for path, value := range extra {
		files[filepath.Clean(path)] = value
	}
	return files
}

func TestDoctor_ValidPagesScaffold(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "periodic-project", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow}),
		sweeper: sweeper,
	})
	if report.HasFailures() {
		t.Fatalf("unexpected failures: %+v", report.Checks)
	}
	if sweeper.calls != 1 {
		t.Fatalf("discovery calls = %d", sweeper.calls)
	}
	if !hasDoctorCheck(report, "Pages AI values", DoctorWarn) || !hasDoctorCheck(report, "Prow discovery", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesMissingProviderMappings(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs:\n  deploy:\n    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main\n"}),
		sweeper: sweeper,
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_KubernetesPlaceholdersAreActionable(t *testing.T) {
	values := `persistence:
  storageClass: "<your-rwx-storage-class>"
  accessMode: ReadWriteMany
ai:
  enabled: true
  api: chat_completions
  endpoint: "http://<your-model-svc>/v1/chat/completions"
  model: "<your-model-id>"
`
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: sweeper,
	})
	if !hasDoctorCheck(report, "Kubernetes storage", DoctorFail) || !hasDoctorCheck(report, "Kubernetes AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
	for _, check := range report.Checks {
		if check.Status == DoctorFail && check.Action == "" {
			t.Fatalf("failure has no next action: %+v", check)
		}
	}
}

func TestDoctor_KubernetesDisabledAI(t *testing.T) {
	values := `persistence:
  storageClass: azurefile-csi
  accessMode: ReadWriteMany
ai:
  enabled: false
`
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: sweeper,
	})
	if report.HasFailures() || !hasDoctorCheck(report, "Kubernetes AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_KubernetesOriginSecurity(t *testing.T) {
	tests := []struct {
		name       string
		originYAML string
		want       DoctorStatus
	}{
		{name: "actions disabled", want: DoctorPass},
		{name: "cluster ip without network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\n", want: DoctorWarn},
		{name: "cluster ip with deny all network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress: []\n", want: DoctorPass},
		{name: "cluster ip with catch all network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - {}\n", want: DoctorWarn},
		{name: "cluster ip with empty pod selector", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - podSelector: {}\n", want: DoctorWarn},
		{name: "cluster ip with empty namespace labels", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchLabels: {}\n", want: DoctorWarn},
		{name: "cluster ip with scoped network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchLabels:\n              name: ingress\n", want: DoctorPass},
		{name: "cluster ip with single ip block", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - ipBlock:\n            cidr: 10.0.0.0/8\n", want: DoctorPass},
		{name: "cluster ip with complementary ip blocks", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - ipBlock:\n            cidr: 0.0.0.0/1\n        - ipBlock:\n            cidr: 128.0.0.0/1\n", want: DoctorWarn},
		{name: "cluster ip with selector expression", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchExpressions:\n              - key: access\n                operator: In\n                values: [ingress]\n", want: DoctorPass},
		{name: "cluster ip with exists expression", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchExpressions:\n              - key: ingress-access\n                operator: Exists\n", want: DoctorWarn},
		{name: "cluster ip with universal namespace exists expression", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchExpressions:\n              - key: kubernetes.io/metadata.name\n                operator: Exists\n", want: DoctorWarn},
		{name: "cluster ip with not in expression", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchExpressions:\n              - key: blocked\n                operator: NotIn\n                values: [true]\n", want: DoctorWarn},
		{name: "cluster ip with does not exist expression", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  ingress:\n    - from:\n        - namespaceSelector:\n            matchExpressions:\n              - key: blocked\n                operator: DoesNotExist\n", want: DoctorWarn},
		{name: "chat cluster ip without network policy", originYAML: "server:\n  chat:\n    enabled: true\n  service:\n    type: ClusterIP\n", want: DoctorWarn},
		{name: "unrestricted public load balancer", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n", want: DoctorWarn},
		{name: "chat public load balancer", originYAML: "server:\n  chat:\n    enabled: true\n  service:\n    type: LoadBalancer\n", want: DoctorWarn},
		{name: "network policy only public load balancer", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\nnetworkPolicy:\n  enabled: true\n", want: DoctorWarn},
		{name: "acknowledged public load balancer", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    publicOriginAcknowledged: true\n", want: DoctorWarn},
		{name: "universal source range", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    loadBalancerSourceRanges: [0.0.0.0/0]\nnetworkPolicy:\n  enabled: true\n", want: DoctorWarn},
		{name: "alternate ipv6 universal range", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    loadBalancerSourceRanges: ['0:0:0:0:0:0:0:0/0']\nnetworkPolicy:\n  enabled: true\n", want: DoctorWarn},
		{name: "complementary ipv4 ranges", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    loadBalancerSourceRanges: [0.0.0.0/1, 128.0.0.0/1]\nnetworkPolicy:\n  enabled: true\n  ingress: []\n", want: DoctorWarn},
		{name: "complementary ipv6 ranges", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    loadBalancerSourceRanges: ['::/1', '8000::/1']\nnetworkPolicy:\n  enabled: true\n  ingress: []\n", want: DoctorWarn},
		{name: "internal missing annotations", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    internal:\n      enabled: true\nnetworkPolicy:\n  enabled: true\n", want: DoctorWarn},
		{name: "source ranges and network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    loadBalancerSourceRanges: [10.0.0.0/8]\nnetworkPolicy:\n  enabled: true\n  ingress: []\n", want: DoctorPass},
		{name: "internal and network policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: LoadBalancer\n    internal:\n      enabled: true\n      annotations:\n        example.com/internal: true\nnetworkPolicy:\n  enabled: true\n  ingress: []\n", want: DoctorPass},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := "persistence:\n  storageClass: azurefile-csi\n  accessMode: ReadWriteMany\nai:\n  enabled: false\n" + test.originYAML
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if !hasDoctorCheck(report, "Kubernetes origin security", test.want) {
				t.Fatalf("checks = %+v", report.Checks)
			}
		})
	}
}

func TestDoctor_InvalidProjectStopsBeforeDiscovery(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job"}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorMapFS{"/consumer/project.yaml": "unknown: true\n"},
		sweeper: sweeper,
	})
	if !report.HasFailures() || sweeper.calls != 0 || !hasDoctorCheck(report, "project.yaml", DoctorFail) {
		t.Fatalf("report=%+v calls=%d", report, sweeper.calls)
	}
}

func TestDoctor_MissingPromptAndZeroJobs(t *testing.T) {
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "    ai: false\n"})
	delete(files, "/consumer/prompts/system.md")
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files: files, sweeper: &doctorFakeSweeper{},
	})
	if !hasDoctorCheck(report, "prompts/system.md", DoctorFail) || !hasDoctorCheck(report, "Prow discovery", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_DiscoveryErrorIsActionable(t *testing.T) {
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "    ai: false\n"}),
		sweeper: &doctorFakeSweeper{err: errors.New("catalog unavailable")},
	})
	for _, check := range report.Checks {
		if check.Name == "Prow discovery" {
			if check.Status != DoctorFail || check.Action == "" || !strings.Contains(check.Detail, "catalog unavailable") {
				t.Fatalf("check = %+v", check)
			}
			return
		}
	}
	t.Fatal("missing Prow discovery check")
}

func hasDoctorCheck(report DoctorReport, name string, status DoctorStatus) bool {
	for _, check := range report.Checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}

type doctorFailingWriter struct{}

func (doctorFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestWriteDoctorReport_PropagatesOutputError(t *testing.T) {
	report := DoctorReport{Checks: []DoctorCheck{{Name: "check", Status: DoctorPass, Detail: "ok"}}}
	if err := WriteDoctorReport(doctorFailingWriter{}, report); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteDoctorReport_SanitizesTerminalControls(t *testing.T) {
	var out strings.Builder
	report := DoctorReport{Checks: []DoctorCheck{{Name: "check\nforged", Status: DoctorFail, Detail: "bad\x1b[31m", Action: "fix\rnow"}}}
	if err := WriteDoctorReport(&out, report); err != nil {
		t.Fatalf("WriteDoctorReport: %v", err)
	}
	if strings.Contains(out.String(), "\r") || strings.Contains(out.String(), "\x1b") || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("terminal controls were not sanitized: %q", out.String())
	}
	if !strings.Contains(out.String(), "check?forged") || !strings.Contains(out.String(), "fix?now") {
		t.Fatalf("sanitized fields missing: %q", out.String())
	}
}

func TestDoctor_PagesParsingIsScopedToDeployJob(t *testing.T) {
	workflow := `jobs:
  unrelated:
    with:
      ai: false
    steps:
      - run: echo "vars.AI_API secrets.AI_TOKEN"
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
		t.Fatalf("unrelated workflow text satisfied deploy checks: %+v", report.Checks)
	}
}

func TestDoctor_KubernetesStorageStrategies(t *testing.T) {
	tests := []struct {
		name       string
		values     string
		wantStatus DoctorStatus
	}{
		{name: "existing claim", values: "persistence:\n  existingClaim: shared-rwx\n", wantStatus: DoctorPass},
		{name: "wrong access mode", values: "persistence:\n  storageClass: fast\n  accessMode: ReadWriteOnce\n", wantStatus: DoctorFail},
		{name: "disabled without claim", values: "persistence:\n  enabled: false\n  storageClass: fast\n", wantStatus: DoctorFail},
		{name: "chart defaults AI disabled", values: "persistence:\n  storageClass: fast\n", wantStatus: DoctorPass},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": test.values}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if !hasDoctorCheck(report, "Kubernetes storage", test.wantStatus) {
				t.Fatalf("checks = %+v", report.Checks)
			}
			if test.name == "chart defaults AI disabled" && !hasDoctorCheck(report, "Kubernetes AI", DoctorPass) {
				t.Fatalf("missing ai.enabled did not inherit false: %+v", report.Checks)
			}
		})
	}
}

type doctorErrorFS struct {
	doctorMapFS
	path string
	err  error
}

func (f doctorErrorFS) ReadFile(path string) ([]byte, error) {
	if filepath.Clean(path) == filepath.Clean(f.path) {
		return nil, f.err
	}
	return f.doctorMapFS.ReadFile(path)
}

func TestDoctor_PromptReadErrorIsDistinct(t *testing.T) {
	base := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "    ai: false\n"})
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorErrorFS{doctorMapFS: base, path: "/consumer/prompts/system.md", err: os.ErrPermission},
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	for _, check := range report.Checks {
		if check.Name == "prompts/system.md" {
			if !strings.Contains(check.Detail, "permission") || !strings.Contains(check.Action, "permissions") {
				t.Fatalf("check = %+v", check)
			}
			return
		}
	}
	t.Fatal("missing prompt check")
}

func TestDoctor_PagesRequiresFullGitHubExpressions(t *testing.T) {
	workflow := `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: vars.AI_API
      ai-endpoint: vars.AI_ENDPOINT
      ai-model: vars.AI_MODEL
    secrets:
      AI_TOKEN: secrets.AI_TOKEN
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
		t.Fatalf("literal strings passed expression validation: %+v", report.Checks)
	}
}

func TestDoctor_PagesWorkflowCanLiveAboveProjectDir(t *testing.T) {
	files := doctorMapFS{
		"/repo/dashboard/project.yaml":       doctorProjectYAML,
		"/repo/dashboard/prompts/system.md":  "# Prompt\n",
		"/repo/.github/workflows/deploy.yml": strings.Replace(doctorPagesWorkflow, "with:\n", "with:\n      project_dir: dashboard\n", 1),
	}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/repo/dashboard"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if report.HasFailures() || !hasDoctorCheck(report, "deployment", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesAcceptsProviderCoordinatesInProjectConfig(t *testing.T) {
	projectYAML := doctorProjectYAML + `ai:
  api: responses
  endpoint: https://provider.example/v1/responses
  model: model-id
`
	workflow := `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
`
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow})
	files["/consumer/project.yaml"] = projectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesSkipFetchDoesNotRequireProviderMappings(t *testing.T) {
	workflow := `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      skip-fetch: true
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesProjectDirMismatchFails(t *testing.T) {
	files := doctorMapFS{
		"/repo/dashboard/project.yaml":       doctorProjectYAML,
		"/repo/dashboard/prompts/system.md":  "# Prompt\n",
		"/repo/.github/workflows/deploy.yml": doctorPagesWorkflow,
	}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/repo/dashboard"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages project_dir", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_KubernetesUsesProjectProviderCoordinates(t *testing.T) {
	projectYAML := doctorProjectYAML + `ai:
  api: responses
  endpoint: https://provider.example/v1/responses
  model: model-id
`
	values := `persistence:
  storageClass: fast
  accessMode: ReadWriteMany
ai:
  enabled: true
`
	files := doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values})
	files["/consumer/project.yaml"] = projectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Kubernetes AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesRequiresReusableDeployTarget(t *testing.T) {
	workflow := strings.Replace(doctorPagesWorkflow, "willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main", "example/other/.github/workflows/build.yml@main", 1)
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages workflow", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestGitHubExpression_AllowsOptionalWhitespace(t *testing.T) {
	for _, value := range []string{"${{ vars.AI_MODEL }}", "${{vars.AI_MODEL}}", "${{  vars.AI_MODEL  }}"} {
		if !githubExpression(value, "vars", "AI_MODEL") {
			t.Errorf("expression %q rejected", value)
		}
	}
}

func TestDoctor_KubernetesMissingCredentialWarns(t *testing.T) {
	values := `persistence:
  storageClass: fast
  accessMode: ReadWriteMany
ai:
  enabled: true
  endpoint: https://provider.example/v1/chat/completions
  model: model
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Kubernetes AI credential", DoctorWarn) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestNormalizeDoctorProjectDir_IsAbsolute(t *testing.T) {
	dir := normalizeDoctorProjectDir(".")
	if !filepath.IsAbs(dir) {
		t.Fatalf("normalized dir = %q, want absolute", dir)
	}
}

func TestReusableDeployReference_RequiresExactPathAndRef(t *testing.T) {
	valid := "willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main"
	if !reusableDeployReference(valid) {
		t.Fatalf("valid reference rejected: %s", valid)
	}
	for _, invalid := range []string{
		"willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@",
		"willie-yao/prow-ai-dashboard/.github/workflows/other.yml@main",
		"./.github/workflows/reusable-deploy.yml@main",
	} {
		if reusableDeployReference(invalid) {
			t.Errorf("invalid reference accepted: %s", invalid)
		}
	}
}

func TestDoctor_KubernetesPlaceholderCredentialWarns(t *testing.T) {
	values := `persistence:
  storageClass: fast
  accessMode: ReadWriteMany
ai:
  enabled: true
  endpoint: https://provider.example/v1/chat/completions
  model: model
  existingSecret: "<your-ai-secret>"
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Kubernetes AI credential", DoctorWarn) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesDynamicAIBranchWarns(t *testing.T) {
	workflow := `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      ai: ${{ vars.ENABLE_AI }}
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorWarn) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_ProjectAPIOverridesStaleWorkflowFallback(t *testing.T) {
	projectYAML := doctorProjectYAML + `ai:
  api: responses
  endpoint: https://provider.example/v1/responses
  model: model-id
`
	workflow := `jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: stale-literal
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
`
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow})
	files["/consumer/project.yaml"] = projectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_DeploymentPresubmitSettingsReachSweep(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		workflow := strings.Replace(doctorPagesWorkflow, "with:\n", "with:\n      include-presubmits: true\n", 1)
		sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "pull-job", JobType: models.JobTypePresubmit}}}
		_ = runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
			files: doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}), sweeper: sweeper,
		})
		if !sweeper.include {
			t.Fatal("Pages include-presubmits did not reach discovery")
		}
	})
	t.Run("kubernetes", func(t *testing.T) {
		values := "persistence:\n  storageClass: fast\nfetcher:\n  includePresubmits: true\n"
		sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "pull-job", JobType: models.JobTypePresubmit}}}
		_ = runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
			files: doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}), sweeper: sweeper,
		})
		if !sweeper.include {
			t.Fatal("Kubernetes includePresubmits did not reach discovery")
		}
	})
}

func TestDoctor_PagesRejectsInvalidBooleanInputs(t *testing.T) {
	for _, key := range []string{"ai", "skip-fetch", "include-presubmits"} {
		t.Run(key, func(t *testing.T) {
			workflow := strings.Replace(doctorPagesWorkflow, "with:\n", "with:\n      "+key+": enabled\n", 1)
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			checkName := "Pages AI"
			if key == "include-presubmits" {
				checkName = "Pages presubmits"
			}
			if !hasDoctorCheck(report, checkName, DoctorFail) {
				t.Fatalf("checks = %+v", report.Checks)
			}
		})
	}
}

func TestGitHubExpression_RejectsWhitespaceInsideIdentifier(t *testing.T) {
	if githubExpression("${{ v a r s.AI_MODEL }}", "vars", "AI_MODEL") {
		t.Fatal("invalid expression was accepted")
	}
}

func TestDoctor_ProjectPresubmitsDoNotSkipDeploymentValidation(t *testing.T) {
	projectYAML := doctorProjectYAML + "source:\n  include_presubmits: true\n"
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs: {}\n"})
	files["/consumer/project.yaml"] = projectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePresubmit}}},
	})
	if !hasDoctorCheck(report, "Pages workflow", DoctorFail) {
		t.Fatalf("project presubmits skipped deployment validation: %+v", report.Checks)
	}
}
