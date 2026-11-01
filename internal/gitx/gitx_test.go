package gitx

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// testRepo is a temporary git repository.
type testRepo struct {
	*testutil.GitRepo
	t    *testing.T
	dir  string
	repo *Repo
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	g := testutil.NewGitRepo(t, nil)
	repo, err := Open(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return &testRepo{GitRepo: g, t: t, dir: g.Dir, repo: repo}
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.Git(args...)
}

func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	r.Write(rel, content)
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	return r.Commit(msg)
}

func TestOpenResolvesRootFromSubdir(t *testing.T) {
	r := newTestRepo(t)
	r.write("a/b/c.txt", "x")
	repo, err := Open(filepath.Join(r.dir, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(r.dir)
	if repo.Root != want {
		t.Errorf("root %q, want %q", repo.Root, want)
	}
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("Open outside a repository must fail")
	}
}

func TestSnapshotMatchesHashObject(t *testing.T) {
	r := newTestRepo(t)
	r.write("clean.txt", "clean\n")
	r.write("modified.txt", "v1\n")
	r.write("staged.txt", "v1\n")
	r.write("deleted.txt", "bye\n")
	r.write("staged-deleted.txt", "bye\n")
	r.write("dir/nested file.txt", "spaces\n")
	r.write("ünïcode.txt", "u\n")
	r.write(".gitignore", "ignored.log\nbuild/\n")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("clean.txt", filepath.Join(r.dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	r.commit("init")

	r.write("modified.txt", "v2\n")
	r.write("staged.txt", "v2\n")
	r.git("add", "staged.txt")
	r.write("staged.txt", "v3\n") // staged and modified again: the working tree wins
	os.Remove(filepath.Join(r.dir, "deleted.txt"))
	r.git("rm", "-q", "staged-deleted.txt")
	r.write("untracked.txt", "new\n")
	r.write("newdir/deep/untracked.txt", "new2\n")
	r.write("ignored.log", "ignored\n")
	r.write("build/out.js", "ignored\n")
	r.write("added.txt", "added\n")
	r.git("add", "added.txt")

	snap, err := r.repo.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clean.txt", "modified.txt", "staged.txt", "dir/nested file.txt", "ünïcode.txt", ".gitignore", "untracked.txt", "newdir/deep/untracked.txt", "added.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "link")
	}
	for _, f := range want {
		got, ok := snap[f]
		if !ok {
			t.Errorf("%s missing from snapshot", f)
			continue
		}
		exp := r.git("hash-object", "--no-filters", "--", f)
		if f == "link" {
			exp = r.git("rev-parse", "HEAD:link")
		}
		if got != exp {
			t.Errorf("%s: %s, want %s", f, got, exp)
		}
	}
	for _, f := range []string{"deleted.txt", "staged-deleted.txt", "ignored.log", "build/out.js"} {
		if _, ok := snap[f]; ok {
			t.Errorf("%s must not be in the snapshot", f)
		}
	}
	if len(snap) != len(want) {
		var keys []string
		for k := range snap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("snapshot has %d entries, want %d: %v", len(snap), len(want), keys)
	}
}

func TestSnapshotEmptyRepo(t *testing.T) {
	r := newTestRepo(t)
	snap, err := r.repo.Snapshot()
	if err != nil || len(snap) != 0 {
		t.Fatalf("%v %v", snap, err)
	}
	r.write("a.txt", "a")
	snap, err = r.repo.Snapshot()
	if err != nil || snap["a.txt"] != HashBytes([]byte("a")) {
		t.Fatalf("untracked file in a repo without commits: %v %v", snap, err)
	}
	if r.repo.Head() != "" {
		t.Error("Head of an empty repo must be empty")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("hello\n"), 0o644)
	h, err := HashFile(p)
	// git hash-object of "hello\n"
	if err != nil || h != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("%s %v", h, err)
	}
	if HashBytes([]byte("hello\n")) != h {
		t.Error("HashBytes != HashFile")
	}
	if HashBytes(nil) != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Error("empty blob hash")
	}
	if _, err := HashFile(dir); err == nil {
		t.Error("directories are not hashable")
	}
	if _, err := HashFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file must error")
	}
	if runtime.GOOS != "windows" {
		os.Symlink("f", filepath.Join(dir, "l"))
		lh, err := HashFile(filepath.Join(dir, "l"))
		if err != nil || lh != HashBytes([]byte("f")) {
			t.Errorf("symlink hashes its target path: %s %v", lh, err)
		}
	}
	res := HashFiles(dir, []string{"f", "missing", "f"})
	if res[0] != h || res[1] != "" || res[2] != h {
		t.Errorf("HashFiles %v", res)
	}
	if len(HashFiles(dir, nil)) != 0 {
		t.Error("HashFiles(nil)")
	}
}

