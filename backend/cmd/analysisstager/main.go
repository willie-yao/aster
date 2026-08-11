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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisstager"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

const requestEnv = "PROW_AI_ANALYSIS_STAGE_REQUEST_B64"

func main() {
	request, err := readRequest()
	if err == nil {
		err = analysisstager.Execute(context.Background(), request, analysisstager.Options{})
	}
	if err != nil {
		message := textutil.Truncate(strings.Join(strings.Fields(redact.Credentials(redact.URLs(err.Error()))), " "), 2048)
		fmt.Fprintln(os.Stderr, "analysis staging failed: "+message)
		os.Exit(1)
	}
	fmt.Println("analysis workspace staged")
}

func readRequest() (agentanalysis.WorkspaceStageRequest, error) {
	encoded := strings.TrimSpace(os.Getenv(requestEnv))
	if encoded == "" {
		return agentanalysis.WorkspaceStageRequest{}, fmt.Errorf("%s is required", requestEnv)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return agentanalysis.WorkspaceStageRequest{}, fmt.Errorf("decode stage request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request agentanalysis.WorkspaceStageRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse stage request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return request, fmt.Errorf("stage request contains trailing data")
	}
	if err := agentanalysis.ValidateWorkspaceStageRequestIdentity(request); err != nil {
		return request, err
	}
	return request, nil
}
