package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisexecutor"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const requestEnv = "PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64"

func main() {
	request, err := readRequest()
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

func readRequest() (agentanalysis.WorkspaceExecutionRequest, error) {
	encoded := strings.TrimSpace(os.Getenv(requestEnv))
	if encoded == "" {
		return agentanalysis.WorkspaceExecutionRequest{}, fmt.Errorf("%s is required", requestEnv)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return agentanalysis.WorkspaceExecutionRequest{}, fmt.Errorf("decode analysis request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request agentanalysis.WorkspaceExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse analysis request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return request, fmt.Errorf("analysis request contains trailing data")
	}
	if err := agentanalysis.ValidateWorkspaceExecutionRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func emit(result agentanalysis.WorkspaceExecutionResult) {
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"version":1,"contract_version":"agent-analysis-workspace-v6","terminal_state":"failed","failure_reason":"encode execution result","usage":{"available":false}}`)
		return
	}
	fmt.Println(string(data))
}
