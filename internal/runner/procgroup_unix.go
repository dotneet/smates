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
	if err := cmd.Start(); err != nil {
		close(exited)
		return err
	}
	pgid := cmd.Process.Pid
	liveGroups.Store(pgid, struct{}{})
	err := cmd.Wait()
	liveGroups.Delete(pgid)
	close(exited)
	if ctx.Err() != nil && cmd.Process != nil {
		// Stragglers that outlived the group leader.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

// KillProcessGroups sends SIGKILL to the process group of every test command still
// running. smates calls it when a second signal terminates it before a cancelled
// run wound down: the groups are out of the terminal's reach, and the SIGKILL their
// grace would send lives in smates.
func KillProcessGroups() {
	liveGroups.Range(func(k, _ any) bool {
		_ = syscall.Kill(-k.(int), syscall.SIGKILL)
		return true
	})
}
