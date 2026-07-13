package jobconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const testDashboard = "sig-cluster-lifecycle-cluster-api-provider-azure"

// exampleRules keeps parser tests covering non-trivial categorization without
// coupling production to a fixed rule set.
var exampleRules = []project.CategoryRule{
	{Match: "conformance", ID: "conformance", Label: "Conformance"},
	{Match: "capi-e2e", ID: "capi-e2e", Label: "CAPI E2E"},
	{Match: "upgrade", ID: "upgrade", Label: "Upgrade"},
	{Match: "coverage", ID: "coverage", Label: "Coverage"},
	{Match: "scalability", ID: "scalability", Label: "Scalability"},
	{Match: "e2e", ID: "e2e", Label: "E2E"},
}

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return data
}

func TestParsePeriodics(t *testing.T) {
	data := loadTestdata(t, "periodics.yaml")
	jobs, err := ParseJobConfig(data, "periodics.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}

	// The fixture has 4 jobs; only 3 belong to the CAPZ dashboard.
	if got := len(jobs); got != 3 {
		t.Fatalf("expected 3 jobs, got %d", got)
	}

	j := jobs[0]
	assertEqual(t, "Name", j.Name, "periodic-cluster-api-provider-azure-conformance-main")
	assertEqual(t, "TabName", j.TabName, "capz-periodic-conformance-main")
	assertEqual(t, "Category", j.Category, "conformance")
	assertEqual(t, "Branch", j.Branch, "main")
	assertEqual(t, "Description", j.Description, "Runs conformance & node conformance tests on a CAPZ cluster")
	assertEqual(t, "MinimumInterval", j.MinimumInterval, "48h")
	assertEqual(t, "Timeout", j.Timeout, "4h")
	assertEqual(t, "ConfigFile", j.ConfigFile, "periodics.yaml")
	assertEqual(t, "JobType", j.JobType, models.JobTypePeriodic)
	assertEqual(t, "Repo", j.Repo, "")
	assertEqual(t, "JobID", j.JobID, "periodic-cluster-api-provider-azure-conformance-main")

	assertEqual(t, "jobs[1].Category", jobs[1].Category, "e2e")
	assertEqual(t, "jobs[1].MinimumInterval", jobs[1].MinimumInterval, "24h")
	assertEqual(t, "jobs[1].Timeout", jobs[1].Timeout, "3h")

	assertEqual(t, "jobs[2].Category", jobs[2].Category, "coverage")
}

func TestParsePresubmits(t *testing.T) {
	data := loadTestdata(t, "presubmits.yaml")
	jobs, err := ParseJobConfig(data, "presubmits.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}

	// 3 jobs in the fixture, 1 is not CAPZ.
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs, got %d", got)
	}

	j := jobs[0]
	assertEqual(t, "Name", j.Name, "pull-cluster-api-provider-azure-e2e")
	assertEqual(t, "TabName", j.TabName, "capz-pr-e2e")
	assertEqual(t, "Category", j.Category, "e2e")
	assertEqual(t, "Branch", j.Branch, "main")
	assertEqual(t, "Timeout", j.Timeout, "3h")
	assertEqual(t, "ConfigFile", j.ConfigFile, "presubmits.yaml")
	assertEqual(t, "JobType", j.JobType, models.JobTypePresubmit)
	assertEqual(t, "Repo", j.Repo, "kubernetes-sigs/cluster-api-provider-azure")
	assertEqual(t, "JobID", j.JobID, "kubernetes-sigs/cluster-api-provider-azure/pull-cluster-api-provider-azure-e2e")

	// Second presubmit: capi-e2e category.
	assertEqual(t, "jobs[1].Category", jobs[1].Category, "capi-e2e")
	assertEqual(t, "jobs[1].Branch", jobs[1].Branch, "release-1.21")
	assertEqual(t, "jobs[1].JobType", jobs[1].JobType, models.JobTypePresubmit)
	assertEqual(t, "jobs[1].Repo", jobs[1].Repo, "kubernetes-sigs/cluster-api-provider-azure")
}

func TestCategorize(t *testing.T) {
	// Default rules with no project-specific overrides.
	cases := []struct {
		name     string
		expected string
	}{
		{"periodic-capz-conformance-main", "conformance"},
		{"pull-capz-capi-e2e-main", "capi-e2e"},
		{"periodic-capz-e2e-main", "e2e"},
		{"periodic-capz-upgrade-main", "upgrade"},
		{"periodic-capz-coverage", "coverage"},
		{"periodic-capz-scalability", "scalability"},
		{"periodic-capz-lint", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := project.CategorizeJob(tc.name, exampleRules)
			if got != tc.expected {
				t.Errorf("categorize(%q) = %q, want %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestCategorizeRespectsRuleOrder(t *testing.T) {
	// Consumer-supplied rules take precedence and are evaluated in order.
	rules := []project.CategoryRule{
		{Match: "managed kubernetes", ID: "aks-e2e", Label: "AKS E2E"},
		{Match: "e2e-aks", ID: "aks-e2e", Label: "AKS E2E"},
		{Match: "conformance", ID: "conformance", Label: "Conformance"},
		{Match: "e2e", ID: "capz-e2e", Label: "CAPZ E2E"},
	}
	cases := []struct {
		name, want string
	}{
		// AKS-specific rules win over the generic "e2e" rule.
		{"periodic-capz-e2e-aks-main", "aks-e2e"},
		{"pull-capz-[Managed Kubernetes]-e2e", "aks-e2e"},
		{"periodic-capz-e2e-main", "capz-e2e"},
		{"periodic-capz-conformance-main", "conformance"},
		{"periodic-capz-lint", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := project.CategorizeJob(tc.name, rules); got != tc.want {
				t.Errorf("categorize(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestSkipJobsForOtherDashboards(t *testing.T) {
	yaml := []byte(`
periodics:
- name: some-other-job
  annotations:
    testgrid-dashboards: sig-other
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestMissingAnnotations(t *testing.T) {
	yaml := []byte(`
periodics:
- name: job-without-annotations
  decoration_config:
    timeout: 1h
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	// No annotations means no testgrid-dashboards, so the job is filtered out.
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestMissingExtraRefs(t *testing.T) {
	yaml := []byte(`
periodics:
- name: capz-job-no-refs
  annotations:
    testgrid-dashboards: sig-cluster-lifecycle-cluster-api-provider-azure
    testgrid-tab-name: capz-no-refs
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	// Branch should be empty when extra_refs is absent.
	if jobs[0].Branch != "" {
		t.Errorf("expected empty Branch, got %q", jobs[0].Branch)
	}
	if jobs[0].Timeout != "" {
		t.Errorf("expected empty Timeout, got %q", jobs[0].Timeout)
	}
}

func TestEmptyInput(t *testing.T) {
	jobs, err := ParseJobConfig([]byte("{}"), "empty.yaml", testDashboard, exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

// CAPI-core-style periodics may use interval: instead of minimum_interval:.
// Both forms must populate MinimumInterval; minimum_interval: wins when both exist.
func TestIntervalFallback(t *testing.T) {
	yaml := []byte(`
periodics:
- name: periodic-cluster-api-test-main
  interval: 3h
  annotations:
    testgrid-dashboards: cluster-api-core-main
- name: periodic-mixed-job
  minimum_interval: 24h
  interval: 3h
  annotations:
    testgrid-dashboards: cluster-api-core-main
- name: periodic-cron-only
  annotations:
    testgrid-dashboards: cluster-api-core-main
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", "cluster-api-core-main", exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}
	assertEqual(t, "interval-only", jobs[0].MinimumInterval, "3h")
	assertEqual(t, "minimum_interval-wins", jobs[1].MinimumInterval, "24h")
	assertEqual(t, "neither-set", jobs[2].MinimumInterval, "")
}

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

// A file may declare both periodics: and presubmits: sections. Presubmits use
// the map key as Repo; periodics leave Repo empty.
func TestParseMixedPeriodicsAndPresubmits(t *testing.T) {
	yaml := []byte(`
periodics:
- name: periodic-mixed-job
  interval: 24h
  annotations:
    testgrid-dashboards: mixed-dashboard
presubmits:
  kubernetes-sigs/cluster-api:
  - name: pull-mixed-job
    annotations:
      testgrid-dashboards: mixed-dashboard
`)
	jobs, err := ParseJobConfig(yaml, "mixed.yaml", "mixed-dashboard", exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs, got %d", got)
	}
	assertEqual(t, "jobs[0].Name", jobs[0].Name, "periodic-mixed-job")
	assertEqual(t, "jobs[0].JobType", jobs[0].JobType, models.JobTypePeriodic)
	assertEqual(t, "jobs[0].Repo", jobs[0].Repo, "")
	assertEqual(t, "jobs[1].Name", jobs[1].Name, "pull-mixed-job")
	assertEqual(t, "jobs[1].JobType", jobs[1].JobType, models.JobTypePresubmit)
	assertEqual(t, "jobs[1].Repo", jobs[1].Repo, "kubernetes-sigs/cluster-api")
}

// Multiple repos under the same presubmits: section are sorted for deterministic output.
func TestParsePresubmits_SortedByRepo(t *testing.T) {
	yaml := []byte(`
presubmits:
  z-org/z-repo:
  - name: pull-z
    annotations:
      testgrid-dashboards: multi-repo-dashboard
  a-org/a-repo:
  - name: pull-a
    annotations:
      testgrid-dashboards: multi-repo-dashboard
  m-org/m-repo:
  - name: pull-m
    annotations:
      testgrid-dashboards: multi-repo-dashboard
`)
	jobs, err := ParseJobConfig(yaml, "multi.yaml", "multi-repo-dashboard", exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if got := len(jobs); got != 3 {
		t.Fatalf("expected 3 jobs, got %d", got)
	}
	wantRepos := []string{"a-org/a-repo", "m-org/m-repo", "z-org/z-repo"}
	for i, w := range wantRepos {
		if jobs[i].Repo != w {
			t.Errorf("jobs[%d].Repo = %q, want %q", i, jobs[i].Repo, w)
		}
	}
}

// JobIDFor builds a stable identifier across repos and job types.
func TestJobIDFor(t *testing.T) {
	cases := []struct {
		name, jobType, repo, jobName, want string
	}{
		{"periodic", models.JobTypePeriodic, "", "periodic-foo", "periodic-foo"},
		{"presubmit", models.JobTypePresubmit, "kubernetes-sigs/cluster-api", "pull-foo", "kubernetes-sigs/cluster-api/pull-foo"},
		{"periodic ignores repo", models.JobTypePeriodic, "kubernetes-sigs/cluster-api", "periodic-foo", "periodic-foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := models.JobIDFor(c.jobType, c.repo, c.jobName)
			if got != c.want {
				t.Errorf("JobIDFor(%q,%q,%q) = %q, want %q", c.jobType, c.repo, c.jobName, got, c.want)
			}
		})
	}
}

// Parser stamps JobID using JobIDFor so callers can key off one field.
func TestParseStampsJobID(t *testing.T) {
	yaml := []byte(`
periodics:
- name: periodic-foo
  annotations:
    testgrid-dashboards: d
presubmits:
  org/repo:
  - name: pull-foo
    annotations:
      testgrid-dashboards: d
`)
	jobs, err := ParseJobConfig(yaml, "mixed.yaml", "d", exampleRules)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs, got %d", got)
	}
	assertEqual(t, "periodic JobID", jobs[0].JobID, "periodic-foo")
	assertEqual(t, "presubmit JobID", jobs[1].JobID, "org/repo/pull-foo")
}
