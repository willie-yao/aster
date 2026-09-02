package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
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

func (f *doctorFakeSweeper) Discover(_ context.Context, _ *project.Config, include bool) (JobSweep, error) {
	f.calls++
	f.include = include
	return JobSweep{Jobs: append([]models.ProwJob(nil), f.jobs...)}, f.err
}

const doctorProjectYAML = `id: project
name: Project
discovery:
  testgrid_dashboard: dashboard
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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: ${{ vars.AI_API }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-model: ${{ vars.AI_MODEL }}
      ai-reasoning-effort: ${{ vars.AI_REASONING_EFFORT }}
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

	for _, check := range report.Checks {
		if check.Name == "Pages AI values" && strings.Contains(check.Action, project.AIReasoningEffortEnv) {
			t.Fatalf("optional reasoning effort reported as required: %+v", check)
		}
	}
}

func TestDoctor_PagesMissingProviderMappings(t *testing.T) {
	sweeper := &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}}
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs:\n  deploy:\n    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main\n"}),
		sweeper: sweeper,
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
	for _, check := range report.Checks {
		if check.Name == "Pages AI" && (!strings.Contains(check.Detail, "ai-endpoint") || !strings.Contains(check.Detail, "ai-model") || strings.Contains(check.Detail, "ai-api")) {
			t.Fatalf("Pages AI detail = %q", check.Detail)
		}
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
		{name: "cluster ip with removed from-only policy", originYAML: "server:\n  actions:\n    enabled: true\n  service:\n    type: ClusterIP\nnetworkPolicy:\n  enabled: true\n  from:\n    - namespaceSelector:\n        matchLabels:\n          name: ingress\n", want: DoctorWarn},
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
		{name: "escalation cluster ip without network policy", originYAML: "server:\n  pullRequestEscalation:\n    enabled: true\n  service:\n    type: ClusterIP\n", want: DoctorWarn},
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

func TestDoctor_KubernetesPullRequestEscalation(t *testing.T) {
	const aiValues = "ai:\n  enabled: true\n  api: chat_completions\n  endpoint: https://model.example.test/v1/chat/completions\n  model: fixture\n"
	tests := []struct {
		name        string
		serverYAML  string
		aiYAML      string
		pullEnabled bool
		want        DoctorStatus
	}{
		{name: "disabled", want: DoctorPass},
		{name: "enabled without ai", serverYAML: "server:\n  pullRequestEscalation:\n    enabled: true\n", want: DoctorFail},
		{name: "enabled without pull request triage", serverYAML: "server:\n  pullRequestEscalation:\n    enabled: true\n", aiYAML: aiValues, want: DoctorFail},
		{name: "enabled without a read token", serverYAML: "server:\n  pullRequestEscalation:\n    enabled: true\n", aiYAML: aiValues, pullEnabled: true, want: DoctorWarn},
		{
			name:        "existing ai secret does not supply a read token",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n",
			aiYAML:      aiValues + "  existingSecret: shared-ai\n",
			pullEnabled: true,
			want:        DoctorWarn,
		},
		{
			name:        "actions supply the bot token fallback",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n  actions:\n    enabled: true\n",
			aiYAML:      aiValues,
			pullEnabled: true,
			want:        DoctorPass,
		},
		{
			name:        "server extra env supplies a read token",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n  extraEnv:\n    - name: GITHUB_TOKEN\n      value: ghp-test\n",
			aiYAML:      aiValues,
			pullEnabled: true,
			want:        DoctorPass,
		},
		{
			name:        "an empty github token does not clear the bot token",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n  actions:\n    enabled: true\n  extraEnv:\n    - name: GITHUB_TOKEN\n",
			aiYAML:      aiValues,
			pullEnabled: true,
			want:        DoctorPass,
		},
		{
			name:        "server extra env clears the read token",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n",
			aiYAML:      aiValues + "  githubReadTokenSecretName: read-token\n",
			pullEnabled: true,
			want:        DoctorWarn,
		},
		{
			name:        "fully configured",
			serverYAML:  "server:\n  pullRequestEscalation:\n    enabled: true\n",
			aiYAML:      aiValues + "  githubReadTokenSecretName: read-token\n",
			pullEnabled: true,
			want:        DoctorPass,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := "persistence:\n  storageClass: azurefile-csi\n  accessMode: ReadWriteMany\n" + test.aiYAML + test.serverYAML
			projectYAML := doctorProjectYAML
			if test.pullEnabled {
				projectYAML += "pull_requests:\n  enabled: true\n"
			}
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files: doctorFiles(map[string]string{
					"/consumer/deploy/values.yaml": values,
					"/consumer/project.yaml":       projectYAML,
				}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if !hasDoctorCheck(report, "Kubernetes pull request escalation", test.want) {
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

// The read-token check is the only offline signal that triage will read GitHub
// anonymously, so each way a deployment can supply the token must be resolved.
func TestDoctor_PullRequestTriageCredential(t *testing.T) {
	const triageProjectYAML = doctorProjectYAML + `pull_requests:
  enabled: true
