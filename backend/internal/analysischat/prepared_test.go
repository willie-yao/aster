package analysischat

import (
	"path/filepath"
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

func TestPreparedCauseTurnRequiresActionableCause(t *testing.T) {
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1")
	group := pattern.CausalGroups[0]
	_, err := PreparedCauseTurn(AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}, detail)
	if err == nil {
		t.Fatal("cause without an eligible failed test was prepared")
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
