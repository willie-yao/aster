package fetcher

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

type recordingPreparedCauseRunner struct {
	calls int
	turns []analysischat.Turn
	reply analysischat.Reply
	err   error
}

func (r *recordingPreparedCauseRunner) Reply(_ context.Context, turn analysischat.Turn) (analysischat.Reply, error) {
	r.calls++
	r.turns = append(r.turns, turn)
	return r.reply, r.err
}

func installPreparedCauseRunner(t *testing.T, runner preparedCauseRunner) {
	t.Helper()
	previous := newPreparedCauseRunner
	previousFingerprint := preparedCauseRuntimeFingerprint
	newPreparedCauseRunner = func(context.Context, *pipeline) (preparedCauseRunner, error) { return runner, nil }
	preparedCauseRuntimeFingerprint = func(context.Context, *pipeline) (string, error) { return "runtime", nil }
	t.Cleanup(func() { newPreparedCauseRunner = previous; preparedCauseRuntimeFingerprint = previousFingerprint })
}

func failPreparedCauseSave(t *testing.T, failureCall int) *int {
	t.Helper()
	previous := savePreparedCauseFindings
	calls := 0
	savePreparedCauseFindings = func(path string, state analysischat.PreparedCauseFindings) error {
		calls++
		if calls == failureCall {
			return errors.New("save failed")
		}
		return previous(path, state)
	}
	t.Cleanup(func() { savePreparedCauseFindings = previous })
	return &calls
}

func preparedCausePipeline(dir string) *pipeline {
	return &pipeline{
		opts: Options{OutDir: dir, PrepareCauseFindings: true}, enableAI: true,
		aiProject: &analysisruntime.Project{
			Provider: project.AIProvider{Model: "model"}, CacheGenerationFingerprint: "cache", SystemPrompt: "prompt",
		},
	}
}

func preparedCauseDetail() models.JobDetail {
	revision := "0123456789abcdef0123456789abcdef01234567"
	group := models.PatternCausalGroup{Builds: []string{"1"}, RootCause: "controller defect", Confidence: "high"}
	pattern := models.PatternAnalysis{
		Subject: "failure", JobID: "job", GeneratedAt: "2026-08-25T00:00:00Z", Systemic: true,
		CausalGroups: []models.PatternCausalGroup{group}, Lifecycle: &models.PatternLifecycle{State: models.PatternLifecycleActive},
	}
	models.AssignPatternIdentity(&pattern)
	return models.JobDetail{
		Name: "job", JobID: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{
			BuildInfo: models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": revision}},
			TestCases: []models.TestCase{{
				Name: "TestCluster", Status: "failed", JUnitFile: "junit.xml",
				AIAnalysis: &models.AIAnalysis{
					GeneratedAt: "2026-08-25T00:00:00Z", RootCause: "controller defect", Severity: "High",
					Disposition: models.AnalysisDispositionCitationsVerified,
					FileLinks:   map[string]string{"pkg/controller.go": "https://github.com/example/repo/blob/" + revision + "/pkg/controller.go"},
				},
			}},
		}},
		PatternAnalyses: []models.PatternAnalysis{pattern},
	}
}

// preparedCauseJob builds a job whose causes have no Fix target: the
// representative failure carries no file links.
func preparedCauseJob(jobID string, systemic bool, lifecycle models.PatternLifecycleState, rootCauses ...string) models.JobDetail {
	detail := models.JobDetail{Name: jobID, JobID: jobID, JobType: models.JobTypePeriodic}
	pattern := models.PatternAnalysis{
		Subject: "failure", JobID: jobID, GeneratedAt: "2026-08-25T00:00:00Z", Systemic: systemic,
		Lifecycle: &models.PatternLifecycle{State: lifecycle},
	}
	for index, rootCause := range rootCauses {
		buildID := strconv.Itoa(index + 1)
		pattern.CausalGroups = append(pattern.CausalGroups, models.PatternCausalGroup{
			Builds: []string{buildID}, RootCause: rootCause, Confidence: "high",
		})
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, JobName: jobID},
			TestCases: []models.TestCase{{
				Name: "TestCluster", Status: "failed", JUnitFile: "junit.xml",
				AIAnalysis: &models.AIAnalysis{
					GeneratedAt: "2026-08-25T00:00:00Z", RootCause: rootCause, Severity: "High",
					Disposition: models.AnalysisDispositionCitationsVerified,
				},
			}},
		})
	}
	models.AssignPatternIdentity(&pattern)
	detail.PatternAnalyses = []models.PatternAnalysis{pattern}
	return detail
}

func TestPrepareCauseFindingsCachesSuccessfulAnswer(t *testing.T) {
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "Change the controller.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	installPreparedCauseRunner(t, runner)
	dir := t.TempDir()
	p := preparedCausePipeline(dir)
	details := []models.JobDetail{preparedCauseDetail()}
	p.prepareCauseFindings(t.Context(), details)
	p.prepareCauseFindings(t.Context(), details)
	if runner.calls != 1 {
		t.Fatalf("calls = %d", runner.calls)
	}
	generation := analysischat.PreparedCauseGeneration("runtime")
	state, err := analysischat.LoadPreparedCauseFindings(filepath.Join(dir, analysischat.PreparedCauseFindingsFilename), generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Findings) != 1 {
		t.Fatalf("state = %+v", state)
	}
	for _, finding := range state.Findings {
		if _, err := time.Parse(time.RFC3339, finding.PreparedAt); err != nil || finding.Reply.Answer == "" {
			t.Fatalf("finding = %+v err=%v", finding, err)
		}
	}
}

