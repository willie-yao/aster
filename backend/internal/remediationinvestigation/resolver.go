package remediationinvestigation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const maxPublishedJobDetailBytes = 64 << 20

// SourceAccess resolves current repository commits and reads arbitrary immutable revisions.
type SourceAccess interface {
	sourceinvestigation.TreeReader
	Current(context.Context, string, string) (sourceinvestigation.Repository, error)
}

// PublishedResolverOptions bind published dashboard data to private investigation provenance.
type PublishedResolverOptions struct {
	DataDir             string
	Config              *project.Config
	ConsumerPrompt      string
	SkillHash           string
	ProviderFingerprint string
	Artifacts           artifacts.Factory
	Source              SourceAccess
}

// PublishedResolver constructs frozen investigation inputs from current published job data.
type PublishedResolver struct {
	dataDir             string
	config              *project.Config
	consumerPrompt      string
	skillHash           string
	providerFingerprint string
	artifacts           artifacts.Factory
	source              SourceAccess
}

func NewPublishedResolver(options PublishedResolverOptions) (*PublishedResolver, error) {
	if strings.TrimSpace(options.DataDir) == "" || options.Config == nil || options.Artifacts == nil || options.Source == nil || strings.TrimSpace(options.ProviderFingerprint) == "" {
		return nil, fmt.Errorf("published remediation investigation resolver dependencies are required")
	}
	return &PublishedResolver{
		dataDir: options.DataDir, config: options.Config, consumerPrompt: options.ConsumerPrompt,
		skillHash: options.SkillHash, providerFingerprint: options.ProviderFingerprint,
		artifacts: options.Artifacts, source: options.Source,
	}, nil
}

func (r *PublishedResolver) RefreshActive() (bool, error) {
	status, err := fetchprogress.Read(fetchprogress.Path(r.dataDir))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status.Outcome == fetchprogress.OutcomeRunning, nil
}

func (r *PublishedResolver) Validate(ctx context.Context, ref OperationRef) error {
	_, err := r.loadPublished(ctx, ref)
	return err
}

func (r *PublishedResolver) Resolve(ctx context.Context, ref OperationRef) (ResolvedOperation, error) {
	published, err := r.loadPublished(ctx, ref)
	if err != nil {
		return ResolvedOperation{}, err
	}
	analysisRepo := r.config.EffectiveAnalysisSourceRepo()
	selectedRepo, policy, configOwned, err := r.selectRepository(published.detail, analysisRepo, published.relevantFiles)
	if err != nil {
		return ResolvedOperation{}, err
	}
	current, err := r.source.Current(ctx, selectedRepo.Owner, selectedRepo.Name)
	if err != nil {
		return ResolvedOperation{}, fmt.Errorf("%w: current source revision unavailable", ErrOperationUnavailable)
	}

	buildRefs := make([]BuildReference, 0, len(published.runs))
	analysisRefs := make([]AnalysisReference, 0, len(published.runs))
	artifactBuilds := make([]analysischat.ArtifactBuild, 0, len(published.runs))
	for _, item := range published.runs {
		failureSource, err := r.failureSource(published.detail, item.run, selectedRepo, configOwned)
		if err != nil {
			return ResolvedOperation{}, err
		}
		buildPrefix, err := buildPrefixFor(published.detail, item.run)
		if err != nil {
			return ResolvedOperation{}, err
		}
		analysis := item.testCase.AIAnalysis
		analysisRefs = append(analysisRefs, AnalysisReference{
			BuildID: item.run.BuildID, TestName: item.testCase.Name, GeneratedAt: analysis.GeneratedAt,
			RootCause: analysis.RootCause, Severity: analysis.Severity,
			RelevantFiles: slices.Clone(analysis.RelevantFiles), Evidence: slices.Clone(analysis.EvidenceCitations),
			SourceRepository: &failureSource,
		})
		buildRefs = append(buildRefs, BuildReference{
			BuildID: item.run.BuildID, BuildPrefix: buildPrefix, ProwURL: item.run.ProwURL,
			WebURL: item.run.WebURL, Source: &failureSource,
		})
		artifactBuilds = append(artifactBuilds, analysischat.ArtifactBuild{BuildPrefix: buildPrefix, Build: item.run.BuildInfo})
	}

	input := FrozenInput{
		PatternID: published.pattern.ID, PatternHash: published.pattern.ContentHash,
		CausalGroupID: published.group.ID, CausalGroupHash: published.group.ContentHash,
		JobID: published.detail.JobID, JobName: published.detail.Name,
		Recurrence: published.pattern.Recurrence, Group: published.group,
		Builds: buildRefs, Analyses: analysisRefs, RelevantFiles: slices.Clone(published.relevantFiles),
		InvestigationSource: current, DestinationPolicy: policy,
		ConsumerPrompt: r.consumerPrompt, ConsumerPromptHash: HashText(r.consumerPrompt),
		SkillHash: r.skillHash, ProviderFingerprint: r.providerFingerprint, Versions: CurrentVersions(),
	}
	if err := ValidateFrozenInput(input); err != nil {
		return ResolvedOperation{}, fmt.Errorf("%w: published evidence could not be frozen", ErrOperationUnavailable)
	}
	return ResolvedOperation{
		Input: input, Source: r.source,
		Browser: analysisruntime.NewPatternBrowser(r.artifacts, artifactBuilds),
	}, nil
}

