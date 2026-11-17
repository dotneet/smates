package runner

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// liveGroups holds the process group IDs of the test commands running now (Unix).
var liveGroups sync.Map

type procGroupKey struct{}

// defaultGrace is how long the process group of a cancelled test command has between
// SIGINT and SIGKILL when ctx sets no other grace (WithProcessGroup).
const defaultGrace = 5 * time.Second

// WithProcessGroup sets how long the process group of a test command started with ctx
// has to stop after ctx is cancelled: it gets SIGINT, then SIGKILL after grace.
func WithProcessGroup(ctx context.Context, grace time.Duration) context.Context {
	return context.WithValue(ctx, procGroupKey{}, grace)
}

func processGroupGrace(ctx context.Context) time.Duration {
	if g, ok := ctx.Value(procGroupKey{}).(time.Duration); ok {
		return g
	}
	return defaultGrace
}

// runCmd runs cmd (created with exec.CommandContext(ctx, ...)) in a process group of
// its own (on Unix), so that cancelling ctx stops the whole tree a runner starts (pool
// workers, test JVMs, test binaries), not only its first process: smates is cancelled
// on SIGTERM and SIGHUP (a CI job's timeout, a closed terminal), which reach only
// smates, and on Ctrl-C, which then reaches the tests through smates rather than from
// the terminal (they are no longer in its foreground process group). The group gets
// SIGINT, then SIGKILL after the grace.
//
// It returns about a grace after the command exits, or after the group was killed,
// even when a process it started keeps the command's output open (a daemon a test
// left running, or one that left the group): cmd.WaitDelay.
func runCmd(ctx context.Context, cmd *exec.Cmd) error {
	grace := processGroupGrace(ctx)
	cmd.WaitDelay = grace + time.Second
	return runGroup(ctx, cmd, grace)
}
