package patterns

import (
	"reflect"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestMergeLastGoodMatrix(t *testing.T) {
	priorPattern := models.PatternAnalysis{Subject: "job", JobID: "job", GeneratedAt: "2026-07-28T00:00:00Z", BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedRootCause: "old", SharedBuilds: []string{"3", "2"}, SuggestedFix: "fix", Summary: "old"}
	models.AssignPatternIdentity(&priorPattern)
	prior := map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{priorPattern}}}
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 2}}}
	report, err := MergeLastGood(details, prior, result)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retained != 1 || details[0].PatternRefresh.State != models.PatternRefreshRetained || !reflect.DeepEqual(details[0].PatternAnalyses, prior["job"].PatternAnalyses) {
		t.Fatalf("report=%+v detail=%+v", report, details[0])
	}
}

func TestMergeLastGoodFreshNonSystemicRemovesPrior(t *testing.T) {
	priorPattern := models.PatternAnalysis{Subject: "job", JobID: "job", Systemic: true, SharedRootCause: "old", SuggestedFix: "fix", Summary: "old"}
	models.AssignPatternIdentity(&priorPattern)
	details := []models.JobDetail{eligibleJob("job")}
	details[0].PatternAnalyses = []models.PatternAnalysis{{Subject: "job", JobID: "job", GeneratedAt: "now", Systemic: false, Confidence: "low", Summary: "unrelated"}}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Succeeded: true}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{priorPattern}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current != 1 || len(CurrentRecurring(details)) != 0 || details[0].PatternAnalyses[0].ID == priorPattern.ID {
		t.Fatalf("report=%+v detail=%+v", report, details[0])
	}
}

func TestMergeLastGoodRejectsCorruptPriorIdentity(t *testing.T) {
	prior := models.PatternAnalysis{ID: "pattern", ContentHash: "wrong", JobID: "job", Systemic: true}
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{prior}}}, result); err == nil {
		t.Fatal("corrupt prior identity was accepted")
	}
}

func TestMergeLastGoodMarksMissingRetainedEvidence(t *testing.T) {
	prior := models.PatternAnalysis{JobID: "job", GeneratedAt: "old", Systemic: true, SharedBuilds: []string{"999"}, Summary: "old"}
	models.AssignPatternIdentity(&prior)
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	report, err := MergeLastGood(details, map[string]models.JobDetail{"job": {PatternAnalyses: []models.PatternAnalysis{prior}}}, result)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retained != 1 || details[0].PatternRefresh.EvidenceAvailable {
		t.Fatalf("report=%+v status=%+v", report, details[0].PatternRefresh)
	}
}

func TestMergeLastGoodRejectsCrossJobPriorPattern(t *testing.T) {
	prior := models.PatternAnalysis{JobID: "other-job", Systemic: true, Summary: "old"}
	models.AssignPatternIdentity(&prior)
	details := []models.JobDetail{eligibleJob("job")}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job": {JobID: "job", Attempts: 1}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{"job": {JobID: "job", PatternAnalyses: []models.PatternAnalysis{prior}}}, result); err == nil {
		t.Fatal("cross-job prior pattern was accepted")
	}
}
