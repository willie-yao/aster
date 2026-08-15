package fetcher

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

func cohortTestWork(jobID, buildID, testName, message, body string) aiWork {
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: buildID}}
	testCase := &models.TestCase{
		Name: testName, Status: "failed", JUnitFile: "junit_01.xml", ClassName: "Kubernetes e2e suite",
		FailureMessage: message, FailureBody: body,
	}
	return aiWork{jobID: jobID, run: run, tc: testCase}
}

func TestGroupAnalysisFailureCohorts(t *testing.T) {
	first := cohortTestWork("job", "1", "DRA test alpha", "device claim failed for DRA test alpha at 0x1234", "request 123e4567-e89b-12d3-a456-426614174000 failed")
	second := cohortTestWork("job", "1", "DRA test beta", "device claim failed for DRA test beta at 0xabcd", "request 9f8e7d6c-5b4a-3210-9999-123456789abc failed")
	differentBuild := cohortTestWork("job", "2", "test C", second.tc.FailureMessage, second.tc.FailureBody)
	differentMessage := cohortTestWork("job", "1", "test D", "different failure", second.tc.FailureBody)
	empty := cohortTestWork("job", "1", "test E", "", "")
	buildSubject := cohortTestWork("job", "1", "Prow job execution", "device claim failed", "request failed")
	buildSubject.tc.Source = models.TestCaseSourceBuild

	work := []aiWork{first, differentBuild, second, differentMessage, empty, buildSubject}
	groups := groupAnalysisFailureCohorts(work)
	if len(groups) != 1 || len(groups[0].Work) != 2 || groups[0].Work[0].tc.Name != "DRA test alpha" || groups[0].Work[1].tc.Name != "DRA test beta" {
		t.Fatalf("groups = %+v", groups)
	}
	telemetry := analysisFailureCohortStats(work)
	if telemetry != (analysisFailureCohortTelemetry{Groups: 1, Candidates: 2, PotentialTasksSaved: 1, LargestGroup: 2}) {
		t.Fatalf("telemetry = %+v", telemetry)
	}
}

func TestAnalysisFailureCohortSignatureDoesNotReplaceShortTestNames(t *testing.T) {
	first := cohortTestWork("job", "1", "a", "fatal", "same body")
	second := cohortTestWork("job", "1", "i", "fitil", "same body")
	left, leftOK := analysisFailureCohortSignature(first)
	right, rightOK := analysisFailureCohortSignature(second)
	if !leftOK || !rightOK || left == right {
		t.Fatalf("signatures = %q and %q", left, right)
	}
}

func TestAnalysisFailureCohortSignaturePreservesEvidenceBoundaries(t *testing.T) {
	base := cohortTestWork("job", "1", "test A", "same failure", "same body")
	baseSignature, ok := analysisFailureCohortSignature(base)
	if !ok || baseSignature == "" {
		t.Fatal("base signature unavailable")
	}
	cases := []struct {
		name   string
		mutate func(*aiWork)
	}{
		{name: "job", mutate: func(work *aiWork) { work.jobID = "other" }},
		{name: "build", mutate: func(work *aiWork) { work.run.BuildID = "2" }},
		{name: "junit", mutate: func(work *aiWork) { work.tc.JUnitFile = "junit_02.xml" }},
		{name: "class", mutate: func(work *aiWork) { work.tc.ClassName = "other" }},
		{name: "message", mutate: func(work *aiWork) { work.tc.FailureMessage = "other" }},
		{name: "body", mutate: func(work *aiWork) { work.tc.FailureBody = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := cohortTestWork(base.jobID, base.run.BuildID, base.tc.Name, base.tc.FailureMessage, base.tc.FailureBody)
			tc.mutate(&work)
			got, ok := analysisFailureCohortSignature(work)
			if !ok || got == baseSignature {
				t.Fatalf("signature = %q, base = %q", got, baseSignature)
			}
		})
	}
}

func TestPlanAnalysisExecutionsUsesOneRepresentativePerCohort(t *testing.T) {
	first := cohortTestWork("job", "1", "DRA test alpha", "same DRA test alpha failure", "same body")
	second := cohortTestWork("job", "1", "DRA test beta", "same DRA test beta failure", "same body")
	single := cohortTestWork("job", "1", "single test", "different", "different")
	executions := planAnalysisExecutions([]aiWork{first, single, second})
	if len(executions) != 2 || len(executions[0].Work) != 2 || len(executions[1].Work) != 1 {
		t.Fatalf("executions = %+v", executions)
	}
}
