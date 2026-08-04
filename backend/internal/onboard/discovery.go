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
	Dashboard               string   `json:"dashboard"`
	MatchingJobs            int      `json:"matching_jobs"`
	PeriodicJobs            int      `json:"periodic_jobs"`
	PresubmitJobs           int      `json:"presubmit_jobs"`
	DashboardPeriodicJobs   int      `json:"dashboard_periodic_jobs"`
	DashboardPresubmitJobs  int      `json:"dashboard_presubmit_jobs"`
	DashboardPostsubmitJobs int      `json:"dashboard_postsubmit_jobs"`
	BranchCoverage          int      `json:"branch_coverage"`
	JobNames                []string `json:"job_names,omitempty"`
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
	AuthenticatedLogin(context.Context, string) (string, error)
}

type catalogClient interface {
	Catalog(context.Context) (*jobconfig.Catalog, error)
}

type prowCatalogClient struct {
	client *http.Client
}

func (c prowCatalogClient) Catalog(ctx context.Context) (*jobconfig.Catalog, error) {
	return jobconfig.FetchCatalog(ctx, c.client)
}

// Discover performs repository-first discovery without rendering or mutation.
func Discover(ctx context.Context, source, token string) (DiscoveryReport, error) {
	sourceDetected := strings.TrimSpace(source) == ""
	var detectedRepo *Repo
	if sourceDetected {
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
	if sourceDetected {
		detected := repo
		detectedRepo = &detected
	}
	client := defaultDiscoveryHTTPClient()
	repositories := githubRepositoryClient{client: client}
	owner, warning := inferDashboardOwner(ctx, token, detectedRepo, repositories)
	report, err := discoverRepository(ctx, repo, token, owner, repositories, prowCatalogClient{client: client})
	if err != nil {
		return DiscoveryReport{}, err
	}
	if warning != "" {
		report.Warnings = append(report.Warnings, warning)
	}
	return report, nil
}

func discoverRepository(ctx context.Context, repo Repo, token string, dashboardOwner Inferred[string], repositories repositoryClient, catalogs catalogClient) (DiscoveryReport, error) {
	metadata, err := repositories.Repository(ctx, repo, token)
	if err != nil {
		return DiscoveryReport{}, err
	}
	repo = metadata.Repo
	catalog, err := catalogs.Catalog(ctx)
	if err != nil {
		return DiscoveryReport{}, fmt.Errorf("discovering Prow jobs for %s: %w", repo.FullName, err)
	}
	matching := matchingDefinitions(catalog, repo.FullName)
	candidates := populateDashboardTotals(RankDashboardCandidates(matching), catalog)
	categoryNames := make([]string, 0, len(matching))
	for _, job := range matching {
		categoryNames = append(categoryNames, job.Name)
	}
	if len(candidates) > 0 {
		categoryNames = append([]string(nil), candidates[0].JobNames...)
	}

	dashboardName := suggestedDashboardName(repo.Name)
	dashboardRepo, siteURL := inferredDashboardDestination(dashboardOwner, dashboardName)
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
			ShortName: Inferred[string]{Source: "no reliable project abbreviation inferred", Confidence: ConfidenceLow},
		},
		DashboardRepo: dashboardRepo,
		BasePath:      Inferred[string]{Value: "/" + dashboardName, Source: "suggested dashboard repository name", Confidence: ConfidenceHigh},
		SiteURL:       siteURL,
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

func inferDashboardOwner(ctx context.Context, token string, detectedRepo *Repo, repositories repositoryClient) (Inferred[string], string) {
	if token != "" {
		login, err := repositories.AuthenticatedLogin(ctx, token)
		if err == nil && login != "" && !strings.Contains(login, token) {
			return Inferred[string]{Value: login, Source: "authenticated GitHub login", Confidence: ConfidenceHigh}, ""
		}
		warning := "The authenticated GitHub login could not be used for the dashboard repository suggestion."
		if detectedRepo != nil && detectedRepo.Owner != "" {
			return Inferred[string]{Value: detectedRepo.Owner, Source: "original Git remote owner", Confidence: ConfidenceHigh}, warning
		}
		return Inferred[string]{Source: "no authenticated GitHub login or detected Git remote owner", Confidence: ConfidenceLow}, warning
	}
	if detectedRepo != nil && detectedRepo.Owner != "" {
		return Inferred[string]{Value: detectedRepo.Owner, Source: "original Git remote owner", Confidence: ConfidenceHigh}, ""
	}
	return Inferred[string]{Source: "no authenticated GitHub login or detected Git remote owner", Confidence: ConfidenceLow}, ""
}

func inferredDashboardDestination(owner Inferred[string], dashboardName string) (Inferred[string], Inferred[string]) {
	if owner.Value == "" {
		source := owner.Source
		if source == "" {
			source = "no authenticated GitHub login or detected Git remote owner"
		}
		return Inferred[string]{Source: source, Confidence: ConfidenceLow}, Inferred[string]{Source: source, Confidence: ConfidenceLow}
	}
	return Inferred[string]{
			Value: owner.Value + "/" + dashboardName, Source: owner.Source + " and source repository name", Confidence: owner.Confidence,
		}, Inferred[string]{
			Value: "https://" + owner.Value + ".github.io/" + dashboardName, Source: owner.Source + " and GitHub Pages repository convention", Confidence: owner.Confidence,
		}
}

func matchingDefinitions(catalog *jobconfig.Catalog, repo string) []jobconfig.JobDefinition {
	if catalog == nil {
		return nil
	}
	out := make([]jobconfig.JobDefinition, 0, len(catalog.Jobs))
	for _, definition := range catalog.Jobs {
		if definition.JobType == jobconfig.JobTypePostsubmit {
			continue
		}
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

func populateDashboardTotals(candidates []DashboardCandidate, catalog *jobconfig.Catalog) []DashboardCandidate {
	indices := make(map[string]int, len(candidates))
	for i := range candidates {
		indices[candidates[i].Dashboard] = i
	}
	if catalog == nil {
		return candidates
	}
	for _, definition := range catalog.Jobs {
		for _, dashboard := range splitDashboards(definition.Annotations["testgrid-dashboards"]) {
			idx, ok := indices[dashboard]
			if !ok {
				continue
			}
			switch definition.JobType {
			case models.JobTypePeriodic:
				candidates[idx].DashboardPeriodicJobs++
			case models.JobTypePresubmit:
				candidates[idx].DashboardPresubmitJobs++
			case jobconfig.JobTypePostsubmit:
				candidates[idx].DashboardPostsubmitJobs++
			}
		}
	}
	return candidates
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

func directSourceMatchSummary(periodic, presubmit int) string {
	total := periodic + presubmit
	label := "matches"
	if total == 1 {
		label = "match"
	}
	return fmt.Sprintf("%d direct source %s: %d periodic, %d presubmit", total, label, periodic, presubmit)
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

func suggestedDashboardName(sourceName string) string {
	const (
		suffix        = "-prow-ai-dashboard"
		maxRepository = 100
	)
	maxSource := maxRepository - len(suffix)
	if len(sourceName) > maxSource {
		sourceName = sourceName[:maxSource]
	}
	return sourceName + suffix
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
	writer := discoveryTextWriter{out: out}
	writer.printf("Source repository: %s\n", safeTerminal(report.SourceRepo.FullName))
	writer.printf("GitHub metadata: %s, default branch %s, visibility %s\n", safeTerminal(report.MetadataSource), safeTerminal(report.SourceRepo.Branch), safeTerminal(report.SourceRepo.Visibility))
	writer.printf("Pinned test-infra revision: %s\n", safeTerminal(report.CatalogRevision))
	writer.printf("Matching Prow jobs: %d\n", len(report.MatchingJobs))
	writer.println("Candidate TestGrid dashboards:")
	if len(report.Candidates) == 0 {
		writer.println("  none")
	}
	for i, candidate := range report.Candidates {
		writer.printf("  %d. %s\n", i+1, safeTerminal(candidate.Dashboard))
		writer.printf("     %s; dashboard tabs: %d periodic, %d presubmit, %d postsubmit\n",
			directSourceMatchSummary(candidate.PeriodicJobs, candidate.PresubmitJobs), candidate.DashboardPeriodicJobs, candidate.DashboardPresubmitJobs, candidate.DashboardPostsubmitJobs)
		if candidate.DashboardPostsubmitJobs > 0 {
			writer.println("     note: postsubmit tabs are not supported by the dashboard fetcher")
		}
	}
	writeInference(&writer, "Suggested project id", report.Identity.ID)
	writeInference(&writer, "Suggested project name", report.Identity.Name)
	writeInference(&writer, "Suggested short name", report.Identity.ShortName)
	writeInference(&writer, "Suggested dashboard repository", report.DashboardRepo)
	writeInference(&writer, "Suggested Pages base path", report.BasePath)
	writeInference(&writer, "Suggested Pages site URL", report.SiteURL)
	if len(report.Categories) == 0 {
		writer.println("Suggested categories: none")
	} else {
		ids := make([]string, 0, len(report.Categories))
		for _, category := range report.Categories {
			ids = append(ids, safeTerminal(category.ID))
		}
		writer.printf("Suggested categories: %s\n", strings.Join(ids, ", "))
	}
	for _, warning := range report.Warnings {
		writer.printf("Warning: %s\n", safeTerminal(warning))
	}
	return writer.err
}

type discoveryTextWriter struct {
	out io.Writer
	err error
}

func (w *discoveryTextWriter) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.out, format, args...)
}

func (w *discoveryTextWriter) println(args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintln(w.out, args...)
}

func writeInference(writer *discoveryTextWriter, label string, inferred Inferred[string]) {
	value := safeTerminal(inferred.Value)
	if value == "" {
		value = "unresolved"
	}
	writer.printf("%s: %s (%s, %s confidence)\n", label, value, safeTerminal(inferred.Source), inferred.Confidence)
}
