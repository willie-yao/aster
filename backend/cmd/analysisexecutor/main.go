package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/analysisexecutor"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("analysisexecutor version=%s commit=%s image=%s go=%s\n", version, commit, imageTag, runtime.Version())
		return
	}
	request, err := readRequest(agentanalysis.WorkspaceExecutionRequestRoot)
	if err != nil {
		emit(agentanalysis.WorkspaceExecutionResult{
			Version: agentanalysis.WorkspaceResultVersion, ContractVersion: agentanalysis.WorkspaceContractVersion,
			TerminalState: engineruntime.TerminalFailed, FailureReason: err.Error(), Usage: agentanalysis.WorkspaceUsage{},
		})
		os.Exit(1)
	}
	result := analysisexecutor.Execute(context.Background(), request, analysisexecutor.Options{})
	emit(result)
	if result.TerminalState != engineruntime.TerminalSucceeded {
		os.Exit(1)
	}
}

func readRequest(root string) (agentanalysis.WorkspaceExecutionRequest, error) {
	return agentanalysis.ReadWorkspaceExecutionRequestFile(root)
}

func emit(result agentanalysis.WorkspaceExecutionResult) {
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"version":1,"contract_version":"agent-analysis-workspace-v8","terminal_state":"failed","failure_reason":"encode execution result","usage":{"available":false}}`)
		return
	}
	fmt.Println(string(data))
}
