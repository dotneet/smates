package state

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A lock file left by a smates process of this host that no longer runs (killed while
// holding it) is broken at once; any holder that may still run keeps the lock until
// the stale timeout.
func TestLockOfDeadHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("processAlive cannot tell on Windows")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	dead := cmd.Process.Pid // exited and reaped
	for _, c := range []struct {
		name   string
		holder string
		broken bool
	}{
		{"dead process of this host", fmt.Sprintf("pid %d on %s at 2026-01-01T00:00:00Z (x)\n", dead, host), true},
		{"must keep: a live process of this host", fmt.Sprintf("pid %d on %s at 2026-01-01T00:00:00Z (x)\n", os.Getppid(), host), false},
		{"must keep: this process", fmt.Sprintf("pid %d on %s at 2026-01-01T00:00:00Z (x)\n", os.Getpid(), host), false},
		{"must keep: a dead pid of another host", fmt.Sprintf("pid %d on other-%s at 2026-01-01T00:00:00Z (x)\n", dead, host), false},
		{"must keep: a holder it cannot parse", fmt.Sprintf("pid %d\n", dead), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.lock")
			if err := os.WriteFile(path, []byte(c.holder), 0o644); err != nil {
				t.Fatal(err)
			}
			l, err := AcquireLock(path, 200*time.Millisecond, time.Hour)
			if got := err == nil; got != c.broken {
				t.Fatalf("acquired = %v (err %v), want %v", got, err, c.broken)
			}
			l.Release()
		})
	}
}
