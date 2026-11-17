package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// localStore opens the store of the repository at dir outside shared mode (flagPath:
// --state, "" for the default).
func localStore(t *testing.T, dir, flagPath string) *stateStore {
	t.Helper()
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	return openStore(repo, cfg, flagPath, io.Discard)
}

// Two smates processes of one worktree (`smates watch` and a pre-push hook) load the
// state, then save in turn: the second save keeps what the first added (entries, flake
// evidence, run records) instead of overwriting it with what it loaded. Without
// --state and with it.
func TestLocalSaveKeepsConcurrentChanges(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "default path"
		if explicit {
			name = "--state"
		}
		t.Run(name, func(t *testing.T) {
			g := newGitRepo(t)
			flagPath := ""
			if explicit {
				flagPath = filepath.Join(t.TempDir(), "s.json.gz")
			}
			t0 := time.Now().Add(-time.Hour)
			seed := state.New()
			seed.Suite("s").Tests["x"] = &state.Entry{Status: state.Pass, At: t0}
			seed.Suite("s").Tests["gone"] = &state.Entry{Status: state.Pass, At: t0}
			if err := localStore(t, g.dir, flagPath).Save(seed); err != nil {
				t.Fatal(err)
			}

			watch, hook := localStore(t, g.dir, flagPath), localStore(t, g.dir, flagPath)
			a, err := watch.Load()
			if err != nil {
				t.Fatal(err)
			}
			b, err := hook.Load()
			if err != nil {
				t.Fatal(err)
			}
			a.Suite("s").Tests["x"] = &state.Entry{Status: state.Fail, At: t0.Add(time.Minute), Flaky: &state.Flake{Count: 1, Last: t0.Add(time.Minute)}}
			a.AddRun(state.Run{ID: "watch", At: t0.Add(time.Minute)})
			if err := watch.Save(a); err != nil {
				t.Fatal(err)
			}
			b.Suite("s").Tests["y"] = &state.Entry{Status: state.Pass, At: t0.Add(2 * time.Minute)}
			delete(b.Suite("s").Tests, "gone") // pruned by a full run
			b.AddRun(state.Run{ID: "hook", At: t0.Add(2 * time.Minute)})
			if err := hook.Save(b); err != nil {
				t.Fatal(err)
			}

			got, err := localStore(t, g.dir, flagPath).Load()
			if err != nil {
				t.Fatal(err)
			}
			tests := got.Suites["s"].Tests
			if x := tests["x"]; x == nil || x.Status != state.Fail || x.Flaky == nil {
				t.Errorf("x lost the concurrent failure and flake evidence: %+v", x)
			}
			if tests["y"] == nil || tests["gone"] != nil {
				t.Errorf("entries: %v", slices.Sorted(func(yield func(string) bool) {
					for k := range tests {
						if !yield(k) {
							return
						}
					}
				}))
			}
			var ids []string
			for _, r := range got.Runs {
				ids = append(ids, r.ID)
			}
			if slices.Sort(ids); !slices.Equal(ids, []string{"hook", "watch"}) {
				t.Errorf("runs %v, want both", ids)
			}
			// The saving process holds the merged state.
			if b.Suites["s"].Tests["x"].Status != state.Fail {
				t.Errorf("the saved state does not hold the merged entries")
			}
		})
	}
}

// A local save waits for the lock like a shared one, and a lock held too long is an
// environment error.
func TestLocalSaveLocked(t *testing.T) {
	g := newGitRepo(t)
	s := localStore(t, g.dir, "")
	lockedState(t, s.path)
	err := s.Save(state.New())
	if err == nil || exitCodeFor(orEnvError(err)) != ExitEnv {
		t.Fatalf("save while locked: %v", err)
	}
}

// writeExecutableAtomic: a new hook is executable, an existing one becomes so where it
// is readable, and a symbolic link is written through (writeFileAtomic too).
func TestWriteAtomicModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	dir := t.TempDir()
	cases := []struct {
		name   string
		before os.FileMode // 0: no file
		exec   bool
		want   os.FileMode
	}{
		{"new hook", 0, true, 0o755},
		{"hook without execute bits", 0o644, true, 0o755},
		{"owner-only hook", 0o600, true, 0o700},
		{"executable hook", 0o750, true, 0o750},
		{"new file", 0, false, 0o644},
		{"file keeps its mode", 0o600, false, 0o600},
		{"file stays non-executable", 0o644, false, 0o644},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name)
			if c.before != 0 {
				writeFileAt(t, p, "old\n")
				if err := os.Chmod(p, c.before); err != nil {
					t.Fatal(err)
				}
			}
			write := writeFileAtomic
			if c.exec {
				write = writeExecutableAtomic
			}
			if err := write(p, []byte("new\n")); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != c.want {
				t.Errorf("mode %o, want %o", got, c.want)
			}
		})
	}

	// Through a symbolic link: the target is replaced, the link stays.
	target := filepath.Join(dir, "real.json")
	writeFileAt(t, target, "{}\n")
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("{\"a\": 1}\n")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "{\"a\": 1}\n" {
		t.Errorf("target: %q", data)
	}
	// No temporary file is left behind.
	if m, _ := filepath.Glob(filepath.Join(dir, ".smates-tmp-*")); len(m) > 0 {
		t.Errorf("left behind: %v", m)
	}
}

// `smates hook install --append` makes an existing hook that was not executable
// executable: git skips such a hook, and the smates block in it would never run.
func TestHookAppendMakesExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	g := newGitRepo(t)
	hook := filepath.Join(g.dir, ".git", "hooks", "pre-push")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	writeFileAt(t, hook, "#!/bin/sh\necho mine\n")
	var out, errb bytes.Buffer
	code := Main([]string{"hook", "install", "-C", g.dir, "--append"}, &out, &errb)
	errs := errb.String()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	fi, err := os.Stat(hook)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook not executable: %v %v\n%s", fi.Mode(), err, errs)
	}
}
