package remediationinvestigation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

type fakeSourceAccess struct {
	current map[string]string
	files   map[string]map[string]string
}

func (f fakeSourceAccess) Current(_ context.Context, owner, name string) (sourceinvestigation.Repository, error) {
	revision := f.current[strings.ToLower(owner+"/"+name)]
	if revision == "" {
		return sourceinvestigation.Repository{}, errors.New("unavailable")
	}
	return sourceinvestigation.Repository{Owner: owner, Name: name, Revision: revision}, nil
}
func (f fakeSourceAccess) ListFiles(_ context.Context, repository sourceinvestigation.Repository) ([]string, error) {
	files := f.files[sourceKey(repository)]
	if files == nil {
		return nil, errors.New("unavailable")
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
func (f fakeSourceAccess) ReadFile(_ context.Context, repository sourceinvestigation.Repository, path string) (string, error) {
	content, ok := f.files[sourceKey(repository)][path]
	if !ok {
		return "", os.ErrNotExist
	}
	return content, nil
}

type fakeArtifactFactory struct{ browsers map[string]artifacts.Browser }

func (f fakeArtifactFactory) ForBuild(prefix, _ string) artifacts.Browser { return f.browsers[prefix] }

func TestPublishedResolverFreezesCurrentActiveCausalGroup(t *testing.T) {
	dataDir, cfg, ref, detail := publishedResolverFixture(t, false)
	failureRevision := strings.Repeat("a", 40)
	currentRevision := strings.Repeat("e", 40)
	resolver, err := NewPublishedResolver(PublishedResolverOptions{
		DataDir: dataDir, Config: cfg, ConsumerPrompt: "Project prompt.", SkillHash: strings.Repeat("c", 64),
		ProviderFingerprint: strings.Repeat("d", 16),
		Artifacts:           fixtureArtifactFactory(detail),
		Source: fakeSourceAccess{
			current: map[string]string{"example/repo": currentRevision},
			files: map[string]map[string]string{
				"example/repo@" + currentRevision: {"controllers/reconcile.go": serviceSourceContent},
				"example/repo@" + failureRevision: {"controllers/reconcile.go": serviceSourceContent},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Input.PatternID != ref.PatternID || resolved.Input.PatternHash != ref.PatternHash || resolved.Input.CausalGroupID != ref.CausalGroupID || resolved.Input.CausalGroupHash != ref.CausalGroupHash {
		t.Fatalf("input identity=%+v", resolved.Input)
	}
	if resolved.Input.InvestigationSource.Revision != currentRevision || len(resolved.Input.Builds) != 2 || len(resolved.Input.Analyses) != 2 {
		t.Fatalf("input=%+v", resolved.Input)
	}
	if len(resolved.Input.DestinationPolicy.Repositories) != 1 || !slicesEqual(resolved.Input.DestinationPolicy.Repositories[0].AllowedPaths, []string{"controllers/reconcile.go"}) {
		t.Fatalf("policy=%+v", resolved.Input.DestinationPolicy)
	}
	paths, truncated, err := resolved.Browser.ListTree(t.Context(), 10)
	if err != nil || truncated || !slicesEqual(paths, []string{"builds/1/log.txt", "builds/2/log.txt"}) {
		t.Fatalf("paths=%v truncated=%v err=%v", paths, truncated, err)
	}
}

func TestPublishedResolverUsesAllowlistedProwConfigurationRepository(t *testing.T) {
	dataDir, cfg, ref, detail := publishedResolverFixture(t, true)
	currentRevision := strings.Repeat("e", 40)
	configRevision := strings.Repeat("b", 40)
	resolver, err := NewPublishedResolver(PublishedResolverOptions{
		DataDir: dataDir, Config: cfg, ConsumerPrompt: "Project prompt.", ProviderFingerprint: strings.Repeat("d", 16),
		Artifacts: fixtureArtifactFactory(detail),
		Source: fakeSourceAccess{
			current: map[string]string{"kubernetes/test-infra": currentRevision},
			files: map[string]map[string]string{
				"kubernetes/test-infra@" + currentRevision: {detail.ConfigFile: "periodics: []\n"},
				"kubernetes/test-infra@" + configRevision:  {detail.ConfigFile: "periodics: []\n"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Input.InvestigationSource.Owner != "kubernetes" || resolved.Input.InvestigationSource.Name != "test-infra" {
		t.Fatalf("source=%+v", resolved.Input.InvestigationSource)
	}
	for _, build := range resolved.Input.Builds {
		if build.Source == nil || build.Source.Revision != configRevision {
			t.Fatalf("build=%+v", build)
		}
	}
	policy := resolved.Input.DestinationPolicy.Repositories[0]
	if policy.Repository != "kubernetes/test-infra" || !slicesEqual(policy.AllowedPaths, []string{"config/jobs/example/"}) {
		t.Fatalf("policy=%+v", policy)
	}
}

func TestPublishedResolverRejectsStaleAndInactiveSubjects(t *testing.T) {
	dataDir, cfg, ref, detail := publishedResolverFixture(t, false)
	resolver, err := NewPublishedResolver(PublishedResolverOptions{
		DataDir: dataDir, Config: cfg, ProviderFingerprint: strings.Repeat("d", 16),
		Artifacts: fixtureArtifactFactory(detail),
		Source:    fakeSourceAccess{current: map[string]string{"example/repo": strings.Repeat("e", 40)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := ref
	stale.PatternHash = strings.Repeat("f", 64)
	if err := resolver.Validate(t.Context(), stale); !errors.Is(err, ErrOperationStale) {
		t.Fatalf("stale err=%v", err)
	}
	detail.PatternAnalyses[0].Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleRecovered}
	models.AssignPatternIdentity(&detail.PatternAnalyses[0])
	writePublishedJob(t, dataDir, detail)
	ref.PatternHash = detail.PatternAnalyses[0].ContentHash
	if err := resolver.Validate(t.Context(), ref); !errors.Is(err, ErrOperationInactive) {
		t.Fatalf("inactive err=%v", err)
	}
}

func publishedResolverFixture(t *testing.T, prowConfig bool) (string, *project.Config, OperationRef, models.JobDetail) {
	t.Helper()
	failureRevision := strings.Repeat("a", 40)
	configRevision := strings.Repeat("b", 40)
	relevant := "controllers/reconcile.go"
	configFile := "config/jobs/example/periodics.yaml"
	if prowConfig {
		relevant = configFile
	}
	group := models.PatternCausalGroup{Builds: []string{"1", "2"}, RootCause: "reconcile is missing applyFix", Confidence: "high"}
	pattern := models.PatternAnalysis{
		Subject: "periodic-test", JobID: "periodic-test", GeneratedAt: "2026-08-12T00:00:00Z",
		BuildsAnalyzed: 2, Recurrence: models.PatternRecurrenceSharedCause,
		CausalGroups: []models.PatternCausalGroup{group}, Systemic: true, Confidence: "high",
		RelevantFiles: []string{relevant}, Summary: "recurring cause",
		Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleActive},
	}
	models.AssignPatternIdentity(&pattern)
	analysis := func(buildID string) models.TestCase {
		return models.TestCase{
			Name: "test", Status: "failed", FailureMessage: "failed",
			AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-08-12T00:00:0" + buildID + "Z", RootCause: "reconcile is missing applyFix",
				Severity: "High", RelevantFiles: []string{relevant},
				EvidenceCitations: []models.EvidenceCitation{{Path: "log.txt", LineStart: 1, LineEnd: 1, Quote: "reconcile missing applyFix"}},
			},
		}
	}
	runs := []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "1", JobName: "periodic-test", Result: "FAILURE", RepoRefs: map[string]string{"example/repo": failureRevision}}, TestCases: []models.TestCase{analysis("1")}},
		{BuildInfo: models.BuildInfo{BuildID: "2", JobName: "periodic-test", Result: "FAILURE", RepoRefs: map[string]string{"example/repo": failureRevision}}, TestCases: []models.TestCase{analysis("2")}},
	}
	detail := models.JobDetail{
		Name: "periodic-test", JobID: "periodic-test", JobType: models.JobTypePeriodic,
		ConfigFile: configFile, ConfigRevision: configRevision, Runs: runs,
		PatternAnalyses: []models.PatternAnalysis{pattern}, PatternRefresh: &models.PatternRefreshStatus{State: models.PatternRefreshCurrent},
	}
	fix := &project.FixPRs{
		Repo:         &project.SourceRepo{Owner: "example", Name: "repo"},
		AgentRuntime: &project.FixAgentRuntime{AllowedCommands: []project.FixAgentCommand{{Argv: []string{"go", "test", "./..."}, Timeout: "10m"}}},
	}
	if prowConfig {
		fix.AllowedRepositories = []project.FixRepository{{Owner: "kubernetes", Name: "test-infra", PathPrefixes: []string{"config/jobs/example/"}}}
	}
	cfg := &project.Config{ID: "test-project", Branding: project.Branding{SourceRepo: project.SourceRepo{Owner: "example", Name: "repo"}}, AI: &project.AI{FixPRs: fix}}
	dataDir := t.TempDir()
	writePublishedJob(t, dataDir, detail)
	group = pattern.CausalGroups[0]
	ref := OperationRef{JobID: detail.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash, CausalGroupID: group.ID, CausalGroupHash: group.ContentHash}
	return dataDir, cfg, ref, detail
}

func fixtureArtifactFactory(detail models.JobDetail) artifacts.Factory {
	browsers := map[string]artifacts.Browser{}
	for _, run := range detail.Runs {
		prefix := "logs/" + detail.Name + "/" + run.BuildID + "/"
		browsers[prefix] = fakeBrowser{files: map[string]string{"log.txt": "reconcile missing applyFix\n"}}
	}
	return fakeArtifactFactory{browsers: browsers}
}

func writePublishedJob(t *testing.T, dataDir string, detail models.JobDetail) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(detail.JobID)), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func slicesEqual(left, right []string) bool {
	return len(left) == len(right) && strings.Join(left, "\x00") == strings.Join(right, "\x00")
}
