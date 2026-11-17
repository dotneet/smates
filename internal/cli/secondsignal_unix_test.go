//go:build !windows

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The first Ctrl-C (or SIGTERM) asks smates to stop and wind down; a second one
// terminates it, also while it waits for something that does not watch for
// cancellation (here the state's lock, which it waits for up to a minute).
func TestSecondSignalTerminates(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			g := newGitRepo(t)
			lock := filepath.Join(g.dir, ".smates", "state.json.gz.lock")
			os.MkdirAll(filepath.Dir(lock), 0o755)
			writeFileAt(t, lock, "pid 1\n") // a live holder's: not stale

			cmd := exec.Command(os.Args[0], "state", "merge", "-C", g.dir)
			cmd.Env = append(os.Environ(), "SMATES_TEST_MAIN=1")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			// Waiting for the lock by now (a signal that comes earlier terminates it
			// at once, which passes too).
			time.Sleep(time.Second)
			cmd.Process.Signal(sig)
			time.Sleep(300 * time.Millisecond)
			cmd.Process.Signal(sig)
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
				t.Fatalf("smates did not stop on a second %s\n%s", sig, stderr.String())
			}
		})
	}
}
