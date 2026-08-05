package onboard

import (
	"context"
	"errors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard/promptauthor"
	"strings"
	"testing"
)

type fakePromptAuthor struct {
	result promptauthor.Result
	err    error
}

func (f fakePromptAuthor) Generate(context.Context, promptauthor.Spec) (promptauthor.Result, error) {
	return f.result, f.err
}
func TestBuildAgentPrompt(t *testing.T) {
	input := promptTestInput("Project", []promptSource{{Path: "README.md", Text: "docs"}})
	input.SourceRepo.Branch = "main"
	input.Jobs = []promptJobSummary{{Name: "periodic-project", Type: "periodic", Repo: "example/project", Branches: []string{"main"}}}
	body, result, err := buildAgentPrompt(context.Background(), Options{PromptAgentModel: "github-copilot/claude-sonnet-4.6"}, scaffoldData{Name: "Project"}, input, fakePromptAuthor{result: promptauthor.Result{Body: "agent prompt"}})
	if err != nil || body != "agent prompt" || result.Status != promptStatusAgentDraft {
		t.Fatalf("body=%q result=%+v err=%v", body, result, err)
	}
	body, result, err = buildAgentPrompt(context.Background(), Options{}, scaffoldData{Name: "Project"}, input, fakePromptAuthor{err: errors.New("unavailable")})
	if err != nil || result.Status != promptStatusAgentFallback || !strings.Contains(result.Handoff, "periodic-project") || !strings.Contains(body, "## Architecture") {
		t.Fatalf("fallback result=%+v err=%v", result, err)
	}
}
func TestValidatePromptMode(t *testing.T) {
	if validatePromptMode("bad") == nil {
		t.Fatal("expected invalid mode")
	}
}
