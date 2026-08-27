package analysischat

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestPreparedCauseFindingsRejectDifferentGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), PreparedCauseFindingsFilename)
	state := PreparedCauseFindings{Generation: "one", Findings: map[string]PreparedCauseFinding{"key": {PreparedAt: "2026-08-25T00:00:00Z"}}}
	if err := SavePreparedCauseFindings(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPreparedCauseFindings(path, "two")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != "two" || len(loaded.Findings) != 0 {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestPreparedCauseGenerationChangesWithRuntime(t *testing.T) {
	if PreparedCauseGeneration("runtime-one") == PreparedCauseGeneration("runtime-two") {
		t.Fatal("generation did not change")
	}
}

func TestPreparedCauseTurnWithoutFixTarget(t *testing.T) {
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1")
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	resolved, err := resolveCauseAnalysis(ref, detail)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.fixTarget != nil {
		t.Fatal("fixture unexpectedly has a Fix target")
	}
	turn, err := PreparedCauseTurn(ref, detail)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Scope != ScopeCause || turn.Question != PreparedCauseQuestion || turn.Pattern == nil || len(turn.EvidenceBuilds) == 0 {
		t.Fatalf("turn = %+v", turn)
	}
}

func TestServiceCreateSeedsPreparedCauseFindingWithoutUsingATurn(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1")
	testCase := analyzedTest("TestCluster", "junit.xml", "2026-08-25T00:00:00Z")
	testCase.AIAnalysis.FileLinks = map[string]string{
		"pkg/controller.go": "https://github.com/example/repo/blob/" + exactFixSourceRevision + "/pkg/controller.go",
	}
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": exactFixSourceRevision}
	detail.Runs[0].TestCases = []models.TestCase{testCase}
	writeJobDetail(t, dir, detail)
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	key, err := PreparedCauseKey(ref)
	if err != nil {
		t.Fatal(err)
	}
	generation := PreparedCauseGeneration("runtime")
	if err := SavePreparedCauseFindings(preparedFindingPath(dir), PreparedCauseFindings{
		Generation: generation,
		Findings: map[string]PreparedCauseFinding{key: {
			Ref: ref, PreparedAt: "2026-08-25T01:00:00Z",
			Reply: Reply{Answer: "The artifact supports changing the controller.", Assessment: "supports", Citations: []Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigurePreparedCauseFindings(generation); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(ref, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if session.TurnsUsed != 0 || len(session.Messages) != 1 || session.Messages[0].RequestID == "" {
		t.Fatalf("session = %+v", session)
	}
	if len(session.Attempts) != 1 || session.Attempts[0].Outcome != requestSucceeded || session.Attempts[0].Turn != 0 {
		t.Fatalf("attempts = %+v", session.Attempts)
	}
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", session.Messages[0].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Analysis.Scope != ScopeCause || candidate.FixTarget.TestName != "TestCluster" || len(candidate.ArtifactCitations) != 1 {
		t.Fatalf("candidate = %+v", candidate)
	}
}

func TestServicePreparedAvailable(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{
		{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"},
		{Builds: []string{"1"}, RootCause: "second cause", Confidence: "high"},
	}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1")
	writeJobDetail(t, dir, detail)

	causeRef := func(group models.PatternCausalGroup) AnalysisRef {
		return AnalysisRef{
			Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
			CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
		}
	}
	ready := causeRef(pattern.CausalGroups[0])
	uncited := causeRef(pattern.CausalGroups[1])
	readyKey, err := PreparedCauseKey(ready)
	if err != nil {
		t.Fatal(err)
	}
	uncitedKey, err := PreparedCauseKey(uncited)
	if err != nil {
		t.Fatal(err)
	}
	generation := PreparedCauseGeneration("runtime")
	if err := SavePreparedCauseFindings(preparedFindingPath(dir), PreparedCauseFindings{
		Generation: generation,
		Findings: map[string]PreparedCauseFinding{
			readyKey: {Ref: ready, PreparedAt: "2026-08-25T01:00:00Z", Reply: Reply{
				Answer: "The artifact supports changing the controller.", Assessment: "supports",
				Citations: []Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}},
			}},
			// A finding with no citation is not usable, so it must not be
			// advertised as waiting.
			uncitedKey: {Ref: uncited, PreparedAt: "2026-08-25T01:00:00Z", Reply: Reply{Answer: "no evidence"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	patternScoped := AnalysisRef{Scope: ScopePattern, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash}
	invalid := AnalysisRef{Scope: ScopeCause, JobID: pattern.JobID}
	refs := []AnalysisRef{ready, uncited, patternScoped, invalid}

	// Preparation is not configured yet, so nothing is waiting anywhere.
	if got := service.PreparedAvailable(refs); !reflect.DeepEqual(got, []bool{false, false, false, false}) {
		t.Fatalf("unconfigured = %v", got)
	}
	if err := service.ConfigurePreparedCauseFindings(generation); err != nil {
		t.Fatal(err)
	}
	if got := service.PreparedAvailable(refs); !reflect.DeepEqual(got, []bool{true, false, false, false}) {
		t.Fatalf("configured = %v", got)
	}
	if got := service.PreparedAvailable(nil); len(got) != 0 {
		t.Fatalf("empty batch = %v", got)
	}

	// A generation the findings were not written under is a miss, matching how
	// the create path revalidates.
	stale, err := NewService(t.Context(), dir, &fakeRunner{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.ConfigurePreparedCauseFindings(PreparedCauseGeneration("other-runtime")); err != nil {
		t.Fatal(err)
	}
	if got := stale.PreparedAvailable([]AnalysisRef{ready}); !reflect.DeepEqual(got, []bool{false}) {
		t.Fatalf("stale generation = %v", got)
	}
}
