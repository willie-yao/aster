package fixpr

import (
	"fmt"
	"strings"

	"github.com/willie-yao/aster/backend/internal/runtime"
)

// ExecutionVerification is the retained tokenless validator result for one
// Agent Sandbox fix. Command output is intentionally omitted from persistence.
type ExecutionVerification struct {
	BaseSHA       string                     `json:"base_sha"`
	Commands      []runtime.ExecutionCommand `json:"commands"`
	Results       []runtime.CommandResult    `json:"results"`
	AllowFailures bool                       `json:"allow_failures,omitempty"`
}

func executionVerificationForAgent(agent *AgentConfig, result runtime.ExecutionResult, expectedBaseSHA string) (*ExecutionVerification, error) {
	if agent == nil || !agent.RequireCommandResults {
		return nil, nil
	}
	if err := runtime.ValidateSuccessfulCommandResults(agent.CommandPolicy.Commands, result.CommandResults); err != nil {
		return nil, fmt.Errorf("agent executor command results: %w", err)
	}
	verification := &ExecutionVerification{
		BaseSHA:  strings.TrimSpace(result.BaseSHA),
		Commands: cloneExecutionCommands(agent.CommandPolicy.Commands),
		Results:  cloneCommandResults(result.CommandResults),
	}
	if err := verification.validate(expectedBaseSHA); err != nil {
		return nil, fmt.Errorf("agent executor verification: %w", err)
	}
	return verification, nil
}

func executionVerificationForAnalysisAgent(agent *AgentConfig, result runtime.ExecutionResult, expectedBaseSHA string) (*ExecutionVerification, error) {
	if agent == nil || !agent.RequireCommandResults {
		return nil, nil
	}
	if err := runtime.ValidateCommandResults(agent.CommandPolicy.Commands, result.CommandResults); err != nil {
		return nil, fmt.Errorf("agent executor command results: %w", err)
	}
	verification := &ExecutionVerification{
		BaseSHA: strings.TrimSpace(result.BaseSHA), Commands: cloneExecutionCommands(agent.CommandPolicy.Commands),
		Results: cloneCommandResults(result.CommandResults), AllowFailures: true,
	}
	if err := verification.validate(expectedBaseSHA); err != nil {
		return nil, fmt.Errorf("agent executor verification: %w", err)
	}
	return verification, nil
}

func (v *ExecutionVerification) validate(baseSHA string) error {
	if v == nil {
		return nil
	}
	if strings.TrimSpace(v.BaseSHA) == "" || !strings.EqualFold(strings.TrimSpace(v.BaseSHA), strings.TrimSpace(baseSHA)) {
		return fmt.Errorf("executor result base SHA does not match the preview base")
	}
	var err error
	if v.AllowFailures {
		err = runtime.ValidateCommandResults(v.Commands, v.Results)
	} else {
		err = runtime.ValidateSuccessfulCommandResults(v.Commands, v.Results)
	}
	if err != nil {
		return fmt.Errorf("executor command results: %w", err)
	}
	return nil
}

func (v *ExecutionVerification) verifyResult() VerifyResult {
	if v == nil {
		return VerifyResult{Status: VerifySkipped, Summary: "verification not configured"}
	}
	labels := make([]string, 0, len(v.Commands))
	for index, command := range v.Commands {
		label := strings.Join(command.Argv, " ")
		labels = append(labels, label)
		if v.AllowFailures && (v.Results[index].TimedOut || v.Results[index].ExitCode != 0) {
			return VerifyResult{Status: VerifyFailed, Summary: label + " failed in Agent Sandbox"}
		}
	}
	return VerifyResult{Status: VerifyPassed, Summary: strings.Join(labels, " and ") + " passed in Agent Sandbox"}
}

func cloneExecutionVerification(in *ExecutionVerification) *ExecutionVerification {
	if in == nil {
		return nil
	}
	return &ExecutionVerification{
		BaseSHA: in.BaseSHA, Commands: cloneExecutionCommands(in.Commands), Results: cloneCommandResults(in.Results),
		AllowFailures: in.AllowFailures,
	}
}

func cloneExecutionCommands(in []runtime.ExecutionCommand) []runtime.ExecutionCommand {
	out := make([]runtime.ExecutionCommand, len(in))
	for i, command := range in {
		out[i] = runtime.ExecutionCommand{Argv: append([]string(nil), command.Argv...), TimeoutSeconds: command.TimeoutSeconds}
	}
	return out
}

func cloneCommandResults(in []runtime.CommandResult) []runtime.CommandResult {
	out := make([]runtime.CommandResult, len(in))
	for i, result := range in {
		out[i] = runtime.CommandResult{
			Argv: append([]string(nil), result.Argv...), ExitCode: result.ExitCode,
			DurationMs: result.DurationMs, TimedOut: result.TimedOut,
		}
	}
	return out
}
