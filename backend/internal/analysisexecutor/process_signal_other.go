//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package analysisexecutor

import (
	"fmt"
	"os"
	"os/exec"
)

func configureOpenCodeProcessGroup(*exec.Cmd) {}

func terminateOpenCodeProcess(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}

func escapeOpenCodeProcessGroupForTest() error { return fmt.Errorf("process groups are unavailable") }

func processSignalName(*os.ProcessState) string { return "" }
