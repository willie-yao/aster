// Package agentsandbox defines the business-neutral Agent Sandbox lifecycle seam.
package agentsandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

const (
	Backend = "agent-sandbox"

	StagedWorkspaceRoot          = "/workspace"
	StagedWorkspaceInputPath     = "/input"
	StagedWorkspaceSourcePath    = "/workspace/source"
	StagedWorkspaceArtifactsPath = "/workspace/artifacts"
	StagedWorkspaceResultPath    = "/workspace/result"
)

var (
	purposePattern      = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,29}[a-z0-9])?$`)
	envNamePattern      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	manifestHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// StagedWorkspace describes one init-populated workspace with fixed mount boundaries.
type StagedWorkspace struct {
	RequestEnv string
	Request    []byte
}

// PreparedWorkspace mounts one immutable content-addressed input snapshot directly.
type PreparedWorkspace struct {
	ManifestHash string
	IdentityHash string
}

// Spec describes one non-secret workload executed through Agent Sandbox.
type Spec struct {
	Purpose           string
	ExecutionID       string
	RequestEnv        string
	Request           []byte
	Timeout           time.Duration
	OutputLimitBytes  int64
	WritableWorkspace bool
	StagedWorkspace   *StagedWorkspace
	PreparedWorkspace *PreparedWorkspace
	WorkObserver      engineruntime.WorkObserver
}

// Result records lifecycle output without interpreting the business contract.
type Result struct {
	Output         string
	FinishedReason string
	Duration       time.Duration
	Resources      engineruntime.ResourceMetadata
	Telemetry      engineruntime.GenerateTelemetry
	Work           *engineruntime.WorkRef
}

// Runner executes one bounded workload and confirms cleanup before returning.
type Runner interface {
	Run(context.Context, Spec) (Result, error)
	Cleanup(context.Context, engineruntime.WorkRef) error
	RuntimeIdentity() string
}

// ValidateSpec checks the generic workload boundary before Kubernetes writes.
func ValidateSpec(spec Spec) error {
	if spec.Purpose != strings.TrimSpace(spec.Purpose) || !purposePattern.MatchString(spec.Purpose) {
		return fmt.Errorf("agent sandbox purpose is invalid")
	}
	if spec.RequestEnv != strings.TrimSpace(spec.RequestEnv) || !envNamePattern.MatchString(spec.RequestEnv) {
		return fmt.Errorf("agent sandbox request environment name is invalid")
	}
	if len(spec.Request) == 0 || len(spec.Request) > 256<<10 {
		return fmt.Errorf("agent sandbox request must be between 1 and 262144 bytes")
	}
	if !utf8.Valid(spec.Request) || strings.IndexByte(string(spec.Request), 0) >= 0 {
		return fmt.Errorf("agent sandbox request must be valid UTF-8 without NUL bytes")
	}
	if spec.Timeout <= 0 || spec.Timeout > 30*time.Minute {
		return fmt.Errorf("agent sandbox timeout must be greater than zero and at most 30m")
	}
	if spec.OutputLimitBytes < 4<<10 || spec.OutputLimitBytes > 1<<20 {
		return fmt.Errorf("agent sandbox output limit must be between 4096 and 1048576")
	}
	if spec.ExecutionID != "" && (!utf8.ValidString(spec.ExecutionID) || len(spec.ExecutionID) > 128 || strings.ContainsAny(spec.ExecutionID, "\r\n\x00")) {
		return fmt.Errorf("agent sandbox execution id is invalid or oversized")
	}
	if spec.StagedWorkspace != nil && spec.PreparedWorkspace != nil {
		return fmt.Errorf("agent sandbox staged and prepared workspaces are mutually exclusive")
	}
	if spec.PreparedWorkspace != nil {
		if spec.WritableWorkspace || !manifestHashPattern.MatchString(spec.PreparedWorkspace.ManifestHash) || !manifestHashPattern.MatchString(spec.PreparedWorkspace.IdentityHash) {
			return fmt.Errorf("agent sandbox prepared workspace is invalid")
		}
	}
	if spec.StagedWorkspace != nil {
		if spec.WritableWorkspace {
			return fmt.Errorf("agent sandbox staged and writable workspaces are mutually exclusive")
		}
		stage := spec.StagedWorkspace
		if stage.RequestEnv != strings.TrimSpace(stage.RequestEnv) || !envNamePattern.MatchString(stage.RequestEnv) || stage.RequestEnv == spec.RequestEnv {
			return fmt.Errorf("agent sandbox stage request environment name is invalid")
		}
		if len(stage.Request) == 0 || len(stage.Request) > 95<<10 || !utf8.Valid(stage.Request) || strings.IndexByte(string(stage.Request), 0) >= 0 {
			return fmt.Errorf("agent sandbox stage request is invalid or oversized")
		}
	}
	return nil
}
