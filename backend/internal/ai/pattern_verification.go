package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/buildsource"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// VerifyPatternRemediation checks structured pattern targets at the current
// pinned revision and, when bounded verification is supported, at every
// correlated failure revision.
func (s *Service) VerifyPatternRemediation(ctx context.Context, pattern models.PatternAnalysis, detail models.JobDetail) (models.PatternRemediationVerification, error) {
	if len(pattern.RemediationTargets) == 0 {
		return patternVerification(actionverify.StateInconclusive, "The recurring pattern has no structured remediation target.", "", ""), nil
	}
	for _, target := range pattern.RemediationTargets {
		if reason := actionverify.PatternTargetReason(target); reason != "" {
			return patternVerification(actionverify.StateInconclusive, reason, "", ""), nil
		}
	}
	reader, repository, revision, err := s.patternRemediationReader(pattern)
	if err != nil {
		return models.PatternRemediationVerification{}, err
	}
	result, err := actionverify.Verify(ctx, reader, actionverify.Input{
		Proposal: pattern.SuggestedFix, RelevantFiles: pattern.RelevantFiles, Targets: pattern.RemediationTargets,
	})
	if err != nil {
		return models.PatternRemediationVerification{}, err
	}
	verification := patternVerification(result.State, result.Reason, repository, revision)
	if verification.State == models.PatternRemediationAlreadyPresent {
		s.verifyPatternFailureRevisions(ctx, pattern, detail, &verification)
	}
	return verification, nil
}

func (s *Service) verifyPatternFailureRevisions(ctx context.Context, pattern models.PatternAnalysis, detail models.JobDetail, verification *models.PatternRemediationVerification) {
	verification.FailureState = models.PatternRemediationInconclusive
	if _, ok := buildsource.NormalizeRevision(verification.Revision); verification.Repository == "" || !ok || !historicalPatternTargetsSupported(pattern.RemediationTargets) {
		return
	}
	owner, name, ok := strings.Cut(verification.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return
	}
	runs := make(map[string]models.BuildResult, len(detail.Runs))
	for _, run := range detail.Runs {
		runs[run.BuildID] = run
	}
	states := map[string]models.PatternRemediationState{}
	verifyRevision := func(revision string) (models.PatternRemediationState, bool) {
		if state, found := states[revision]; found {
			return state, true
		}
		reader := NewGitHubRepoReader(owner, name, revision, s.githubReadToken)
		result, err := actionverify.Verify(ctx, &targetArchiveReader{reader: reader, targets: pattern.RemediationTargets}, actionverify.Input{
			Proposal: pattern.SuggestedFix, RelevantFiles: pattern.RelevantFiles, Targets: pattern.RemediationTargets,
		})
		if err != nil {
			return models.PatternRemediationInconclusive, false
		}
		state := patternVerification(result.State, result.Reason, verification.Repository, revision).State
		states[revision] = state
		return state, true
	}

	latestFailure := time.Time{}
	for _, buildID := range pattern.SharedBuilds {
		run, found := runs[buildID]
		if !found {
			return
		}
		source, found := s.resolvePatternBuildSource(run.BuildInfo, owner, name)
		if !found {
			return
		}
		state, verified := verifyRevision(source.Revision)
		if !verified {
			return
		}
		if state != models.PatternRemediationUnresolved {
			verification.FailureState = state
			verification.FailureBuilds = nil
			return
		}
		verification.FailureBuilds = append(verification.FailureBuilds, buildID)
		if run.Started.After(latestFailure) {
			latestFailure = run.Started
		}
	}
	if len(verification.FailureBuilds) != len(pattern.SharedBuilds) || len(pattern.SharedBuilds) == 0 {
		return
	}
	verification.FailureState = models.PatternRemediationUnresolved

	for _, run := range detail.Runs {
		if !run.Passed || !run.Started.After(latestFailure) {
			continue
		}
		source, found := s.resolvePatternBuildSource(run.BuildInfo, owner, name)
		if !found {
			continue
		}
		state, verified := verifyRevision(source.Revision)
		if verified && state == models.PatternRemediationAlreadyPresent {
			verification.PassingBuilds = append(verification.PassingBuilds, run.BuildID)
		}
	}
}