`
	const storage = `persistence:
  storageClass: azurefile-csi
  accessMode: ReadWriteMany
`
	cases := []struct {
		name   string
		values string
		want   DoctorStatus
		// detail distinguishes the three warn branches, which otherwise share a
		// status and could pass through the wrong one.
		detail string
	}{
		{name: "no token", values: storage + "ai:\n  enabled: false\n", want: DoctorWarn, detail: "reads GitHub anonymously"},
		{name: "inline token", values: storage + "ai:\n  enabled: false\n  githubReadToken: ghp-test\n", want: DoctorPass},
		{name: "secret name", values: storage + "ai:\n  enabled: false\n  githubReadTokenSecretName: gh-read\n", want: DoctorPass},
		{name: "placeholder token", values: storage + "ai:\n  enabled: false\n  githubReadTokenSecretName: <your-secret>\n", want: DoctorWarn, detail: "reads GitHub anonymously"},
		{name: "fetcher extraEnv", values: storage + "ai:\n  enabled: false\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      value: ghp-test\n", want: DoctorPass},
		{name: "extraEnv secretKeyRef", values: storage + "ai:\n  enabled: false\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      valueFrom:\n        secretKeyRef:\n          name: gh\n          key: token\n", want: DoctorPass},
		{name: "extraEnv optional secretKeyRef", values: storage + "ai:\n  enabled: false\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      valueFrom:\n        secretKeyRef:\n          name: gh\n          key: token\n          optional: true\n", want: DoctorWarn, detail: "mounted as optional"},
		{name: "extraEnv empty value", values: storage + "ai:\n  enabled: false\n  githubReadTokenSecretName: gh-read\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      value: \"\"\n", want: DoctorWarn, detail: "blanks the read token"},
		{name: "empty fallback does not shadow the chart token", values: storage + "ai:\n  enabled: false\n  githubReadTokenSecretName: gh-read\nfetcher:\n  extraEnv:\n    - name: GITHUB_TOKEN\n      value: \"\"\n", want: DoctorPass},
		{name: "last duplicate wins", values: storage + "ai:\n  enabled: false\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      value: ghp-test\n    - name: GITHUB_READ_TOKEN\n      value: \"\"\n", want: DoctorWarn, detail: "reads GitHub anonymously"},
		{name: "optional override cannot take away the chart token", values: storage + "ai:\n  enabled: false\n  githubReadTokenSecretName: gh-read\nfetcher:\n  extraEnv:\n    - name: GITHUB_READ_TOKEN\n      valueFrom:\n        secretKeyRef:\n          name: gh\n          key: token\n          optional: true\n", want: DoctorPass},
		{name: "github fallback supplies a token alongside an ai secret", values: storage + "ai:\n  enabled: true\n  api: chat_completions\n  endpoint: https://model.example.test/v1/chat/completions\n  model: fixture\n  existingSecret: shared-ai\nfetcher:\n  extraEnv:\n    - name: GITHUB_TOKEN\n      value: ghp-test\n", want: DoctorPass},
		{name: "unrelated extraEnv", values: storage + "ai:\n  enabled: false\nfetcher:\n  extraEnv:\n    - name: HTTPS_PROXY\n      value: http://proxy\n", want: DoctorWarn, detail: "reads GitHub anonymously"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := doctorFiles(map[string]string{"/consumer/deploy/values.yaml": tc.values})
			files["/consumer/project.yaml"] = triageProjectYAML
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   files,
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if report.HasFailures() {
				t.Fatalf("unexpected failures: %+v", report.Checks)
			}
			if !hasDoctorCheck(report, "pull request triage credential", tc.want) {
				t.Fatalf("want %s, checks = %+v", tc.want, report.Checks)
			}
			if tc.detail == "" {
				return
			}
			for _, check := range report.Checks {
				if check.Name == "pull request triage credential" && strings.Contains(check.Detail, tc.detail) {
					return
				}
			}
			t.Fatalf("want detail containing %q, checks = %+v", tc.detail, report.Checks)
		})
	}
}

// The provider Secret does not configure GitHub reads.
func TestDoctor_PullRequestTriageExistingSecretStillReadsAnonymously(t *testing.T) {
	const triageProjectYAML = doctorProjectYAML + `pull_requests:
  enabled: true
