package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixexecutor"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const requestEnv = "PROW_AI_FIX_EXECUTION_REQUEST_B64"

func main() {
	request, err := readRequest()
	if err != nil {
		emit(engineruntime.ExecutionResult{
			Version: engineruntime.ExecutionContractVersion, Files: map[string]string{},
			TerminalState: engineruntime.TerminalFailed, FailureReason: err.Error(),
		})
		os.Exit(1)
	}
	result := fixexecutor.Execute(context.Background(), request, fixexecutor.Options{})
	emit(result)
	if result.TerminalState != engineruntime.TerminalSucceeded {
		os.Exit(1)
	}
}

func readRequest() (engineruntime.ExecutionRequest, error) {
	encoded := strings.TrimSpace(os.Getenv(requestEnv))
	if encoded == "" {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("%s is required", requestEnv)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return engineruntime.ExecutionRequest{}, fmt.Errorf("decode execution request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request engineruntime.ExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse execution request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func emit(result engineruntime.ExecutionResult) {
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"version":2,"terminal_state":"failed","failure_reason":"encode execution result"}`)
		return
	}
	fmt.Println(string(data))
}