type publishedRun struct {
	run      models.BuildResult
	testCase models.TestCase
}

type publishedSubject struct {
	detail        models.JobDetail
	pattern       models.PatternAnalysis
	group         models.PatternCausalGroup
	runs          []publishedRun
	relevantFiles []string
}

func (r *PublishedResolver) loadPublished(_ context.Context, ref OperationRef) (publishedSubject, error) {
	file, err := os.Open(filepath.Join(r.dataDir, "jobs", models.JobDataFilename(ref.JobID)))
	if err != nil {
		if os.IsNotExist(err) {
			return publishedSubject{}, ErrOperationNotFound
		}
		return publishedSubject{}, fmt.Errorf("%w: published job data unavailable", ErrOperationUnavailable)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPublishedJobDetailBytes+1))
	if err != nil || len(data) > maxPublishedJobDetailBytes {
		return publishedSubject{}, fmt.Errorf("%w: published job data unavailable", ErrOperationUnavailable)
	}
	var detail models.JobDetail
	if json.Unmarshal(data, &detail) != nil || detail.JobID != ref.JobID {
		return publishedSubject{}, ErrOperationNotFound
	}
	if detail.PatternRefresh != nil && detail.PatternRefresh.State != models.PatternRefreshCurrent {
		return publishedSubject{}, ErrOperationStale
	}
	patternsWithIDs, _ := models.BackfillPatternIdentities(detail.PatternAnalyses)
	var pattern *models.PatternAnalysis
	for index := range patternsWithIDs {
		if patternsWithIDs[index].ID == ref.PatternID {
			pattern = &patternsWithIDs[index]
			break
		}
	}
	if pattern == nil || !pattern.Systemic {
		return publishedSubject{}, ErrOperationNotFound
	}
	if pattern.ContentHash != ref.PatternHash || models.PatternHash(*pattern) != ref.PatternHash {
		return publishedSubject{}, ErrOperationStale
	}
	if !models.PatternIsActive(*pattern) {
		return publishedSubject{}, ErrOperationInactive
	}
	if models.PatternAllowsActions(*pattern) || pattern.Recurrence != models.PatternRecurrenceSharedCause && pattern.Recurrence != models.PatternRecurrenceMixedCauses {
		return publishedSubject{}, ErrOperationInvalid
	}
	var group *models.PatternCausalGroup
	for index := range pattern.CausalGroups {
		if pattern.CausalGroups[index].ID == ref.CausalGroupID {
			group = &pattern.CausalGroups[index]
			break
		}
	}
	if group == nil || group.ContentHash != ref.CausalGroupHash || models.PatternCausalGroupHash(*group) != ref.CausalGroupHash || len(group.Builds) < 2 {
		return publishedSubject{}, ErrOperationStale
	}

	runsByID := make(map[string]models.BuildResult, len(detail.Runs))
	for _, run := range detail.Runs {
		runsByID[run.BuildID] = run
	}
	seenBuilds := map[string]bool{}
	runs := make([]publishedRun, 0, len(group.Builds))
	relevant := slices.Clone(pattern.RelevantFiles)
	for _, buildID := range group.Builds {
		if strings.TrimSpace(buildID) == "" || seenBuilds[buildID] {
			return publishedSubject{}, ErrOperationStale
		}
		seenBuilds[buildID] = true
		run, ok := runsByID[buildID]
		if !ok {
			return publishedSubject{}, ErrOperationStale
		}
		representative := patterns.RepresentativeAnalyzedFailure(&run)
		if representative == nil || representative.AIAnalysis == nil {
			return publishedSubject{}, ErrOperationUnavailable
		}
		runs = append(runs, publishedRun{run: run, testCase: *representative})
		relevant = append(relevant, representative.AIAnalysis.RelevantFiles...)
	}
	relevant, err = canonicalRelevantFiles(relevant)
	if err != nil || len(relevant) == 0 {
		return publishedSubject{}, fmt.Errorf("%w: no bounded destination paths are available", ErrOperationUnavailable)
	}
	return publishedSubject{detail: detail, pattern: *pattern, group: *group, runs: runs, relevantFiles: relevant}, nil
}