`
	values := `persistence:
  storageClass: azurefile-csi
  accessMode: ReadWriteMany
ai:
  enabled: true
  api: chat_completions
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture
  existingSecret: shared-ai
`
	files := doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values})
	files["/consumer/project.yaml"] = triageProjectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "pull request triage credential", DoctorWarn) {
		t.Fatalf("checks = %+v", report.Checks)
	}
	for _, check := range report.Checks {
		if check.Name == "pull request triage credential" && strings.Contains(check.Detail, "reads GitHub anonymously") {
			return
		}
	}
	t.Fatalf("anonymous-read warning missing: %+v", report.Checks)
}

// The reusable workflow always passes the Actions token to the fetch step, so
// Pages consumers must not be told to configure a credential they already have.
func TestDoctor_PullRequestTriageCredentialPassesOnPages(t *testing.T) {
	const triageProjectYAML = doctorProjectYAML + `pull_requests:
  enabled: true
`
	files := doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow})
	files["/consumer/project.yaml"] = triageProjectYAML
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   files,
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "pull request triage credential", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

// Triage that is off, a project with no deployment profile, or a Pages workflow
// that never reaches the reusable deploy must not report on a credential doctor
// does not use or cannot resolve.
func TestDoctor_PullRequestTriageCredentialStaysSilent(t *testing.T) {
	const triageProjectYAML = doctorProjectYAML + `pull_requests:
  enabled: true
`
	cases := []struct {
		name   string
		triage bool
		files  map[string]string
	}{
		{name: "triage disabled", files: map[string]string{"/consumer/deploy/values.yaml": "persistence:\n  existingClaim: data\nai:\n  enabled: false\n"}},
		{name: "no deployment profile"},
		{
			name:   "pages workflow misses the reusable deploy",
			triage: true,
			files:  map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs:\n  deploy:\n    uses: other/repo/.github/workflows/build.yml@main\n"},
		},
		{
			name:   "pages workflow is not valid yaml",
			triage: true,
			files:  map[string]string{"/consumer/.github/workflows/deploy.yml": "jobs: [\n"},
		},
		{
			name:   "pages skips the fetch entirely",
			triage: true,
			files: map[string]string{"/consumer/.github/workflows/deploy.yml": `jobs:
  deploy:
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
    with:
      skip-fetch: true
