//go:build linux || darwin

package analysisexecutor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"golang.org/x/sys/unix"
)

func TestExecuteRejectsFIFOResultWithoutBlocking(t *testing.T) {
	root, request := executorTestFixture(t)
	started := time.Now()
	result := Execute(context.Background(), request, Options{
		WorkspaceRoot: root, TempRoot: t.TempDir(),
		RunOpenCode: func(context.Context, OpenCodeSpec) ([]byte, error) {
			path := filepath.Join(root, agentanalysis.WorkspaceResultDir, agentanalysis.WorkspaceResultFile)
			if err := unix.Mkfifo(path, 0o600); err != nil {
				return nil, err
			}
			return executorAnalysisJSON(), nil
		},
	})
	if time.Since(started) > 2*time.Second {
		t.Fatal("FIFO result blocked executor finalization")
	}
	if result.TerminalState != engineruntime.TerminalFailed || !strings.Contains(result.FailureReason, "modified") {
		t.Fatalf("result=%+v", result)
	}
}
