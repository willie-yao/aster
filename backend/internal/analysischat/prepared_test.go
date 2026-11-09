package analysischat

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestPreparedCauseGenerationRejectsPreviousTask(t *testing.T) {
	const previousQuestion = "Investigate this causal group across its failed member builds and the newest later completed run when available. Determine whether the cause still appears, was not reproduced because the trigger changed, or has evidence of recovery. A passing run alone does not prove a fix. Cite the strongest direct evidence. If the published cause or remediation is wrong, challenge it and propose a revision. Historical source evidence does not establish current remediation state."
	payload, err := json.Marshal([]string{"runtime", previousQuestion})
	if err != nil {
		t.Fatal(err)
	}
	previousGeneration := hashBytes(payload)
	generation := PreparedCauseGeneration("runtime")
	if previousGeneration == generation {
		t.Fatal("the guided task reused the previous preparation generation")
	}
	path := filepath.Join(t.TempDir(), PreparedCauseFindingsFilename)
	if err := SavePreparedCauseFindings(path, PreparedCauseFindings{
		Generation: previousGeneration,
		Findings:   map[string]PreparedCauseFinding{"old": {Reply: Reply{Answer: "Old cause check."}}},
		Failures:   map[string]PreparedCauseFailure{"old-failure": {AttemptedAt: "2026-08-25T00:00:00Z"}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPreparedCauseFindings(path, generation)
	if err != nil || len(loaded.Findings) != 0 || len(loaded.Failures) != 0 || loaded.Generation != generation {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestPreparedCauseGenerationChangesWithRuntime(t *testing.T) {
	if PreparedCauseGeneration("runtime-one") == PreparedCauseGeneration("runtime-two") {
		t.Fatal("generation did not change")
	}
}

func TestPreparedCauseKeyChangesWithComparisonBuild(t *testing.T) {
	ref := AnalysisRef{Scope: ScopeCause, JobID: "job", PatternID: "pattern", PatternHash: "pattern-hash", CausalGroupID: "cause", CausalGroupHash: "cause-hash"}
	first, err := PreparedCauseKey(ref, "build-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PreparedCauseKey(ref, "build-2")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("comparison build did not change the prepared finding key")
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
	if turn.Scope != ScopeCause || turn.Question != PreparedCauseQuestion || turn.Pattern == nil || len(turn.EvidenceBuilds) == 0 || !turn.HistoricalSourceOnly {
		t.Fatalf("turn = %+v", turn)
	}
	question := strings.Join(strings.Fields(turn.Question), " ")
	for _, want := range []string{
		"Investigate a fix for this causal group", "newest later completed run",
		"cause still appears", "trigger changed", "evidence of recovery",
		"A passing run alone does not prove a fix", "No desired outcome supplied",
		"label a narrow provisional outcome", "not operator intent or permission to change policy",
		"Follow the fix-investigation guidance", "preserving test purpose, coverage, assertions, and version policy",
		"Cite the strongest direct evidence", "challenge it and propose a revision",
		"Historical source evidence does not establish current remediation state",
		"current-source or policy gap before implementation",
	} {
		if !strings.Contains(question, want) {
			t.Errorf("prepared question missing %q", want)
		}
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
	key, err := PreparedCauseKey(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	generation := PreparedCauseGeneration("runtime")
	if err := SavePreparedCauseFindings(preparedFindingPath(dir), PreparedCauseFindings{
		Generation: generation,
		Findings: map[string]PreparedCauseFinding{key: {
			Ref: ref, PreparedAt: "2026-08-25T01:00:00Z",
			Reply: Reply{Answer: "The artifact supports changing the controller.", Assessment: "supports", StopReason: "rounds", Citations: []Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}}},
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
	if session.TurnsUsed != 0 || len(session.Messages) != 1 || session.Messages[0].RequestID == "" || session.Messages[0].StopReason != "rounds" {
		t.Fatalf("session = %+v", session)
	}
	if message := session.Messages[0]; message.Role != "assistant" || !message.Prepared || message.Actor != "" {
		t.Fatalf("prepared message acquired operator intent: %+v", message)
	}
	if len(session.Attempts) != 1 || session.Attempts[0].Outcome != requestSucceeded || session.Attempts[0].Turn != 0 ||
		session.Attempts[0].Actor != "" || session.Attempts[0].Question != PreparedCauseQuestion {
		t.Fatalf("attempts = %+v", session.Attempts)
	}
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", session.Messages[0].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Analysis.Scope != ScopeCause || candidate.FixTarget.TestName != "TestCluster" || len(candidate.ArtifactCitations) != 1 ||
		candidate.UserRequest != "" || candidate.AssistantAnswer != session.Messages[0].Content {
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
	readyKey, err := PreparedCauseKey(ready, "")
	if err != nil {
		t.Fatal(err)
	}
	uncitedKey, err := PreparedCauseKey(uncited, "")
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
	detail.Runs = append(detail.Runs, models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: "2", JobName: detail.Name, Result: "SUCCESS", Passed: true,
		Started: detail.Runs[0].Started.Add(time.Minute),
	}})
	writeJobDetail(t, dir, detail)
	if got := service.PreparedAvailable([]AnalysisRef{ready}); !reflect.DeepEqual(got, []bool{false}) {
		t.Fatalf("changed comparison = %v", got)
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

func TestServiceArchiveDismissesPreparedCauseFinding(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{Builds: []string{"1"}, RootCause: "cause", Confidence: "high"}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dir, causalPatternDetail(pattern, "1"))
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}
	key, err := PreparedCauseKey(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	generation := PreparedCauseGeneration("runtime")
	if err := SavePreparedCauseFindings(preparedFindingPath(dir), PreparedCauseFindings{
		Generation: generation,
		Findings: map[string]PreparedCauseFinding{key: {
			Ref: ref, PreparedAt: "2026-08-25T01:00:00Z",
			Reply: Reply{Answer: "The artifact supports the cause.", Assessment: "supports", Citations: []Citation{{Path: "builds/1/build-log.txt", Quote: "failure"}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var nowNanos atomic.Int64
	start := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	service := newTestService(t, t.Context(), dir, &fakeRunner{}, Options{
		HistoryRetention: 24 * time.Hour,
		Now:              func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() },
	})
	if err := service.ConfigurePreparedCauseFindings(generation); err != nil {
		t.Fatal(err)
	}

	first, err := service.CreatePrepared(ref, "alice", "prepared-first")
	if err != nil || len(first.Messages) != 1 || !first.Messages[0].Prepared {
		t.Fatalf("prepared session = %+v %v", first, err)
	}
	if err := service.Archive(first.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if got := service.PreparedAvailable([]AnalysisRef{ref}); !reflect.DeepEqual(got, []bool{false}) {
		t.Fatalf("dismissed availability = %v", got)
	}
	if _, err := service.CreatePrepared(ref, "alice", "prepared-second"); !errors.Is(err, ErrPreparedFindingNotFound) {
		t.Fatalf("dismissed prepared create: %v", err)
	}
	replacement, err := service.Create(ref, "alice", "replacement")
	if err != nil || replacement.ID == first.ID || len(replacement.Messages) != 0 || len(replacement.Attempts) != 0 {
		t.Fatalf("replacement = %+v %v", replacement, err)
	}

	nowNanos.Store(start.Add(25 * time.Hour).UnixNano())
	if got := service.PreparedAvailable([]AnalysisRef{ref}); !reflect.DeepEqual(got, []bool{true}) {
		t.Fatalf("lapsed dismissal availability = %v", got)
	}
}