`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := doctorFiles(tc.files)
			if tc.triage {
				files["/consumer/project.yaml"] = triageProjectYAML
			}
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   files,
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			for _, check := range report.Checks {
				if check.Name == "pull request triage credential" {
					t.Fatalf("unexpected credential check: %+v", check)
				}
			}
		})
	}
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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
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
		{name: "your prefix existing claim", values: "persistence:\n  existingClaim: your-data\n", wantStatus: DoctorPass},
		{name: "your prefix storage class", values: "persistence:\n  storageClass: your-rwx\n", wantStatus: DoctorPass},
		{name: "invalid existing claim", values: "persistence:\n  existingClaim: INVALID_CLAIM\n", wantStatus: DoctorFail},
		{name: "existing claim leading whitespace", values: "persistence:\n  existingClaim: \" your-data\"\n", wantStatus: DoctorFail},
		{name: "existing claim trailing whitespace", values: "persistence:\n  existingClaim: \"your-data \"\n", wantStatus: DoctorFail},
		{name: "invalid storage class", values: "persistence:\n  storageClass: INVALID_CLASS\n", wantStatus: DoctorFail},
		{name: "storage class leading whitespace", values: "persistence:\n  storageClass: \" your-rwx\"\n", wantStatus: DoctorFail},
		{name: "storage class trailing whitespace", values: "persistence:\n  storageClass: \"your-rwx \"\n", wantStatus: DoctorFail},
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

func TestK8sYourPrefixStoragePlanApplyAndDoctor(t *testing.T) {
	for _, test := range []struct {
		name          string
		storageClass  string
		existingClaim string
	}{
		{name: "storage class", storageClass: "your-rwx"},
		{name: "existing claim", existingClaim: "your-data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, _, _, _ := wizardDependencies("")
			deps.files = localScaffoldWriter{}
			disabled := false
			opts := Options{
				TestGrid: "dashboard-a", DashboardRepo: defaultTestDashboardRepo,
				SourceRepo: "example/project", Mode: modeK8s, EngineRef: "main",
				OutDir: filepath.Join(t.TempDir(), "consumer"), PromptMode: promptModeTemplate, AIEnabled: &disabled,
				K8sStorageClass: test.storageClass, K8sExistingClaim: test.existingClaim,
			}
			plan, err := buildPlan(context.Background(), opts, planningContext{}, deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyPlan(context.Background(), plan, "", deps); err != nil {
				t.Fatal(err)
			}
			values, err := os.ReadFile(filepath.Join(opts.OutDir, "deploy", "values.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			report := DoctorReport{}
			checkKubernetes(&report, values, &plan.Project)
			if !hasDoctorCheck(report, "Kubernetes storage", DoctorPass) {
				t.Fatalf("checks = %+v", report.Checks)
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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: vars.AI_API
      ai-endpoint: vars.AI_ENDPOINT
      ai-model: vars.AI_MODEL
      ai-reasoning-effort: vars.AI_REASONING_EFFORT
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

func TestDoctor_PagesSkipFetchDoesNotRequireProviderMappings(t *testing.T) {
	workflow := `jobs:
  deploy:
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
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

