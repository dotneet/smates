package runner

import (
	"context"
	"os/exec"
	"time"
)

func runGroup(_ context.Context, cmd *exec.Cmd, _ time.Duration) error { return cmd.Run() }

// KillProcessGroups does nothing on Windows, where test commands run in no group of
// their own.
func KillProcessGroups() {}
