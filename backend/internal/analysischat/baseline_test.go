package analysischat

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func passingBaselineRun(id string, started time.Time) models.BuildResult {
	return models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: id, JobName: "periodic-demo", Started: started,
		Result: "SUCCESS", Passed: true, JUnitComplete: true,
		JUnitURLs: []string{"https://artifacts.example/" + id + "/junit.xml"},
	}}
}

func TestSelectCauseBaselineRun(t *testing.T) {
	base := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	group := models.PatternCausalGroup{Builds: []string{"member-new", "member-old"}}
	members := []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "member-old", Result: "FAILURE", Started: base.Add(4 * time.Hour)}},
		{BuildInfo: models.BuildInfo{BuildID: "member-new", Result: "FAILURE", Started: base.Add(6 * time.Hour)}},
	}
	passing := passingBaselineRun("passing", base.Add(2*time.Hour))
	for _, tc := range []struct {
		name string
		runs []models.BuildResult
		edit func([]models.BuildResult)
		want string
	}{
		{name: "passing before first failure", runs: []models.BuildResult{
			passingBaselineRun("older", base), passing, passingBaselineRun("later", base.Add(7*time.Hour)),
		}, want: "passing"},
		{name: "none retained"},
		{name: "passing between members is not baseline", runs: []models.BuildResult{
			passing, passingBaselineRun("interleaved", base.Add(5*time.Hour)),
		}, want: "passing"},
		{name: "only passing between members", runs: []models.BuildResult{
			passingBaselineRun("interleaved", base.Add(5*time.Hour)),
		}},
		{name: "pending excluded", runs: []models.BuildResult{passingBaselineRun("pending", base)}, edit: func(runs []models.BuildResult) {
			runs[0].Result = "PENDING"
		}},
		{name: "metadata without artifacts excluded", runs: []models.BuildResult{passing}, edit: func(runs []models.BuildResult) {
			runs[0].JUnitURLs = nil
		}},
		{name: "incomplete artifacts excluded", runs: []models.BuildResult{passing}, edit: func(runs []models.BuildResult) {
			runs[0].JUnitComplete = false
		}},
		{name: "truncated artifacts excluded", runs: []models.BuildResult{passing}, edit: func(runs []models.BuildResult) {
			runs[0].JUnitTruncated = true
		}},
		{name: "another failing test excluded", runs: []models.BuildResult{passing}, edit: func(runs []models.BuildResult) {
			runs[0].Passed = false
			runs[0].Result = "FAILURE"
		}},
		{name: "failed JUnit contradicts passing result", runs: []models.BuildResult{passing}, edit: func(runs []models.BuildResult) {
			runs[0].TestsFailed = 1
		}},
		{name: "skip unavailable newer passing run", runs: []models.BuildResult{
			passingBaselineRun("older", base), passing,
		}, edit: func(runs []models.BuildResult) {
			runs[1].JUnitURLs = nil
		}, want: "older"},
		{name: "numeric build identity breaks time ties", runs: []models.BuildResult{
			passingBaselineRun("9", base), passingBaselineRun("10", base),
		}, want: "10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := slices.Clone(tc.runs)
			if tc.edit != nil {
				tc.edit(runs)
			}
			runs = append(runs, members...)
			for range 2 {
				selected := selectCauseBaselineRun(group, runs)
				got := ""
				if selected != nil {
					got = selected.BuildID
				}
				if got != tc.want {
					t.Fatalf("baseline = %q, want %q", got, tc.want)
				}
				slices.Reverse(runs)
			}
		})
	}
	if got := selectCauseBaselineRun(models.PatternCausalGroup{Builds: []string{"absent"}}, []models.BuildResult{passing}); got != nil {
		t.Fatalf("baseline without retained members = %+v", got)
	}
}

