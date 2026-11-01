package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// worktreeFixture is a repository with a custom-runner suite (no real test runner)
// and a recorded state in the main worktree.
func worktreeFixture(t *testing.T, extraConfig string) *gitRepo {
	t.Helper()
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("SMATES_NO_STATE_SEED", "")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("smates.yml", `version: 1
orphans: all
suites:
  - name: s
    runner: custom
    root: .
    tests: ["t/*_check.py"]
    command: sh -c 'rc=0; [ $# -gt 0 ] || set -- t/*_check.py; for f; do python3 "$f" || rc=1; done; exit $rc' sh
    rules: [{tests: ["**"], deps: ["src/**"]}]
`+extraConfig)
	write(".gitignore", ".smates/\n")
	write("t/a_check.py", "assert open(\"src/lib\").read()\n")
	write("t/b_check.py", "pass\n")
	write("src/lib", "1")
	g.git("add", "-A")
	g.commit("fixture")
	return g
}

func smates(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func mustRun(t *testing.T, args ...string) (string, string) {
	t.Helper()
	code, out, errb := smates(t, args...)
	if code != 0 {
		t.Fatalf("smates %v: exit %d\n%s%s", args, code, out, errb)
	}
	return out, errb
}

func addWorktree(t *testing.T, g *gitRepo, branch string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), branch)
	g.git(append([]string{"worktree", "add", "-q", "-b", branch, dir}, extra...)...)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return dir
}

