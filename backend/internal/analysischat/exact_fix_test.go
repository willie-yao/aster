package analysischat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/prowbuild"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/storage"
)

const exactFixSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func exactFixService(t *testing.T, reply Reply, runnerErr error) (*Service, SessionView, string) {
	t.Helper()
	service, session, requestID, _ := exactFixServiceRunner(t, reply, runnerErr)
	return service, session, requestID
}

func exactFixServiceRunner(t *testing.T, reply Reply, runnerErr error) (*Service, SessionView, string, *fakeRunner) {
	t.Helper()
	return exactFixServiceRunnerWithTest(t, reply, runnerErr, analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
}

// exactFixServiceWithTest builds the exact-JUnit fixture around one caller-supplied
// analyzed test, so a case can vary the published analysis it starts from.
func exactFixServiceWithTest(t *testing.T, reply Reply, analyzed models.TestCase) (*Service, SessionView, string) {
	t.Helper()
	service, session, requestID, _ := exactFixServiceRunnerWithTest(t, reply, nil, analyzed)
	return service, session, requestID
}

func exactFixServiceRunnerWithTest(t *testing.T, reply Reply, runnerErr error, analyzed models.TestCase) (*Service, SessionView, string, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	detail := testDetail(analyzed)
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:" + exactFixSourceRevision}
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: reply, err: runnerErr}
	service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat"), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
		AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	_, sendErr := service.Send(t.Context(), session.ID, "Alice", requestID, "What exact change does the artifact support?")
	if runnerErr == nil && sendErr != nil {
		t.Fatal(sendErr)
	}
	if runnerErr != nil && sendErr == nil {
		t.Fatal("failed chat turn unexpectedly succeeded")
	}
	return service, session, requestID, runner
}

func TestServiceAnalysisFixCandidateSharesExactAnalysisAndEvidence(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations:        []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		ProposedRevision: &Revision{RootCause: "The terminal branch omits Ready.", SuggestedFix: "Record Ready before returning."},
	}, nil)
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SessionID != session.ID || candidate.RequestID != requestID || candidate.Analysis.Scope != ScopeTest ||
		candidate.Analysis.JobID != "periodic-demo" || candidate.Analysis.BuildID != "123" || candidate.Analysis.TestName != "TestCluster" ||
		candidate.Analysis.AnalysisGeneratedAt != "2026-08-13T01:00:00Z" || candidate.ResponseHash == "" || candidate.AnalysisContentHash == "" ||
		candidate.SourceRepositorySnapshot.Revision != exactFixSourceRevision || len(candidate.ArtifactCitations) != 1 {
		t.Fatalf("candidate = %+v", candidate)
	}
	shared, err := service.AnalysisFixCandidate(session.ID, "Bob", requestID)
	if err != nil || shared.ResponseHash != candidate.ResponseHash {
		t.Fatalf("shared candidate = %+v err=%v", shared, err)
	}
}

func TestServiceAnalysisFixCandidatePinsMultiRepoCloneRecords(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "logs", "job", "123")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{
		"started.json": `{"timestamp":1000,"repos":{"example/repo":"main","example/other":"master"},"repo-commit":"` +
			exactFixSourceRevision + `","repo-version":"` + exactFixSourceRevision + `"}`,
		"clone-records.json": `[{"refs":{"org":"example","repo":"repo","base_ref":"main"},"final_sha":"` +
			exactFixSourceRevision + `"},{"refs":{"org":"example","repo":"other","base_ref":"master"},"final_sha":"fedcba9876543210fedcba9876543210fedcba98"}]`,
	}
	for name, contents := range artifacts {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := prowbuild.FetchBuildInfo(t.Context(), backend, prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = info.RepoRefs
	detail.Runs[0].Commit, detail.Runs[0].RepoVersion = info.Commit, info.RepoVersion
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{
		Answer: "The published analysis explains the failure.", Assessment: "explains",
	}}, Options{StateDir: filepath.Join(dir, ".chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	session, err := service.Create(AnalysisRef{
		JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
		AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	if session.SourceRepository == nil || session.SourceRepository.Revision != exactFixSourceRevision {
		t.Fatalf("session source = %+v", session.SourceRepository)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "Explain the failed test."); err != nil {
		t.Fatal(err)
	}
	if candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); err != nil ||
		candidate.SourceRepositorySnapshot.Revision != exactFixSourceRevision {
		t.Fatalf("candidate source = %+v, err=%v", candidate.SourceRepositorySnapshot, err)
	}
}

func TestServiceAnalysisFixCandidateRejectsChangedAnalysisEvidenceAndSource(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready through `markReady`.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:" + exactFixSourceRevision}
	detail.Runs[0].TestCases[0].AIAnalysis.FileLinks = map[string]string{"pkg/controller.go": "https://github.com/example/repo/blob/" + exactFixSourceRevision + "/pkg/controller.go"}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed source evidence error = %v", err)
	}

	detail = testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:fedcba9876543210fedcba9876543210fedcba98"}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("changed source revision error = %v", err)
	}
}

