package onboard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

type fakeRepositoryClient struct {
	metadata      RepositoryMetadata
	err           error
	authLogin     string
	authErr       error
	authCalls     int
	authTokenSeen string
}

func (f fakeRepositoryClient) Repository(context.Context, Repo, string) (RepositoryMetadata, error) {
	return f.metadata, f.err
}

func (f *fakeRepositoryClient) AuthenticatedLogin(_ context.Context, token string) (string, error) {
	f.authCalls++
	f.authTokenSeen = token
	return f.authLogin, f.authErr
}

type fakeCatalogClient struct {
	catalog *jobconfig.Catalog
	err     error
}

func (f fakeCatalogClient) Catalog(context.Context) (*jobconfig.Catalog, error) {
	return f.catalog, f.err
}

func TestRankDashboardCandidates(t *testing.T) {
	jobs := []jobconfig.JobDefinition{
		{
			Name: "periodic-a", JobType: models.JobTypePeriodic,
			Refs:        []jobconfig.RepoRef{{Org: "example", Repo: "project", BaseRef: "main"}},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-b, dashboard-a"},
		},
		{
			Name: "periodic-b", JobType: models.JobTypePeriodic,
			Refs:        []jobconfig.RepoRef{{Org: "example", Repo: "project", BaseRef: "release-1"}},
			Annotations: map[string]string{"testgrid-dashboards": "dashboard-a"},
		},
		{
			Name: "pull-a", JobType: models.JobTypePresubmit, Repo: "example/project",
			Branches: []string{"^main$"}, Annotations: map[string]string{"testgrid-dashboards": "dashboard-b"},
		},
	}
	got := RankDashboardCandidates(jobs)
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
	}
	if got[0].Dashboard != "dashboard-a" || got[0].PeriodicJobs != 2 || got[0].BranchCoverage != 2 {
		t.Fatalf("first candidate = %+v", got[0])
	}
	if got[1].Dashboard != "dashboard-b" || got[1].PeriodicJobs != 1 || got[1].PresubmitJobs != 1 {
		t.Fatalf("second candidate = %+v", got[1])
	}
	if !reflect.DeepEqual(got[0].JobNames, []string{"periodic-a", "periodic-b"}) {
		t.Fatalf("job ordering = %v", got[0].JobNames)
	}
}

