package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestReadExecutionRequestFromFixedChunks(t *testing.T) {
	request := stagerCommandTestRequest(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := agentanalysis.EncodeWorkspaceExecutionRequestChunks(data)
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range chunks {
		t.Setenv(agentanalysis.WorkspaceExecutionRequestChunkEnv(index), value)
	}
	got, err := readExecutionRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != request.Hash {
		t.Fatalf("request hash=%s want=%s", got.Hash, request.Hash)
	}
}

func TestReadExecutionRequestRejectsSparseChunks(t *testing.T) {
	request := stagerCommandTestRequest(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := agentanalysis.EncodeWorkspaceExecutionRequestChunks(data)
	if err != nil {
		t.Fatal(err)
	}
	chunks[0], chunks[1] = "", chunks[0]
	for index, value := range chunks {
		t.Setenv(agentanalysis.WorkspaceExecutionRequestChunkEnv(index), value)
	}
	if _, err := readExecutionRequest(); err == nil {
		t.Fatal("sparse chunks were accepted")
	}
}

func TestSelectedModeSupportsRequestWriterOnly(t *testing.T) {
	if mode, err := selectedMode([]string{"request"}); err != nil || mode != "request" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	if _, err := selectedMode([]string{"unknown"}); err == nil {
		t.Fatal("unknown mode was accepted")
	}
}

func stagerCommandTestRequest(t *testing.T) agentanalysis.WorkspaceExecutionRequest {
	t.Helper()
	revision := strings.Repeat("a", 40)
	failure := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "specific failure"},
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(failure, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}, "Inspect this project.", []agentanalysis.WorkspaceFile{{Path: "logs/build.log", Size: 1, SHA256: strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	provider := modelprovider.Normalize(modelprovider.Config{
		CredentialMode: modelprovider.CredentialModeGateway, API: "chat_completions",
		Endpoint: "https://gateway.example.svc.cluster.local/v1/chat/completions", Model: "fixture",
		Auth: modelprovider.Auth{Type: modelprovider.AuthTypeNone},
	})
	request, err := agentanalysis.NewWorkspaceExecutionRequest(manifest, provider, time.Minute, 20, 200000, 64000, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
