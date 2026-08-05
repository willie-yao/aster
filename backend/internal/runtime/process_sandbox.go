package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// SandboxSpec describes one process and the additional resources a sandbox may
// expose beyond its minimal immutable platform runtime. Environment is the
// complete child environment. Path entries are recursive after symlink
// resolution, and writable paths are also readable. Empty resource allowlists
// deny that resource rather than making it unrestricted.
type SandboxSpec struct {
	Command        []string
	WorkDir        string
	HomeDir        string
	TempDir        string
	Environment    []string
	ReadPaths      []string
	WritePaths     []string
	NetworkDomains []string
	UnixSockets    []string
	AllowLocalBind bool
}

// ProcessSandbox constructs a command under one process isolation policy.
// Enforcing backends must apply the requested allowlists or return an error.
type ProcessSandbox interface {
	Command(context.Context, SandboxSpec) (*exec.Cmd, error)
}

// ProcessSandboxRunner executes a command with backend-specific supervision.
// LocalAgentRuntime uses it when the sandbox provides one.
type ProcessSandboxRunner interface {
	Run(context.Context, SandboxSpec) ([]byte, error)
}

// directProcessSandbox preserves local execution without OS isolation. It is a
// transition backend until callers opt into an enforcing sandbox.
type directProcessSandbox struct{}

func (directProcessSandbox) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return nil, fmt.Errorf("runtime: sandbox command is required")
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.WorkDir
	cmd.Env = append([]string{}, spec.Environment...)
	configureProcessTreeCancellation(cmd)
	return cmd, nil
}

func configureProcessTreeCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
}
