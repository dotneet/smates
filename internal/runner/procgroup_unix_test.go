//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A runner's children must not outlive a cancelled run, even ones that ignore SIGINT
// (a shell's background jobs do).
func TestProcessGroupStopsWholeTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	rc := &RunContext{Suite: &Suite{}}
	ctx, cancel := context.WithCancel(WithProcessGroup(context.Background(), 300*time.Millisecond))
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	_, _ = rc.ExecIn(ctx, dir, []string{"sh", "-c", "sleep 30 & echo $! > pid.tmp && mv pid.tmp pid; wait"}, nil, io.Discard, io.Discard)
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancelled run did not return")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the cancelled run", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