func TestCauseBaselinePersistsWithoutChangingMembershipFixOrPreparedFindings(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{
		Builds: []string{"member-old", "member-new"}, RootCause: "cause", Confidence: "high",
	}}, nil)
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "member-old", "member-new")
	for index := range detail.Runs {
		detail.Runs[index].Result = "FAILURE"
		detail.Runs[index].TestCases = []models.TestCase{
			analyzedTest("TestCluster", "junit.xml", "2026-08-12T12:00:00Z"),
		}
	}
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	withoutBaseline, err := resolveCauseAnalysis(ref, detail)
	if err != nil {
		t.Fatal(err)
	}
	baseline := passingBaselineRun("baseline", detail.Runs[0].Started.Add(-time.Hour))
	baseline.Commit, baseline.Revision = "baseline-commit", "baseline-revision"
	detail.Runs = append(detail.Runs, baseline)
	withBaseline, err := resolveCauseAnalysis(ref, detail)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutBaseline.fixTarget, withBaseline.fixTarget) ||
		!reflect.DeepEqual(withoutBaseline.pattern, withBaseline.pattern) ||
		!reflect.DeepEqual(withoutBaseline.evidenceBuilds, withBaseline.evidenceBuilds) {
		t.Fatal("baseline changed cause membership, recovery or Fix target")
	}
	preparedTurn, err := PreparedCauseTurn(ref, detail)
	if err != nil || preparedTurn.Baseline != nil {
		t.Fatalf("prepared baseline = %+v, error = %v", preparedTurn.Baseline, err)
	}
	writeJobDetail(t, dir, detail)
	key, err := PreparedCauseKey(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePreparedCauseFindings(preparedFindingPath(dir), PreparedCauseFindings{
		Generation: "generation", Findings: map[string]PreparedCauseFinding{
			key: {Ref: ref, Reply: Reply{Answer: "prepared", Citations: []Citation{{Path: "builds/member-old/build-log.txt"}}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	opts := Options{StateDir: filepath.Join(dir, ".baseline-chat")}
	first := newTestService(t, t.Context(), dir, &fakeRunner{}, opts)
	if err := first.ConfigurePreparedCauseFindings("generation"); err != nil {
		t.Fatal(err)
	}
	created, err := first.CreatePrepared(ref, "alice", "create")
	if err != nil || created.BaselineBuildID != "baseline" || len(created.Messages) != 1 || !created.Messages[0].Prepared {
		t.Fatalf("created = %+v, error = %v", created, err)
	}
	runner := &fakeRunner{reply: Reply{Answer: "baseline comparison", Assessment: "explains"}}
	restarted := newTestService(t, t.Context(), dir, runner, opts)
	if err := restarted.ConfigurePreparedCauseFindings("generation"); err != nil {
		t.Fatal(err)
	}
	found, err := restarted.Find(ref, "bob")
	if err != nil || found.ID != created.ID || found.BaselineBuildID != "baseline" {
		t.Fatalf("restored lookup = %+v, error = %v", found, err)
	}
	detail.Runs = append(detail.Runs, passingBaselineRun("new-baseline", baseline.Started.Add(30*time.Minute)))
	writeJobDetail(t, dir, detail)
	if _, err := restarted.Find(ref, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("lookup reused stale baseline: %v", err)
	}
	replay, err := restarted.Create(ref, "alice", "create")
	if err != nil || replay.ID != created.ID || replay.BaselineBuildID != "baseline" {
		t.Fatalf("create replay changed baseline = %+v, error = %v", replay, err)
	}
	if got := restarted.PreparedAvailable([]AnalysisRef{ref}); !slices.Equal(got, []bool{true}) {
		t.Fatalf("baseline changed prepared availability: %v", got)
	}
	second, err := restarted.CreatePrepared(ref, "bob", "replacement")
	if err != nil || second.ID == created.ID || second.BaselineBuildID != "new-baseline" ||
		len(second.Messages) != 1 || second.Messages[0].RequestID != created.Messages[0].RequestID {
		t.Fatalf("replacement = %+v, error = %v", second, err)
	}
	if _, err := restarted.Send(t.Context(), created.ID, "alice", "ask", "What changed?"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	turn := runner.turns[0]
	runner.mu.Unlock()
	if turn.Baseline == nil || turn.Baseline.ArtifactBuild.Build.BuildID != baseline.BuildID ||
		turn.Baseline.ArtifactBuild.BuildPrefix != "logs/periodic-demo/baseline/" ||
		turn.Baseline.ArtifactBuild.Build.Commit != baseline.Commit || turn.Baseline.ArtifactBuild.Build.Revision != baseline.Revision ||
		!turn.Baseline.ArtifactBuild.Build.Passed || !turn.Baseline.ArtifactBuild.Build.Started.Equal(baseline.Started) ||
		!slices.Equal(turn.Baseline.TestNames, []string{"TestCluster"}) {
		t.Fatalf("restored baseline = %+v", turn.Baseline)
	}
	if err := restarted.Archive(created.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	writeJobDetail(t, dir, models.JobDetail{JobID: ref.JobID, Name: ref.JobID})
	saved, err := restarted.Get(created.ID, "bob")
	if err != nil || saved.BaselineBuildID != "baseline" || !saved.ReadOnly {
		t.Fatalf("saved conversation = %+v, error = %v", saved, err)
	}
	page, err := restarted.List(HistoryQuery{Scope: ScopeCause}, "bob")
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].BaselineBuildID != "baseline" {
		t.Fatalf("saved history = %+v, error = %v", page, err)
	}
}
