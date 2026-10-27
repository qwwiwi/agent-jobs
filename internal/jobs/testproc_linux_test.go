package jobs

import (
	"os/exec"
	"syscall"
)

// parentDeathSignalWorks: on Linux a test's child gets SIGKILL when the test process dies.
const parentDeathSignalWorks = true

// setParentDeathSignal has the kernel send SIGKILL to cmd's process once the test process (strictly:
// the thread that started it; the Go runtime does not end threads of unlocked goroutines) dies.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
