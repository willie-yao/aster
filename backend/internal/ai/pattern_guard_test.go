package ai

import "testing"

func TestAnalyzePatternPublishesAnalysisOnlyContract(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	service.sourceRepoOwner, service.sourceRepoName = "example", "repo"
	pattern, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if err != nil || pattern == nil {
		t.Fatalf("pattern=%+v error=%v", pattern, err)
	}
	if pattern.FileLinks != nil || pattern.SourceRef != "" || pattern.RemediationVerification != nil {
		t.Fatalf("source remediation fields were published: %+v", pattern)
	}
}
