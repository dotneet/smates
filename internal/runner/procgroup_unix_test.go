//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A runner's children must not outlive a cancelled run, even ones that ignore SIGINT
// (a shell's background jobs do): in watch mode (a grace set on the context) and in a
// plain run stopped by SIGTERM or SIGHUP, which reach only smates.
func TestProcessGroupStopsWholeTree(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"watch", func() context.Context { return WithProcessGroup(context.Background(), 300*time.Millisecond) }},
		{"plain run", context.Background},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			rc := &RunContext{Suite: &Suite{}}
			ctx, cancel := context.WithCancel(c.ctx())
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
			if time.Since(start) > 15*time.Second {
				t.Fatal("cancelled run did not return")
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			deadline := time.Now().Add(10 * time.Second)
			for syscall.Kill(pid, 0) == nil {
				if time.Now().After(deadline) {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("grandchild %d survived the cancelled run", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// A command that exits while a process it started keeps its output open returns its
// own exit code once cmd.WaitDelay passes, instead of waiting for that process.
func TestExecInOutputHeldOpen(t *testing.T) {
	dir := t.TempDir()
	rc := &RunContext{Suite: &Suite{}}
	ctx := WithProcessGroup(context.Background(), 200*time.Millisecond)
	var out strings.Builder
	start := time.Now()
	code, err := rc.ExecIn(ctx, dir, []string{"sh", "-c", "(sleep 30; echo late) & echo $! > pid; exit 3"}, nil, &out, &out)
	if data, rerr := os.ReadFile(filepath.Join(dir, "pid")); rerr == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); pid > 0 {
			defer syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if err != nil || code != 3 {
		t.Errorf("code %d, err %v; want 3", code, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v for the background process", d)
	}
}

// KillProcessGroups (a second signal to smates) kills every running test command's
// group at once, including children that ignore SIGINT, without waiting for a grace.
func TestKillProcessGroups(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ctx := WithProcessGroup(context.Background(), time.Hour)
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap "" INT; sleep 60 & echo $! > "$0"; wait`, pidFile)
	done := make(chan error, 1)
	go func() { done <- runCmd(ctx, cmd) }()
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command did not start")
		}
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	KillProcessGroups()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command survived KillProcessGroups")
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(child, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(child, syscall.SIGKILL)
			t.Fatal("a child of the command survived KillProcessGroups")
		}
	}
	liveGroups.Range(func(k, _ any) bool {
		t.Errorf("group %v still registered after its command ended", k)
		return true
	})
}