func TestServiceAnalysisFixCandidateReportsCurrentUnknownSource(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The published analysis explains the failure.", Assessment: "explains",
	}, nil)
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main", "example/other": "master"}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrSourceRevisionUnknown) {
		t.Fatalf("unknown current source error = %v", err)
	}
}

func TestValidateAdmittedFixOriginIgnoresReanalysis(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{Answer: "Investigate the terminal update.", Assessment: "explains"}, nil)
	candidate, err := service.AnalysisFixCandidate(session.ID, "alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	origin := FixOrigin{Analysis: candidate.Analysis, FixTarget: candidate.FixTarget, Original: candidate.Original}
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T02:00:00Z"))
	detail.Runs[0].TestCases[0].AIAnalysis.RootCause = "a different hypothesis"
	writeJobDetail(t, service.dataDir, detail)
	if err := ValidateAdmittedFixOrigin(service.dataDir, origin); err != nil {
		t.Fatalf("test origin after reanalysis: %v", err)
	}
	if err := ValidateFixOrigin(service.dataDir, origin); err == nil {
		t.Fatal("strict chat admission accepted a regenerated analysis")
	}
	detail.Runs[0].TestCases[0].AIAnalysis = nil
	writeJobDetail(t, service.dataDir, detail)
	if err := ValidateAdmittedFixOrigin(service.dataDir, origin); err != nil {
		t.Fatalf("test origin without current AI analysis: %v", err)
	}

	causeDir, causeDetail, causeOrigin := admittedCauseFixture(t)
	causeDetail.Runs[1].TestCases[0].AIAnalysis = nil
	writeJobDetail(t, causeDir, causeDetail)
	if err := ValidateAdmittedFixOrigin(causeDir, causeOrigin); err != nil {
		t.Fatalf("cause origin without target analysis: %v", err)
	}
	// A different AI-selected representative must not displace the admitted target.
	causeDetail.Runs[1].TestCases[0].AIAnalysis = &models.AIAnalysis{
		GeneratedAt: "new", RootCause: "new hypothesis", Severity: "Low",
		Disposition: models.AnalysisDispositionPreliminary,
	}
	causeDetail.Runs[1].TestCases = append(causeDetail.Runs[1].TestCases, analyzedTest("OtherTest", "other.xml", "new"))
	causeDetail.Runs[1].TestCases[1].AIAnalysis.Severity = "Critical"
	writeJobDetail(t, causeDir, causeDetail)
	if err := ValidateAdmittedFixOrigin(causeDir, causeOrigin); err != nil {
		t.Fatalf("cause origin after representative re-ranking: %v", err)
	}
}

func TestValidateAdmittedFixOriginRejectsChangedTargetAndCause(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*models.JobDetail)
	}{
		{"target passed", func(d *models.JobDetail) { d.Runs[1].TestCases[0].Status = "passed" }},
		{"target removed", func(d *models.JobDetail) { d.Runs[1].TestCases = nil }},
		{"ambiguous target", func(d *models.JobDetail) {
			d.Runs[1].TestCases = append(d.Runs[1].TestCases, d.Runs[1].TestCases[0])
		}},
		{"membership", func(d *models.JobDetail) { d.PatternAnalyses[0].CausalGroups[0].Builds = []string{"1"} }},
		{"remediation", func(d *models.JobDetail) {
			d.PatternAnalyses[0].CausalGroups[0].Remediation.SuggestedFix = "Different remedy"
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			dir, detail, origin := admittedCauseFixture(t)
			change.mutate(&detail)
			writeJobDetail(t, dir, detail)
			if err := ValidateAdmittedFixOrigin(dir, origin); err == nil {
				t.Fatal("changed target or cause retained admitted origin")
			}
		})
	}
}

