//go:build !windows

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/testutil"
)

// A SIGTERM to smates' process group (timeout(1), a cancelled CI job) stops the agent
// command too, although that runs in a process group of its own: smates cancels it.
func TestSIGTERMStopsAgentCommand(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	g := newGitRepo(t)
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	script := testutil.WriteMode(t, t.TempDir(), "agent.sh", "#!/bin/sh\ncat >/dev/null\nsleep 60 & echo $! > "+pidFile+".tmp; mv "+pidFile+".tmp "+pidFile+"\nwait\n", 0o755)
	testutil.Write(t, g.dir, "smates.yml", `version: 1
orphans: agent
agent:
  command: `+script+`
  timeout: 5m
suites:
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: sh -c 'for f; do sh "$f" || exit 1; done' sh
`)
	testutil.Write(t, g.dir, ".gitignore", ".smates/\n")
	testutil.Write(t, g.dir, "a/x_check.sh", "true\n")
	g.git("add", "-A")
	g.commit("fixture")
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "-C", g.dir, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("run --all: %d %s", code, errb.String())
	}
	testutil.Write(t, g.dir, "a/data.cfg", "x") // an orphan: the agent is asked

	// smates in a session of its own: a process group to signal, and no controlling
	// terminal (with one, the agent command would share smates' group).
	cmd := exec.Command(os.Args[0], "select", "-C", g.dir)
	cmd.Env = append(os.Environ(), "SMATES_TEST_MAIN=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	var pid string
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid = strings.TrimSpace(string(data))
			break
		}
		select {
		case <-exited:
			t.Fatalf("smates exited before the agent ran:\n%s", stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatalf("the agent did not start:\n%s", stderr.String())
		}
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("smates did not exit on SIGTERM:\n%s", stderr.String())
	}
	// A killed orphan is reaped by init: give it a moment.
	for deadline := time.Now().Add(10 * time.Second); processAlive(pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			exec.Command("kill", "-9", pid).Run()
			t.Fatalf("the agent's child %s outlived smates\n%s", pid, stderr.String())
		}
	}
}

// processAlive reports whether pid is a live process (not a zombie).
func processAlive(pid string) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
