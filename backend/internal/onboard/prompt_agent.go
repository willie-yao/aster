package onboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard/promptauthor"
	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func buildPromptHandoff(input promptDraftInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# prow-ai-dashboard prompt author handoff\n\nProject: %s\nSource repository: %s\n\n", input.ProjectName, input.SourceRepo.FullName)
	b.WriteString("Use the bundled system-prompt-generation skill. Write only prompts/system.md.\n\nMatched Prow jobs:\n")
	for _, j := range input.Jobs {
		fmt.Fprintf(&b, "- %s (%s; %s; %s)\n", j.Name, j.Type, j.Repo, strings.Join(j.Branches, ", "))
	}
	return b.String()
}

func buildAgentPrompt(ctx context.Context, opts Options, data scaffoldData, input promptDraftInput, author promptauthor.Runtime) (string, promptPreparationResult, error) {
	handoff := buildPromptHandoff(input)
	model := opts.PromptAgentModel
	if model == "" {
		model = "github-copilot/claude-sonnet-4.6"
	}
	ref := input.SourceRepo.Branch
	if ref == "" {
		ref = "main"
	}
	res, err := author.Generate(ctx, promptauthor.Spec{Repo: agentruntime.RepoRef{Owner: input.SourceRepo.Owner, Name: input.SourceRepo.Name, Ref: ref, Token: opts.GitHubToken}, Instruction: handoff, NativeModel: model, UseAmbientAuth: true, MaxTurns: 12, Timeout: opts.PromptTimeout})
	if err != nil {
		p, renderErr := render(systemPromptTmpl, data)
		return p, promptPreparationResult{Requested: promptRequestAgent, Status: promptStatusAgentFallback, Output: promptOutputTemplate, Handoff: handoff, Failure: &promptPreparationFailure{Stage: promptStageFinalPromptValidation, Category: promptFailureUnknown, cause: err}}, renderErr
	}
	return res.Body, promptPreparationResult{Requested: promptRequestAgent, Status: promptStatusAgentDraft, Output: promptOutputAgentDraft}, nil
}
