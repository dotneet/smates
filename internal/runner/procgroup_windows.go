package runner

import (
	"context"
	"os/exec"
	"time"
)

func runGroup(_ context.Context, cmd *exec.Cmd, _ time.Duration) error { return cmd.Run() }