func (r *PublishedResolver) selectRepository(detail models.JobDetail, analysisRepo project.SourceRepo, relevant []string) (project.SourceRepo, DestinationPolicy, bool, error) {
	fix := r.config.EffectiveFixPRs()
	if fix.Repo == nil {
		return project.SourceRepo{}, DestinationPolicy{}, false, ErrOperationUnavailable
	}
	selected := project.SourceRepo{Owner: analysisRepo.Owner, Name: analysisRepo.Name}
	configOwned := false
	configPath := strings.TrimSpace(detail.ConfigFile)
	if configPath != "" && slices.Contains(relevant, configPath) {
		for _, allowed := range fix.AllowedRepositories {
			if strings.EqualFold(allowed.Owner, "kubernetes") && strings.EqualFold(allowed.Name, "test-infra") && pathAllowedByPolicy(configPath, allowed.PathPrefixes) {
				selected = project.SourceRepo{Owner: allowed.Owner, Name: allowed.Name}
				configOwned = true
				break
			}
		}
	}

	policy := RepositoryPolicy{Repository: selected.Owner + "/" + selected.Name}
	defaultRepo := strings.EqualFold(selected.Owner, fix.Repo.Owner) && strings.EqualFold(selected.Name, fix.Repo.Name)
	if defaultRepo {
		policy.AllowedPaths = slices.Clone(relevant)
		policy.AllowedCommands = validationCommands(fix.AgentRuntime.AllowedCommands)
	} else {
		matched := false
		for _, allowed := range fix.AllowedRepositories {
			if !strings.EqualFold(selected.Owner, allowed.Owner) || !strings.EqualFold(selected.Name, allowed.Name) {
				continue
			}
			policy.AllowedPaths = slices.Clone(allowed.PathPrefixes)
			policy.AllowedCommands = validationCommands(allowed.AllowedCommands)
			matched = true
			break
		}
		if !matched {
			return project.SourceRepo{}, DestinationPolicy{}, false, fmt.Errorf("%w: investigation source is not an allowed destination", ErrOperationUnavailable)
		}
	}
	if len(policy.AllowedPaths) == 0 {
		return project.SourceRepo{}, DestinationPolicy{}, false, ErrOperationUnavailable
	}
	return selected, DestinationPolicy{Project: r.config.ID, Repositories: []RepositoryPolicy{policy}}, configOwned, nil
}

