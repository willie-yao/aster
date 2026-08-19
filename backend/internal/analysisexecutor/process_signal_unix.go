//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package analysisexecutor

import (
	"os"
	"os/exec"
	"syscall"
)

func configureOpenCodeProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

func terminateOpenCodeProcess(process *os.Process) {
	if process == nil {
		return
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
		return
	}
	_ = process.Kill()
}

func escapeOpenCodeProcessGroupForTest() error { return syscall.Setpgid(0, 0) }

func processSignalName(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	switch status.Signal() {
	case syscall.SIGKILL:
		return "sigkill"
	case syscall.SIGTERM:
		return "sigterm"
	case syscall.SIGABRT:
		return "sigabrt"
	case syscall.SIGSEGV:
		return "sigsegv"
	case syscall.SIGBUS:
		return "sigbus"
	case syscall.SIGILL:
		return "sigill"
	case syscall.SIGQUIT:
		return "sigquit"
	case syscall.SIGINT:
		return "sigint"
	case syscall.SIGPIPE:
		return "sigpipe"
	case syscall.SIGXCPU:
		return "sigxcpu"
	case syscall.SIGXFSZ:
		return "sigxfsz"
	default:
		return "signal_other"
	}
}