func admittedCauseFixture(t *testing.T) (string, models.JobDetail, FixOrigin) {
	t.Helper()
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{
		Builds: []string{"2", "1"}, RootCause: "terminal update", Confidence: "high",
		Remediation: &models.PatternCausalGroupRemediation{BuildID: "2", SuggestedFix: "Fix the terminal update"},
	}}, nil)
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1", "2")
	for i := range detail.Runs {
		detail.Runs[i].TestCases = []models.TestCase{analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z")}
	}
	writeJobDetail(t, dir, detail)
	group := pattern.CausalGroups[0]
	ref := AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: models.PatternHash(pattern),
		CausalGroupID: group.ID, CausalGroupHash: models.PatternCausalGroupHash(group),
	}
	resolved, err := resolveFromDetail(ref, detail)
	if err != nil || resolved.fixTarget == nil || resolved.fixTarget.ref.BuildID != "2" {
		t.Fatalf("cause target = %+v, %v", resolved.fixTarget, err)
	}
	return dir, detail, FixOrigin{
		Analysis: ref, FixTarget: resolved.fixTarget.ref,
		Original: analysisSnapshot(resolved.testCase.AIAnalysis),
	}
}

func TestServiceAnalysisFixCandidateAcceptsContextOnlyButRejectsFailedTurns(t *testing.T) {
	service, session, requestID, runner := exactFixServiceRunner(t, Reply{Answer: "No artifact evidence was needed.", Assessment: "explains"}, nil)
	if candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); err != nil || len(candidate.ArtifactCitations) != 0 {
		t.Fatalf("context-only candidate error = %v", err)
	}
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "Which function should change?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", secondRequestID); err != nil {
		t.Fatalf("ungrounded conversation candidate error = %v", err)
	}
	runner.mu.Lock()
	turns := len(runner.turns)
	runner.mu.Unlock()
	if turns != 2 {
		t.Fatalf("provider calls = %d", turns)
	}

	failed, failedSession, failedRequest := exactFixService(t, Reply{}, ErrProviderRequestFailed)
	if _, err := failed.AnalysisFixCandidate(failedSession.ID, "Alice", failedRequest); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("failed turn candidate error = %v", err)
	}
}

func TestServiceAnalysisFixCandidateAcceptsMissingSourcePaths(t *testing.T) {
	service, session, requestID, runner := exactFixServiceRunner(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); err != nil {
		t.Fatalf("missing source path preflight error = %v", err)
	}
	runner.mu.Lock()
	turns := len(runner.turns)
	runner.mu.Unlock()
	if turns != 1 {
		t.Fatalf("provider calls = %d", turns)
	}
}