func TestChangedSinceAndMergeBase(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a")
	r.write("b.txt", "b")
	r.write("c.txt", "c")
	base := r.commit("base")
	r.git("checkout", "-q", "-b", "feature")
	r.write("a.txt", "a2")
	r.commit("feature")
	r.write("b.txt", "b2") // unstaged
	r.write("s.txt", "s")
	r.git("add", "s.txt") // staged new
	os.Remove(filepath.Join(r.dir, "c.txt"))
	r.write("u/new.txt", "u") // untracked
	r.write(".gitignore", "*.log\n")
	r.write("x.log", "ignored")

	got, err := r.repo.ChangedSince(base)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{".gitignore", "a.txt", "b.txt", "c.txt", "s.txt", "u/new.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ChangedSince = %v, want %v", got, want)
	}
	if _, err := r.repo.ChangedSince("no-such-ref"); err == nil {
		t.Error("unknown base must fail")
	}

	// main moves on; merge base stays at base.
	r.git("stash", "-u", "-q")
	r.git("checkout", "-q", "main")
	r.write("m.txt", "m")
	r.commit("main moves")
	r.git("checkout", "-q", "feature")
	if mb := r.repo.MergeBase("main"); mb != base {
		t.Errorf("MergeBase = %q, want %q", mb, base)
	}
	if mb := r.repo.MergeBase("nope"); mb != "" {
		t.Errorf("MergeBase of unknown ref = %q", mb)
	}
	if !r.repo.RefExists("main") || r.repo.RefExists("nope") || !r.repo.RefExists("HEAD~1") || r.repo.RefExists("HEAD~5") {
		t.Error("RefExists")
	}
	if r.repo.CurrentBranch() != "feature" {
		t.Errorf("CurrentBranch %q", r.repo.CurrentBranch())
	}
	r.git("checkout", "-q", "--detach")
	if r.repo.CurrentBranch() != "" {
		t.Error("detached HEAD has no branch")
	}
	if b, ok := r.repo.BlobContent(r.git("rev-parse", base+":a.txt")); !ok || string(b) != "a" {
		t.Errorf("BlobContent %q %v", b, ok)
	}
	if _, ok := r.repo.BlobContent("0000000000000000000000000000000000000000"); ok {
		t.Error("missing blob")
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Run("main local", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		if b := r.repo.DefaultBranch(); b != "main" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("master local", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		r.git("branch", "-m", "master")
		if b := r.repo.DefaultBranch(); b != "master" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("unknown falls back to main", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		r.git("branch", "-m", "trunk")
		if b := r.repo.DefaultBranch(); b != "main" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("origin HEAD wins", func(t *testing.T) {
		up := newTestRepo(t)
		up.write("a", "a")
		up.commit("x")
		up.git("branch", "-m", "develop")
		r := newTestRepo(t)
		r.git("remote", "add", "origin", up.dir)
		r.git("fetch", "-q", "origin")
		r.git("remote", "set-head", "origin", "develop")
		r.git("checkout", "-q", "-b", "main", "origin/develop")
		if b := r.repo.DefaultBranch(); b != "develop" {
			t.Errorf("got %q", b)
		}
	})
}

func TestFirstParentHistory(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "1")
	r.write("b.txt", "1")
	root := r.commit("root")
	r.write("a.txt", "2")
	r.git("mv", "b.txt", "c.txt")
	second := r.commit("second\twith tab")
	r.git("checkout", "-q", "-b", "feat")
	r.write("feat/x.txt", "x")
	r.commit("feat")
	r.git("checkout", "-q", "main")
	r.write("d.txt", "d")
	r.commit("third")
	r.git("merge", "-q", "--no-ff", "-m", "merge feat", "feat")

	commits, err := r.repo.FirstParentCommits("", 0)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range commits {
		subjects = append(subjects, c.Subject)
	}
	if strings.Join(subjects, "|") != "merge feat|third|second\twith tab|root" {
		t.Fatalf("subjects %q", subjects)
	}
	if commits[3].SHA != root || commits[3].Parent != "" || commits[2].SHA != second || commits[2].Parent != root {
		t.Errorf("commits %+v", commits)
	}
	if c, _ := r.repo.FirstParentCommits("", 2); len(c) != 2 {
		t.Errorf("limit: %d", len(c))
	}
	if c, _ := r.repo.FirstParentCommits(second, 0); len(c) != 2 || c[1].Subject != "third" {
		t.Errorf("since: %+v", c)
	}

	show := func(c Commit) string {
		ch, err := r.repo.ChangedInCommit(c)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, x := range ch {
			s = append(s, string(x.Status)+" "+x.Path)
		}
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	if got := show(commits[3]); got != "A a.txt,A b.txt" {
		t.Errorf("root: %s", got)
	}
	if got := show(commits[2]); got != "A c.txt,D b.txt,M a.txt" {
		t.Errorf("second: %s", got)
	}
	if got := show(commits[0]); got != "A feat/x.txt" {
		t.Errorf("merge (first-parent diff): %s", got)
	}
	for _, c := range commits {
		if c.Shallow {
			t.Errorf("full clone: %s marked shallow", c.Subject)
		}
	}
}

func TestFirstParentHistoryShallow(t *testing.T) {
	up := newTestRepo(t)
	for _, msg := range []string{"one", "two", "three"} {
		up.write("f"+msg, msg)
		up.write("common", msg)
		up.commit(msg)
	}
	dir := filepath.Join(t.TempDir(), "clone")
	up.git("clone", "-q", "--depth", "2", "file://"+up.dir, dir)
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: dir, repo: repo}
	commits, err := r.repo.FirstParentCommits("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].Shallow || !commits[1].Shallow || commits[1].Parent != "" {
		t.Fatalf("commits %+v", commits)
	}
	if ch, err := r.repo.ChangedInCommit(commits[0]); err != nil || len(ch) != 2 {
		t.Errorf("inside the clone: %v %v", ch, err)
	}
	if ch, err := r.repo.ChangedInCommit(commits[1]); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("boundary commit must not be diffed as a root: %v %v", ch, err)
	}
}
