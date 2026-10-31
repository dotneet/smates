package runner

import (
	"context"
	"os/exec"
	"time"
)

type procGroupKey struct{}

// WithProcessGroup makes test commands started with ctx run in their own process group
// (on Unix), so that cancelling ctx stops the whole tree a runner starts (workers, test
// binaries), not only its first process: the group gets SIGINT, then SIGKILL after
// grace. `smates watch` uses it to stop a run on quit or when files change; plain
// `smates run` leaves the children in the terminal's group, where Ctrl-C reaches them
// directly.
func WithProcessGroup(ctx context.Context, grace time.Duration) context.Context {
	return context.WithValue(ctx, procGroupKey{}, grace)
}

func processGroupGrace(ctx context.Context) (time.Duration, bool) {
	g, ok := ctx.Value(procGroupKey{}).(time.Duration)
	return g, ok
}

// runCmd runs cmd (created with exec.CommandContext(ctx, ...)).
func runCmd(ctx context.Context, cmd *exec.Cmd) error {
	grace, ok := processGroupGrace(ctx)
	if !ok {
		return cmd.Run()
	}
	return runGroup(ctx, cmd, grace)
}