func TestServiceAnalysisFixCandidateAccumulatesConversationEvidence(t *testing.T) {
	cited := Citation{Path: "artifacts/build-log.txt", LineStart: 42, LineEnd: 44, Quote: "terminal bootstrap failure"}
	service, session, firstRequestID, runner := exactFixServiceRunner(t, Reply{
		Answer: "The build log records the terminal bootstrap failure.", Assessment: "supports",
		Citations: []Citation{cited},
	}, nil)
	runner.mu.Lock()
	runner.reply = Reply{
		Answer:     "The retry branch in `markReady` should stop requeueing that condition.",
		Assessment: "supports",
	}
	runner.mu.Unlock()
	secondRequestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", secondRequestID, "Which function should change to stop this?"); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", secondRequestID)
	if err != nil {
		t.Fatalf("conversation-scoped candidate error = %v", err)
	}
	if candidate.RequestID != secondRequestID ||
		candidate.AssistantAnswer != "The retry branch in `markReady` should stop requeueing that condition." ||
		!slices.Equal(candidate.ArtifactCitations, []Citation{cited}) {
		t.Fatalf("candidate = %+v", candidate)
	}
	first, err := service.AnalysisFixCandidate(session.ID, "Alice", firstRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResponseHash == candidate.ResponseHash {
		t.Fatal("promoted turns shared one response identity")
	}

	// A later turn and its evidence must not change an already promoted answer.
	runner.mu.Lock()
	runner.reply = Reply{
		Answer: "The kubelet log agrees.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/kubelet.log", LineStart: 7, LineEnd: 7, Quote: "not ready"}},
	}
	runner.mu.Unlock()
	if _, err := service.Send(t.Context(), session.ID, "Alice", testRequestID(t), "Does the kubelet log agree?"); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.AnalysisFixCandidate(session.ID, "Alice", secondRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ResponseHash != candidate.ResponseHash || !slices.Equal(replayed.ArtifactCitations, candidate.ArtifactCitations) {
		t.Fatalf("later turn changed the promoted response identity: %+v", replayed)
	}
	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{StateDir: service.opts.StateDir, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.AnalysisFixCandidate(session.ID, "Alice", secondRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ResponseHash != candidate.ResponseHash || !slices.Equal(restored.ArtifactCitations, candidate.ArtifactCitations) {
		t.Fatalf("restart changed the promoted response identity: %+v", restored)
	}
}

func TestConversationCitationsBoundsOrderAndScope(t *testing.T) {
	early := Citation{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "early"}
	shared := Citation{Path: "build-log.txt", LineStart: 2, LineEnd: 2, Quote: "shared"}
	promoted := Citation{Path: "junit.xml", LineStart: 3, LineEnd: 3, Quote: "promoted"}
	later := Citation{Path: "junit.xml", LineStart: 4, LineEnd: 4, Quote: "later"}
	messages := []Message{
		{Role: "user", RequestID: "one"},
		{Role: "assistant", RequestID: "one", Citations: []Citation{early, shared}},
		{Role: "user", RequestID: "two"},
		{Role: "assistant", RequestID: "two", Citations: []Citation{promoted, shared}},
		{Role: "user", RequestID: "three"},
		{Role: "assistant", RequestID: "three", Citations: []Citation{later}},
	}
	got := conversationCitations(messages, "two")
	if !slices.Equal(got, []Citation{promoted, shared, early}) {
		t.Fatalf("accumulated citations = %+v", got)
	}
	if got := conversationCitations(messages, "missing"); got != nil {
		t.Fatalf("unknown request citations = %+v", got)
	}

	overflow := []Message{{Role: "assistant", RequestID: "one"}, {Role: "assistant", RequestID: "two"}}
	for i := range maxConversationFixCitations {
		overflow[0].Citations = append(overflow[0].Citations, Citation{Path: "build-log.txt", LineStart: i + 1, LineEnd: i + 1, Quote: "old"})
		overflow[1].Citations = append(overflow[1].Citations, Citation{Path: "junit.xml", LineStart: i + 1, LineEnd: i + 1, Quote: "new"})
	}
	bounded := conversationCitations(overflow, "two")
	if len(bounded) != maxConversationFixCitations || !slices.Equal(bounded, overflow[1].Citations) {
		t.Fatalf("bounded citations = %d entries: %+v", len(bounded), bounded)
	}
}

func TestServiceAnalysisFixCandidateSurvivesRestartAndRejectsStaleAnalysis(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{StateDir: service.opts.StateDir, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	before, err := restarted.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ResponseHash == "" {
		t.Fatal("restart lost selected response identity")
	}
	detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T02:00:00Z"))
	detail.Runs[0].RepoRefs = map[string]string{"example/repo": "main:" + exactFixSourceRevision}
	writeJobDetail(t, service.dataDir, detail)
	if _, err := restarted.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrAnalysisChanged) {
		t.Fatalf("stale analysis error = %v", err)
	}
}

func TestServiceAnalysisFixCandidateHashChangesWithAnswerOrEvidence(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations: []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
	}, nil)
	before, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := service.store.context()
	defer cancel()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[session.ID]
		for index := range current.View.Messages {
			message := &current.View.Messages[index]
			if message.Role == "assistant" && message.RequestID == requestID {
				message.Content = "Changed answer"
				message.Citations[0].Quote = "changed evidence"
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ResponseHash == after.ResponseHash {
		t.Fatal("changed answer and evidence retained the same identity")
	}
}

func TestFixCandidateResponseHashIncludesQualification(t *testing.T) {
	candidate := FixCandidate{AssistantAnswer: "Investigate the retry path."}
	original, err := fixCandidateResponseHash(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.AssistantUnverified = true
	unverified, err := fixCandidateResponseHash(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.AssistantUnverifiedReason = UnverifiedCitation
	qualified, err := fixCandidateResponseHash(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if original == unverified || qualified == unverified {
		t.Fatal("changed evidence qualification retained the same response identity")
	}
}

func TestServiceExactFixSourceIneligibilityIsProviderFree(t *testing.T) {
	sha := "a866aca055bcaa205648e81d15c67668179fdfab"
	other := "b866aca055bcaa205648e81d15c67668179fdfab"
	for _, tc := range []struct {
		name  string
		build func(*models.BuildInfo)
	}{
		{name: "mismatched checkout", build: func(build *models.BuildInfo) {
			build.RepoRefs = map[string]string{"example/repo": "main"}
			build.Commit, build.RepoVersion = sha, other
		}},
		{name: "multiple repositories", build: func(build *models.BuildInfo) {
			build.RepoRefs = map[string]string{"example/repo": "main", "example/other": "main"}
			build.Commit, build.RepoVersion = sha, sha
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			detail := testDetail(analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z"))
			tc.build(&detail.Runs[0].BuildInfo)
			writeJobDetail(t, dir, detail)
			runner := &fakeRunner{reply: Reply{Answer: "The published analysis explains the failure.", Assessment: "explains"}}
			service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat")})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
				t.Fatal(err)
			}
			session, err := service.Create(AnalysisRef{
				JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster", JUnitFile: "junit.xml",
				AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
			}, "Alice", testRequestID(t))
			if err != nil {
				t.Fatal(err)
			}
			if session.SourceRepository != nil {
				t.Fatalf("source repository = %+v", session.SourceRepository)
			}

			runner.mu.Lock()
			turns := len(runner.turns)
			runner.mu.Unlock()
			if turns != 0 {
				t.Fatalf("Fix preflight made %d provider calls", turns)
			}
			requestID := testRequestID(t)
			if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "Explain the published context."); err != nil {
				t.Fatalf("normal chat error = %v", err)
			}
			runner.mu.Lock()
			turns = len(runner.turns)
			runner.mu.Unlock()
			if turns != 1 {
				t.Fatalf("normal chat provider calls = %d", turns)
			}
			if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrSourceRevisionUnknown) {
				t.Fatalf("ambiguous source candidate error = %v", err)
			}
		})
	}
}

func TestServiceExactFixDoesNotSalvagePersistedAmbiguousSource(t *testing.T) {
	service, session, requestID := exactFixService(t, Reply{
		Answer: "The artifact supports `markReady`.", Assessment: "supports",
		Citations: []Citation{{Path: "build-log.txt", LineStart: 10, LineEnd: 10, Quote: "ready"}},
	}, nil)
	ctx, cancel := service.store.context()
	if err := service.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[session.ID]
		current.Resolved.Source.Revision = ""
		current.Resolved.Build.RepoRefs = map[string]string{"example/repo": "ambiguous"}
		current.Resolved.Build.Commit = exactFixSourceRevision
		current.Resolved.Build.RepoVersion = exactFixSourceRevision
		return true, nil
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	restarted, err := NewService(t.Context(), service.dataDir, &fakeRunner{}, Options{StateDir: service.opts.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	if view, err := restarted.Get(session.ID, "Alice"); err != nil {
		t.Fatal(err)
	} else if view.SourceRepository != nil {
		t.Fatalf("persisted ambiguous source was salvaged: %+v", view.SourceRepository)
	}
	if _, err := restarted.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrSourceRevisionUnknown) {
		t.Fatalf("ambiguous persisted source error = %v", err)
	}
}

func TestServiceCauseAnalysisFixCandidateReportsUnknownSource(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{
		Builds: []string{"2", "1"}, RootCause: "same cause", Confidence: "high",
		Remediation: &models.PatternCausalGroupRemediation{BuildID: "2", SuggestedFix: "change the controller"},
	}}, nil)
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1", "2")
	for i := range detail.Runs {
		detail.Runs[i].RepoRefs = map[string]string{"example/repo": "main", "example/other": "master"}
		detail.Runs[i].TestCases = []models.TestCase{analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z")}
	}
	writeJobDetail(t, dir, detail)
	service, err := NewService(t.Context(), dir, &fakeRunner{reply: Reply{
		Answer: "Both builds show the same failure.", Assessment: "explains",
	}}, Options{StateDir: filepath.Join(dir, ".chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	group := pattern.CausalGroups[0]
	session, err := service.Create(AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "Explain this cause."); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrSourceRevisionUnknown) {
		t.Fatalf("cause source error = %v", err)
	}
	for i := range detail.Runs {
		detail.Runs[i].RepoRefs = map[string]string{
			"example/repo":  "main:" + exactFixSourceRevision,
			"example/other": "master:fedcba9876543210fedcba9876543210fedcba98",
		}
	}
	writeJobDetail(t, dir, detail)
	restarted, err := NewService(t.Context(), dir, &fakeRunner{}, Options{StateDir: filepath.Join(dir, ".chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AnalysisFixCandidate(session.ID, "Alice", requestID); !errors.Is(err, ErrSourceRevisionUnknown) {
		t.Fatalf("cause with later-pinned current source error = %v", err)
	}
}

// Fix generation rejects an oversized context outright, so a long conversation
// must drop its oldest evidence rather than hand generation a context it cannot
// use. Quotes are never truncated, because that would break their verification.
func TestConversationCitationsBoundsTotalQuoteBytes(t *testing.T) {
	messages := make([]Message, 0, 8)
	for i := range 8 {
		messages = append(messages,
			Message{Role: "user", RequestID: fmt.Sprintf("r%d", i)},
			Message{Role: "assistant", RequestID: fmt.Sprintf("r%d", i), Citations: []Citation{
				{Path: fmt.Sprintf("log-%d.txt", i), Quote: strings.Repeat("q", 2000)},
				{Path: fmt.Sprintf("other-%d.txt", i), Quote: strings.Repeat("z", 2000)},
			}},
		)
	}
	got := conversationCitations(messages, "r7")
	total := 0
	for _, citation := range got {
		total += len(citation.Quote)
		if len(citation.Quote) != 2000 {
			t.Fatalf("a quote was truncated to %d bytes", len(citation.Quote))
		}
	}
	if total > maxConversationFixQuoteBytes {
		t.Fatalf("selected %d quote bytes, over the %d budget", total, maxConversationFixQuoteBytes)
	}
	// The selected answer's own evidence must survive the budget.
	if len(got) == 0 || got[0].Path != "log-7.txt" {
		t.Fatalf("newest evidence was dropped: %+v", got)
	}
}

func TestServiceAnalysisFixCandidateAcceptsUsablePreliminaryAnalysis(t *testing.T) {
	reply := Reply{
		Answer: "The artifact shows the terminal branch never records Ready.", Assessment: "supports",
		Citations:        []Citation{{Path: "artifacts/junit.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		ProposedRevision: &Revision{RootCause: "The terminal branch omits Ready.", SuggestedFix: "Record Ready before returning."},
	}
	for _, tc := range []struct {
		name     string
		warnings []string
	}{
		{name: "remediation warning", warnings: []string{models.AnalysisWarningRemediation}},
		{name: "artifact grounding warning", warnings: []string{models.AnalysisWarningArtifactGrounding}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analyzed := analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z")
			analyzed.AIAnalysis.Disposition = models.AnalysisDispositionPreliminary
			analyzed.AIAnalysis.DispositionWarnings = tc.warnings
			service, session, requestID := exactFixServiceWithTest(t, reply, analyzed)
			_, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
			if err != nil {
				t.Fatalf("usable preliminary analysis was rejected: %v", err)
			}
		})
	}
}

func TestServiceCauseAnalysisFixCandidateSurvivesRepublishedPatternTimestamp(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{
		Builds: []string{"2", "1"}, RootCause: "same cause", Confidence: "high",
		Remediation: &models.PatternCausalGroupRemediation{BuildID: "2", SuggestedFix: "change the controller"},
	}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleActive}
	models.AssignPatternIdentity(&pattern)
	// publish writes one pass over the same cached verdict. Only the pattern's
	// generation timestamp differs between passes.
	publish := func(generatedAt string) {
		published := pattern
		published.GeneratedAt = generatedAt
		detail := causalPatternDetail(published, "1", "2")
		for i := range detail.Runs {
			run := &detail.Runs[i]
			run.RepoRefs = map[string]string{"example/repo": "main:" + exactFixSourceRevision}
			testCase := analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z")
			testCase.AIAnalysis.FileLinks = map[string]string{
				"pkg/controller.go": "https://github.com/example/repo/blob/" + exactFixSourceRevision + "/pkg/controller.go",
			}
			run.TestCases = []models.TestCase{testCase}
		}
		writeJobDetail(t, dir, detail)
	}
	publish("2026-08-12T12:00:00Z")
	runner := &fakeRunner{reply: Reply{
		Answer: "Both builds show the same controller defect.", Assessment: "supports",
		Citations: []Citation{{Path: "builds/2/build-log.txt", Quote: "same failure"}},
	}}
	service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	group := pattern.CausalGroups[0]
	session, err := service.Create(AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	publish("2026-08-12T12:30:00Z")
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); err != nil {
		t.Fatalf("Fix preflight after republished pattern error = %v", err)
	}
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatalf("Fix candidate after republished pattern error = %v (analysis changed = %v)",
			err, errors.Is(err, ErrAnalysisChanged))
	}
	if candidate.FixTarget.BuildID != "2" || candidate.FixTarget.TestName != "TestCluster" {
		t.Fatalf("candidate = %+v", candidate)
	}
}

func TestServiceCauseAnalysisFixCandidateAcceptsTransientWithoutSourceLinksOrCitations(t *testing.T) {
	dir := t.TempDir()
	pattern := causalPatternForChat([]models.PatternCausalGroup{{
		Builds: []string{"2", "1"}, RootCause: "same cause", Confidence: "high",
		Remediation: &models.PatternCausalGroupRemediation{BuildID: "2", SuggestedFix: "change the controller"},
	}}, nil)
	pattern.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleRecovered}
	models.AssignPatternIdentity(&pattern)
	detail := causalPatternDetail(pattern, "1", "2")
	for i := range detail.Runs {
		run := &detail.Runs[i]
		run.RepoRefs = map[string]string{"example/repo": "main:" + exactFixSourceRevision}
		testCase := analyzedTest("TestCluster", "junit.xml", "2026-08-13T01:00:00Z")
		testCase.AIAnalysis.Severity = "Transient-Ignore"
		testCase.AIAnalysis.FileLinks = nil
		run.TestCases = []models.TestCase{testCase}
	}
	writeJobDetail(t, dir, detail)
	runner := &fakeRunner{reply: Reply{
		Answer: "Investigate retry handling for the recurring timeout.", Assessment: "inconclusive",
		Unverified: true, UnverifiedReason: UnverifiedCitation,
	}}
	service, err := NewService(t.Context(), dir, runner, Options{StateDir: filepath.Join(dir, ".chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureSourceRepository(sourceinvestigation.Repository{Owner: "example", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	group := pattern.CausalGroups[0]
	session, err := service.Create(AnalysisRef{
		Scope: ScopeCause, JobID: pattern.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
		CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
	}, "Alice", testRequestID(t))
	if err != nil {
		t.Fatal(err)
	}
	requestID := testRequestID(t)
	if _, err := service.Send(t.Context(), session.ID, "Alice", requestID, "What should change?"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.AnalysisFixCandidate(session.ID, "Alice", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Analysis.Scope != ScopeCause || candidate.FixTarget.Scope != ScopeTest ||
		candidate.FixTarget.BuildID != "2" || candidate.FixTarget.TestName != "TestCluster" ||
		candidate.AnalysisContentHash == "" || candidate.SourceRepositorySnapshot.Revision != exactFixSourceRevision ||
		len(candidate.ArtifactCitations) != 0 ||
		!candidate.AssistantUnverified || candidate.AssistantUnverifiedReason != UnverifiedCitation {
		t.Fatalf("candidate = %+v", candidate)
	}
}
