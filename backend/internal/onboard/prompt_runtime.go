package onboard

import (
	"context"
	"os"

	"github.com/willie-yao/aster/backend/internal/onboard/promptauthor"
	"github.com/willie-yao/aster/backend/internal/orka"
)

type failedPromptAuthor struct{ err error }

func (f failedPromptAuthor) Generate(context.Context, promptauthor.Spec) (promptauthor.Result, error) {
	return promptauthor.Result{Runtime: promptRuntimeOrka}, f.err
}

func newPromptAuthor(opts Options) promptauthor.Runtime {
	if effectivePromptAgentRuntime(opts) != promptRuntimeOrka {
		return promptauthor.NewOpenCodeRuntime()
	}
	agent, err := orka.NewAgentRuntimeFromEnv(orka.FromEnvConfig{
		Namespace:   opts.PromptOrkaNamespace,
		AgentRef:    opts.PromptOrkaAgentRef,
		GitSecret:   opts.PromptOrkaGitSecret,
		API:         opts.PromptOrkaAPI,
		MaxRetries:  1,
		Purpose:     orka.AgentPurposePromptAuthor,
		KubeContext: os.Getenv("ORKA_KUBE_CONTEXT"),
	})
	if err != nil {
		return failedPromptAuthor{err: err}
	}
	return &promptauthor.OpenCodeRuntime{Agent: agent, Runtime: promptRuntimeOrka, AgentOwnsProvider: true}
}