func loadState(t *testing.T, path string) *state.State {
	t.Helper()
	st, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSeedFromMainWorktree(t *testing.T) {
	g := worktreeFixture(t, "")
	o, e := mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
	mainState := loadState(t, filepath.Join(g.dir, ".smates", "state.json.gz"))
	if mainState.Empty() {
		t.Fatalf("main worktree recorded nothing\n%s%s", o, e)
	}

	wt := addWorktree(t, g, "feat")
	_, errb := mustRun(t, "select", "-C", wt, "--no-agent")
	if !strings.Contains(errb, "seeded this worktree from the main worktree") {
		t.Errorf("no seed message:\n%s", errb)
	}
	if strings.Count(errb, "seeded") != 1 {
		t.Errorf("want exactly one seed line:\n%s", errb)
	}
	seeded := loadState(t, filepath.Join(wt, ".smates", "state.json.gz"))
	if seeded.Empty() || len(seeded.Suites["s"].Tests) != len(mainState.Suites["s"].Tests) {
		t.Fatalf("seeded state: %+v", seeded.Suites)
	}
	if seeded.Commit != mainState.Commit || len(seeded.Files) == 0 {
		t.Errorf("the snapshot of an ancestor commit should be kept: commit %q, %d files", seeded.Commit, len(seeded.Files))
	}
	// Only once.
	if _, errb := mustRun(t, "select", "-C", wt, "--no-agent"); strings.Contains(errb, "seeded") {
		t.Errorf("seeded again:\n%s", errb)
	}
	out, _ := mustRun(t, "state", "show", "-C", wt)
	if !strings.Contains(out, "seeded:   from the main worktree") || !strings.Contains(out, "location: this worktree") {
		t.Errorf("state show:\n%s", out)
	}

	// A worktree on a commit the main worktree's snapshot is not in the history of
	// gets the entries but not the snapshot.
	base := g.git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(g.dir, "src", "lib"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git("add", "-A")
	g.commit("two")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
	old := addWorktree(t, g, "old", base)
	mustRun(t, "select", "-C", old, "--no-agent")
	st := loadState(t, filepath.Join(old, ".smates", "state.json.gz"))
	if st.Empty() || len(st.Files) != 0 || st.Commit != "" {
		t.Errorf("foreign snapshot kept: %d files, commit %q, empty %v", len(st.Files), st.Commit, st.Empty())
	}

	// `state clear` is not undone by seeding again.
	mustRun(t, "state", "clear", "-C", wt)
	if _, errb := mustRun(t, "select", "-C", wt, "--no-agent"); strings.Contains(errb, "seeded") {
		t.Errorf("seeded after clear:\n%s", errb)
	}
	if _, err := os.Stat(filepath.Join(wt, ".smates", "state.json.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state after clear + select: %v", err)
	}
}

func TestSeedOptOut(t *testing.T) {
	for _, c := range []struct{ name, config, env string }{
		{"env", "", "1"},
		{"config", "state_seed: false\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := worktreeFixture(t, c.config)
			mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
			t.Setenv("SMATES_NO_STATE_SEED", c.env)
			wt := addWorktree(t, g, "feat")
			if _, errb := mustRun(t, "select", "-C", wt, "--no-agent"); strings.Contains(errb, "seeded") {
				t.Errorf("seeded although disabled:\n%s", errb)
			}
			if _, err := os.Stat(filepath.Join(wt, ".smates", "state.json.gz")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("state written although seeding is off: %v", err)
			}
		})
	}
}

func TestSeedFromRemote(t *testing.T) {
	g := worktreeFixture(t, "")
	bare := t.TempDir()
	g.git("init", "-q", "--bare", bare)
	g.git("remote", "add", "origin", bare)
	g.git("push", "-q", "origin", "main")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")
	mustRun(t, "state", "push", "-C", g.dir)
	if err := os.Remove(filepath.Join(g.dir, ".smates", "state.json.gz")); err != nil {
		t.Fatal(err)
	}
	wt := addWorktree(t, g, "feat")
	_, errb := mustRun(t, "select", "-C", wt, "--no-agent")
	if !strings.Contains(errb, "seeded this worktree from origin refs/smates/state/main") {
		t.Errorf("no remote seed:\n%s", errb)
	}
	if st := loadState(t, filepath.Join(wt, ".smates", "state.json.gz")); st.Empty() {
		t.Error("remote seed wrote an empty state")
	}
	if _, err := os.Stat(filepath.Join(wt, ".smates", ".state.json.gz.pulled")); err != nil {
		t.Errorf("the fetch was not recorded for the hourly throttle: %v", err)
	}
}

func TestSharedStateWorktrees(t *testing.T) {
	g := worktreeFixture(t, "state_shared: true\n")
	wt := addWorktree(t, g, "feat")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")

	common := filepath.Join(g.dir, ".git", "smates", "state.json.gz")
	out, _ := mustRun(t, "state", "path", "-C", wt)
	if got := strings.TrimSpace(out); got != common {
		t.Errorf("state path in the worktree = %q, want %q", got, common)
	}
	if st := loadState(t, common); st.Empty() {
		t.Fatal("shared state empty after a run in the main worktree")
	}
	mainSnap := loadState(t, filepath.Join(g.dir, ".smates", "state.snapshot.json.gz"))
	if len(mainSnap.Files) == 0 || !mainSnap.Empty() {
		t.Errorf("main worktree snapshot: %d files, entries %v", len(mainSnap.Files), mainSnap.Suites)
	}
	// The linked worktree sees the entries but not the main worktree's snapshot.
	out, _ = mustRun(t, "state", "show", "-C", wt)
	if !strings.Contains(out, "location: shared by all worktrees") || !strings.Contains(out, "snapshot: 0 files") || !strings.Contains(out, "suite s: 2 tests") {
		t.Errorf("state show in the linked worktree:\n%s", out)
	}
	if _, errb := mustRun(t, "select", "-C", wt, "--no-agent"); strings.Contains(errb, "seeded") {
		t.Errorf("seeded although the shared state exists:\n%s", errb)
	}
	if _, err := os.Stat(filepath.Join(wt, ".smates", "state.json.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a worktree-local state was written in shared mode: %v", err)
	}
}

func TestSharedStateConcurrentWriters(t *testing.T) {
	g := worktreeFixture(t, "")
	dirs := []string{g.dir, addWorktree(t, g, "w1"), addWorktree(t, g, "w2")}
	cfg := &config.Config{StateShared: true}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	const rounds = 15
	var wg sync.WaitGroup
	for i, d := range dirs {
		repo, err := gitx.Open(d)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := openStore(repo, cfg, "", io.Discard)
			for r := range rounds {
				st, err := store.Load()
				if err != nil {
					t.Error(err)
					return
				}
				now := time.Now()
				st.Files = map[string]string{fmt.Sprintf("wt%d", i): fmt.Sprint(r)}
				st.Commit, st.Created = fmt.Sprintf("c%d", i), now
				st.Suite("s").Tests[fmt.Sprintf("wt%d/t%d", i, r)] = &state.Entry{Status: state.Pass, At: now}
				// Every writer also re-runs one test they all share.
				st.Suite("s").Tests["shared"] = &state.Entry{Status: state.Pass, At: now, DurationMs: int64(i)}
				st.Runs = append(st.Runs, state.Run{ID: fmt.Sprintf("%d-%d", i, r), At: now})
				if err := store.Save(st); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	common := filepath.Join(g.dir, ".git", "smates", "state.json.gz")
	st := loadState(t, common)
	if n := len(st.Suites["s"].Tests); n != len(dirs)*rounds+1 {
		t.Errorf("shared state has %d entries, want %d", n, len(dirs)*rounds+1)
	}
	if len(st.Runs) != len(dirs)*rounds {
		t.Errorf("shared state has %d run records, want %d", len(st.Runs), len(dirs)*rounds)
	}
	for i, d := range dirs {
		snap := loadState(t, filepath.Join(d, ".smates", "state.snapshot.json.gz"))
		if snap.Files[fmt.Sprintf("wt%d", i)] != fmt.Sprint(rounds-1) || len(snap.Files) != 1 {
			t.Errorf("worktree %d snapshot: %v", i, snap.Files)
		}
	}
	if _, err := os.Stat(common + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file left behind: %v", err)
	}
}

func TestSharedStateLock(t *testing.T) {
	g := worktreeFixture(t, "")
	repo, err := gitx.Open(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateShared: true}
	cfg.Normalize()
	store := openStore(repo, cfg, "", io.Discard)
	lock := store.path + ".lock"
	os.MkdirAll(filepath.Dir(lock), 0o755)

	oldWait := state.LockWait
	state.LockWait = 100 * time.Millisecond
	t.Cleanup(func() { state.LockWait = oldWait })

	st := state.New()
	st.Suite("s").Tests["x"] = &state.Entry{Status: state.Pass, At: time.Now()}
	// A live holder: the save gives up.
	if err := os.WriteFile(lock, []byte("pid 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("save with a held lock: %v", err)
	}
	// A crashed holder: the lock is taken over once stale.
	past := time.Now().Add(-state.LockStale - time.Minute)
	os.Chtimes(lock, past, past)
	if err := store.Save(st); err != nil {
		t.Fatalf("save with a stale lock: %v", err)
	}
	if loadState(t, store.path).Suites["s"].Tests["x"] == nil {
		t.Error("entry not saved")
	}
}