func TestPrepareCauseFindingsRefreshesWhenComparisonChanges(t *testing.T) {
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "The comparison does not prove a fix.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	installPreparedCauseRunner(t, runner)
	dir := t.TempDir()
	p := preparedCausePipeline(dir)
	detail := preparedCauseDetail()
	details := []models.JobDetail{detail}
	p.prepareCauseFindings(t.Context(), details)
	detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: "2", JobName: detail.Name, Result: "SUCCESS", Passed: true,
		Started: time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC),
	}})
	details[0] = detail
	p.prepareCauseFindings(t.Context(), details)
	if runner.calls != 2 || len(runner.turns) != 2 || runner.turns[1].Comparison == nil || runner.turns[1].Comparison.ArtifactBuild.Build.BuildID != "2" {
		t.Fatalf("calls=%d turns=%+v", runner.calls, runner.turns)
	}
	state, err := analysischat.LoadPreparedCauseFindings(filepath.Join(dir, analysischat.PreparedCauseFindingsFilename), analysischat.PreparedCauseGeneration("runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Findings) != 1 {
		t.Fatalf("findings = %+v", state.Findings)
	}
	ref := detail.PatternAnalyses[0].CausalGroups[0]
	wantKey, err := analysischat.PreparedCauseKey(analysischat.AnalysisRef{
		Scope: analysischat.ScopeCause, JobID: detail.JobID,
		PatternID: detail.PatternAnalyses[0].ID, PatternHash: detail.PatternAnalyses[0].ContentHash,
		CausalGroupID: ref.ID, CausalGroupHash: ref.ContentHash,
	}, "2")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Findings[wantKey]; !ok {
		t.Fatalf("comparison finding key missing: %+v", state.Findings)
	}
}

func TestPrepareCauseFindingsPrefersPublishedCauses(t *testing.T) {
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "Change the controller.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	installPreparedCauseRunner(t, runner)
	p := preparedCausePipeline(t.TempDir())
	p.prepareCauseFindings(t.Context(), []models.JobDetail{
		preparedCauseJob("unpublished", false, models.PatternLifecycleActive, "unpublished cause a", "unpublished cause b"),
		preparedCauseJob("published", true, models.PatternLifecycleActive, "published cause a", "published cause b"),
	})
	if runner.calls != maxPreparedCauseFindingsPerRun {
		t.Fatalf("calls = %d", runner.calls)
	}
	if prepared := preparedJobIDs(runner); !slices.Equal(prepared, []string{"published", "published", "unpublished"}) {
		t.Fatalf("prepared jobs = %v", prepared)
	}
}

func TestPrepareCauseFindingsSkipsInactivePatterns(t *testing.T) {
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "Change the controller.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	installPreparedCauseRunner(t, runner)
	p := preparedCausePipeline(t.TempDir())
	p.prepareCauseFindings(t.Context(), []models.JobDetail{
		preparedCauseJob("verified-fixed", true, models.PatternLifecycleVerifiedFixed, "repaired cause"),
		preparedCauseJob("recovered", true, models.PatternLifecycleRecovered, "recovered cause"),
		preparedCauseJob("active", true, models.PatternLifecycleActive, "active cause"),
	})
	if prepared := preparedJobIDs(runner); !slices.Equal(prepared, []string{"active"}) {
		t.Fatalf("prepared jobs = %v", prepared)
	}
}

func TestPrepareCauseFindingsStopsWhenFailureStateCannotPersist(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		reply  analysischat.Reply
		runErr error
	}{
		{name: "provider failure", runErr: errors.New("provider failed")},
		{name: "unverified reply", reply: analysischat.Reply{Answer: "unsupported"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &recordingPreparedCauseRunner{reply: testCase.reply, err: testCase.runErr}
			installPreparedCauseRunner(t, runner)
			saves := failPreparedCauseSave(t, 2)
			p := preparedCausePipeline(t.TempDir())
			p.prepareCauseFindings(t.Context(), []models.JobDetail{
				preparedCauseJob("published", true, models.PatternLifecycleActive, "cause one", "cause two"),
			})
			if runner.calls != 1 {
				t.Fatalf("provider calls = %d, want remaining work stopped", runner.calls)
			}
			if *saves != 2 {
				t.Fatalf("save calls = %d, want initial state plus failed retry state", *saves)
			}
		})
	}
}

func preparedJobIDs(runner *recordingPreparedCauseRunner) []string {
	prepared := make([]string, 0, len(runner.turns))
	for _, turn := range runner.turns {
		prepared = append(prepared, turn.JobID)
	}
	return prepared
}
