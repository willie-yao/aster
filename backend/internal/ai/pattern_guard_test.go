package ai

import (
	"context"
	"sync/atomic"
	"testing"
)

type unusedPatternRepo struct {
	calls int32
}

func (r *unusedPatternRepo) ListTree(context.Context) ([]string, error) {
	atomic.AddInt32(&r.calls, 1)
	return []string{"controllers/machine.go"}, nil
}

func (r *unusedPatternRepo) ReadFile(context.Context, string) (string, bool, error) {
	atomic.AddInt32(&r.calls, 1)
	return "package controllers", true, nil
}

func TestAnalyzePatternDoesNotUseSourceToolsForAnalysisOnlyContract(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, patternToolResponse(sharedPatternResponse()))
	service := newPatternTestService(t, srv.URL)
	repo := &unusedPatternRepo{}
	service.SetPatternRepoReader(repo)
	service.SetSourceRepo("example", "repo")
	pattern, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3))
	if err != nil || pattern == nil {
		t.Fatalf("pattern=%+v error=%v", pattern, err)
	}
	if atomic.LoadInt32(&repo.calls) != 0 {
		t.Fatalf("source calls=%d", repo.calls)
	}
	if pattern.FileLinks != nil || pattern.SourceRef != "" || pattern.RemediationVerification != nil {
		t.Fatalf("source remediation fields were published: %+v", pattern)
	}
}