func (s *Service) resolvePatternBuildSource(build models.BuildInfo, owner, name string) (BuildSource, bool) {
	repository := strings.ToLower(strings.TrimSpace(owner + "/" + name))
	configured := strings.ToLower(strings.TrimSpace(s.sourceRepoOwner + "/" + s.sourceRepoName))
	if repository == configured {
		return ResolveBuildSource(build, owner, name)
	}
	build.Commit = ""
	build.RepoVersion = ""
	return ResolveBuildSource(build, owner, name)
}

func historicalPatternTargetsSupported(targets []models.RemediationTarget) bool {
	for _, target := range targets {
		switch target.Intent {
		case models.RemediationIntentModifySymbol:
			if _, _, ok := actionverify.RequiredCallParts(target.RequiredCall); !ok {
				return false
			}
		case models.RemediationIntentSetConfiguration, models.RemediationIntentRemoveConfiguration, models.RemediationIntentSetJobEnvironment:
		default:
			return false
		}
	}
	return len(targets) > 0
}

type targetArchiveReader struct {
	reader interface {
		ReadFile(context.Context, string) (string, bool, error)
	}
	targets []models.RemediationTarget
}

func (r *targetArchiveReader) ReadSourceArchive(ctx context.Context) (actionverify.Archive, error) {
	return actionverify.BuildTargetArchive(ctx, r, r.targets)
}

func (r *targetArchiveReader) ListFiles(ctx context.Context) ([]string, error) {
	tree, ok := r.reader.(interface {
		ListTree(context.Context) ([]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("pinned source tree is unavailable")
	}
	return tree.ListTree(ctx)
}

func (r *targetArchiveReader) ReadFile(ctx context.Context, path string) (string, bool, error) {
	return r.reader.ReadFile(ctx, path)
}

func (s *Service) patternRemediationReader(pattern models.PatternAnalysis) (actionverify.Reader, string, string, error) {
	repository, revision := "", ""
	explicit, implicit := false, false
	for _, target := range pattern.RemediationTargets {
		if target.Repository == "" {
			if explicit {
				return nil, "", "", fmt.Errorf("pattern targets mix default and explicit repositories")
			}
			implicit = true
			continue
		}
		if implicit {
			return nil, "", "", fmt.Errorf("pattern targets mix default and explicit repositories")
		}
		explicit = true
		if repository == "" {
			repository, revision = target.Repository, target.Revision
			continue
		}
		if target.Repository != repository || target.Revision != revision {
			return nil, "", "", fmt.Errorf("pattern targets use different repositories or revisions")
		}
	}
	if !explicit {
		reader, ok := s.patternRepo.(actionverify.Reader)
		if !ok {
			return nil, "", "", fmt.Errorf("pattern source archive reader is unavailable")
		}
		if identity, ok := s.patternRepo.(interface {
			SourceIdentity() (string, string, string)
		}); ok {
			owner, name, ref := identity.SourceIdentity()
			return reader, owner + "/" + name, ref, nil
		}
		if sourceRepository, sourceRevision, ok := strings.Cut(pattern.SourceRef, "@"); ok {
			return reader, sourceRepository, sourceRevision, nil
		}
		return nil, "", "", fmt.Errorf("pattern source identity is unavailable")
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, "", "", fmt.Errorf("pattern target repository is invalid")
	}
	reader, ok := NewGitHubRepoReader(owner, name, revision, s.githubReadToken).(actionverify.Reader)
	if !ok {
		return nil, "", "", fmt.Errorf("pattern target archive reader is unavailable")
	}
	return reader, repository, revision, nil
}

func patternVerification(state, reason, repository, revision string) models.PatternRemediationVerification {
	verification := models.PatternRemediationVerification{
		Reason: strings.TrimSpace(reason), Repository: strings.TrimSpace(repository), Revision: strings.ToLower(strings.TrimSpace(revision)),
	}
	switch state {
	case actionverify.StateAlreadyPresent:
		verification.State = models.PatternRemediationAlreadyPresent
	case actionverify.StateUnresolved:
		verification.State = models.PatternRemediationUnresolved
	default:
		verification.State = models.PatternRemediationInconclusive
	}
	return verification
}
