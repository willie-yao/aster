package onboard

import (
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
)

func buildPromptJobSummaries(jobs []models.ProwJob, definitions []jobconfig.JobDefinition, sourceRepo Repo, selectedDashboard string) []promptJobSummary {
	summaries := make([]promptJobSummary, 0, len(jobs))
	for _, job := range jobs {
		summary := promptJobSummary{Name: job.Name, Type: job.JobType, ConfigFile: job.ConfigFile, Repo: job.Repo}
		if job.Branch != "" {
			summary.Branches = append(summary.Branches, job.Branch)
		}
		if definition := matchingPromptJobDefinition(job, definitions); definition != nil {
			if summary.ConfigFile == "" {
				summary.ConfigFile = definition.ConfigFile
			}
			if summary.Repo == "" {
				summary.Repo = definition.Repo
			}
			summary.Branches = append(summary.Branches, definition.Branches...)
			for _, ref := range definition.Refs {
				refName := ref.FullRepo()
				if ref.BaseRef != "" {
					if refName != "" {
						refName += "@"
					}
					refName += ref.BaseRef
				}
				if refName != "" {
					summary.Branches = append(summary.Branches, refName)
				}
				if summary.Repo == "" && strings.EqualFold(ref.FullRepo(), sourceRepo.FullName) {
					summary.Repo = ref.FullRepo()
				}
			}
			summary.Dashboards = append(summary.Dashboards, splitDashboards(definition.Annotations["testgrid-dashboards"])...)
		}
		if len(summary.Dashboards) == 0 && selectedDashboard != "" {
			summary.Dashboards = append(summary.Dashboards, selectedDashboard)
		}
		summary.Branches = sortedUniqueStrings(summary.Branches)
		summary.Dashboards = sortedUniqueStrings(summary.Dashboards)
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Name != summaries[j].Name {
			return summaries[i].Name < summaries[j].Name
		}
		if summaries[i].Type != summaries[j].Type {
			return summaries[i].Type < summaries[j].Type
		}
		return summaries[i].ConfigFile < summaries[j].ConfigFile
	})
	return summaries
}

func matchingPromptJobDefinition(job models.ProwJob, definitions []jobconfig.JobDefinition) *jobconfig.JobDefinition {
	for i := range definitions {
		definition := &definitions[i]
		if definition.Name != job.Name || definition.JobType != job.JobType || job.ConfigFile != "" && definition.ConfigFile != job.ConfigFile {
			continue
		}
		if job.Repo != "" && !strings.EqualFold(definition.Repo, job.Repo) {
			continue
		}
		return definition
	}
	return nil
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
