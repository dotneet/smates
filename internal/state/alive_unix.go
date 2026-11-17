//go:build !windows

package state

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with pid runs (or may run: one this user may
// not signal counts as alive).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
