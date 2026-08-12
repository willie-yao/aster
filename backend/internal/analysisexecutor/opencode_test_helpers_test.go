package analysisexecutor

import (
	"context"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

func promptOpenCode(ctx context.Context, client *http.Client, baseURL, sessionID string, spec OpenCodeSpec) ([]byte, error) {
	instruction, err := agentanalysis.WorkspaceFinalizationInstruction([]agentanalysis.WorkspaceEvidenceHandle{{ID: "artifact-001", Root: agentanalysis.WorkspaceArtifactsDir, Path: "fixture.log", LineStart: 1, LineEnd: 1}})
	if err != nil {
		return nil, err
	}
	return promptOpenCodeFinalization(ctx, client, baseURL, sessionID, spec, instruction)
}
