package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

// Confidence describes how strongly discovery supports an inferred value.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Inferred records a suggested value and why it was selected.
type Inferred[T any] struct {
	Value      T          `json:"value"`
	Source     string     `json:"source"`
	Confidence Confidence `json:"confidence"`
}

// Repo is a normalized GitHub repository identity.
type Repo struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	Visibility string `json:"visibility,omitempty"`
	Branch     string `json:"default_branch,omitempty"`
}

// RepositoryMetadata is the bounded GitHub metadata used for suggestions.
type RepositoryMetadata struct {
	Repo     Repo  `json:"repo"`
	Private  bool  `json:"private"`
	Upstream *Repo `json:"upstream,omitempty"`
}

// DashboardCandidate is a ranked TestGrid dashboard for a source repository.
type DashboardCandidate struct {
	Dashboard      string   `json:"dashboard"`
	MatchingJobs   int      `json:"matching_jobs"`
	PeriodicJobs   int      `json:"periodic_jobs"`
	PresubmitJobs  int      `json:"presubmit_jobs"`
	BranchCoverage int      `json:"branch_coverage"`
	JobNames       []string `json:"job_names,omitempty"`
}

// IdentitySuggestions contains editable project identity defaults.
type IdentitySuggestions struct {
	ID        Inferred[string] `json:"id"`
	Name      Inferred[string] `json:"name"`
	ShortName Inferred[string] `json:"short_name"`
}

// DiscoveryReport is a read-only repository-first discovery result.
type DiscoveryReport struct {
	SourceRepo      Repo                      `json:"source_repo"`
	MetadataSource  string                    `json:"metadata_source"`
	Metadata        RepositoryMetadata        `json:"metadata"`
	CatalogRevision string                    `json:"catalog_revision,omitempty"`
	MatchingJobs    []jobconfig.JobDefinition `json:"matching_jobs,omitempty"`
	Candidates      []DashboardCandidate      `json:"candidate_testgrid_dashboards,omitempty"`
	Identity        IdentitySuggestions       `json:"suggested_identity"`
	DashboardRepo   Inferred[string]          `json:"suggested_dashboard_repo"`
	BasePath        Inferred[string]          `json:"suggested_pages_base_path"`
	SiteURL         Inferred[string]          `json:"suggested_pages_site_url"`
	Categories      []project.CategoryRule    `json:"suggested_categories,omitempty"`
	Warnings        []string                  `json:"warnings,omitempty"`
}

type repositoryClient interface {
	Repository(context.Context, Repo, string) (RepositoryMetadata, error)
}

type catalogClient interface {
	ForRepo(context.Context, string) (*jobconfig.Catalog, error)
}

type prowCatalogClient struct {
	client *http.Client
}

func (c prowCatalogClient) ForRepo(ctx context.Context, repo string) (*jobconfig.Catalog, error) {
	return jobconfig.FetchCatalogForRepo(ctx, c.client, repo)
}

// Discover performs repository-first discovery without rendering or mutation.
func Discover(ctx context.Context, source, token string) (DiscoveryReport, error) {
	if strings.TrimSpace(source) == "" {
		remote, err := (gitRemoteDetector{}).Origin(ctx)
		if err != nil {
			return DiscoveryReport{}, fmt.Errorf("source repository is required and no GitHub origin remote was detected")
		}
		source = remote
	}
	repo, err := NormalizeGitHubRepo(source)
	if err != nil {
		return DiscoveryReport{}, err
	}
	client := defaultDiscoveryHTTPClient()
	return discoverRepository(ctx, repo, token, githubRepositoryClient{client: client}, prowCatalogClient{client: client})
}

func discoverRepository(ctx context.Context, repo Repo, token string, repositories repositoryClient, catalogs catalogClient) (DiscoveryReport, error) {
	metadata, err := repositories.Repository(ctx, repo, token)
	if err != nil {
		return DiscoveryReport{}, err
	}
	repo = metadata.Repo
	catalog, err := catalogs.ForRepo(ctx, repo.FullName)
	if err != nil {
		return DiscoveryReport{}, fmt.Errorf("discovering Prow jobs for %s: %w", repo.FullName, err)
	}
	matching := matchingDefinitions(catalog, repo.FullName)
	candidates := RankDashboardCandidates(matching)
	categoryNames := make([]string, 0, len(matching))
	for _, job := range matching {
		categoryNames = append(categoryNames, job.Name)
	}
	if len(candidates) > 0 {
		categoryNames = append([]string(nil), candidates[0].JobNames...)
	}

	dashboardRepo := repo.Owner + "/" + repo.Name + "-prow-ai-dashboard"
	shortName := suggestShortName(repo.Name)
	shortConfidence := ConfidenceLow
	shortSource := "repository name did not provide a safe abbreviation"
	if shortName != "" {
		shortConfidence = ConfidenceMedium
		shortSource = "initials from the GitHub repository name"
	}
	report := DiscoveryReport{
		SourceRepo:      repo,
		MetadataSource:  "GitHub repository API",
		Metadata:        metadata,
		CatalogRevision: catalog.Revision,
		MatchingJobs:    matching,
		Candidates:      candidates,
		Identity: IdentitySuggestions{
			ID:        Inferred[string]{Value: repo.Name, Source: "GitHub repository name", Confidence: ConfidenceHigh},
			Name:      Inferred[string]{Value: labelFor(repo.Name), Source: "GitHub repository name", Confidence: ConfidenceMedium},
			ShortName: Inferred[string]{Value: shortName, Source: shortSource, Confidence: shortConfidence},
		},
		DashboardRepo: Inferred[string]{Value: dashboardRepo, Source: "source repository owner and name", Confidence: ConfidenceHigh},
		BasePath:      Inferred[string]{Value: "/" + repo.Name + "-prow-ai-dashboard", Source: "suggested dashboard repository name", Confidence: ConfidenceHigh},
		SiteURL:       Inferred[string]{Value: "https://" + repo.Owner + ".github.io/" + repo.Name + "-prow-ai-dashboard", Source: "GitHub Pages repository convention", Confidence: ConfidenceHigh},
		Categories:    InferCategories(categoryNames),
	}
	if metadata.Upstream != nil && metadata.Upstream.FullName != repo.FullName {
		report.Warnings = append(report.Warnings, "The repository is a fork of "+metadata.Upstream.FullName+". Prow configuration often references the upstream repository instead.")
	}
	if len(matching) == 0 {
		report.Warnings = append(report.Warnings, "No kubernetes/test-infra jobs were found for this repository. Provide a TestGrid dashboard or artifact bucket explicitly.")
	} else if len(candidates) == 0 {
		report.Warnings = append(report.Warnings, "Matching Prow jobs do not advertise a TestGrid dashboard. Provide a TestGrid dashboard or artifact bucket explicitly.")
	}
	return report, nil
}

