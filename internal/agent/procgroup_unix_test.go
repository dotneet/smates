//go:build !windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noTerminal makes the agent command run in a process group of its own, as it does
// without a controlling terminal (CI), also when the tests run in one.
func noTerminal(t *testing.T) {
	old := hasTerminal
	hasTerminal = func() bool { return false }
	t.Cleanup(func() { hasTerminal = old })
}

// A command stopped by its context takes its children with it: the agent a wrapper
// script starts does not keep running (nor holding the output pipes) after Decide
// gave up on it.
func TestCommandBackendKillsChildren(t *testing.T) {
	noTerminal(t)
	f := newFixture(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cfg := testConfig()
	cfg.Command = writeScript(t, "cat >/dev/null; sleep 60 & echo $! > "+pidFile+".tmp; mv "+pidFile+".tmp "+pidFile+"; wait")
	cfg.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Stop it once the child runs (the brief before it may take seconds under load).
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	_, err := New(cfg).Decide(ctx, f.request(cfg))
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	// A killed orphan is reaped by init: give it a moment.
	for deadline := time.Now().Add(10 * time.Second); processExists(pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			exec.Command("kill", "-9", pid).Run()
			t.Fatalf("the command's child %s outlived the cancellation", pid)
		}
	}
}

// processExists reports whether pid is a live process (not a zombie).
func processExists(pid string) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// With a controlling terminal the command stays in smates' process group, where it
// can prompt on the terminal (in a background group, reading it would stop it).
func TestKillGroupOnCancelWithTerminal(t *testing.T) {
	old := hasTerminal
	t.Cleanup(func() { hasTerminal = old })
	for _, term := range []bool{false, true} {
		hasTerminal = func() bool { return term }
		cmd := exec.Command("true")
		killGroupOnCancel(cmd)
		if own := cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid; own == term {
			t.Errorf("terminal %v: own process group %v", term, own)
		}
	}
}
