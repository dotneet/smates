//go:build !windows

package trace

import (
	"os"
	"syscall"
)

var forwardedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