func matchingDefinitions(catalog *jobconfig.Catalog, repo string) []jobconfig.JobDefinition {
	if catalog == nil {
		return nil
	}
	out := make([]jobconfig.JobDefinition, 0, len(catalog.Jobs))
	for _, definition := range catalog.Jobs {
		if definition.TestsRepo(repo) {
			out = append(out, definition)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].JobType != out[j].JobType {
			return out[i].JobType < out[j].JobType
		}
		return out[i].ConfigFile < out[j].ConfigFile
	})
	return out
}

// RankDashboardCandidates groups TestGrid annotations and returns the strongest
// candidate first with deterministic tie breaking.
func RankDashboardCandidates(jobs []jobconfig.JobDefinition) []DashboardCandidate {
	type aggregate struct {
		candidate DashboardCandidate
		branches  map[string]struct{}
	}
	groups := map[string]*aggregate{}
	for _, job := range jobs {
		for _, dashboard := range splitDashboards(job.Annotations["testgrid-dashboards"]) {
			group := groups[dashboard]
			if group == nil {
				group = &aggregate{candidate: DashboardCandidate{Dashboard: dashboard}, branches: map[string]struct{}{}}
				groups[dashboard] = group
			}
			group.candidate.MatchingJobs++
			group.candidate.JobNames = append(group.candidate.JobNames, job.Name)
			switch job.JobType {
			case models.JobTypePeriodic:
				group.candidate.PeriodicJobs++
			case models.JobTypePresubmit:
				group.candidate.PresubmitJobs++
			}
			for _, branch := range job.Branches {
				if branch = strings.TrimSpace(branch); branch != "" {
					group.branches[branch] = struct{}{}
				}
			}
			for _, ref := range job.Refs {
				if branch := strings.TrimSpace(ref.BaseRef); branch != "" {
					group.branches[branch] = struct{}{}
				}
			}
		}
	}
	out := make([]DashboardCandidate, 0, len(groups))
	for _, group := range groups {
		sort.Strings(group.candidate.JobNames)
		group.candidate.BranchCoverage = len(group.branches)
		out = append(out, group.candidate)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MatchingJobs != out[j].MatchingJobs {
			return out[i].MatchingJobs > out[j].MatchingJobs
		}
		if out[i].PeriodicJobs != out[j].PeriodicJobs {
			return out[i].PeriodicJobs > out[j].PeriodicJobs
		}
		if out[i].PresubmitJobs != out[j].PresubmitJobs {
			return out[i].PresubmitJobs > out[j].PresubmitJobs
		}
		if out[i].BranchCoverage != out[j].BranchCoverage {
			return out[i].BranchCoverage > out[j].BranchCoverage
		}
		return out[i].Dashboard < out[j].Dashboard
	})
	return out
}

func splitDashboards(value string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func suggestShortName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(parts) < 2 || len(parts) > 5 {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		r, _ := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(r))
	}
	if b.Len() < 2 {
		return ""
	}
	return b.String()
}

func defaultDiscoveryHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func safeTerminal(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, value)
}

// WriteDiscovery prints a discovery report as text or JSON.
func WriteDiscovery(out io.Writer, report DiscoveryReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprintf(out, "Source repository: %s\n", safeTerminal(report.SourceRepo.FullName))
	fmt.Fprintf(out, "GitHub metadata: %s, default branch %s, visibility %s\n", safeTerminal(report.MetadataSource), safeTerminal(report.SourceRepo.Branch), safeTerminal(report.SourceRepo.Visibility))
	fmt.Fprintf(out, "Pinned test-infra revision: %s\n", safeTerminal(report.CatalogRevision))
	fmt.Fprintf(out, "Matching Prow jobs: %d\n", len(report.MatchingJobs))
	fmt.Fprintln(out, "Candidate TestGrid dashboards:")
	if len(report.Candidates) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for i, candidate := range report.Candidates {
		fmt.Fprintf(out, "  %d. %s\n", i+1, safeTerminal(candidate.Dashboard))
		fmt.Fprintf(out, "     %d periodic jobs, %d presubmit jobs, %d branch value(s)\n", candidate.PeriodicJobs, candidate.PresubmitJobs, candidate.BranchCoverage)
	}
	fmt.Fprintf(out, "Suggested project id: %s (%s, %s confidence)\n", safeTerminal(report.Identity.ID.Value), safeTerminal(report.Identity.ID.Source), report.Identity.ID.Confidence)
	fmt.Fprintf(out, "Suggested dashboard repository: %s\n", safeTerminal(report.DashboardRepo.Value))
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", safeTerminal(warning))
	}
	return nil
}
