package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalcritic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/criticexecutor"
)

func main() {
	request, err := readRequest()
	if err != nil {
		emit(causalcritic.ExecutionResult{
			SchemaVersion: causalcritic.ExecutionSchemaVersion, ContractVersion: causalcritic.ContractVersion,
			PairHash: request.Input.PairHash, TerminalState: "failed",
			Usage: causalcritic.GatewayUsage{Status: "unavailable", Source: "gateway_response"}, FailureReason: err.Error(),
		})
		os.Exit(1)
	}
	result := criticexecutor.Execute(context.Background(), request, criticexecutor.Options{})
	emit(result)
	if result.TerminalState != "succeeded" {
		os.Exit(1)
	}
}

func readRequest() (causalcritic.ExecutionRequest, error) {
	encoded := strings.TrimSpace(os.Getenv(causalcritic.RequestEnv))
	if encoded == "" {
		return causalcritic.ExecutionRequest{}, fmt.Errorf("%s is required", causalcritic.RequestEnv)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return causalcritic.ExecutionRequest{}, fmt.Errorf("decode critic request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request causalcritic.ExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse critic request: %w", err)
	}
	if err := causalcritic.ValidateExecutionRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func emit(result causalcritic.ExecutionResult) {
	data, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"schema_version":1,"contract_version":"causal-critic-v1","terminal_state":"failed","usage":{"status":"unavailable","source":"gateway_response"},"failure_reason":"encode critic result"}`)
		return
	}
	fmt.Println(string(data))
}
