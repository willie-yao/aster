package fetcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
)

type recordingPreparedCauseRunner struct {
	calls int
	reply analysischat.Reply
	err   error
}

func (r *recordingPreparedCauseRunner) Reply(context.Context, analysischat.Turn) (analysischat.Reply, error) {
	r.calls++
	return r.reply, r.err
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
					Disposition: models.AnalysisDispositionGrounded,
					FileLinks:   map[string]string{"pkg/controller.go": "https://github.com/example/repo/blob/" + revision + "/pkg/controller.go"},
				},
			}},
		}},
		PatternAnalyses: []models.PatternAnalysis{pattern},
	}
}

func TestPrepareCauseFindingsCachesSuccessfulAnswer(t *testing.T) {
	runner := &recordingPreparedCauseRunner{reply: analysischat.Reply{
		Answer: "Change the controller.", Assessment: "supports",
		Citations: []analysischat.Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
	}}
	previous := newPreparedCauseRunner
	previousFingerprint := preparedCauseRuntimeFingerprint
	newPreparedCauseRunner = func(context.Context, *pipeline) (preparedCauseRunner, error) { return runner, nil }
	preparedCauseRuntimeFingerprint = func(context.Context, *pipeline) (string, error) { return "runtime", nil }
	t.Cleanup(func() { newPreparedCauseRunner = previous; preparedCauseRuntimeFingerprint = previousFingerprint })
	dir := t.TempDir()
	p := &pipeline{
		opts: Options{OutDir: dir, PrepareCauseFindings: true}, enableAI: true,
		aiProject: &analysisruntime.Project{
			Provider: project.AIProvider{Model: "model"}, CacheGenerationFingerprint: "cache", SystemPrompt: "prompt",
		},
	}
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