func TestDoctor_KubernetesRequiresValuesProviderCoordinates(t *testing.T) {
	values := `persistence:
  storageClass: fast
  accessMode: ReadWriteMany
ai:
  enabled: true
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Kubernetes AI", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesRequiresReusableDeployTarget(t *testing.T) {
	workflow := strings.Replace(doctorPagesWorkflow, "willie-yao/aster/.github/workflows/reusable-deploy.yml@main", "example/other/.github/workflows/build.yml@main", 1)
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
	for _, valid := range []string{
		"willie-yao/aster/.github/workflows/reusable-deploy.yml@main",
	} {
		if !reusableDeployReference(valid) {
			t.Errorf("valid reference rejected: %s", valid)
		}
	}
	for _, invalid := range []string{
		"willie-yao/aster/.github/workflows/reusable-deploy.yml@",
		"willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main",
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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
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

func TestDoctor_PagesOmittedAPIDefaultsToChatCompletions(t *testing.T) {
	workflow := `jobs:
  deploy:
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main
    with:
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-model: ${{ vars.AI_MODEL }}
      ai-reasoning-effort: ${{ vars.AI_REASONING_EFFORT }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Pages AI", DoctorPass) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestDoctor_PagesRejectsInvalidBooleanInputs(t *testing.T) {
	for _, key := range []string{"ai", "skip-fetch"} {
		t.Run(key, func(t *testing.T) {
			workflow := strings.Replace(doctorPagesWorkflow, "with:\n", "with:\n      "+key+": enabled\n", 1)
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": workflow}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if !hasDoctorCheck(report, "Pages AI", DoctorFail) {
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
	projectYAML := strings.Replace(doctorProjectYAML, "  testgrid_dashboard: dashboard\n", "  testgrid_dashboard: dashboard\n  include_presubmits: true\n", 1)
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

// A credential with surrounding whitespace is rejected by the endpoint as
// invalid, with an error that names neither the file nor the field, so doctor
// has to catch it here.
func TestDoctor_KubernetesCredentialWhitespaceFails(t *testing.T) {
	values := `persistence:
  storageClass: fast
  accessMode: ReadWriteMany
ai:
  enabled: true
  endpoint: https://provider.example/v1/chat/completions
  model: model
  token: "sk-secret\n"
`
	report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
		files:   doctorFiles(map[string]string{"/consumer/deploy/values.yaml": values}),
		sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
	})
	if !hasDoctorCheck(report, "Kubernetes AI credential", DoctorFail) {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

// Pull request triage is undiscoverable from project.yaml alone, so doctor
// points at it when the consumer has never made a decision about it.
func TestDoctor_PullRequestTriageHint(t *testing.T) {
	cases := []struct {
		name        string
		pullRequest string
		want        bool
	}{
		{name: "absent", want: true},
		{name: "explicitly disabled", pullRequest: "pull_requests:\n  enabled: false\n"},
		{name: "enabled", pullRequest: "pull_requests:\n  enabled: true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
				files: doctorFiles(map[string]string{
					"/consumer/project.yaml":                 doctorProjectYAML + tc.pullRequest,
					"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow,
				}),
				sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
			})
			if got := hasDoctorCheck(report, "pull request triage", DoctorPass); got != tc.want {
				t.Fatalf("hint present = %v, want %v: %+v", got, tc.want, report.Checks)
			}
			// A parse failure returns before any of this, which would make the
			// silent cases pass for the wrong reason.
			if !hasDoctorCheck(report, "Prow discovery", DoctorPass) {
				t.Fatalf("doctor returned early: %+v", report.Checks)
			}
		})
	}
}

// The warning has to reflect what actually runs, so it reads the value already
// resolved from project.yaml.
func TestDoctor_IncludePresubmitsWarning(t *testing.T) {
	t.Run("silent by default", func(t *testing.T) {
		report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
			files:   doctorFiles(map[string]string{"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow}),
			sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "job", JobType: models.JobTypePeriodic}}},
		})
		if hasDoctorCheck(report, "discovery.include_presubmits", DoctorWarn) {
			t.Fatalf("checks = %+v", report.Checks)
		}
	})
	t.Run("project.yaml", func(t *testing.T) {
		report := runDoctor(context.Background(), DoctorOptions{ProjectDir: "/consumer"}, doctorDependencies{
			files: doctorFiles(map[string]string{
				"/consumer/project.yaml":                 strings.Replace(doctorProjectYAML, "  testgrid_dashboard: dashboard\n", "  testgrid_dashboard: dashboard\n  include_presubmits: true\n", 1),
				"/consumer/.github/workflows/deploy.yml": doctorPagesWorkflow,
			}),
			sweeper: &doctorFakeSweeper{jobs: []models.ProwJob{{Name: "pull-job", JobType: models.JobTypePresubmit}}},
		})
		if !hasDoctorCheck(report, "discovery.include_presubmits", DoctorWarn) {
			t.Fatalf("checks = %+v", report.Checks)
		}
	})
}
