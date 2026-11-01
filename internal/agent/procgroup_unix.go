//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel runs cmd in a process group of its own and kills the whole group
// when its context is done: an agent command is often a wrapper (a shell script, npx)
// whose agent process would otherwise outlive the timeout, keep working on the
// repository and hold the output pipes until WaitDelay.
//
// Not when smates has a controlling terminal: an agent in a group of its own is in the
// terminal's background, where reading /dev/tty (an ssh passphrase, sudo, pinentry)
// stops it until the timeout. It then stays in smates' group, which Ctrl-C stops as a
// whole; only the command's own process is killed on a timeout.
func killGroupOnCancel(cmd *exec.Cmd) {
	if hasTerminal() {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// hasTerminal reports whether this process has a controlling terminal (a variable for
// tests, which run in one when started from a shell).
var hasTerminal = func() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
