package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/state"
)

// TestRejectsUnexpectedArguments: a command that takes no positional argument rejects
// one as a usage error (exit 2) before doing anything, instead of ignoring it.
func TestRejectsUnexpectedArguments(t *testing.T) {
	g := newGitRepo(t)
	path := filepath.Join(g.dir, ".smates", "state.json.gz")
	st := state.New()
	st.Suite("s").Tests["a_test.go"] = &state.Entry{Status: state.Pass}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-C", g.dir, "--stdout", "extra"},
		{"list", "-C", g.dir, "extra"},
		{"ls", "-C", g.dir, "extra"},
		{"doctor", "-C", g.dir, "extra"},
		{"select", "-C", g.dir, "extra"},
		{"state", "show", "-C", g.dir, "extra"},
		{"state", "path", "-C", g.dir, "extra"},
		{"state", "clear", "-C", g.dir, "extra"},
		{"state", "clear", "extra", "-C", g.dir},
		{"state", "push", "-C", g.dir, "extra"},
		{"state", "pull", "-C", g.dir, "extra"},
		{"hook", "uninstall", "-C", g.dir, "extra"},
		{"hook", "status", "-C", g.dir, "extra"},
		{"hook", "uninstall", "--claude", "-C", g.dir, "extra"},
		{"hook", "status", "--codex", "-C", g.dir, "extra"},
		{"guide", "extra"},
		{"version", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Main(args, &out, &errb); code != ExitUsage || !strings.Contains(errb.String(), "unexpected arguments") {
			t.Errorf("%v: exit %d, want %d\n%s%s", args, code, ExitUsage, out.String(), errb.String())
		}
	}
	// Nothing was done: the state is still there.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state cleared: %v", err)
	}
	// Must keep: runner arguments after "--" are not positional arguments of select.
	for _, args := range [][]string{
		{"select", "-C", g.dir, "--", "-t", "name"},
		{"select", "-C", g.dir, "--", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Main(args, &out, &errb); code == ExitUsage {
			t.Errorf("%v: exit %d\n%s", args, code, errb.String())
		}
	}
}

// TestStateReplaceTakesTheLock: `smates state pull` and `smates state clear` change the
// state under the lock a run's save takes, so that neither overwrites the other.
func TestStateReplaceTakesTheLock(t *testing.T) {
	g := newGitRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	g.git("init", "-q", "--bare", remote)
	g.git("remote", "add", "origin", remote)
	pushed := state.New()
	pushed.Suite("s").Tests["a_test.go"] = &state.Entry{Status: state.Pass, At: time.Now().UTC()}
	src := filepath.Join(t.TempDir(), "pushed.json.gz")
	if err := pushed.Save(src); err != nil {
		t.Fatal(err)
	}
	ref := stateRefPrefix + "main"
	if err := pushState(g.repo, src, "origin", ref, io.Discard); err != nil {
		t.Fatal(err)
	}
	pulled := func(path string) bool {
		st, err := state.Load(path)
		return err == nil && st.Suites["s"] != nil && st.Suites["s"].Tests["a_test.go"] != nil
	}
	for _, tc := range []struct {
		name   string
		args   []string
		before bool // a state exists before
		done   func(path string) bool
	}{
		{"pull", []string{"state", "pull", "--ref", ref}, false, pulled},
		{"pull over a state", []string{"state", "pull", "--ref", ref}, true, pulled},
		{"clear", []string{"state", "clear"}, true, func(path string) bool { _, err := os.Stat(path); return os.IsNotExist(err) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json.gz")
			if tc.before {
				if err := state.New().Save(path); err != nil {
					t.Fatal(err)
				}
			}
			was, _ := os.ReadFile(path)
			lock, err := state.AcquireLock(path+".lock", time.Second, state.LockStale)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan int)
			var errb bytes.Buffer
			go func() {
				done <- Main(append(tc.args, "-C", g.dir, "--state", path), io.Discard, &errb)
			}()
			select {
			case code := <-done:
				t.Fatalf("finished while the state was locked: exit %d\n%s", code, errb.String())
			case <-time.After(500 * time.Millisecond):
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, was) {
				t.Fatal("the state changed while locked")
			}
			lock.Release()
			if code := <-done; code != 0 {
				t.Fatalf("exit %d\n%s", code, errb.String())
			}
			if !tc.done(path) {
				t.Errorf("not done:\n%s", errb.String())
			}
		})
	}
}
