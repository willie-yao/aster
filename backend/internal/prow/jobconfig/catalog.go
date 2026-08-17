package jobconfig

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
	"gopkg.in/yaml.v3"
)

// JobTypePostsubmit identifies postsubmit definitions used for discovery
// reporting. The dashboard fetcher does not ingest postsubmit artifacts.
const JobTypePostsubmit = "postsubmit"

// Catalog is a snapshot of Prow job definitions from one source revision.
type Catalog struct {
	Revision string                   `json:"revision,omitempty"`
	Jobs     map[string]JobDefinition `json:"jobs"`
}

// JobDefinition is the Prow metadata needed to select and observe verification jobs.
type JobDefinition struct {
	Name              string            `json:"name"`
	JobType           string            `json:"job_type"`
	Repo              string            `json:"repo,omitempty"`
	Context           string            `json:"context,omitempty"`
	Cluster           string            `json:"cluster,omitempty"`
	ConfigFile        string            `json:"config_file,omitempty"`
	Cron              string            `json:"cron,omitempty"`
	Interval          string            `json:"interval,omitempty"`
	MinimumInterval   string            `json:"minimum_interval,omitempty"`
	AlwaysRun         *bool             `json:"always_run,omitempty"`
	Optional          bool              `json:"optional,omitempty"`
	RunBeforeMerge    bool              `json:"run_before_merge,omitempty"`
	Trigger           string            `json:"trigger,omitempty"`
	RerunCommand      string            `json:"rerun_command,omitempty"`
	Branches          []string          `json:"branches,omitempty"`
	SkipBranches      []string          `json:"skip_branches,omitempty"`
	RunIfChanged      string            `json:"run_if_changed,omitempty"`
	SkipIfOnlyChanged string            `json:"skip_if_only_changed,omitempty"`
	Refs              []RepoRef         `json:"refs,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// RepoRef identifies a repository checkout used by a Prow job.
type RepoRef struct {
	Org       string `json:"org"`
	Repo      string `json:"repo"`
	BaseRef   string `json:"base_ref,omitempty"`
	PathAlias string `json:"path_alias,omitempty"`
}

// FullRepo returns the ref as "org/repo".
func (r RepoRef) FullRepo() string {
	if r.Org == "" || r.Repo == "" {
		return ""
	}
	return r.Org + "/" + r.Repo
}

// ID returns the stable identity used by dashboard job state.
func (j JobDefinition) ID() string {
	return models.JobIDFor(j.JobType, j.Repo, j.Name)
}

// EffectiveRerunCommand returns the configured command or Prow's default.
func (j JobDefinition) EffectiveRerunCommand() string {
	if command := strings.TrimSpace(j.RerunCommand); command != "" {
		return command
	}
	if j.JobType == models.JobTypePresubmit && j.Name != "" {
		return "/test " + j.Name
	}
	return ""
}

// TestsRepo reports whether this job checks out owner/name.
func (j JobDefinition) TestsRepo(repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return false
	}
	if (j.JobType == models.JobTypePresubmit || j.JobType == JobTypePostsubmit) && strings.EqualFold(j.Repo, repo) {
		return true
	}
	for _, ref := range j.Refs {
		if strings.EqualFold(ref.FullRepo(), repo) {
			return true
		}
	}
	return false
}

// AppliesToBranch reports whether this job's branch selectors admit branch.
func (j JobDefinition) AppliesToBranch(branch string) (bool, error) {
	return BranchSelectorMatches(j.Name, j.Branches, j.SkipBranches, branch)
}

// BranchSelectorMatches applies Prow's branches and skip_branches regex
// selectors. skip_branches wins, and an empty branches list admits every
// branch. jobName only labels selector errors.
func BranchSelectorMatches(jobName string, branches, skipBranches []string, branch string) (bool, error) {
	for _, pattern := range skipBranches {
		matches, err := regexp.MatchString(pattern, branch)
		if err != nil {
			return false, fmt.Errorf("invalid skip_branches selector for %s: %w", jobName, err)
		}
		if matches {
			return false, nil
		}
	}
	if len(branches) == 0 {
		return true, nil
	}
	for _, pattern := range branches {
		matches, err := regexp.MatchString(pattern, branch)
		if err != nil {
			return false, fmt.Errorf("invalid branches selector for %s: %w", jobName, err)
		}
		if matches {
			return true, nil
		}
	}
	return false, nil
}

// ParseCatalog parses every periodic, presubmit, and postsubmit in one Prow YAML file.
func ParseCatalog(data []byte, filename string) ([]JobDefinition, error) {
	var pf periodicsFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	var psf presubmitsFile
	if err := yaml.Unmarshal(data, &psf); err != nil {
		return nil, err
	}
	var pof postsubmitsFile
	if err := yaml.Unmarshal(data, &pof); err != nil {
		return nil, err
	}

	out := make([]JobDefinition, 0, len(pf.Periodics))
	for _, job := range pf.Periodics {
		out = append(out, convertDefinition(job, filename, models.JobTypePeriodic, ""))
	}
	repos := make([]string, 0, len(psf.Presubmits))
	for repo := range psf.Presubmits {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if !strings.Contains(repo, "/") {
			return nil, fmt.Errorf("presubmit repo key %q is not org/repo", repo)
		}
		for _, job := range psf.Presubmits[repo] {
			out = append(out, convertDefinition(job, filename, models.JobTypePresubmit, repo))
		}
	}
	postRepos := make([]string, 0, len(pof.Postsubmits))
	for repo := range pof.Postsubmits {
		postRepos = append(postRepos, repo)
	}
	sort.Strings(postRepos)
	for _, repo := range postRepos {
		if !strings.Contains(repo, "/") {
			return nil, fmt.Errorf("postsubmit repo key %q is not org/repo", repo)
		}
		for _, job := range pof.Postsubmits[repo] {
			out = append(out, convertDefinition(job, filename, JobTypePostsubmit, repo))
		}
	}
	return out, nil
}

func convertDefinition(r rawJob, filename, jobType, repo string) JobDefinition {
	refs := make([]RepoRef, 0, len(r.ExtraRefs))
	for _, ref := range r.ExtraRefs {
		refs = append(refs, RepoRef(ref))
	}
	annotations := make(map[string]string, len(r.Annotations))
	for key, value := range r.Annotations {
		annotations[key] = value
	}
	return JobDefinition{
		Name: r.Name, JobType: jobType, Repo: repo, Context: r.Context,
		Cluster: r.Cluster, ConfigFile: filename, Cron: r.Cron,
		Interval: r.Interval, MinimumInterval: r.MinimumInterval,
		AlwaysRun: r.AlwaysRun, Optional: r.Optional, RunBeforeMerge: r.RunBeforeMerge,
		Trigger: r.Trigger, RerunCommand: r.RerunCommand,
		Branches: append([]string(nil), r.Branches...), SkipBranches: append([]string(nil), r.SkipBranches...),
		RunIfChanged: r.RunIfChanged, SkipIfOnlyChanged: r.SkipIfOnlyChanged,
		Refs: refs, Annotations: annotations,
	}
}

// CatalogFromJobs builds a realized catalog for bucket-discovered Prow jobs.
func CatalogFromJobs(jobs []models.ProwJob, revision string) *Catalog {
	catalog := &Catalog{Revision: revision, Jobs: map[string]JobDefinition{}}
	for _, job := range jobs {
		if job.Name == "" || job.JobType == "" {
			continue
		}
		definition := JobDefinition{
			Name: job.Name, JobType: job.JobType, Repo: job.Repo,
			ConfigFile: job.ConfigFile, MinimumInterval: job.MinimumInterval,
		}
		if job.Repo != "" && job.JobType == models.JobTypePeriodic {
			org, repo, ok := strings.Cut(job.Repo, "/")
			if ok {
				definition.Refs = []RepoRef{{Org: org, Repo: repo, BaseRef: job.Branch}}
			}
		}
		key := definition.ID()
		if _, exists := catalog.Jobs[key]; exists {
			continue
		}
		catalog.Jobs[key] = definition
	}
	return catalog
}
