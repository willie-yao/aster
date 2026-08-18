package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/analysisstager"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	stageRequestEnv   = "PROW_AI_ANALYSIS_STAGE_REQUEST_B64"
	publishRequestEnv = "PROW_AI_ANALYSIS_PUBLISH_REQUEST_B64"
	cleanupRequestEnv = "PROW_AI_ANALYSIS_CLEANUP_REQUEST_B64"
)

var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("analysisstager version=%s commit=%s image=%s go=%s\n", version, commit, imageTag, runtime.Version())
		return
	}
	ctx := context.Background()
	mode, err := selectedMode(os.Args[1:])
	var output any
	if err == nil {
		switch mode {
		case "stage":
			var request agentanalysis.WorkspaceStageRequest
			err = readRequest(stageRequestEnv, &request)
			var execution agentanalysis.WorkspaceExecutionRequest
			if err == nil {
				execution, err = readExecutionRequest()
			}
			if err == nil {
				err = analysisstager.Execute(ctx, request, execution, analysisstager.Options{})
			}
			output = map[string]any{"version": 1, "status": "staged", "manifest_hash": request.ManifestHash, "request_hash": execution.Hash}
		case "request":
			var request agentanalysis.WorkspaceExecutionRequest
			request, err = readExecutionRequest()
			if err == nil {
				err = analysisstager.WriteExecutionRequest(request, analysisstager.Options{})
			}
			output = map[string]any{"version": 1, "status": "written", "request_hash": request.Hash}
		case "publish":
			var request agentanalysis.WorkspacePublishRequest
			err = readRequest(publishRequestEnv, &request)
			if err == nil {
				output, err = analysisstager.Publish(ctx, request, analysisstager.PublishOptions{})
			}
		case "cleanup":
			var request agentanalysis.WorkspaceCleanupRequest
			err = readRequest(cleanupRequestEnv, &request)
			if err == nil {
				output, err = analysisstager.Cleanup(ctx, request, "")
			}
		}
	}
	if err != nil {
		message := textutil.Truncate(strings.Join(strings.Fields(redact.Credentials(redact.URLs(err.Error()))), " "), 2048)
		fmt.Fprintln(os.Stderr, "analysis staging failed: "+message)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, "analysis staging failed: encode terminal result")
		os.Exit(1)
	}
}

func selectedMode(args []string) (string, error) {
	if len(args) == 1 && args[0] == "request" {
		return "request", nil
	}
	if len(args) != 0 {
		return "", fmt.Errorf("analysis staging mode is invalid")
	}
	selected := ""
	for _, item := range []struct{ mode, name string }{{"stage", stageRequestEnv}, {"publish", publishRequestEnv}, {"cleanup", cleanupRequestEnv}} {
		mode, name := item.mode, item.name
		if strings.TrimSpace(os.Getenv(name)) == "" {
			continue
		}
		if selected != "" {
			return "", fmt.Errorf("exactly one analysis staging request is required")
		}
		selected = mode
	}
	if selected == "" {
		return "", fmt.Errorf("%s is required", stageRequestEnv)
	}
	return selected, nil
}

func readRequest(name string, target any) error {
	encoded := strings.TrimSpace(os.Getenv(name))
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode staging request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse staging request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("staging request contains trailing data")
	}
	return nil
}

func readExecutionRequest() (agentanalysis.WorkspaceExecutionRequest, error) {
	data, err := agentanalysis.DecodeWorkspaceExecutionRequestChunks(os.LookupEnv)
	if err != nil {
		return agentanalysis.WorkspaceExecutionRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request agentanalysis.WorkspaceExecutionRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("parse workspace execution request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return request, fmt.Errorf("workspace execution request contains trailing data")
	}
	if err := agentanalysis.ValidateWorkspaceExecutionRequest(request); err != nil {
		return request, err
	}
	return request, nil
}
