// Package jobconfig parses Prow job configuration YAML files from the
// kubernetes/test-infra repository and extracts job metadata for the
// configured testgrid dashboard.
package jobconfig

import (
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"gopkg.in/yaml.v3"
)

// rawJob is the intermediate representation used to unmarshal a single job
// entry from the Prow YAML before converting it to a models.ProwJob.
type rawJob struct {
	Name              string            `yaml:"name"`
	Context           string            `yaml:"context"`
	Cluster           string            `yaml:"cluster"`
	Cron              string            `yaml:"cron"`
	MinimumInterval   string            `yaml:"minimum_interval"`
	Interval          string            `yaml:"interval"`
	AlwaysRun         *bool             `yaml:"always_run"`
	Optional          bool              `yaml:"optional"`
	RunBeforeMerge    bool              `yaml:"run_before_merge"`
	Trigger           string            `yaml:"trigger"`
	RerunCommand      string            `yaml:"rerun_command"`
	Branches          []string          `yaml:"branches"`
	SkipBranches      []string          `yaml:"skip_branches"`
	RunIfChanged      string            `yaml:"run_if_changed"`
	SkipIfOnlyChanged string            `yaml:"skip_if_only_changed"`
	DecorationConfig  *decorationConfig `yaml:"decoration_config"`
	ExtraRefs         []extraRef        `yaml:"extra_refs"`
	Annotations       map[string]string `yaml:"annotations"`
}

type decorationConfig struct {
	Timeout string `yaml:"timeout"`
}

type extraRef struct {
	Org       string `yaml:"org"`
	Repo      string `yaml:"repo"`
	BaseRef   string `yaml:"base_ref"`
	PathAlias string `yaml:"path_alias"`
}

// periodicsFile represents the top-level structure of a periodics YAML file.
type periodicsFile struct {
	Periodics []rawJob `yaml:"periodics"`
}

// presubmitsFile represents the top-level structure of a presubmits YAML file.
type presubmitsFile struct {
	Presubmits map[string][]rawJob `yaml:"presubmits"`
}

// ParseJobConfig parses Prow YAML and returns jobs whose testgrid-dashboards
// annotation contains the given dashboard. filename is recorded in ConfigFile.
// categories controls substring-to-category mapping; nil leaves jobs ungrouped.
//
// Both `periodics:` and `presubmits:` top-level sections are recognized.
// Files containing only one of the two are the common case; files that
// declare both are also supported and emit both shapes. Returned jobs carry
// JobType and, for presubmits, Repo from the presubmits map key.
func ParseJobConfig(data []byte, filename, dashboard string, categories []project.CategoryRule) ([]models.ProwJob, error) {
	var pf periodicsFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	var psf presubmitsFile
	if err := yaml.Unmarshal(data, &psf); err != nil {
		return nil, err
	}

	var result []models.ProwJob
	for _, r := range pf.Periodics {
		if !matchesDashboard(r, dashboard) {
			continue
		}
		result = append(result, convertJob(r, filename, models.JobTypePeriodic, "", categories))
	}

	// Presubmits are keyed by "org/repo"; sort the keys so the output is
	// deterministic regardless of map iteration order.
	repos := make([]string, 0, len(psf.Presubmits))
	for k := range psf.Presubmits {
		repos = append(repos, k)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, r := range psf.Presubmits[repo] {
			if !matchesDashboard(r, dashboard) {
				continue
			}
			result = append(result, convertJob(r, filename, models.JobTypePresubmit, repo, categories))
		}
	}
	return result, nil
}

// matchesDashboard returns true when the job's testgrid-dashboards annotation
// contains the given dashboard name.
func matchesDashboard(r rawJob, dashboard string) bool {
	dashboards := r.Annotations["testgrid-dashboards"]
	for _, d := range strings.Split(dashboards, ",") {
		if strings.TrimSpace(d) == dashboard {
			return true
		}
	}
	return false
}

func convertJob(r rawJob, filename, jobType, repo string, categories []project.CategoryRule) models.ProwJob {
	// The engine models one interval string. Prefer minimum_interval, then
	// interval. Cron-only periodics keep an empty interval.
	interval := r.MinimumInterval
	if interval == "" {
		interval = r.Interval
	}
	j := models.ProwJob{
		Name:            r.Name,
		TabName:         r.Annotations["testgrid-tab-name"],
		Description:     r.Annotations["description"],
		MinimumInterval: interval,
		ConfigFile:      filename,
		Category:        project.CategorizeJob(r.Name, categories),
		JobType:         jobType,
		Repo:            repo,
		JobID:           models.JobIDFor(jobType, repo, r.Name),
	}
	if r.DecorationConfig != nil {
		j.Timeout = r.DecorationConfig.Timeout
	}
	if len(r.ExtraRefs) > 0 {
		j.Branch = r.ExtraRefs[0].BaseRef
	}
	return j
}
