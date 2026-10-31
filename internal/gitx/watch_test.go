package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScanTreeChanges(t *testing.T) {
	r := newTestRepo(t)
	r.write(".gitignore", "*.log\nbuild/\n")
	r.write("a.txt", "a\n")
	r.write("b.txt", "b\n")
	r.write("dir/c.txt", "c\n")
	r.commit("init")
	r.git("branch", "other")

	prev := r.scan(nil)
	step := func(name string, want ...string) {
		t.Helper()
		cur := r.scan(prev)
		got, err := r.repo.Changes(prev, cur)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: changes %q, want %q", name, got, want)
		}
		prev = cur
	}
	step("nothing")

	r.write("a.txt", "a2\n")
	r.write("new/deep/n.txt", "n\n")
	r.write("x.log", "ignored\n")
	r.write("build/out.txt", "ignored\n")
	step("edit and create", "a.txt", "new/deep/n.txt")

	r.write("a.txt", "a3\n") // already dirty, edited again
	step("edit dirty file", "a.txt")

	r.git("add", "a.txt")
	step("staging is no change")

	r.git("commit", "-q", "-m", "a")
	step("committing is no change")

	r.write("b.txt", "b2\n")
	step("edit", "b.txt")
	r.git("checkout", "-q", "--", "b.txt")
	step("revert", "b.txt")

	os.Remove(filepath.Join(r.dir, "dir", "c.txt"))
	step("delete", "dir/c.txt")
	r.git("checkout", "-q", "--", "dir/c.txt")
	step("restore", "dir/c.txt")

	// Switching branches changes clean files: told apart by the commits' diff.
	os.RemoveAll(filepath.Join(r.dir, "new"))
	step("remove untracked", "new/deep/n.txt")
	r.git("checkout", "-q", "other")
	step("checkout", "a.txt")
}

func TestScanTreeRacyRewrite(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.commit("init")
	r.write("a.txt", "two\n")
	p := filepath.Join(r.dir, "a.txt")
	fi, _ := os.Stat(p)
	prev := r.scan(nil)
	if !prev.Files["a.txt"].Racy {
		t.Fatal("a file written just now must be racy")
	}
	// Same size, same mtime (a coarse clock): only the content tells.
	r.write("a.txt", "six\n")
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	cur := r.scan(prev)
	got, err := r.repo.Changes(prev, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("changes %q, want [a.txt]", got)
	}
}

func (r *testRepo) scan(prev *TreeState) *TreeState {
	r.t.Helper()
	var also map[string]FileSig
	if prev != nil {
		also = prev.Dirty()
	}
	ts, err := r.repo.ScanTree(also)
	if err != nil {
		r.t.Fatal(err)
	}
	return ts
}