func TestRankDashboardCandidates_NoDashboards(t *testing.T) {
	jobs := []jobconfig.JobDefinition{{Name: "periodic-a", JobType: models.JobTypePeriodic}}
	if got := RankDashboardCandidates(jobs); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

func TestRankDashboardCandidates_DeterministicTieBreak(t *testing.T) {
	jobs := []jobconfig.JobDefinition{
		{Name: "b", JobType: models.JobTypePeriodic, Annotations: map[string]string{"testgrid-dashboards": "z"}},
		{Name: "a", JobType: models.JobTypePeriodic, Annotations: map[string]string{"testgrid-dashboards": "a"}},
	}
	got := RankDashboardCandidates(jobs)
	if len(got) != 2 || got[0].Dashboard != "a" || got[1].Dashboard != "z" {
		t.Fatalf("candidate order = %+v", got)
	}
}

func TestJobDefinitionTestsRepo_PresubmitAndExtraRefs(t *testing.T) {
	presubmit := jobconfig.JobDefinition{JobType: models.JobTypePresubmit, Repo: "example/project"}
	periodic := jobconfig.JobDefinition{JobType: models.JobTypePeriodic, Refs: []jobconfig.RepoRef{{Org: "example", Repo: "project"}}}
	if !presubmit.TestsRepo("example/project") || !periodic.TestsRepo("example/project") {
		t.Fatalf("repository matching failed: presubmit=%t periodic=%t", presubmit.TestsRepo("example/project"), periodic.TestsRepo("example/project"))
	}
}

func TestDiscoverRepository_SuggestionsAndPinnedRevision(t *testing.T) {
	repositories := fakeRepositoryClient{metadata: RepositoryMetadata{Repo: Repo{
		Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure",
		FullName: "kubernetes-sigs/cluster-api-provider-azure", Branch: "main", Visibility: "public",
	}}}
	catalogs := fakeCatalogClient{catalog: &jobconfig.Catalog{Revision: "abcdef123456", Jobs: map[string]jobconfig.JobDefinition{
		"periodic": {
			Name: "periodic-capz-e2e", JobType: models.JobTypePeriodic,
			Refs:        []jobconfig.RepoRef{{Org: "kubernetes-sigs", Repo: "cluster-api-provider-azure", BaseRef: "main"}},
			Annotations: map[string]string{"testgrid-dashboards": "capz-dashboard"},
		},
	}}}
	report, err := discoverRepository(context.Background(), repositories.metadata.Repo, "", Inferred[string]{Value: "kubernetes-sigs", Source: "authenticated GitHub login", Confidence: ConfidenceHigh}, &repositories, catalogs)
	if err != nil {
		t.Fatalf("discoverRepository: %v", err)
	}
	if report.CatalogRevision != "abcdef123456" || len(report.Candidates) != 1 || report.Candidates[0].Dashboard != "capz-dashboard" {
		t.Fatalf("report = %+v", report)
	}
	if report.DashboardRepo.Value != "kubernetes-sigs/cluster-api-provider-azure-prow-ai-dashboard" || report.DashboardRepo.Source != "authenticated GitHub login and source repository name" {
		t.Fatalf("dashboard repo = %+v", report.DashboardRepo)
	}
	if report.Identity.ID.Confidence != ConfidenceHigh || report.Identity.Name.Value != "Cluster API Provider Azure" || report.Identity.ShortName.Value != "" || report.Identity.ShortName.Confidence != ConfidenceLow {
		t.Fatalf("identity = %+v", report.Identity)
	}
}

func TestDiscoverRepository_NoMatchingJobsWarns(t *testing.T) {
	repo := Repo{Owner: "example", Name: "project", FullName: "example/project"}
	report, err := discoverRepository(context.Background(), repo, "", Inferred[string]{}, &fakeRepositoryClient{metadata: RepositoryMetadata{Repo: repo}}, fakeCatalogClient{catalog: &jobconfig.Catalog{Revision: "abc", Jobs: map[string]jobconfig.JobDefinition{}}})
	if err != nil {
		t.Fatalf("discoverRepository: %v", err)
	}
	if len(report.Warnings) == 0 || !strings.Contains(report.Warnings[0], "No kubernetes/test-infra jobs") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestWriteDiscovery_EscapesTerminalControlCharacters(t *testing.T) {
	var out strings.Builder
	report := DiscoveryReport{
		SourceRepo:    Repo{FullName: "example/project", Branch: "main\x1b[31m", Visibility: "public"},
		Candidates:    []DashboardCandidate{{Dashboard: "dashboard\nforged", PeriodicJobs: 1}},
		Identity:      IdentitySuggestions{ID: Inferred[string]{Value: "project", Source: "repo", Confidence: ConfidenceHigh}},
		DashboardRepo: Inferred[string]{Value: "example/dashboard"},
	}
	if err := WriteDiscovery(&out, report, false); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "dashboard\nforged") {
		t.Fatalf("terminal controls were not escaped: %q", out.String())
	}
	if !strings.Contains(out.String(), "dashboard?forged") {
		t.Fatalf("sanitized dashboard missing: %q", out.String())
	}
}

func TestSuggestedDashboardNameIsValidForLongSourceName(t *testing.T) {
	source := strings.Repeat("a", 100)
	name := suggestedDashboardName(source)
	if len(name) != 100 || !strings.HasSuffix(name, "-prow-ai-dashboard") {
		t.Fatalf("suggested name length=%d value=%q", len(name), name)
	}
}

func TestWriteDiscovery_TextIncludesAllSuggestions(t *testing.T) {
	var out strings.Builder
	report := DiscoveryReport{
		SourceRepo: Repo{FullName: "example/project", Branch: "main", Visibility: "public"},
		Identity: IdentitySuggestions{
			ID:        Inferred[string]{Value: "project", Source: "repo", Confidence: ConfidenceHigh},
			Name:      Inferred[string]{Value: "Project", Source: "repo", Confidence: ConfidenceMedium},
			ShortName: Inferred[string]{Value: "PRJ", Source: "initials", Confidence: ConfidenceMedium},
		},
		DashboardRepo: Inferred[string]{Value: "example/project-prow-ai-dashboard", Source: "repo", Confidence: ConfidenceHigh},
		BasePath:      Inferred[string]{Value: "/project-prow-ai-dashboard", Source: "dashboard repo", Confidence: ConfidenceHigh},
		SiteURL:       Inferred[string]{Value: "https://example.github.io/project-prow-ai-dashboard", Source: "Pages", Confidence: ConfidenceHigh},
		Categories:    []project.CategoryRule{{ID: "e2e"}, {ID: "conformance"}},
	}
	if err := WriteDiscovery(&out, report, false); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}
	for _, want := range []string{"Suggested project name", "Suggested short name", "Suggested Pages base path", "Suggested Pages site URL", "e2e, conformance", "high confidence"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestWriteDiscovery_TextPropagatesWriteError(t *testing.T) {
	err := WriteDiscovery(failingWriter{}, DiscoveryReport{}, false)
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestDirectSourceMatchSummary(t *testing.T) {
	tests := []struct {
		periodic  int
		presubmit int
		want      string
	}{
		{periodic: 1, want: "1 direct source match: 1 periodic, 0 presubmit"},
		{periodic: 1, presubmit: 1, want: "2 direct source matches: 1 periodic, 1 presubmit"},
	}
	for _, test := range tests {
		if got := directSourceMatchSummary(test.periodic, test.presubmit); got != test.want {
			t.Errorf("directSourceMatchSummary(%d, %d) = %q, want %q", test.periodic, test.presubmit, got, test.want)
		}
	}
}

func TestPopulateDashboardTotals(t *testing.T) {
	matching := []jobconfig.JobDefinition{{
		Name: "source-periodic", JobType: models.JobTypePeriodic,
		Annotations: map[string]string{"testgrid-dashboards": "shared-dashboard"},
	}}
	catalog := &jobconfig.Catalog{Jobs: map[string]jobconfig.JobDefinition{
		"source":     matching[0],
		"periodic-2": {Name: "other-periodic", JobType: models.JobTypePeriodic, Annotations: map[string]string{"testgrid-dashboards": "shared-dashboard"}},
		"presubmit":  {Name: "other-presubmit", JobType: models.JobTypePresubmit, Repo: "example/other", Annotations: map[string]string{"testgrid-dashboards": "shared-dashboard"}},
		"postsubmit": {Name: "source-postsubmit", JobType: jobconfig.JobTypePostsubmit, Repo: "example/source", Annotations: map[string]string{"testgrid-dashboards": "shared-dashboard"}},
	}}
	candidates := populateDashboardTotals(RankDashboardCandidates(matching), catalog)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	candidate := candidates[0]
	if candidate.PeriodicJobs != 1 || candidate.PresubmitJobs != 0 {
		t.Fatalf("source-match counts = %+v", candidate)
	}
	if candidate.DashboardPeriodicJobs != 2 || candidate.DashboardPresubmitJobs != 1 || candidate.DashboardPostsubmitJobs != 1 {
		t.Fatalf("dashboard totals = %+v", candidate)
	}
}

func TestWriteDiscovery_LabelsMatchAndDashboardCounts(t *testing.T) {
	var out strings.Builder
	report := DiscoveryReport{
		SourceRepo: Repo{FullName: "example/source"},
		Candidates: []DashboardCandidate{{
			Dashboard: "shared-dashboard", PeriodicJobs: 1,
			DashboardPeriodicJobs: 45, DashboardPresubmitJobs: 6, DashboardPostsubmitJobs: 7,
		}},
		Identity:      IdentitySuggestions{ID: Inferred[string]{Value: "source"}},
		DashboardRepo: Inferred[string]{Value: "example/source-dashboard"},
	}
	if err := WriteDiscovery(&out, report, false); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}
	for _, want := range []string{
		"1 direct source match: 1 periodic, 0 presubmit",
		"dashboard tabs: 45 periodic, 6 presubmit, 7 postsubmit",
		"postsubmit tabs are not supported",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestInferDashboardOwnerPreference(t *testing.T) {
	detected := Repo{Owner: "fork-owner", Name: "project", FullName: "fork-owner/project"}

	t.Run("authenticated login", func(t *testing.T) {
		repositories := &fakeRepositoryClient{authLogin: "authenticated-owner"}
		owner, warning := inferDashboardOwner(context.Background(), "fixture-token", &detected, repositories)
		if owner.Value != "authenticated-owner" || owner.Source != "authenticated GitHub login" || owner.Confidence != ConfidenceHigh || warning != "" {
			t.Fatalf("owner=%+v warning=%q", owner, warning)
		}
		if repositories.authCalls != 1 || repositories.authTokenSeen != "fixture-token" {
			t.Fatalf("auth calls=%d token=%q", repositories.authCalls, repositories.authTokenSeen)
		}
	})

	t.Run("detected remote owner", func(t *testing.T) {
		repositories := &fakeRepositoryClient{}
		owner, warning := inferDashboardOwner(context.Background(), "", &detected, repositories)
		if owner.Value != "fork-owner" || owner.Source != "original Git remote owner" || owner.Confidence != ConfidenceHigh || warning != "" {
			t.Fatalf("owner=%+v warning=%q", owner, warning)
		}
		if repositories.authCalls != 0 {
			t.Fatalf("auth calls = %d", repositories.authCalls)
		}
	})

	t.Run("no safe owner", func(t *testing.T) {
		repositories := &fakeRepositoryClient{}
		owner, warning := inferDashboardOwner(context.Background(), "", nil, repositories)
		if owner.Value != "" || owner.Confidence != ConfidenceLow || warning != "" {
			t.Fatalf("owner=%+v warning=%q", owner, warning)
		}
	})

	t.Run("credential isolation", func(t *testing.T) {
		repositories := &fakeRepositoryClient{authLogin: "fixture-token"}
		owner, warning := inferDashboardOwner(context.Background(), "fixture-token", nil, repositories)
		if owner.Value != "" || warning == "" {
			t.Fatalf("owner=%+v warning=%q", owner, warning)
		}
		if strings.Contains(owner.Source+warning, "fixture-token") {
			t.Fatalf("credential leaked: owner=%+v warning=%q", owner, warning)
		}
	})
}

func TestInferredDashboardDestinationWithoutOwner(t *testing.T) {
	repo, site := inferredDashboardDestination(Inferred[string]{Source: "owner unresolved", Confidence: ConfidenceLow}, "project-prow-ai-dashboard")
	if repo.Value != "" || site.Value != "" || repo.Confidence != ConfidenceLow || site.Confidence != ConfidenceLow {
		t.Fatalf("repo=%+v site=%+v", repo, site)
	}
}
