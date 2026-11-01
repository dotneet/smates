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
	return worktreeFixtureIn(t, extraConfig, t.TempDir())
}

// worktreeFixtureIn builds the fixture in dir, which may be reached through a symlink
// (as macOS temp dirs are: /var -> /private/var).
func worktreeFixtureIn(t *testing.T, extraConfig, dir string) *gitRepo {
	t.Helper()
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("SMATES_NO_STATE_SEED", "")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	g := newGitRepoIn(t, dir)
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
	// Next to the repository, through the same (possibly symlinked) parent; the path is
	// left unresolved, as a user would type it.
	dir := filepath.Join(filepath.Dir(g.dir), "wt-"+branch)
	g.git(append([]string{"worktree", "add", "-q", "-b", branch, dir}, extra...)...)
	return dir
}

// assertSamePath compares paths after resolving symlinks: git reports resolved paths
// (on macOS, temp dirs under /var are /private/var).
func assertSamePath(t *testing.T, what, got, want string) {
	t.Helper()
	resolve := func(p string) string {
		// The file itself may not exist yet: resolve the deepest existing parent.
		rest := ""
		for q := filepath.Clean(p); ; q = filepath.Dir(q) {
			if r, err := filepath.EvalSymlinks(q); err == nil {
				return filepath.Join(r, rest)
			}
			if filepath.Dir(q) == q {
				return filepath.Clean(p)
			}
			rest = filepath.Join(filepath.Base(q), rest)
		}
	}
	if resolve(got) != resolve(want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
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
	var dout, derr bytes.Buffer
	Main([]string{"doctor", "-C", wt}, &dout, &derr)
	if d := dout.String() + derr.String(); !strings.Contains(d, "2 tests recorded, no snapshot in this worktree yet") || strings.Contains(d, "snapshot from commit") {
		t.Errorf("doctor in the linked worktree:\n%s", d)
	}
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
	checkSharedWorktrees(t, worktreeFixture(t, "state_shared: true\n"))
}

// TestSharedStateWorktreesThroughSymlink reaches the repository and its worktrees
// through a symlinked directory, as on macOS (/var -> /private/var), where git
// reports the resolved paths.
func TestSharedStateWorktreesThroughSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dir := filepath.Join(link, "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	checkSharedWorktrees(t, worktreeFixtureIn(t, "state_shared: true\n", dir))
}

func checkSharedWorktrees(t *testing.T, g *gitRepo) {
	t.Helper()
	wt := addWorktree(t, g, "feat")
	mustRun(t, "run", "-C", g.dir, "--no-agent", "--all")

	common := filepath.Join(g.dir, ".git", "smates", "state.json.gz")
	for _, d := range []string{g.dir, wt} {
		out, _ := mustRun(t, "state", "path", "-C", d)
		assertSamePath(t, "state path in "+d, strings.TrimSpace(out), common)
	}
	if st := loadState(t, common); st.Empty() {
		t.Fatal("shared state empty after a run in the main worktree")
	}
	mainSnap := loadState(t, filepath.Join(g.dir, ".smates", "state.snapshot.json.gz"))
	if len(mainSnap.Files) == 0 || !mainSnap.Empty() {
		t.Errorf("main worktree snapshot: %d files, entries %v", len(mainSnap.Files), mainSnap.Suites)
	}
	// The linked worktree sees the entries but not the main worktree's snapshot.
	out, _ := mustRun(t, "state", "show", "-C", wt)
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

// sharedStore opens a shared-mode store of the repository at dir.
func sharedStore(t *testing.T, dir string) *stateStore {
	t.Helper()
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateShared: true}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	return openStore(repo, cfg, "", io.Discard)
}

// TestSharedLoadReadsOnce: another worktree saving while this process loads must not
// make the load's base newer than its state, or the next save would take the older
// copy for a change of this process and overwrite the other worktree's update.
func TestSharedLoadReadsOnce(t *testing.T) {
	g := worktreeFixture(t, "")
	wt := addWorktree(t, g, "feat")
	t0 := time.Now().Add(-time.Hour)
	ours, other := sharedStore(t, g.dir), sharedStore(t, wt)
	st := state.New()
	st.Suite("s").Tests["x"] = &state.Entry{Status: state.Pass, At: t0}
	if err := ours.Save(st); err != nil {
		t.Fatal(err)
	}

	reads := 0
	readSharedState = func(path string) ([]byte, error) {
		data, err := os.ReadFile(path)
		reads++
		if reads == 1 {
			// The other worktree saves a newer result of x right after this read.
			ost, lerr := other.Load()
			if lerr != nil {
				t.Fatal(lerr)
			}
			ost.Suite("s").Tests["x"] = &state.Entry{Status: state.Fail, At: t0.Add(time.Minute)}
			if serr := other.Save(ost); serr != nil {
				t.Fatal(serr)
			}
		}
		return data, err
	}
	t.Cleanup(func() { readSharedState = os.ReadFile })
	mine, err := ours.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 { // ours once, other once
		t.Errorf("shared state read %d times, want once per load", reads)
	}
	mine.Suite("s").Tests["y"] = &state.Entry{Status: state.Pass, At: time.Now()}
	if err := ours.Save(mine); err != nil {
		t.Fatal(err)
	}
	got := loadState(t, ours.path).Suites["s"].Tests
	if got["x"] == nil || got["x"].Status != state.Fail || got["y"] == nil {
		t.Errorf("the other worktree's newer x was overwritten: x=%+v y=%v", got["x"], got["y"] != nil)
	}
}

// TestSharedClearNotUndone: `state clear` in one worktree is not undone by the next
// save of a process that loaded the state before.
func TestSharedClearNotUndone(t *testing.T) {
	g := worktreeFixture(t, "state_seed: false\n")
	wt := addWorktree(t, g, "feat")
	ours := sharedStore(t, wt)
	st := state.New()
	st.Suite("s").Tests["old"] = &state.Entry{Status: state.Pass, At: time.Now()}
	if err := ours.Save(st); err != nil {
		t.Fatal(err)
	}
	mine, err := ours.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := sharedStore(t, g.dir).Clear(); err != nil {
		t.Fatal(err)
	}
	mine.Suite("s").Tests["new"] = &state.Entry{Status: state.Pass, At: time.Now()}
	if err := ours.Save(mine); err != nil {
		t.Fatal(err)
	}
	got := loadState(t, ours.path).Suites["s"].Tests
	if got["old"] != nil || got["new"] == nil {
		t.Errorf("after clear: %v", got)
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

// TestSharedFailedSaveKeepsSnapshot: when the shared entries cannot be saved, this
// worktree's snapshot must not advance either, or the next run would take the new
// content as tested and skip tests whose results were never recorded.
func TestSharedFailedSaveKeepsSnapshot(t *testing.T) {
	g := worktreeFixture(t, "")
	oldWait := state.LockWait
	state.LockWait = 100 * time.Millisecond
	t.Cleanup(func() { state.LockWait = oldWait })

	store := sharedStore(t, g.dir)
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	st.Files, st.Commit, st.Created = map[string]string{"src/lib": "v1"}, "c1", t0
	st.Suite("s").Tests["x"] = &state.Entry{Status: state.Pass, At: t0}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	checkSnap := func(what string) {
		t.Helper()
		snap := loadState(t, store.snapPath)
		if snap.Files["src/lib"] != "v1" || snap.Commit != "c1" || !snap.Created.Equal(t0) {
			t.Errorf("%s: snapshot advanced although the entries were not saved: %v %q %v", what, snap.Files, snap.Commit, snap.Created)
		}
	}
	advance := func() {
		st.Files, st.Commit, st.Created = map[string]string{"src/lib": "v2"}, "c2", time.Now()
		st.Suite("s").Tests["x"] = &state.Entry{Status: state.Fail, At: time.Now()}
	}

	// The lock is held by a live process.
	lock := store.path + ".lock"
	if err := os.WriteFile(lock, []byte("pid 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	advance()
	if err := store.Save(st); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("save with a held lock: %v", err)
	}
	checkSnap("held lock")
	os.Remove(lock)

	// The shared file cannot be written (a directory is in its way).
	store.path = filepath.Join(t.TempDir(), "blocked", "state.json.gz")
	if err := os.MkdirAll(store.path, 0o755); err != nil {
		t.Fatal(err)
	}
	advance()
	if err := store.Save(st); err == nil {
		t.Fatal("save to an unwritable shared path succeeded")
	}
	checkSnap("unwritable shared file")
}