func (r *PublishedResolver) failureSource(detail models.JobDetail, run models.BuildResult, selected project.SourceRepo, configOwned bool) (sourceinvestigation.Repository, error) {
	if configOwned {
		repository := sourceinvestigation.Repository{Owner: selected.Owner, Name: selected.Name, Revision: strings.ToLower(strings.TrimSpace(detail.ConfigRevision))}
		if sourceinvestigation.ValidateRepository(repository) != nil {
			return sourceinvestigation.Repository{}, fmt.Errorf("%w: frozen Prow config revision unavailable", ErrOperationUnavailable)
		}
		return repository, nil
	}
	source, ok := ai.ResolveBuildSource(run.BuildInfo, selected.Owner, selected.Name)
	if !ok {
		return sourceinvestigation.Repository{}, fmt.Errorf("%w: build %s source revision unavailable", ErrOperationUnavailable, run.BuildID)
	}
	return sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}, nil
}

func buildPrefixFor(detail models.JobDetail, run models.BuildResult) (string, error) {
	if detail.JobType != models.JobTypePeriodic && detail.JobType != models.JobTypePresubmit {
		return "", ErrOperationUnavailable
	}
	if detail.JobType == models.JobTypePresubmit && (detail.Repo == "" || run.PullNumber == "") {
		return "", ErrOperationUnavailable
	}
	return (prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo},
		JobName:     detail.Name, BuildID: run.BuildID, PullNumber: run.PullNumber,
	}).BuildPath(), nil
}

func canonicalRelevantFiles(values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		clean, err := artifacts.SafePath(value)
		if err != nil || clean == "" || clean != value {
			return nil, ErrOperationUnavailable
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	slices.Sort(out)
	return out, nil
}

func validationCommands(commands []project.FixAgentCommand) []ValidationCommand {
	out := make([]ValidationCommand, 0, len(commands))
	for _, command := range commands {
		out = append(out, ValidationCommand{Argv: slices.Clone(command.Argv), Timeout: command.Timeout})
	}
	return out
}

// GitHubSourceAccess uses existing bounded GitHub source readers for exact revisions.
type GitHubSourceAccess struct {
	token string
}

func NewGitHubSourceAccess(token string) *GitHubSourceAccess {
	return &GitHubSourceAccess{token: token}
}

func (g *GitHubSourceAccess) Current(ctx context.Context, owner, name string) (sourceinvestigation.Repository, error) {
	reader := ai.NewGitHubRepoReader(owner, name, "", g.token)
	resolver, ok := reader.(interface {
		ResolveRef(context.Context) error
		SourceIdentity() (string, string, string)
	})
	if !ok || resolver.ResolveRef(ctx) != nil {
		return sourceinvestigation.Repository{}, ErrOperationUnavailable
	}
	resolvedOwner, resolvedName, revision := resolver.SourceIdentity()
	repository := sourceinvestigation.Repository{Owner: resolvedOwner, Name: resolvedName, Revision: revision}
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return sourceinvestigation.Repository{}, err
	}
	return repository, nil
}

func (g *GitHubSourceAccess) ListFiles(ctx context.Context, repository sourceinvestigation.Repository) ([]string, error) {
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return nil, err
	}
	return ai.NewGitHubRepoReader(repository.Owner, repository.Name, repository.Revision, g.token).ListTree(ctx)
}

func (g *GitHubSourceAccess) ReadFile(ctx context.Context, repository sourceinvestigation.Repository, path string) (string, error) {
	if err := sourceinvestigation.ValidateRepository(repository); err != nil {
		return "", err
	}
	content, found, err := ai.NewGitHubRepoReader(repository.Owner, repository.Name, repository.Revision, g.token).ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	if !found {
		return "", os.ErrNotExist
	}
	return content, nil
}
