//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel runs cmd in a process group of its own and kills the whole group
// when its context is done: an agent command is often a wrapper (a shell script, npx)
// whose agent process would otherwise outlive the timeout, keep working on the
// repository and hold the output pipes until WaitDelay.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
