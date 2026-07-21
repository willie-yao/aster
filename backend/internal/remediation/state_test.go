package remediation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["x"] = &Remediation{ID: "x", FindingID: "finding", Attempts: []Attempt{{Number: 1, PRNumber: 7}}}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Remediations["x"]; got == nil || got.Attempts[0].PRNumber != 7 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if filepath.Base(filepath.Join(dir, FileName)) != FileName {
		t.Fatal("bad state filename")
	}
}

func TestEvidenceForPattern(t *testing.T) {
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: " timeout ", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "old", Status: "failed"}}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "timed out after 42 seconds", JUnitFile: "junit.xml"}}},
	}}}
	got := EvidenceForPattern(pattern, details)
	if got.PatternID != "pattern" || got.BuildWatermark != "2" || len(got.Tests) != 1 {
		t.Fatalf("evidence = %+v", got)
	}
	if got.Tests[0].Identity != "suite\x00class\x00test" || got.Tests[0].ErrorHash == "" {
		t.Fatalf("test evidence = %+v", got.Tests[0])
	}
}

func TestStateSaveWritesRedactedPublicProjection(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["x"] = &Remediation{
		ID: "x", JobID: "job", Evidence: Evidence{RootCause: "private root cause"},
		Attempts: []Attempt{{Number: 1, URL: "https://github.com/o/r/pull/7", Status: StatusOpen}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private root cause") || !strings.Contains(string(data), "pull/7") {
		t.Fatalf("public state = %s", data)
	}
}

func TestPublicProjectionUsesCurrentFindingID(t *testing.T) {
	state := NewState()
	state.Remediations["old"] = &Remediation{ID: "old", FindingID: "current"}
	public := state.Public()
	if _, ok := public.Remediations["current"]; !ok {
		t.Fatalf("public = %+v", public)
	}
	if _, ok := public.Remediations["old"]; ok {
		t.Fatalf("stale finding key published: %+v", public)
	}
}

func TestUntrackedPatternsExcludesEvidenceMatchedAttempt(t *testing.T) {
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "2"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "same"}},
	}}}}
	evidence := EvidenceForPattern(pattern, details)
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", Evidence: evidence,
		Attempts: []Attempt{{Number: 1, Status: StatusStillFailingSameCause}},
	}
	if got := UntrackedPatterns(state, []models.PatternAnalysis{pattern}, details); len(got) != 0 {
		t.Fatalf("untracked = %+v", got)
	}
}

func TestUntrackedPatternsKeepsDistinctCauseOnSameJob(t *testing.T) {
	oldPattern := models.PatternAnalysis{ID: "old", JobID: "job", Subject: "job", SharedBuilds: []string{"1"}}
	newPattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "old-test", Status: "failed", FailureMessage: "old"}}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "new-test", Status: "failed", FailureMessage: "new"}}},
	}}}
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", Evidence: EvidenceForPattern(oldPattern, details),
		Attempts: []Attempt{{Number: 1}},
	}
	if got := UntrackedPatterns(state, []models.PatternAnalysis{newPattern}, details); len(got) != 1 {
		t.Fatalf("untracked = %+v", got)
	}
}

func TestEvidenceForPatternPrefersQualifiedJobID(t *testing.T) {
	pattern := models.PatternAnalysis{JobID: "repo-b/job", Subject: "job", SharedBuilds: []string{"1"}}
	details := []models.JobDetail{
		{JobID: "repo-a/job", Name: "job", Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "wrong", Status: "failed"}}}}},
		{JobID: "repo-b/job", Name: "job", Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "right", Status: "failed"}}}}},
	}
	evidence := EvidenceForPattern(pattern, details)
	if len(evidence.Tests) != 1 || evidence.Tests[0].Name != "right" {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestClassificationUsesQualifiedTestIdentity(t *testing.T) {
	pattern := models.PatternAnalysis{JobID: "job", Subject: "job", SharedBuilds: []string{"3", "2", "1"}}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{
			{Name: "test", SuiteName: "suite", ClassName: "persistent", Status: "failed"},
			{Name: "test", SuiteName: "suite", ClassName: "target", Status: "failed"},
		}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{
			{Name: "test", SuiteName: "suite", ClassName: "persistent", Status: "failed"},
			{Name: "test", SuiteName: "suite", ClassName: "target", Status: "passed"},
		}},
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{
			{Name: "test", SuiteName: "suite", ClassName: "persistent", Status: "failed"},
			{Name: "test", SuiteName: "suite", ClassName: "target", Status: "failed"},
		}},
	}}}
	pattern.SharedBuilds = []string{"3", "1"}
	if got := classificationForPattern(pattern, details); got != string(models.ClassificationFlaky) {
		t.Fatalf("classification = %q", got)
	}
}

func TestClassificationPrefersFlakyAcrossMultipleTests(t *testing.T) {
	pattern := models.PatternAnalysis{JobID: "job", Subject: "job", SharedBuilds: []string{"3", "2", "1"}}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "3"}, TestCases: []models.TestCase{{Name: "persistent", Status: "failed"}, {Name: "flaky", Status: "failed"}}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "persistent", Status: "failed"}, {Name: "flaky", Status: "passed"}}},
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "persistent", Status: "failed"}, {Name: "flaky", Status: "failed"}}},
	}}}
	if got := classificationForPattern(pattern, details); got != string(models.ClassificationFlaky) {
		t.Fatalf("classification = %q", got)
	}
}

func TestLoadForRepoResetsAnotherTarget(t *testing.T) {
	dir := t.TempDir()
	state := NewStateForRepo("old/repo")
	state.Remediations["pattern"] = &Remediation{ID: "pattern"}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadForRepo(dir, "new/repo")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Repo != "new/repo" || len(loaded.Remediations) != 0 {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestLoadForRepoReturnsReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadForRepo(dir, "o/r"); err == nil {
		t.Fatal("read error was treated as empty state")
	}
}

func TestPublicProjectionRedactsObservationEvidence(t *testing.T) {
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern",
		Attempts: []Attempt{{
			Status: StatusInconclusive, OutcomeReason: "private/repo object secret-path",
			IneligibleCommits: map[string]string{"private-build": "secret-ineligible"},
			Observations: []BuildObservation{{
				BuildID: "1", JobName: "job", JobType: models.JobTypePeriodic,
				SourceRepo: "private/repo", SourceCommit: "secret-sha", HeadSHA: "secret-head",
				Result: "SUCCESS", Outcome: OutcomeInconclusive, Reason: "read private/repo/secret-path",
				MatchedTests: []string{"suite\x00class\x00test"}, FailedMatches: []string{"secret-failure"},
			}},
		}},
	}
	data, err := json.Marshal(state.Public())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private/repo", "secret-sha", "secret-head", "suite\\u0000class", "secret-failure"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("public projection leaked %q: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), `"build_id":"1"`) {
		t.Fatalf("public projection omitted build link data: %s", data)
	}
	if !strings.Contains(string(data), "Prow verification did not have enough evidence") ||
		!strings.Contains(string(data), "Prow evidence was incomplete") {
		t.Fatalf("public projection omitted safe reason text: %s", data)
	}
}
