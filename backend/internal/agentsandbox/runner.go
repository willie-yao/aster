// Package agentsandbox defines the business-neutral Agent Sandbox lifecycle seam.
package agentsandbox

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

const (
	Backend       = "agent-sandbox"
	WorkspaceRoot = "/workspace"
)

var (
	purposePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,29}[a-z0-9])?$`)
	envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// Spec describes one non-secret workload executed through Agent Sandbox.
type Spec struct {
	Purpose           string
	ExecutionID       string
	RequestEnv        string
	Request           []byte
	Timeout           time.Duration
	OutputLimitBytes  int64
	WritableWorkspace bool
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

// ValidateSpec checks the generic workload boundary before Kubernetes writes.
func ValidateSpec(spec Spec) error {
	if spec.Purpose != strings.TrimSpace(spec.Purpose) || !purposePattern.MatchString(spec.Purpose) {
		return fmt.Errorf("agent sandbox purpose is invalid")
	}
	if spec.RequestEnv != strings.TrimSpace(spec.RequestEnv) || !envNamePattern.MatchString(spec.RequestEnv) {
		return fmt.Errorf("agent sandbox request environment name is invalid")
	}
	const maxRequestBytes = 256 << 10
	if len(spec.Request) == 0 || len(spec.Request) > maxRequestBytes {
		return fmt.Errorf("agent sandbox request must be between 1 and %d bytes", maxRequestBytes)
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
	return nil
}
