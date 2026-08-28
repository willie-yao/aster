package analysischat

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
)

type resolvedFixTarget struct {
	ref      AnalysisRef
	build    models.BuildInfo
	testCase models.TestCase
}

type resolvedAnalysis struct {
	ref            AnalysisRef
	jobID          string
	buildPrefix    string
	build          models.BuildInfo
	testCase       models.TestCase
	patterns       []models.PatternAnalysis
	pattern        *models.PatternAnalysis
	evidenceBuilds []ArtifactBuild
	comparison     *CauseComparison
	fixTarget      *resolvedFixTarget
}

func (s *Service) resolve(ref AnalysisRef) (resolvedAnalysis, error) {
	ref, err := normalizeAnalysisRef(ref)
	if err != nil {
		return resolvedAnalysis{}, err
	}
	detail, err := s.loadJobDetail(ref.JobID)
	if err != nil {
		return resolvedAnalysis{}, err
	}
	return resolveFromDetail(ref, detail)
}

func (s *Service) loadJobDetail(jobID string) (models.JobDetail, error) {
	file, err := os.Open(filepath.Join(s.dataDir, "jobs", models.JobDataFilename(jobID)))
	if err != nil {
		if os.IsNotExist(err) {
			return models.JobDetail{}, ErrAnalysisNotFound
		}
		return models.JobDetail{}, fmt.Errorf("reading analysis job data: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJobDetailBytes+1))
	if err != nil {
		return models.JobDetail{}, fmt.Errorf("reading analysis job data: %w", err)
	}
	if len(data) > maxJobDetailBytes {
		return models.JobDetail{}, fmt.Errorf("analysis job data exceeds %d bytes", maxJobDetailBytes)
	}
	var detail models.JobDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return models.JobDetail{}, fmt.Errorf("decoding analysis job data: %w", err)
	}
	if detail.JobID != "" && detail.JobID != jobID {
		return models.JobDetail{}, ErrAnalysisNotFound
	}
	return detail, nil
}

func resolveFromDetail(ref AnalysisRef, detail models.JobDetail) (resolvedAnalysis, error) {
	switch ref.Scope {
	case ScopePattern:
		return resolvePatternAnalysis(ref, detail)
	case ScopeCause:
		return resolveCauseAnalysis(ref, detail)
	}

	var run *models.BuildResult
	for i := range detail.Runs {
		if detail.Runs[i].BuildID == ref.BuildID {
			run = &detail.Runs[i]
			break
		}
	}
	if run == nil {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}

	var matches []models.TestCase
	for _, testCase := range run.TestCases {
		testName := strings.TrimSpace(testCase.Name)
		source := strings.TrimSpace(testCase.Source)
		suiteName := strings.TrimSpace(testCase.SuiteName)
		className := strings.TrimSpace(testCase.ClassName)
		if testName != ref.TestName ||
			ref.Source == models.TestCaseSourceBuild && source != models.TestCaseSourceBuild ||
			ref.Source == "" && source == models.TestCaseSourceBuild ||
			ref.SuiteName != "" && suiteName != ref.SuiteName ||
			ref.ClassName != "" && className != ref.ClassName ||
			ref.JUnitFile != "" && testCase.JUnitFile != ref.JUnitFile {
			continue
		}
		if testCase.AIAnalysis != nil {
			matches = append(matches, testCase)
		}
	}
	if len(matches) == 0 {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	if len(matches) > 1 {
		return resolvedAnalysis{}, fmt.Errorf("%w: suite_name, class_name, or junit_file is required to disambiguate the test", ErrInvalidRequest)
	}
	testCase := cloneTestCase(matches[0])
	if ref.AnalysisGeneratedAt != "" && ref.AnalysisGeneratedAt != testCase.AIAnalysis.GeneratedAt {
		return resolvedAnalysis{}, ErrAnalysisChanged
	}
	ref.TestName = strings.TrimSpace(testCase.Name)
	ref.Source = strings.TrimSpace(testCase.Source)
	ref.SuiteName = strings.TrimSpace(testCase.SuiteName)
	ref.ClassName = strings.TrimSpace(testCase.ClassName)
	ref.JUnitFile = testCase.JUnitFile
	ref.AnalysisGeneratedAt = testCase.AIAnalysis.GeneratedAt

	artifactBuild, err := artifactBuildFor(detail, *run)
	if err != nil {
		return resolvedAnalysis{}, err
	}
	return resolvedAnalysis{
		ref:         ref,
		jobID:       ref.JobID,
		buildPrefix: artifactBuild.BuildPrefix,
		build:       cloneBuildInfo(run.BuildInfo),
		testCase:    testCase,
		patterns:    clonePatternAnalyses(detail.PatternAnalyses),
	}, nil
}

func resolvePatternAnalysis(ref AnalysisRef, detail models.JobDetail) (resolvedAnalysis, error) {
	var selected *models.PatternAnalysis
	for i := range detail.PatternAnalyses {
		pattern := &detail.PatternAnalyses[i]
		if pattern.ID == ref.PatternID {
			selected = pattern
			break
		}
	}
	if selected == nil || !selected.Systemic {
		return resolvedAnalysis{}, ErrPatternNotFound
	}
	if models.PatternHash(*selected) != ref.PatternHash {
		return resolvedAnalysis{}, ErrPatternChanged
	}
	if err := validatePatternChatBounds(*selected); err != nil {
		return resolvedAnalysis{}, err
	}
	selectedRuns, availableCount, eligibleCount := selectPatternEvidenceRuns(*selected, detail.Runs)
	if detail.PatternRefresh != nil && detail.PatternRefresh.State != models.PatternRefreshCurrent && availableCount != eligibleCount {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	builds := make([]ArtifactBuild, 0, len(selectedRuns))
	for _, run := range selectedRuns {
		build, err := artifactBuildFor(detail, run)
		if err != nil {
			return resolvedAnalysis{}, err
		}
		builds = append(builds, build)
	}
	if len(builds) == 0 {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	pattern := clonePatternAnalyses([]models.PatternAnalysis{*selected})[0]
	ref.PatternHash = models.PatternHash(pattern)
	severity := severityFromConfidence(pattern.Confidence)
	testCase := models.TestCase{
		Name: pattern.Subject,
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: pattern.GeneratedAt, RootCause: pattern.SharedRootCause, Severity: severity,
			SuggestedFix: pattern.SuggestedFix, RelevantFiles: slices.Clone(pattern.RelevantFiles),
		},
	}
	return resolvedAnalysis{
		ref: ref, jobID: ref.JobID, buildPrefix: builds[0].BuildPrefix,
		build: cloneBuildInfo(builds[0].Build), testCase: testCase,
		patterns: clonePatternAnalyses(detail.PatternAnalyses), pattern: &pattern,
		evidenceBuilds: cloneArtifactBuilds(builds),
	}, nil
}

func resolveCauseAnalysis(ref AnalysisRef, detail models.JobDetail) (resolvedAnalysis, error) {
	var selected *models.PatternAnalysis
	for i := range detail.PatternAnalyses {
		pattern := &detail.PatternAnalyses[i]
		if pattern.ID == ref.PatternID {
			selected = pattern
			break
		}
	}
	if selected == nil {
		return resolvedAnalysis{}, ErrPatternNotFound
	}
	if models.PatternHash(*selected) != ref.PatternHash {
		return resolvedAnalysis{}, ErrPatternChanged
	}
	var selectedGroup *models.PatternCausalGroup
	for i := range selected.CausalGroups {
		group := &selected.CausalGroups[i]
		if group.ID == ref.CausalGroupID {
			selectedGroup = group
			break
		}
	}
	if selectedGroup == nil {
		return resolvedAnalysis{}, ErrCauseNotFound
	}
	if models.PatternCausalGroupHash(*selectedGroup) != ref.CausalGroupHash {
		return resolvedAnalysis{}, ErrCauseChanged
	}
	if len(selectedGroup.Builds) == 0 || len(selectedGroup.Builds) > maxPatternChatBuildsPerGroup {
		return resolvedAnalysis{}, fmt.Errorf("%w: causal group has %d builds, maximum %d", ErrInvalidRequest, len(selectedGroup.Builds), maxPatternChatBuildsPerGroup)
	}

	selectedRuns, availableCount, eligibleCount := selectCauseEvidenceRuns(*selectedGroup, detail.Runs)
	if availableCount != eligibleCount {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	builds := make([]ArtifactBuild, 0, len(selectedRuns))
	for _, run := range selectedRuns {
		build, err := artifactBuildFor(detail, run)
		if err != nil {
			return resolvedAnalysis{}, err
		}
		builds = append(builds, build)
	}
	if len(builds) == 0 {
		return resolvedAnalysis{}, ErrAnalysisNotFound
	}
	comparison, err := causeComparisonFor(detail, *selectedGroup, selectedRuns)
	if err != nil {
		return resolvedAnalysis{}, err
	}

	pattern := clonePatternAnalyses([]models.PatternAnalysis{*selected})[0]
	var group models.PatternCausalGroup
	for i := range pattern.CausalGroups {
		if pattern.CausalGroups[i].ID == ref.CausalGroupID {
			group = pattern.CausalGroups[i]
			break
		}
	}
	ref.PatternHash = models.PatternHash(pattern)
	ref.CausalGroupHash = models.PatternCausalGroupHash(group)
	suggestedFix := ""
	if group.Remediation != nil {
		suggestedFix = group.Remediation.SuggestedFix
	}
	relevantFiles := []string(nil)
	if group.CauseLocation != nil {
		relevantFiles = slices.Clone(group.CauseLocation.Files)
	}
	causePattern := pattern
	causePattern.BuildsAnalyzed = len(group.Builds)
	causePattern.Confidence = group.Confidence
	causePattern.CausalGroups = []models.PatternCausalGroup{group}
	causePattern.UnclassifiedBuilds = nil
	causePattern.SharedRootCause = group.RootCause
	causePattern.SharedBuilds = slices.Clone(group.Builds)
	causePattern.SuggestedFix = suggestedFix
	causePattern.RelevantFiles = slices.Clone(relevantFiles)
	causePattern.RemediationTargets = nil
	causePattern.Lifecycle = models.CausalGroupLifecycle(detail, group.Builds)
	causePattern.Summary = group.RootCause
	var fixTarget *resolvedFixTarget
	if models.PatternIsActive(pattern) {
		fixTarget = selectCauseFixTarget(ref.JobID, group, detail.Runs)
	}
	testCase := models.TestCase{
		Name: pattern.Subject,
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: pattern.GeneratedAt, RootCause: group.RootCause,
			Severity: severityFromConfidence(group.Confidence), SuggestedFix: suggestedFix,
			RelevantFiles: slices.Clone(relevantFiles),
		},
	}
	return resolvedAnalysis{
		ref: ref, jobID: ref.JobID, buildPrefix: builds[0].BuildPrefix,
		build: cloneBuildInfo(builds[0].Build), testCase: testCase,
		patterns: clonePatternAnalyses(detail.PatternAnalyses), pattern: &causePattern,
		evidenceBuilds: cloneArtifactBuilds(builds), comparison: cloneCauseComparison(comparison), fixTarget: fixTarget,
	}, nil
}

func selectCauseFixTarget(jobID string, group models.PatternCausalGroup, runs []models.BuildResult) *resolvedFixTarget {
	affected := make(map[string]struct{}, len(group.Builds))
	for _, buildID := range group.Builds {
		if buildID = strings.TrimSpace(buildID); buildID != "" {
			affected[buildID] = struct{}{}
		}
	}
	selectRun := func(run models.BuildResult) *resolvedFixTarget {
		if _, ok := affected[run.BuildID]; !ok {
			return nil
		}
		testCase := representativeCauseFailure(run.TestCases)
		if testCase == nil || testCase.Source == models.TestCaseSourceBuild || strings.TrimSpace(testCase.JUnitFile) == "" || len(testCase.AIAnalysis.FileLinks) == 0 {
			return nil
		}
		for i := range run.TestCases {
			if run.TestCases[i].Name == testCase.Name {
				if &run.TestCases[i] != testCase {
					return nil
				}
				break
			}
		}
		return &resolvedFixTarget{
			ref: AnalysisRef{
				Scope: ScopeTest, JobID: jobID, BuildID: run.BuildID, TestName: testCase.Name,
				Source: testCase.Source, SuiteName: testCase.SuiteName, ClassName: testCase.ClassName,
				JUnitFile: testCase.JUnitFile, AnalysisGeneratedAt: testCase.AIAnalysis.GeneratedAt,
			},
			build: cloneBuildInfo(run.BuildInfo), testCase: cloneTestCase(*testCase),
		}
	}
	preferred := ""
	if group.Remediation != nil {
		preferred = strings.TrimSpace(group.Remediation.BuildID)
	}
	if preferred != "" {
		for _, run := range runs {
			if run.BuildID == preferred {
				if target := selectRun(run); target != nil {
					return target
				}
				break
			}
		}
	}
	for _, run := range runs {
		if run.BuildID == preferred {
			continue
		}
		if target := selectRun(run); target != nil {
			return target
		}
	}
	return nil
}

func representativeCauseFailure(testCases []models.TestCase) *models.TestCase {
	var representative *models.TestCase
	for i := range testCases {
		testCase := &testCases[i]
		if testCase.Status != "failed" || !models.AnalysisHasUsableDiagnosis(testCase.AIAnalysis) {
			continue
		}
		if representative == nil || models.SeverityRank(testCase.AIAnalysis.Severity) > models.SeverityRank(representative.AIAnalysis.Severity) {
			representative = testCase
		}
	}
	return representative
}

func selectCauseEvidenceRuns(group models.PatternCausalGroup, runs []models.BuildResult) ([]models.BuildResult, int, int) {
	eligible := make(map[string]struct{}, len(group.Builds))
	for _, buildID := range group.Builds {
		if buildID = strings.TrimSpace(buildID); buildID != "" {
			eligible[buildID] = struct{}{}
		}
	}
	matchingRuns := make([]models.BuildResult, 0, len(eligible))
	seenRuns := make(map[string]struct{}, len(eligible))
	for _, run := range runs {
		if _, ok := eligible[run.BuildID]; !ok {
			continue
		}
		if _, duplicate := seenRuns[run.BuildID]; duplicate {
			continue
		}
		seenRuns[run.BuildID] = struct{}{}
		matchingRuns = append(matchingRuns, run)
	}
	sortPatternEvidenceRuns(matchingRuns)
	return matchingRuns, len(matchingRuns), len(eligible)
}

func causeComparisonFor(detail models.JobDetail, group models.PatternCausalGroup, memberRuns []models.BuildResult) (*CauseComparison, error) {
	run := selectCauseComparisonRun(group, detail.Runs)
	if run == nil {
		return nil, nil
	}
	build, err := artifactBuildFor(detail, *run)
	if err != nil {
		return nil, err
	}
	return &CauseComparison{ArtifactBuild: build, TestNames: representativeCauseTestNames(memberRuns)}, nil
}

func selectCauseComparisonRun(group models.PatternCausalGroup, runs []models.BuildResult) *models.BuildResult {
	members := make(map[string]struct{}, len(group.Builds))
	for _, buildID := range group.Builds {
		if buildID = strings.TrimSpace(buildID); buildID != "" {
			members[buildID] = struct{}{}
		}
	}
	ordered := slices.Clone(runs)
	sortPatternEvidenceRuns(ordered)
	var candidate *models.BuildResult
	for index := range ordered {
		run := &ordered[index]
		if _, member := members[run.BuildID]; member {
			return candidate
		}
		if candidate == nil && run.Result != "PENDING" {
			candidate = run
		}
	}
	return nil
}

func representativeCauseTestNames(runs []models.BuildResult) []string {
	names := make([]string, 0, len(runs))
	seen := make(map[string]struct{}, len(runs))
	for index := range runs {
		failure := representativeCauseFailure(runs[index].TestCases)
		if failure == nil {
			continue
		}
		name := strings.TrimSpace(failure.Name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func severityFromConfidence(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high":
		return "High"
	case "medium":
		return "Medium"
	case "low":
		return "Low"
	default:
		return "Unknown"
	}
}

func validatePatternChatBounds(pattern models.PatternAnalysis) error {
	if len(pattern.CausalGroups) > maxPatternChatCausalGroups {
		return fmt.Errorf("%w: pattern has %d causal groups, maximum %d", ErrInvalidRequest, len(pattern.CausalGroups), maxPatternChatCausalGroups)
	}
	for _, group := range pattern.CausalGroups {
		if len(group.Builds) > maxPatternChatBuildsPerGroup {
			return fmt.Errorf("%w: causal group %q has %d builds, maximum %d", ErrInvalidRequest, group.ID, len(group.Builds), maxPatternChatBuildsPerGroup)
		}
	}
	if len(pattern.UnclassifiedBuilds) > maxPatternChatUnclassifiedBuilds {
		return fmt.Errorf("%w: pattern has %d unclassified builds, maximum %d", ErrInvalidRequest, len(pattern.UnclassifiedBuilds), maxPatternChatUnclassifiedBuilds)
	}
	return nil
}

func selectPatternEvidenceRuns(pattern models.PatternAnalysis, runs []models.BuildResult) ([]models.BuildResult, int, int) {
	repeatedGroups := make([]models.PatternCausalGroup, 0, len(pattern.CausalGroups))
	eligible := map[string]struct{}{}
	for _, group := range pattern.CausalGroups {
		if len(group.Builds) < 2 {
			continue
		}
		repeatedGroups = append(repeatedGroups, group)
		for _, buildID := range group.Builds {
			if buildID = strings.TrimSpace(buildID); buildID != "" {
				eligible[buildID] = struct{}{}
			}
		}
	}
	if len(repeatedGroups) == 0 {
		for _, buildID := range pattern.SharedBuilds {
			if buildID = strings.TrimSpace(buildID); buildID != "" {
				eligible[buildID] = struct{}{}
			}
		}
	}

	matchingRuns := make([]models.BuildResult, 0, len(eligible))
	seenRuns := map[string]struct{}{}
	for _, run := range runs {
		if _, ok := eligible[run.BuildID]; !ok {
			continue
		}
		if _, duplicate := seenRuns[run.BuildID]; duplicate {
			continue
		}
		seenRuns[run.BuildID] = struct{}{}
		matchingRuns = append(matchingRuns, run)
	}
	sortPatternEvidenceRuns(matchingRuns)

	fillEligible := eligible
	if len(repeatedGroups) > 0 && len(pattern.SharedBuilds) > 0 {
		fillEligible = make(map[string]struct{}, len(pattern.SharedBuilds))
		for _, buildID := range pattern.SharedBuilds {
			buildID = strings.TrimSpace(buildID)
			if _, ok := eligible[buildID]; ok {
				fillEligible[buildID] = struct{}{}
			}
		}
	}
	selected := make([]models.BuildResult, 0, maxPatternEvidenceBuilds)
	selectedIDs := map[string]struct{}{}
	for _, group := range repeatedGroups {
		groupBuilds := make(map[string]struct{}, len(group.Builds))
		for _, buildID := range group.Builds {
			groupBuilds[strings.TrimSpace(buildID)] = struct{}{}
		}
		for _, run := range matchingRuns {
			if _, ok := groupBuilds[run.BuildID]; !ok {
				continue
			}
			if _, exists := selectedIDs[run.BuildID]; exists {
				continue
			}
			selected = append(selected, run)
			selectedIDs[run.BuildID] = struct{}{}
			break
		}
		if len(selected) == maxPatternEvidenceBuilds {
			return selected, len(matchingRuns), len(eligible)
		}
	}
	for _, run := range matchingRuns {
		if _, exists := selectedIDs[run.BuildID]; exists {
			continue
		}
		if _, ok := fillEligible[run.BuildID]; !ok {
			continue
		}
		selected = append(selected, run)
		if len(selected) == maxPatternEvidenceBuilds {
			break
		}
	}
	return selected, len(matchingRuns), len(eligible)
}

func sortPatternEvidenceRuns(runs []models.BuildResult) {
	slices.SortStableFunc(runs, func(left, right models.BuildResult) int {
		if !left.Started.Equal(right.Started) {
			if left.Started.After(right.Started) {
				return -1
			}
			return 1
		}
		leftID, leftErr := strconv.ParseUint(left.BuildID, 10, 64)
		rightID, rightErr := strconv.ParseUint(right.BuildID, 10, 64)
		if leftErr == nil && rightErr == nil && leftID != rightID {
			if leftID > rightID {
				return -1
			}
			return 1
		}
		return strings.Compare(right.BuildID, left.BuildID)
	})
}

func artifactBuildFor(detail models.JobDetail, run models.BuildResult) (ArtifactBuild, error) {
	jobLocation := prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo}
	if detail.JobType != models.JobTypePeriodic && detail.JobType != models.JobTypePresubmit {
		return ArtifactBuild{}, fmt.Errorf("%w: unsupported job type %q", ErrInvalidRequest, detail.JobType)
	}
	if detail.JobType == models.JobTypePresubmit && (detail.Repo == "" || run.PullNumber == "") {
		return ArtifactBuild{}, fmt.Errorf("%w: presubmit build identity is incomplete", ErrInvalidRequest)
	}
	prefix := (prowbuild.BuildLocation{
		JobLocation: jobLocation, JobName: detail.Name, BuildID: run.BuildID, PullNumber: run.PullNumber,
	}).BuildPath()
	return ArtifactBuild{BuildPrefix: prefix, Build: cloneBuildInfo(run.BuildInfo)}, nil
}

func cloneArtifactBuilds(builds []ArtifactBuild) []ArtifactBuild {
	out := slices.Clone(builds)
	for i := range out {
		out[i].Build = cloneBuildInfo(out[i].Build)
	}
	return out
}

func cloneCauseComparison(comparison *CauseComparison) *CauseComparison {
	if comparison == nil {
		return nil
	}
	clone := *comparison
	clone.ArtifactBuild.Build = cloneBuildInfo(comparison.ArtifactBuild.Build)
	clone.TestNames = slices.Clone(comparison.TestNames)
	return &clone
}

func causeComparisonBuildID(comparison *CauseComparison) string {
	if comparison == nil {
		return ""
	}
	return strings.TrimSpace(comparison.ArtifactBuild.Build.BuildID)
}

func clonePatternAnalyses(patterns []models.PatternAnalysis) []models.PatternAnalysis {
	out := slices.Clone(patterns)
	for i := range out {
		out[i].SharedBuilds = slices.Clone(patterns[i].SharedBuilds)
		out[i].CausalGroups = slices.Clone(patterns[i].CausalGroups)
		for groupIndex := range out[i].CausalGroups {
			out[i].CausalGroups[groupIndex].Builds = slices.Clone(patterns[i].CausalGroups[groupIndex].Builds)
			out[i].CausalGroups[groupIndex].CauseLocation = patterns[i].CausalGroups[groupIndex].CauseLocation.Clone()
			if patterns[i].CausalGroups[groupIndex].Remediation != nil {
				remediation := *patterns[i].CausalGroups[groupIndex].Remediation
				out[i].CausalGroups[groupIndex].Remediation = &remediation
			}
		}
		out[i].UnclassifiedBuilds = slices.Clone(patterns[i].UnclassifiedBuilds)
		out[i].RemediationTargets = slices.Clone(patterns[i].RemediationTargets)
		out[i].RelevantFiles = slices.Clone(patterns[i].RelevantFiles)
		out[i].FileLinks = maps.Clone(patterns[i].FileLinks)
		if patterns[i].Lifecycle != nil {
			lifecycle := *patterns[i].Lifecycle
			lifecycle.PassingBuilds = slices.Clone(patterns[i].Lifecycle.PassingBuilds)
			lifecycle.RecoveryBuilds = slices.Clone(patterns[i].Lifecycle.RecoveryBuilds)
			out[i].Lifecycle = &lifecycle
		}
	}
	return out
}
