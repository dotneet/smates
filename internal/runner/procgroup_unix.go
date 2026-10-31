//go:build !windows

package runner

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func runGroup(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	exited := make(chan struct{})
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		go func() {
			select {
			case <-exited:
			case <-time.After(grace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()
		return nil
	}
	err := cmd.Run()
	close(exited)
	if ctx.Err() != nil && cmd.Process != nil {
		// Stragglers that outlived the group leader.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}
