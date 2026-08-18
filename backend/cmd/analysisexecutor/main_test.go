package main

import (
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestReadRequestRequiresFileAndIgnoresLegacyEnvironment(t *testing.T) {
	request := executorTestRequest(t)
	t.Setenv("PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64", "legacy")
	root := t.TempDir()
	if _, err := readRequest(root); err == nil {
		t.Fatal("legacy environment request was accepted without a request file")
	}
	if err := agentanalysis.WriteWorkspaceExecutionRequestFile(root, request); err != nil {
		t.Fatal(err)
	}
	got, err := readRequest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != request.Hash {
		t.Fatalf("request hash=%s want=%s", got.Hash, request.Hash)
	}
}

func executorTestRequest(t *testing.T) agentanalysis.WorkspaceExecutionRequest {
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
